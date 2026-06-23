package controller_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

const (
	// watchPoolName is the in-scope NodePool whose key every positive watch event
	// must enqueue; offPoolName is an out-of-scope pool used to label objects that
	// must NOT enqueue a reconcile of an in-scope pool.
	watchPoolName = "np-watch"
	offPoolName   = "np-offscope"
	controllerNS  = "node-rotation-system"
)

// reconcileRecorder wraps the manager client and records every NodePool fetched
// by Reconcile's entry Get. The production RotationReconciler.Reconcile opens
// with `r.Get(ctx, req.NamespacedName, &NodePool{})` for each enqueued request,
// so recording NodePool Gets is a faithful proxy for "the controller observed a
// reconcile request for this pool" — it lets the test assert what the real
// SetupWithManager watches/predicates/map-funcs enqueued, without touching
// production code (SetupWithManager hardcodes Complete(r)).
type reconcileRecorder struct {
	client.Client

	mu   sync.Mutex
	seen map[string]int
}

func newReconcileRecorder(c client.Client) *reconcileRecorder {
	return &reconcileRecorder{Client: c, seen: map[string]int{}}
}

func (r *reconcileRecorder) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*karpv1.NodePool); ok {
		r.mu.Lock()
		r.seen[key.Name]++
		r.mu.Unlock()
	}
	return r.Client.Get(ctx, key, obj, opts...)
}

func (r *reconcileRecorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[name]
}

