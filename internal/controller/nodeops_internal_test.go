package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

func testNode(anns map[string]string, unschedulable bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Annotations: anns},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
	}
}

func TestApplyFreezeSetsMarkers(t *testing.T) {
	n := testNode(nil, false)
	if !applyFreeze(n, "nc-old") {
		t.Fatal("applyFreeze on a fresh node must report a change")
	}
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Errorf("do-not-disrupt: got %q", n.Annotations[karpv1.DoNotDisruptAnnotationKey])
	}
	if n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Errorf("surge-for: got %q", n.Annotations[annotations.SurgeFor])
	}
}

func TestApplyFreezeIdempotent(t *testing.T) {
	n := testNode(map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.SurgeFor:             "nc-old",
	}, false)
	if applyFreeze(n, "nc-old") {
		t.Error("applyFreeze on an already-frozen node must report no change")
	}
}

func TestApplyCordonOnSchedulableNode(t *testing.T) {
	n := testNode(nil, false)
	if !applyCordon(n) {
		t.Fatal("applyCordon on a schedulable node must report a change")
	}
	if !n.Spec.Unschedulable {
		t.Error("node must be marked unschedulable")
	}
	if n.Annotations[annotations.Cordoned] != "true" {
		t.Errorf("cordoned marker: got %q", n.Annotations[annotations.Cordoned])
	}
}

func TestApplyCordonNeverAdoptsOperatorCordon(t *testing.T) {
	// Already unschedulable with no controller marker: an operator cordon. The
	// controller must not write the flag or adopt it with a marker (spec §3.3).
	n := testNode(nil, true)
	if applyCordon(n) {
		t.Error("applyCordon must be a no-op on an operator-cordoned node")
	}
	if _, ok := n.Annotations[annotations.Cordoned]; ok {
		t.Error("operator cordon must never gain the controller marker")
	}
}

func TestApplyCordonIdempotentOnOwnCordon(t *testing.T) {
	n := testNode(map[string]string{annotations.Cordoned: "true"}, true)
	if applyCordon(n) {
		t.Error("applyCordon on an already controller-cordoned node must report no change")
	}
}

func TestApplyUnfreezeRemovesMarkersAndUncordons(t *testing.T) {
	n := testNode(map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.SurgeFor:             "nc-old",
		annotations.Cordoned:             "true",
	}, true)
	if !applyUnfreeze(n) {
		t.Fatal("applyUnfreeze must report a change")
	}
	if _, ok := n.Annotations[karpv1.DoNotDisruptAnnotationKey]; ok {
		t.Error("do-not-disrupt must be removed")
	}
	if _, ok := n.Annotations[annotations.SurgeFor]; ok {
		t.Error("surge-for must be removed")
	}
	if _, ok := n.Annotations[annotations.Cordoned]; ok {
		t.Error("cordoned marker must be removed")
	}
	if n.Spec.Unschedulable {
		t.Error("a controller-cordoned node must be uncordoned")
	}
}

func TestApplyUnfreezeLeavesOperatorCordon(t *testing.T) {
	// surge-for present (controller froze it) but no cordoned marker: the node was
	// already operator-cordoned, so unfreeze drops the freeze markers but must not
	// uncordon (spec §5.3 sweep rule).
	n := testNode(map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.SurgeFor:             "nc-old",
	}, true)
	if !applyUnfreeze(n) {
		t.Fatal("applyUnfreeze must report a change for the removed freeze markers")
	}
	if !n.Spec.Unschedulable {
		t.Error("an operator cordon (no marker) must be left in place")
	}
}

func TestApplyUnfreezeNoChangeWhenClean(t *testing.T) {
	n := testNode(nil, false)
	if applyUnfreeze(n) {
		t.Error("applyUnfreeze on a clean node must report no change")
	}
}
