package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// nodePoolFromLabel enqueues the owning NodePool for a labeled object and
// nothing for an unlabeled one (issue #14 — shared by the NodeClaim and Node
// watches).
func TestNodePoolFromLabel(t *testing.T) {
	labeled := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:   "nc-1",
		Labels: map[string]string{karpv1.NodePoolLabelKey: testPoolName},
	}}
	reqs := nodePoolFromLabel(context.Background(), labeled)
	if len(reqs) != 1 || reqs[0].Name != testPoolName {
		t.Fatalf("labeled object: got %v, want one request for %q", reqs, testPoolName)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   surgeNode,
		Labels: map[string]string{karpv1.NodePoolLabelKey: testPoolName},
	}}
	if reqs := nodePoolFromLabel(context.Background(), node); len(reqs) != 1 || reqs[0].Name != testPoolName {
		t.Fatalf("labeled node: got %v, want one request for %q", reqs, testPoolName)
	}

	unlabeled := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-manual"}}
	if reqs := nodePoolFromLabel(context.Background(), unlabeled); reqs != nil {
		t.Fatalf("unlabeled object: got %v, want nil", reqs)
	}
}

// placeholderToNodePool maps a placeholder to its NodePool only when it is in the
// controller namespace, carries the surge-for marker, and has the nodepool label.
func TestPlaceholderToNodePool(t *testing.T) {
	r := &RotationReconciler{Namespace: testNS}
	ph := func(ns string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      surge.PlaceholderName("nc-old"),
			Namespace: ns,
			Labels:    labels,
		}}
	}
	full := map[string]string{
		annotations.SurgeFor:    "nc-old",
		karpv1.NodePoolLabelKey: testPoolName,
	}

	if reqs := r.placeholderToNodePool(context.Background(), ph(testNS, full)); len(reqs) != 1 || reqs[0].Name != testPoolName {
		t.Fatalf("placeholder: got %v, want one request for %q", reqs, testPoolName)
	}

	cases := map[string]*corev1.Pod{
		"wrong namespace":   ph("default", full),
		"missing surge-for": ph(testNS, map[string]string{karpv1.NodePoolLabelKey: testPoolName}),
		"missing nodepool":  ph(testNS, map[string]string{annotations.SurgeFor: "nc-old"}),
		"no labels at all":  ph(testNS, nil),
	}
	for name, pod := range cases {
		if reqs := r.placeholderToNodePool(context.Background(), pod); reqs != nil {
			t.Errorf("%s: got %v, want nil", name, reqs)
		}
	}
}

// placeholderRunning enqueues on reaching Running and nothing for the non-events
// (already-Running update, deletion).
func TestPlaceholderRunningPredicate(t *testing.T) {
	p := placeholderRunning()
	running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	pending := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}

	if !p.Create(event.CreateEvent{Object: running}) {
		t.Error("create of a Running placeholder should enqueue")
	}
	if p.Create(event.CreateEvent{Object: pending}) {
		t.Error("create of a Pending placeholder should not enqueue")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: pending, ObjectNew: running}) {
		t.Error("pending → running should enqueue")
	}
	if p.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: running}) {
		t.Error("running → running (no transition) should not enqueue")
	}
	if p.Delete(event.DeleteEvent{Object: running}) {
		t.Error("placeholder deletion should not enqueue")
	}
}

// nodeBecameReady enqueues only when the Ready condition flips to True.
func TestNodeBecameReadyPredicate(t *testing.T) {
	p := nodeBecameReady()
	ready := nodeWithReady(corev1.ConditionTrue)
	notReady := nodeWithReady(corev1.ConditionFalse)

	if !p.Create(event.CreateEvent{Object: ready}) {
		t.Error("create of a Ready node should enqueue")
	}
	if p.Create(event.CreateEvent{Object: notReady}) {
		t.Error("create of a NotReady node should not enqueue")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: notReady, ObjectNew: ready}) {
		t.Error("notReady → ready should enqueue")
	}
	if p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: ready}) {
		t.Error("ready → ready (no transition) should not enqueue")
	}
	if p.Delete(event.DeleteEvent{Object: ready}) {
		t.Error("node deletion should not enqueue")
	}
}

func nodeWithReady(status corev1.ConditionStatus) client.Object {
	return &corev1.Node{Status: corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
	}}
}
