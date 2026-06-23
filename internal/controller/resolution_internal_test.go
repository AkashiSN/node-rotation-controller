package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
)

// poolWithLabels builds a NodePool carrying the given labels, for the resolution
// tests that need a pool matched by more than one selector.
func poolWithLabels(name string, labels map[string]string) *karpv1.NodePool {
	return &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// rotPolicy builds a RotationPolicy with the given name/selector and a well-formed
// all-week window so ToPolicy succeeds.
func rotPolicy(name string, sel map[string]string) *noderotationv1alpha1.RotationPolicy {
	return testRotationPolicy(name, sel)
}

func reconcilePool(t *testing.T, r *RotationReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// TestReconcileConflictBlocks: two equally-specific RotationPolicies match the
// same NodePool (a tie). Reconcile must refuse to rotate it — raising the conflict
// gauge, dropping the stale rotation gauges (ForgetPool), and emitting a Warning
// event — never guessing which policy applies (spec §5.4, #119 §3).
func TestReconcileConflictBlocks(t *testing.T) {
	pool := poolWithLabels("p", map[string]string{"workload": "api", "tier": "web"})
	a := rotPolicy("a", map[string]string{"workload": "api"})
	b := rotPolicy("b", map[string]string{"tier": "web"})
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, a, b)
	ev := events.NewFakeRecorder(16)
	r.Events = ev

	reconcilePool(t, r, "p")

	if blocked, ok := rec.conflicts["p"]; !ok || !blocked {
		t.Errorf("policy_conflict gauge = %v (present=%v), want blocked=true", blocked, ok)
	}
	if len(rec.forgotten) == 0 || rec.forgotten[len(rec.forgotten)-1] != "p" {
		t.Errorf("ForgetPool not called for the conflicted pool: %v", rec.forgotten)
	}
	if _, rotated := rec.obs["p"]; rotated {
		t.Error("a conflicted pool must not run the rotation body (no ObservePool)")
	}
	gotEvents := drain(ev)
	if len(gotEvents) != 1 || !strings.Contains(gotEvents[0], reasonPolicyConflict) {
		t.Errorf("want one PolicyConflict Warning event, got %v", gotEvents)
	}
}

// TestReconcileRuntimeInvalidPolicyBlocks: a single matching policy whose window
// passes the CRD HH:MM pattern but fails runtime validation (end before start) is
// a conflict — the controller refuses to act on an unsafe policy.
func TestReconcileRuntimeInvalidPolicyBlocks(t *testing.T) {
	pool := testNodePool(nil) // labels workload=api
	bad := testRotationPolicy("bad", map[string]string{"workload": "api"})
	bad.Spec.MaintenanceWindows[0].Start = "06:00"
	bad.Spec.MaintenanceWindows[0].End = "02:00" // overnight wrap: runtime-invalid
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, bad)

	reconcilePool(t, r, testPoolName)

	if blocked, ok := rec.conflicts[testPoolName]; !ok || !blocked {
		t.Errorf("policy_conflict gauge = %v (present=%v), want blocked=true", blocked, ok)
	}
	if _, rotated := rec.obs[testPoolName]; rotated {
		t.Error("a pool governed by a runtime-invalid policy must not rotate")
	}
}

// TestReconcileNoMatchNoOp: a NodePool matched by no RotationPolicy is not rotated
// (the expireAfter backstop still applies). Reconcile drops its series and never
// flags a conflict (spec §5.4 / #119 §4).
func TestReconcileNoMatchNoOp(t *testing.T) {
	pool := testNodePool(nil) // labels workload=api
	other := rotPolicy("batch", map[string]string{"workload": "batch"})
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, other)

	reconcilePool(t, r, testPoolName)

	if len(rec.forgotten) == 0 || rec.forgotten[len(rec.forgotten)-1] != testPoolName {
		t.Errorf("an unmatched pool must have its series forgotten: %v", rec.forgotten)
	}
	if blocked := rec.conflicts[testPoolName]; blocked {
		t.Error("an unmatched pool is not a conflict")
	}
	if _, rotated := rec.obs[testPoolName]; rotated {
		t.Error("an unmatched pool must not run the rotation body")
	}
}

// TestReconcileMatchedProceeds: a single governing policy resolves cleanly, so the
// rotation body runs (ObservePool emitted) and the conflict gauge is cleared to 0.
func TestReconcileMatchedProceeds(t *testing.T) {
	pool := testNodePool(nil)
	good := rotPolicy("api", map[string]string{"workload": "api"})
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, good)

	reconcilePool(t, r, testPoolName)

	if _, rotated := rec.obs[testPoolName]; !rotated {
		t.Error("a governed pool must run the rotation body (ObservePool)")
	}
	if blocked, ok := rec.conflicts[testPoolName]; !ok || blocked {
		t.Errorf("policy_conflict gauge = %v (present=%v), want cleared (false)", blocked, ok)
	}
}

// TestEmitConflictDedupAndClear: the conflict Warning is deduplicated on its detail
// and re-fires only after ClearConflict, mirroring the findings dedup.
func TestEmitConflictDedupAndClear(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	w := newWarningEmitter(rec)
	ctx := context.Background()
	pool := warnPool()

	w.EmitConflict(ctx, pool, "tie: [a b]")
	if got := drain(rec); len(got) != 1 || !strings.Contains(got[0], reasonPolicyConflict) {
		t.Fatalf("want one PolicyConflict event, got %v", got)
	}
	// Same conflict again: no re-fire.
	w.EmitConflict(ctx, pool, "tie: [a b]")
	if got := drain(rec); len(got) != 0 {
		t.Fatalf("want no re-fire on unchanged conflict, got %v", got)
	}
	// Resolved, then recurs: must re-fire.
	w.ClearConflict(pool.Name)
	w.EmitConflict(ctx, pool, "tie: [a b]")
	if got := drain(rec); len(got) != 1 {
		t.Fatalf("want re-fire after ClearConflict, got %v", got)
	}
}
