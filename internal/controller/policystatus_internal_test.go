package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
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