// waitEnqueuedAfter blocks until the named pool's reconcile count climbs past
// base — i.e. a reconcile was enqueued *after* the baseline was taken — then
// returns the new count (so it can chain as the next baseline). Callers must
// capture base from a quiescent point (waitQuiescent) immediately before the
// stimulus: an in-scope pool self-requeues every longRequeue/shortRequeue (60s /
// 30s), both well beyond this 20s deadline, so once the count has settled the
// only thing that can bump it within the window is the watch under test — never
// a stray self-requeue. Checking count > 0 instead (the old waitEnqueued) was a
// false positive: after the first NodePool-create reconcile the count is already
// nonzero, so every later positive case returned immediately without proving its
// watch actually fired.
func (r *reconcileRecorder) waitEnqueuedAfter(t *testing.T, name string, base int) int {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if got := r.count(name); got > base {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("reconcile for %q was not enqueued after the stimulus within 20s (count stuck at %d)", name, base)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitQuiescent waits until the named pool's reconcile count holds steady for a
// short settle window, then returns it as a clean baseline — so a following
// assertion (waitEnqueuedAfter for a positive, assertNoNewBeyond for a negative)
// measures only the next stimulus, not an earlier in-flight reconcile still
// draining. The settle window confirms the last reconcile finished ≥500ms ago,
// so its self-requeue (≥30s out for an in-scope pool) cannot fire inside the
// 20s/2s assertion window that follows.
func (r *reconcileRecorder) waitQuiescent(t *testing.T, name string) int {
	t.Helper()
	const settle = 500 * time.Millisecond
	deadline := time.After(10 * time.Second)
	last := r.count(name)
	stableSince := time.Now()
	for {
		select {
		case <-deadline:
			t.Fatalf("reconcile count for %q never settled within 10s", name)
		case <-time.After(50 * time.Millisecond):
			if got := r.count(name); got != last {
				last = got
				stableSince = time.Now()
			} else if time.Since(stableSince) >= settle {
				return last
			}
		}
	}
}

// TestSetupWithManagerWatches registers the production
// RotationReconciler.SetupWithManager path under envtest and asserts that each of
// the four watches (NodePool, labeled NodeClaim, placeholder Pod reaching
// Running, Node becoming Ready) enqueues the expected NodePool reconcile, while
// the negative cases (unlabeled NodeClaim, placeholder in another namespace,
// Pod without the surge markers, an already-Ready Node update) enqueue nothing.
// This exercises the real cache/watch/predicate/map-func wiring (issue #79),
// which the existing smoke test bypasses by registering a test-only controller.
func TestSetupWithManagerWatches(t *testing.T) {
	// envtest needs the etcd/kube-apiserver binaries that KUBEBUILDER_ASSETS
	// points at; 'make test' sets it, so skip rather than fail under a plain
	// 'go test ./...' (or 'go test -short') that runs without the assets.
	if testing.Short() || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest requires KUBEBUILDER_ASSETS; run via 'make test'")
	}

	testEnv := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			CRDs:  karpapis.CRDs,
			Paths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.New(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	rec := newReconcileRecorder(mgr.GetClient())
	reconciler := &controller.RotationReconciler{
		Client:            rec,
		Namespace:         controllerNS,
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
	}
	// The point of the test: register through the production wiring, not a
	// hand-rolled For(&NodePool{}) controller.
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var mgrErr error
	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		mgrErr = mgr.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-mgrDone
		if mgrErr != nil && !errors.Is(mgrErr, context.Canceled) {
			t.Errorf("manager exited with error: %v", mgrErr)
		}
	})

	// Watches must be established before any Create, otherwise an event can fire
	// before the informer is listening and never be observed.
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatal("cache did not sync within 30s")
	}

	// Test-side object setup/mutation uses a direct (uncached) API client so
	// read-after-write (the status subresource updates) is strongly consistent;
	// the controller still reads through the manager's informer cache, which is
	// what the watches observe.
	api, err := client.New(cfg, client.Options{Scheme: scheme.New()})
	if err != nil {
		t.Fatalf("direct client: %v", err)
	}
	mustCreate(t, ctx, api, namespace(controllerNS))

	// ── Positive: in-scope NodePool create enqueues itself ──────────────────
	// Nothing has touched watchPoolName yet, so baseline 0: the For(&NodePool{})
	// watch must drive its first reconcile.
	mustCreate(t, ctx, api, inScopeNodePool(watchPoolName))
	rec.waitEnqueuedAfter(t, watchPoolName, 0)

	// An out-of-scope pool exists so labeled-but-off-scope objects below have a
	// real target whose key must never be reconciled into an in-scope step.
	mustCreate(t, ctx, api, offScopeNodePool(offPoolName))

	// Each positive case below settles watchPoolName to a quiescent baseline
	// *immediately before* its stimulus, then requires the count to climb past
	// that baseline — so it proves the specific watch under test fired, not just
	// that some earlier reconcile (the NodePool create, or a self-requeue) had
	// already bumped the count.

	// ── Positive: labeled NodeClaim mapped to its NodePool ──────────────────
	t.Run("labeled NodeClaim enqueues its NodePool", func(t *testing.T) {
		nc := &karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "nc-watch",
				Labels: map[string]string{karpv1.NodePoolLabelKey: watchPoolName},
			},
			Spec: nodeClaimSpec(),
		}
		base := rec.waitQuiescent(t, watchPoolName)
		mustCreate(t, ctx, api, nc)
		rec.waitEnqueuedAfter(t, watchPoolName, base)
	})

	// ── Positive: placeholder Pod Pending -> Running ────────────────────────
	t.Run("placeholder Pod reaching Running enqueues its NodePool", func(t *testing.T) {
		ph := placeholderPod("nc-old", controllerNS, map[string]string{
			annotations.SurgeFor:    "nc-old",
			karpv1.NodePoolLabelKey: watchPoolName,
		})
		// Create lands Pending (no predicate match); the transition to Running is
		// what placeholderRunning() enqueues on. Baseline after the no-op create so
		// only the Running transition can satisfy the wait.
		mustCreate(t, ctx, api, ph)
		base := rec.waitQuiescent(t, watchPoolName)
		setPodRunning(t, ctx, api, ph)
		rec.waitEnqueuedAfter(t, watchPoolName, base)
	})

	// ── Positive: Node Ready=False -> Ready=True ────────────────────────────
	t.Run("Node becoming Ready enqueues its NodePool", func(t *testing.T) {
		n := labeledNode("node-watch", watchPoolName, corev1.ConditionFalse)
		mustCreate(t, ctx, api, n)
		base := rec.waitQuiescent(t, watchPoolName)
		setNodeReady(t, ctx, api, n, corev1.ConditionTrue)
		rec.waitEnqueuedAfter(t, watchPoolName, base)
	})

	// ── Positive: RotationPolicy change re-evaluates NodePools ──────────────
	// A RotationPolicy create maps to every NodePool (allNodePools), so the
	// governed in-scope pool re-reconciles. The selector matches workload=api only,
	// so offPoolName (workload=batch) stays unmatched and never self-requeues — the
	// negative cases below rely on that.
	t.Run("RotationPolicy change enqueues the matched NodePool", func(t *testing.T) {
		base := rec.waitQuiescent(t, watchPoolName)
		mustCreate(t, ctx, api, watchRotationPolicy())
		rec.waitEnqueuedAfter(t, watchPoolName, base)
	})

	// ── Negative cases ──────────────────────────────────────────────────────
	// Each labels (or namespaces) an object so the watch's predicate/map-func
	// drops it. The target key checked is offPoolName, whose reconcile must stay
	// flat: if a negative object leaked through, it would enqueue offPoolName.
	// offPoolName is out of scope, so it never self-requeues — once settled, any
	// later bump is necessarily a leaked watch event, not the periodic requeue.
	// Each case settles offPoolName to a baseline *before* applying its stimulus,
	// then asserts the count never climbs past that baseline within negativeWindow.

	t.Run("unlabeled NodeClaim enqueues nothing", func(t *testing.T) {
		nc := &karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "nc-manual"},
			Spec:       nodeClaimSpec(),
		}
		base := rec.waitQuiescent(t, offPoolName)
		mustCreate(t, ctx, api, nc)
		rec.assertNoNewBeyond(t, base)
	})

	t.Run("placeholder in another namespace enqueues nothing", func(t *testing.T) {
		mustCreate(t, ctx, api, namespace("other-ns"))
		ph := placeholderPod("nc-elsewhere", "other-ns", map[string]string{
			annotations.SurgeFor:    "nc-elsewhere",
			karpv1.NodePoolLabelKey: offPoolName,
		})
		mustCreate(t, ctx, api, ph)
		base := rec.waitQuiescent(t, offPoolName)
		setPodRunning(t, ctx, api, ph)
		rec.assertNoNewBeyond(t, base)
	})

	t.Run("Pod without surge markers enqueues nothing", func(t *testing.T) {
		// In the controller namespace, but missing surge-for and nodepool labels:
		// placeholderToNodePool must drop it even on reaching Running.
		p := placeholderPod("plain-pod", controllerNS, nil)
		mustCreate(t, ctx, api, p)
		base := rec.waitQuiescent(t, offPoolName)
		setPodRunning(t, ctx, api, p)
		rec.assertNoNewBeyond(t, base)
	})

	t.Run("already-Ready Node update with no transition enqueues nothing", func(t *testing.T) {
		// Create not-Ready, then flip to Ready via the status subresource — that
		// genuine False->True transition is the one nodeBecameReady enqueues on.
		// Settle offPoolName *before* the transition so the wait proves the
		// transition itself enqueued (count past the pre-transition baseline), not
		// offPoolName's earlier create reconcile. A subsequent Ready->Ready update
		// (a label edit) carries no readiness transition and must NOT re-enqueue.
		n := labeledNode("node-already-ready", offPoolName, corev1.ConditionFalse)
		mustCreate(t, ctx, api, n)
		preTransition := rec.waitQuiescent(t, offPoolName)
		setNodeReady(t, ctx, api, n, corev1.ConditionTrue)
		base := rec.waitEnqueuedAfter(t, offPoolName, preTransition)
		patchNodeLabel(t, ctx, api, n, "touched", "yes")
		rec.assertNoNewBeyond(t, base)
	})
}

