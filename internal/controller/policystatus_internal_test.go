package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

// poolGov builds a NodePool with the given labels and (optionally) an
// active-rotation anchor so the rotating count can be exercised.
func poolGov(name string, labels map[string]string, anchored bool) karpv1.NodePool {
	p := karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if anchored {
		p.Annotations = map[string]string{annotations.ActiveRotation: "claim-x"}
	}
	return p
}

// readyCond returns the Ready condition from a status, or a zero Condition.
func readyCond(st noderotationv1alpha1.RotationPolicyStatus) metav1.Condition {
	for _, c := range st.Conditions {
		if c.Type == noderotationv1alpha1.ConditionTypeReady {
			return c
		}
	}
	return metav1.Condition{}
}

func TestComputeStatus_MatchedAndRotating(t *testing.T) {
	target := testRotationPolicy("api", map[string]string{"workload": "api"})
	target.Generation = 4
	pools := []karpv1.NodePool{
		poolGov("p1", map[string]string{"workload": "api"}, true),   // governed + rotating
		poolGov("p2", map[string]string{"workload": "api"}, false),  // governed, idle
		poolGov("p3", map[string]string{"workload": "batch"}, true), // not governed
	}
	st := computeStatus(target, []noderotationv1alpha1.RotationPolicy{*target}, pools)

	if st.ObservedGeneration != 4 {
		t.Errorf("ObservedGeneration = %d, want 4", st.ObservedGeneration)
	}
	if st.MatchedNodePools != 2 {
		t.Errorf("MatchedNodePools = %d, want 2", st.MatchedNodePools)
	}
	if st.RotatingNodePools != 1 {
		t.Errorf("RotatingNodePools = %d, want 1", st.RotatingNodePools)
	}
	if c := readyCond(st); c.Status != metav1.ConditionTrue || c.Reason != noderotationv1alpha1.ReasonAccepted {
		t.Errorf("Ready = %s/%s, want True/Accepted", c.Status, c.Reason)
	}
}

func TestComputeStatus_Conflict(t *testing.T) {
	// Two equally-specific policies both match p (workload=api OR tier=web): a tie.
	a := testRotationPolicy("a", map[string]string{"workload": "api"})
	b := testRotationPolicy("b", map[string]string{"tier": "web"})
	pools := []karpv1.NodePool{poolGov("p", map[string]string{"workload": "api", "tier": "web"}, false)}

	st := computeStatus(a, []noderotationv1alpha1.RotationPolicy{*a, *b}, pools)

	if st.MatchedNodePools != 0 {
		t.Errorf("MatchedNodePools = %d, want 0 (tie governs nothing)", st.MatchedNodePools)
	}
	c := readyCond(st)
	if c.Status != metav1.ConditionFalse || c.Reason != noderotationv1alpha1.ReasonConflict {
		t.Errorf("Ready = %s/%s, want False/Conflict", c.Status, c.Reason)
	}
	if c.Message == "" {
		t.Error("Conflict message should name the contested pool and tied policy")
	}
}

func TestComputeStatus_Invalid(t *testing.T) {
	// An overnight window (end before start) passes the OpenAPI HH:MM pattern but
	// fails reconcile-time validation, so the policy is Invalid.
	bad := testRotationPolicy("bad", map[string]string{"workload": "api"})
	bad.Spec.MaintenanceWindows[0].Start = "22:00"
	bad.Spec.MaintenanceWindows[0].End = "02:00"
	pools := []karpv1.NodePool{poolGov("p", map[string]string{"workload": "api"}, false)}

	st := computeStatus(bad, []noderotationv1alpha1.RotationPolicy{*bad}, pools)

	c := readyCond(st)
	if c.Status != metav1.ConditionFalse || c.Reason != noderotationv1alpha1.ReasonInvalid {
		t.Errorf("Ready = %s/%s, want False/Invalid", c.Status, c.Reason)
	}
}

func TestComputeStatus_InvalidBeatsConflict(t *testing.T) {
	// bad is invalid AND ties with good for p — invalid must win the reason.
	bad := testRotationPolicy("bad", map[string]string{"workload": "api"})
	bad.Spec.MaintenanceWindows[0].Start = "22:00"
	bad.Spec.MaintenanceWindows[0].End = "02:00"
	good := testRotationPolicy("good", map[string]string{"tier": "web"})
	pools := []karpv1.NodePool{poolGov("p", map[string]string{"workload": "api", "tier": "web"}, false)}

	st := computeStatus(bad, []noderotationv1alpha1.RotationPolicy{*bad, *good}, pools)

	if c := readyCond(st); c.Reason != noderotationv1alpha1.ReasonInvalid {
		t.Errorf("Ready reason = %s, want Invalid (intrinsic precedence)", c.Reason)
	}
}

// --- RotationPolicyStatusReconciler tests ----------------------------------

func newStatusReconciler(objs ...client.Object) (*RotationPolicyStatusReconciler, client.Client) {
	cl := fake.NewClientBuilder().
		WithScheme(scheme.New()).
		WithObjects(objs...).
		WithStatusSubresource(&noderotationv1alpha1.RotationPolicy{}).
		Build()
	return &RotationPolicyStatusReconciler{Client: cl}, cl
}

