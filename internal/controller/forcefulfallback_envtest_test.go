package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// ffExpireAfter is the candidate's Forceful Expiration interval; ffGap positions
// the injected clock so deadline − now = ffGap. With the fixture's
// readyTimeout (15m) + drainBound (tGP 30m + buffer), t_rot is ≳45m, so a 20m
// gap satisfies the surge-less trigger deadline − now < t_rot (spec §3.3).
const (
	ffExpireAfter = 14 * 24 * time.Hour
	ffGap         = 20 * time.Minute
)

// ffStartEnv boots envtest with Karpenter's CRDs plus the noderotation.io
// RotationPolicy CRD (whose surge.forcefulFallback reservation #156 D4 lifts) and
// returns a direct client. Like the other *_envtest_test.go files it skips when
// KUBEBUILDER_ASSETS is unset (run via 'make test').
func ffStartEnv(t *testing.T) client.Client {
	t.Helper()
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
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	cl, err := client.New(cfg, client.Options{Scheme: scheme.New()})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return cl
}

// ffPolicy builds an always-open RotationPolicy governing pools labeled
// tier=<tier>, with surge.forcefulFallback set to enabled. The CRD must now
// accept enabled:true (the #156 D2 reservation CEL rule is removed in D4).
func ffPolicy(name, tier string, enabled bool) *noderotationv1alpha1.RotationPolicy {
	return &noderotationv1alpha1.RotationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: noderotationv1alpha1.RotationPolicySpec{
			NodePoolSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": tier}},
			MaintenanceWindows: []noderotationv1alpha1.MaintenanceWindow{{
				Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
				Start: "00:00", End: "23:59",
			}},
			Surge: noderotationv1alpha1.Surge{
				ForcefulFallback: noderotationv1alpha1.FeatureToggle{Enabled: enabled},
			},
		},
	}
}

// ffNodePool builds an in-scope NodePool with a fixed terminationGracePeriod so
// the derived drainBound (tGP + buffer) is deterministic.
func ffNodePool(name, tier string) *karpv1.NodePool {
	return &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"tier": tier}},
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef:           &karpv1.NodeClassReference{Group: "eks.amazonaws.com", Kind: "NodeClass", Name: "default"},
				Requirements:           []karpv1.NodeSelectorRequirementWithMinValues{},
				TerminationGracePeriod: &metav1.Duration{Duration: 30 * time.Minute},
			}},
		},
	}
}

// ffNodeClaim builds a Ready candidate NodeClaim in the pool with the Karpenter
// termination finalizer (so a delete leaves a DeletionTimestamp rather than
// vanishing) and a 14d expireAfter. It is intentionally unscheduled
// (status.nodeName empty): the surge-less path needs no node, and the disabled
// baseline's createPlaceholder is a no-op for an unscheduled candidate — so
// neither scenario needs a Node object.
func ffNodeClaim(name, pool string) *karpv1.NodeClaim {
	e := ffExpireAfter
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Labels:     map[string]string{karpv1.NodePoolLabelKey: pool},
			Finalizers: []string{"karpenter.sh/termination"},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: "eks.amazonaws.com", Kind: "NodeClass", Name: "default"},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
			ExpireAfter:  karpv1.NillableDuration{Duration: &e},
		},
	}
}

// ffSeed creates the policy/pool/claim, stamps the claim Ready via the status
// subresource, and returns the reconciler with an injected clock positioned so
// the candidate's deadline (creation + expireAfter) is ffGap ahead of now —
// inside t_rot, so a graceful surge cannot win the race (spec §3.3).
func ffSeed(t *testing.T, cl client.Client, tier string, enabled bool) (*controller.RotationReconciler, string, string) {
	t.Helper()
	ctx := context.Background()
	polName := "ff-" + tier
	poolName := "np-" + tier
	claimName := "nc-" + tier

	if err := cl.Create(ctx, ffPolicy(polName, tier, enabled)); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if err := cl.Create(ctx, ffNodePool(poolName, tier)); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	nc := ffNodeClaim(claimName, poolName)
	if err := cl.Create(ctx, nc); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	// Status (Ready) lives on the subresource; a plain Create never persists it.
	nc.Status.Conditions = []status.Condition{{
		Type:               status.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "ready",
		LastTransitionTime: metav1.Now(),
	}}
	if err := cl.Status().Update(ctx, nc); err != nil {
		t.Fatalf("update claim status: %v", err)
	}

	// Re-read to learn the apiserver-assigned creationTimestamp, then pin the
	// clock so deadline − now = ffGap (< t_rot).
	if err := cl.Get(ctx, types.NamespacedName{Name: claimName}, nc); err != nil {
		t.Fatalf("re-read claim: %v", err)
	}
	now := nc.CreationTimestamp.Add(ffExpireAfter - ffGap)

	r := &controller.RotationReconciler{
		Client:            cl,
		Namespace:         "node-rotation-system",
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
		Clock:             func() time.Time { return now },
	}
	return r, poolName, claimName
}