// ── object builders / mutators ──────────────────────────────────────────────

func mustCreate(t *testing.T, ctx context.Context, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func nodeClaimSpec() karpv1.NodeClaimSpec {
	return karpv1.NodeClaimSpec{
		NodeClassRef: &karpv1.NodeClassReference{
			Group: "eks.amazonaws.com",
			Kind:  "NodeClass",
			Name:  "default",
		},
		Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
	}
}

func inScopeNodePool(name string) *karpv1.NodePool {
	return nodePool(name, map[string]string{"workload": "api"})
}

// watchRotationPolicy governs the workload=api NodePool with a well-formed
// all-week window, used to prove the RotationPolicy watch enqueues its pools.
func watchRotationPolicy() *noderotationv1alpha1.RotationPolicy {
	return &noderotationv1alpha1.RotationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "watch-policy"},
		Spec: noderotationv1alpha1.RotationPolicySpec{
			NodePoolSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"workload": "api"}},
			MaintenanceWindows: []noderotationv1alpha1.MaintenanceWindow{{
				Timezone: "UTC",
				Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
				Start:    "00:00",
				End:      "23:59",
			}},
		},
	}
}

func offScopeNodePool(name string) *karpv1.NodePool {
	return nodePool(name, map[string]string{"workload": "batch"})
}