func reconcilePolicy(t *testing.T, r *RotationPolicyStatusReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile(%s): %v", name, err)
	}
}

func TestStatusReconciler_WritesStatus(t *testing.T) {
	pol := testRotationPolicy("api", map[string]string{"workload": "api"})
	pool := poolGov("p1", map[string]string{"workload": "api"}, true)
	r, cl := newStatusReconciler(pol, &pool)

	reconcilePolicy(t, r, "api")

	var got noderotationv1alpha1.RotationPolicy
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "api"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.MatchedNodePools != 1 || got.Status.RotatingNodePools != 1 {
		t.Errorf("status = matched %d rotating %d, want 1/1", got.Status.MatchedNodePools, got.Status.RotatingNodePools)
	}
	if c := readyCond(got.Status); c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s, want True", c.Status)
	}
}

func TestStatusReconciler_NoOpWhenUnchanged(t *testing.T) {
	pol := testRotationPolicy("api", map[string]string{"workload": "api"})
	pool := poolGov("p1", map[string]string{"workload": "api"}, false)
	r, cl := newStatusReconciler(pol, &pool)

	reconcilePolicy(t, r, "api")
	var first noderotationv1alpha1.RotationPolicy
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "api"}, &first)

	reconcilePolicy(t, r, "api") // second pass must not write again
	var second noderotationv1alpha1.RotationPolicy
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "api"}, &second)

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("status churned: rv %s -> %s (no-op guard failed)", first.ResourceVersion, second.ResourceVersion)
	}
}

func TestStatusReconciler_MissingPolicyIsNoError(t *testing.T) {
	r, _ := newStatusReconciler()
	reconcilePolicy(t, r, "gone") // IgnoreNotFound — must not error
}

// newStatusReconcilerWithStatusUpdateErr builds a reconciler whose Status().Update
// always fails with updateErr, so the conflict/error handling on the status write
// can be exercised deterministically.
func newStatusReconcilerWithStatusUpdateErr(updateErr error, objs ...client.Object) *RotationPolicyStatusReconciler {
	cl := fake.NewClientBuilder().
		WithScheme(scheme.New()).
		WithObjects(objs...).
		WithStatusSubresource(&noderotationv1alpha1.RotationPolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return updateErr
			},
		}).
		Build()
	return &RotationPolicyStatusReconciler{Client: cl}
}

func TestStatusReconciler_ConflictRequeuesWithoutError(t *testing.T) {
	// A benign optimistic-concurrency conflict on the status write must NOT surface
	// as a reconcile error (which controller-runtime logs at ERROR + stack trace).
	// It is silently requeued for a fresh recompute, mirroring the anchor write
	// (rotation_controller.go) — so a logged ERROR always signals a real problem (#236).
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: noderotationv1alpha1.GroupVersion.Group, Resource: "rotationpolicies"},
		"api", errors.New("the object has been modified"))
	pol := testRotationPolicy("api", map[string]string{"workload": "api"})
	pool := poolGov("p1", map[string]string{"workload": "api"}, true)
	r := newStatusReconcilerWithStatusUpdateErr(conflict, pol, &pool)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "api"}})
	if err != nil {
		t.Fatalf("conflict must not return an error, got %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("conflict must requeue for a fresh recompute, got %+v", res)
	}
}

func TestStatusReconciler_NonConflictErrorPropagates(t *testing.T) {
	// A non-conflict status-write failure is a real problem and must still surface
	// as a reconcile error so the ERROR channel stays meaningful.
	boom := errors.New("boom")
	pol := testRotationPolicy("api", map[string]string{"workload": "api"})
	pool := poolGov("p1", map[string]string{"workload": "api"}, true)
	r := newStatusReconcilerWithStatusUpdateErr(boom, pol, &pool)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "api"}}); !errors.Is(err, boom) {
		t.Fatalf("non-conflict error must propagate, got %v", err)
	}
}

// --- #244: the two informer caches can be at different revisions ------------