// ffReconcile drives the pool reconcile to steady state (a few passes: the first
// starts the rotation, later passes re-drive the in-flight one).
func ffReconcile(t *testing.T, r *controller.RotationReconciler, poolName string) {
	t.Helper()
	ctx := context.Background()
	for i := range 3 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName}}); err != nil {
			t.Fatalf("reconcile pass %d: %v", i, err)
		}
	}
}

func ffGetPool(t *testing.T, cl client.Client, name string) *karpv1.NodePool {
	t.Helper()
	var p karpv1.NodePool
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &p); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	return &p
}

func ffGetClaim(t *testing.T, cl client.Client, name string) *karpv1.NodeClaim {
	t.Helper()
	var c karpv1.NodeClaim
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &c); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	return &c
}

func ffPlaceholderExists(t *testing.T, cl client.Client, claimName string) bool {
	t.Helper()
	var pod corev1.Pod
	err := cl.Get(context.Background(), types.NamespacedName{
		Namespace: "node-rotation-system", Name: surge.PlaceholderName(claimName),
	}, &pod)
	return err == nil
}

// TestForcefulFallbackDeletesWithoutSurge verifies that, with
// surge.forcefulFallback enabled and a candidate that cannot finish a graceful
// surge before its deadline (deadline−now < t_rot), the controller rotates it
// surge-less: it deletes the NodeClaim WITHOUT creating a placeholder Pod, marks
// the rotation forceful-fallback, and reaches draining (spec §3.3).
func TestForcefulFallbackDeletesWithoutSurge(t *testing.T) {
	cl := ffStartEnv(t)
	r, poolName, claimName := ffSeed(t, cl, "ffon", true)

	ffReconcile(t, r, poolName)

	if ffPlaceholderExists(t, cl, claimName) {
		t.Error("surge-less rotation must NOT create a placeholder Pod")
	}
	c := ffGetClaim(t, cl, claimName)
	if c.DeletionTimestamp == nil {
		t.Error("surge-less rotation must delete the candidate NodeClaim")
	}
	pool := ffGetPool(t, cl, poolName)
	if got := pool.Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Errorf("active-rotation-state: got %q, want %q", got, annotations.StateDraining)
	}
	if got := pool.Annotations[annotations.RotationMode]; got != annotations.RotationModeForcefulFallback {
		t.Errorf("rotation-mode: got %q, want %q", got, annotations.RotationModeForcefulFallback)
	}
}

// TestForcefulFallbackDisabledKeepsBaseline verifies the opt-in gate: with
// ForcefulFallback disabled, the same near-deadline candidate is NOT rotated
// surge-less — the controller does not delete it and never stamps the
// forceful-fallback marker (it falls through to the baseline surge path).
func TestForcefulFallbackDisabledKeepsBaseline(t *testing.T) {
	cl := ffStartEnv(t)
	r, poolName, claimName := ffSeed(t, cl, "ffoff", false)

	ffReconcile(t, r, poolName)

	if ffPlaceholderExists(t, cl, claimName) {
		t.Error("an unscheduled baseline candidate must not have a placeholder")
	}
	c := ffGetClaim(t, cl, claimName)
	if c.DeletionTimestamp != nil {
		t.Error("with forcefulFallback disabled the controller must NOT delete the candidate")
	}
	pool := ffGetPool(t, cl, poolName)
	if _, ok := pool.Annotations[annotations.RotationMode]; ok {
		t.Error("rotation-mode must be absent when forcefulFallback is disabled")
	}
}