func nodePool(name string, labels map[string]string) *karpv1.NodePool {
	return &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: "eks.amazonaws.com",
						Kind:  "NodeClass",
						Name:  "default",
					},
					Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
				},
			},
		},
	}
}

func placeholderPod(claim, ns string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      surge.PlaceholderName(claim),
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "pause",
				Image: "registry.k8s.io/pause:3.10",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
				},
			}},
		},
	}
}

func labeledNode(name, pool string, ready corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{karpv1.NodePoolLabelKey: pool},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}},
		},
	}
}

// setPodRunning moves a Pod's phase to Running via the status subresource,
// retrying on the inevitable resourceVersion conflicts under an active informer.
func setPodRunning(t *testing.T, ctx context.Context, c client.Client, ph *corev1.Pod) {
	t.Helper()
	retryStatus(t, ctx, c, types.NamespacedName{Namespace: ph.Namespace, Name: ph.Name}, &corev1.Pod{}, func(o client.Object) {
		o.(*corev1.Pod).Status.Phase = corev1.PodRunning
	})
}

// setNodeReady sets a Node's Ready condition via the status subresource.
func setNodeReady(t *testing.T, ctx context.Context, c client.Client, n *corev1.Node, status corev1.ConditionStatus) {
	t.Helper()
	retryStatus(t, ctx, c, types.NamespacedName{Name: n.Name}, &corev1.Node{}, func(o client.Object) {
		node := o.(*corev1.Node)
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	})
}

func retryStatus(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName, into client.Object, mutate func(client.Object)) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if err := c.Get(ctx, key, into); err != nil {
			t.Fatalf("get %s for status update: %v", key, err)
		}
		mutate(into)
		err := c.Status().Update(ctx, into)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			t.Fatalf("status update %s: %v", key, err)
		}
		select {
		case <-deadline:
			t.Fatalf("status update %s kept conflicting for 10s", key)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// patchNodeLabel edits a Node label (a non-readiness update) to prove a
// Ready->Ready transition does not enqueue.
func patchNodeLabel(t *testing.T, ctx context.Context, c client.Client, n *corev1.Node, k, v string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		var got corev1.Node
		if err := c.Get(ctx, types.NamespacedName{Name: n.Name}, &got); err != nil {
			t.Fatalf("get node %s: %v", n.Name, err)
		}
		if got.Labels == nil {
			got.Labels = map[string]string{}
		}
		got.Labels[k] = v
		err := c.Update(ctx, &got)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			t.Fatalf("update node %s: %v", n.Name, err)
		}
		select {
		case <-deadline:
			t.Fatalf("node update %s kept conflicting for 10s", n.Name)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// negativeWindow is how long the negative cases watch offPoolName for a leaked
// reconcile. offPoolName is out of scope and never self-requeues, so this only
// needs to outlast informer delivery of a (wrongly) enqueued event, not any
// periodic requeue.
const negativeWindow = 2 * time.Second

// assertNoNewBeyond asserts offPoolName's reconcile count does not climb past
// base within negativeWindow — used by the negative cases, which target the
// out-of-scope pool (it never self-requeues, so any climb is a leaked watch
// event) after baselining a prior, intended enqueue.
func (r *reconcileRecorder) assertNoNewBeyond(t *testing.T, base int) {
	t.Helper()
	deadline := time.After(negativeWindow)
	for {
		select {
		case <-deadline:
			return
		case <-time.After(50 * time.Millisecond):
			if got := r.count(offPoolName); got > base {
				t.Fatalf("reconcile for %q was unexpectedly enqueued (count %d -> %d)", offPoolName, base, got)
			}
		}
	}
}