// TestStatusReconciler_StalePolicyGetConflictRetriesPromptly pins the traced #244
// failure. The policy cache and the NodePool cache are distinct informers at
// distinct revisions: the reconcile fired by the NodePool event sees the pool but
// can still Get the policy from a cache that has not yet observed the status this
// controller wrote a moment ago. The write then carries a stale resourceVersion and
// is rejected with a conflict — and because nothing else will re-trigger this
// reconciler (a status write bumps no generation, so GenerationChangedPredicate
// filters it, and no further NodePool event is coming), the requeue IS the only
// path back to a correct status. It must therefore be prompt, not a shortRequeue
// (30s) away, or the cluster advertises "governs 0 NodePool(s)" for half a minute.
func TestStatusReconciler_StalePolicyGetConflictRetriesPromptly(t *testing.T) {
	ctx := context.Background()
	pol := testRotationPolicy("api", map[string]string{"workload": "api"})
	pool := poolGov("p1", map[string]string{"workload": "api"}, true)

	stale := &noderotationv1alpha1.RotationPolicy{}
	serveStale := false // enabled below, once the stale snapshot has been taken
	cl := fake.NewClientBuilder().
		WithScheme(scheme.New()).
		WithObjects(pol, &pool).
		WithStatusSubresource(&noderotationv1alpha1.RotationPolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if p, ok := obj.(*noderotationv1alpha1.RotationPolicy); ok && serveStale {
					stale.DeepCopyInto(p) // a policy cache lagging the last status write
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	// Snapshot the policy as the lagging cache still holds it (old resourceVersion,
	// empty status), then advance the API server past it with a status write.
	if err := cl.Get(ctx, types.NamespacedName{Name: "api"}, stale); err != nil {
		t.Fatal(err)
	}
	fresh := stale.DeepCopy()
	fresh.Status.ObservedGeneration = 1
	if err := cl.Status().Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	serveStale = true

	r := &RotationPolicyStatusReconciler{Client: cl}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "api"}})
	if err != nil {
		t.Fatalf("stale-cache conflict must not error: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 5*time.Second {
		t.Errorf("RequeueAfter = %v, want a prompt retry in (0, 5s]: the requeue is the only "+
			"path back to a correct status, so a shortRequeue-scale wait leaves it wrong that long", res.RequeueAfter)
	}

	// The retry runs against a caught-up cache and converges.
	serveStale = false
	reconcilePolicy(t, r, "api")
	var got noderotationv1alpha1.RotationPolicy
	if err := cl.Get(ctx, types.NamespacedName{Name: "api"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.MatchedNodePools != 1 || got.Status.RotatingNodePools != 1 {
		t.Errorf("after retry: matched %d rotating %d, want 1/1", got.Status.MatchedNodePools, got.Status.RotatingNodePools)
	}
}

// TestStatusReconciler_StalePoolListWriteIsRechecked covers the other direction of
// the same split-cache hazard (#244's original hypothesis): a reconcile that Gets a
// current policy but Lists NodePools from a lagging pool cache computes a stale
// count and writes it WITHOUT a conflict — its resourceVersion is current. Nothing
// re-triggers the reconciler afterwards, so a successful write must also schedule
// one recheck against a caught-up cache; the DeepEqual guard makes that recheck a
// no-op (and terminates the chain) whenever the recompute is stable.
func TestStatusReconciler_StalePoolListWriteIsRechecked(t *testing.T) {
	ctx := context.Background()
	pol := testRotationPolicy("api", map[string]string{"workload": "api"})
	pool := poolGov("p1", map[string]string{"workload": "api"}, true)

	poolsStale := true
	cl := fake.NewClientBuilder().
		WithScheme(scheme.New()).
		WithObjects(pol, &pool).
		WithStatusSubresource(&noderotationv1alpha1.RotationPolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*karpv1.NodePoolList); ok && poolsStale {
					return nil // pool cache has not observed p1 yet: an empty List
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	r := &RotationPolicyStatusReconciler{Client: cl}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "api"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var wrote noderotationv1alpha1.RotationPolicy
	if err := cl.Get(ctx, types.NamespacedName{Name: "api"}, &wrote); err != nil {
		t.Fatal(err)
	}
	if wrote.Status.MatchedNodePools != 0 {
		t.Fatalf("precondition: the stale List should have produced matched=0, got %d", wrote.Status.MatchedNodePools)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("a successful status write must requeue a recheck, got %+v", res)
	}

	// The requeued reconcile sees the caught-up pool cache and corrects the status.
	poolsStale = false
	res2, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "api"}})
	if err != nil {
		t.Fatalf("Reconcile (recheck): %v", err)
	}
	var got noderotationv1alpha1.RotationPolicy
	if err := cl.Get(ctx, types.NamespacedName{Name: "api"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.MatchedNodePools != 1 || got.Status.RotatingNodePools != 1 {
		t.Errorf("stale status not corrected: matched %d rotating %d, want 1/1", got.Status.MatchedNodePools, got.Status.RotatingNodePools)
	}
	if res2.RequeueAfter <= 0 {
		t.Fatalf("the corrective write must itself requeue one recheck, got %+v", res2)
	}

	// Convergence: the third reconcile recomputes the same status, so the DeepEqual
	// guard suppresses the write and returns an empty Result — the chain ends and
	// the requeue cannot hot-loop.
	rv := got.ResourceVersion
	res3, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "api"}})
	if err != nil {
		t.Fatalf("Reconcile (stable): %v", err)
	}
	if res3.RequeueAfter != 0 {
		t.Errorf("a no-op recompute must not requeue (hot-loop guard), got %+v", res3)
	}
	var stable noderotationv1alpha1.RotationPolicy
	if err := cl.Get(ctx, types.NamespacedName{Name: "api"}, &stable); err != nil {
		t.Fatal(err)
	}
	if stable.ResourceVersion != rv {
		t.Errorf("status churned on the stable pass: rv %s -> %s", rv, stable.ResourceVersion)
	}
}
