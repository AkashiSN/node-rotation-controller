package controller

import (
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// The node side-effect mutators below are pure: each takes a *corev1.Node,
// brings it to the desired state in place, and returns whether anything changed
// so the caller can skip a no-op Update. Keeping the decision logic free of I/O
// makes the §3.3/§5.3 cordon-ownership rules (never adopt an operator cordon,
// never touch an operator cordon on cleanup) directly testable.

// applyFreeze marks the node as controller-frozen for the rotation named by
// claimName: the karpenter.sh/do-not-disrupt annotation (blocks voluntary
// disruption during the surge) plus the surge-for ownership marker (spec §5.3).
func applyFreeze(n *corev1.Node, claimName string) bool {
	changed := setAnnotation(n, karpv1.DoNotDisruptAnnotationKey, "true")
	changed = setAnnotation(n, annotations.SurgeFor, claimName) || changed
	return changed
}

// applyCordon cordons the candidate node, recording the controller's ownership
// with the cordoned marker — but only when the controller itself flips the flag.
// A node already unschedulable without the marker is an operator cordon and is
// never adopted: no flag write, no marker (spec §3.3, §5.3).
func applyCordon(n *corev1.Node) bool {
	if n.Spec.Unschedulable && !hasAnnotation(n, annotations.Cordoned) {
		return false // operator cordon — leave it untouched and unmarked
	}
	changed := false
	if !n.Spec.Unschedulable {
		n.Spec.Unschedulable = true
		changed = true
	}
	if setAnnotation(n, annotations.Cordoned, "true") {
		changed = true
	}
	return changed
}

// applyUnfreeze reverses applyFreeze/applyCordon: it always removes the freeze
// markers (do-not-disrupt + surge-for), and uncordons only when the controller's
// cordoned marker is present — an operator's pre-existing cordon (no marker) is
// left in place (spec §5.3 sweep rule).
func applyUnfreeze(n *corev1.Node) bool {
	changed := removeAnnotation(n, karpv1.DoNotDisruptAnnotationKey)
	changed = removeAnnotation(n, annotations.SurgeFor) || changed
	if hasAnnotation(n, annotations.Cordoned) {
		removeAnnotation(n, annotations.Cordoned)
		n.Spec.Unschedulable = false
		changed = true
	}
	return changed
}

// setAnnotation sets key=value, returning true if that changed the object.
func setAnnotation(o *corev1.Node, key, value string) bool {
	if o.Annotations[key] == value {
		return false
	}
	if o.Annotations == nil {
		o.Annotations = map[string]string{}
	}
	o.Annotations[key] = value
	return true
}

// removeAnnotation deletes key, returning true if it was present.
func removeAnnotation(o *corev1.Node, key string) bool {
	if _, ok := o.Annotations[key]; !ok {
		return false
	}
	delete(o.Annotations, key)
	return true
}

func hasAnnotation(o *corev1.Node, key string) bool {
	_, ok := o.Annotations[key]
	return ok
}
