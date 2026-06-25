package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// anchoredPool builds the governed-then-orphaned NodePool: name=api, the given
// labels, with an in-flight rotation anchored on nc-old — the precondition for
// the §5.4 governance-loss reap (issue #141).
func anchoredPool(labels map[string]string) *karpv1.NodePool {
	return &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name:        testPoolName,
		Labels:      labels,
		Annotations: map[string]string{annotations.ActiveRotation: "nc-old"},
	}}
}

// assertReaped checks the controller drove the in-flight rotation to a clean
// terminal state when it ceased to govern the pool: placeholder Pod deleted,
// candidate node unfrozen (surge-for + controller-owned do-not-disrupt removed),
// and the active-rotation anchor cleared — nothing orphaned (issue #141).
func assertReaped(t *testing.T, r *RotationReconciler) {
	t.Helper()
	if placeholderExists(t, r) {
		t.Error("orphaned placeholder Pod should have been deleted on governance loss")
	}
	n := getNodeObj(t, r, surgeNode)
	if _, ok := n.Annotations[annotations.SurgeFor]; ok {
		t.Error("surge-for marker should have been removed on governance loss")
	}
	if _, ok := n.Annotations[karpv1.DoNotDisruptAnnotationKey]; ok {
		t.Error("controller-owned do-not-disrupt should have been removed on governance loss")
	}
	if p := getPool(t, r); p.Annotations[annotations.ActiveRotation] != "" {
		t.Errorf("active-rotation anchor should have been cleared, got %q", p.Annotations[annotations.ActiveRotation])
	}
}

// TestReconcileNoMatchReapsAnchoredRotation: a NodePool with an in-flight rotation
// loses governance (no RotationPolicy selects it any longer). Because no future
// reconcile will touch the now-ungoverned pool, the controller must reap the
// artifacts it owns — placeholder, freeze markers, anchor — before ceding the
// pool, rather than orphan them (issue #141).
func TestReconcileNoMatchReapsAnchoredRotation(t *testing.T) {
	pool := anchoredPool(map[string]string{"workload": "api"})
	other := rotPolicy("batch", map[string]string{"workload": "batch"}) // does not match the pool
	r := newReconciler(t, testNow, nil,
		pool, other,
		placeholderPod(surgeNode, corev1.PodRunning),
		frozenNode(),
	)

	reconcilePool(t, r, testPoolName)

	assertReaped(t, r)
}

// TestReconcileConflictReapsAnchoredRotation: a NodePool with an in-flight rotation
// becomes contested by an equal-specificity tie. The controller refuses to keep
// rotating it — still surfacing the conflict — and must additionally reap the
// in-flight rotation's artifacts rather than leave a do-not-disrupt marker and
// placeholder dangling on a pool it no longer advances (issue #141).
func TestReconcileConflictReapsAnchoredRotation(t *testing.T) {
	pool := anchoredPool(map[string]string{"workload": "api", "tier": "web"})
	a := rotPolicy("a", map[string]string{"workload": "api"})
	b := rotPolicy("b", map[string]string{"tier": "web"})
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec,
		pool, a, b,
		placeholderPod(surgeNode, corev1.PodRunning),
		frozenNode(),
	)

	reconcilePool(t, r, testPoolName)

	assertReaped(t, r)
	// Reaping the orphan must not paper over the misconfiguration: the pool is
	// still flagged as conflicted so an operator sees why it stopped rotating.
	if blocked, ok := rec.conflicts[testPoolName]; !ok || !blocked {
		t.Errorf("policy_conflict gauge = %v (present=%v), want blocked=true", blocked, ok)
	}
}
