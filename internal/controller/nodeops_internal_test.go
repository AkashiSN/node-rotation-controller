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
	if n.Annotations[annotations.DoNotDisruptOwned] != "true" {
		t.Errorf("do-not-disrupt-owned marker: got %q", n.Annotations[annotations.DoNotDisruptOwned])
	}
	if n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Errorf("surge-for: got %q", n.Annotations[annotations.SurgeFor])
	}
}

func TestApplyFreezeIdempotent(t *testing.T) {
	n := testNode(map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.DoNotDisruptOwned:    "true",
		annotations.SurgeFor:             "nc-old",
	}, false)
	if applyFreeze(n, "nc-old") {
		t.Error("applyFreeze on an already-frozen node must report no change")
	}
}

func TestApplyFreezeNeverAdoptsOperatorDoNotDisrupt(t *testing.T) {
	// do-not-disrupt already present without the owned marker is an operator's.
	// The controller still freezes the node (surge-for) so it can find and clean
	// up the rotation, but it must not claim ownership of the do-not-disrupt —
	// no owned marker — so unfreeze later preserves the operator's protection
	// (spec §3.3, §5.3).
	n := testNode(map[string]string{karpv1.DoNotDisruptAnnotationKey: "true"}, false)
	if !applyFreeze(n, "nc-old") {
		t.Fatal("applyFreeze must report a change for the added surge-for marker")
	}
	if n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Errorf("surge-for: got %q", n.Annotations[annotations.SurgeFor])
	}
	if _, ok := n.Annotations[annotations.DoNotDisruptOwned]; ok {
		t.Error("an operator's do-not-disrupt must never gain the controller owned marker")
	}
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Error("operator do-not-disrupt must be left in place")
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

func TestApplyFreezeProtectsNonTrueDoNotDisrupt(t *testing.T) {
	// Karpenter blocks voluntary disruption only when the node value is exactly
	// "true" (statenode.go: == "true"); "false" or any other value is NOT
	// protection. A node carrying such a non-protective value without our owned
	// marker is not an operator's active protection, so the controller must set
	// "true" and take ownership — otherwise the surge pair stays disruptable.
	for _, v := range []string{"false", ""} {
		n := testNode(map[string]string{karpv1.DoNotDisruptAnnotationKey: v}, false)
		if !applyFreeze(n, "nc-old") {
			t.Fatalf("value %q: applyFreeze must report a change", v)
		}
		if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
			t.Errorf("value %q: do-not-disrupt must be set to true, got %q", v, n.Annotations[karpv1.DoNotDisruptAnnotationKey])
		}
		if n.Annotations[annotations.DoNotDisruptOwned] != "true" {
			t.Errorf("value %q: controller must take ownership, owned marker got %q", v, n.Annotations[annotations.DoNotDisruptOwned])
		}
	}
}

func TestApplyUnfreezeRemovesMarkersAndUncordons(t *testing.T) {
	n := testNode(map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.DoNotDisruptOwned:    "true",
		annotations.SurgeFor:             "nc-old",
		annotations.Cordoned:             "true",
	}, true)
	if !applyUnfreeze(n) {
		t.Fatal("applyUnfreeze must report a change")
	}
	if _, ok := n.Annotations[karpv1.DoNotDisruptAnnotationKey]; ok {
		t.Error("do-not-disrupt must be removed")
	}
	if _, ok := n.Annotations[annotations.DoNotDisruptOwned]; ok {
		t.Error("do-not-disrupt-owned marker must be removed")
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

func TestApplyUnfreezePreservesOperatorDoNotDisrupt(t *testing.T) {
	// surge-for present (controller froze it) but no owned marker: the
	// do-not-disrupt was an operator's, present before the controller froze the
	// node. Unfreeze drops the controller's own markers but must leave the
	// operator's do-not-disrupt in place (spec §3.3, §5.3).
	n := testNode(map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.SurgeFor:             "nc-old",
	}, false)
	if !applyUnfreeze(n) {
		t.Fatal("applyUnfreeze must report a change for the removed surge-for marker")
	}
	if _, ok := n.Annotations[annotations.SurgeFor]; ok {
		t.Error("surge-for must be removed")
	}
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Error("an operator's do-not-disrupt (no owned marker) must be preserved")
	}
}

func TestApplyUnfreezeLeavesOperatorCordon(t *testing.T) {
	// owned marker present (controller froze it) but no cordoned marker: the node
	// was already operator-cordoned, so unfreeze drops the freeze markers (and the
	// controller's do-not-disrupt) but must not uncordon (spec §5.3 sweep rule).
	n := testNode(map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.DoNotDisruptOwned:    "true",
		annotations.SurgeFor:             "nc-old",
	}, true)
	if !applyUnfreeze(n) {
		t.Fatal("applyUnfreeze must report a change for the removed freeze markers")
	}
	if _, ok := n.Annotations[karpv1.DoNotDisruptAnnotationKey]; ok {
		t.Error("a controller-owned do-not-disrupt must be removed")
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
