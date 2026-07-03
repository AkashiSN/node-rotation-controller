package controller

import (
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// The node side-effect mutators below are pure: each takes a *corev1.Node,
// brings it to the desired state in place, and returns whether anything changed
// so the caller can skip a no-op Update. Keeping the decision logic free of I/O
// makes the §3.3/§5.3 ownership rules — never adopt an operator's cordon or
// do-not-disrupt, never touch either on cleanup — directly testable.

// applyFreeze marks the node as controller-frozen for the rotation named by
// claimName: the karpenter.sh/do-not-disrupt annotation (blocks voluntary
// disruption during the surge), the do-not-disrupt-owned marker that attributes
// that annotation to the controller, and the surge-for marker that pairs the
// node with this rotation (spec §3.3, §5.3).
//
// do-not-disrupt is adopted conditionally, mirroring applyCordon: a node already
// carrying do-not-disrupt without the controller's owned marker is an operator's
// protection, so the controller leaves both the annotation and ownership alone —
// no owned marker — and unfreeze later preserves it. surge-for is always written:
// the node still belongs to this rotation and must be findable for cleanup even
// when the controller does not own the do-not-disrupt.
func applyFreeze(n *corev1.Node, claimName string) bool {
	changed := false
	if !operatorOwnsDoNotDisrupt(n) {
		changed = setAnnotation(n, karpv1.DoNotDisruptAnnotationKey, "true") || changed
		changed = setAnnotation(n, annotations.DoNotDisruptOwned, "true") || changed
	}
	changed = setAnnotation(n, annotations.SurgeFor, claimName) || changed
	return changed
}

// operatorOwnsDoNotDisrupt reports whether the node carries an operator's active
// do-not-disrupt protection: the value is exactly "true" (the only value
// Karpenter honors — its node disruption check is `== "true"`, so "false" or any
// other value is not protection) and the controller's owned marker is absent.
// Only such a node is left untouched by applyFreeze/applyUnfreeze; a
// non-protective value is overwritten and taken over, so the surge pair is always
// actually protected (spec §3.3, §5.3).
func operatorOwnsDoNotDisrupt(n *corev1.Node) bool {
	return n.Annotations[karpv1.DoNotDisruptAnnotationKey] == "true" &&
		!hasAnnotation(n, annotations.DoNotDisruptOwned)
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

// applyUnfreeze reverses applyFreeze (+ applyCordon) on a surge-frozen node: it
// removes the surge-for marker, removes do-not-disrupt only when the controller
// owns it (the do-not-disrupt-owned marker is present), and, when the
// controller's cordoned marker is present, lifts the cordon too. Callers apply
// it to nodes carrying the controller's surge-for marker. An operator's
// pre-existing do-not-disrupt (no owned marker) is preserved, mirroring the way
// applyUncordon preserves an operator's cordon (spec §3.3, §5.3).
func applyUnfreeze(n *corev1.Node) bool {
	changed := removeAnnotation(n, annotations.SurgeFor)
	if hasAnnotation(n, annotations.DoNotDisruptOwned) {
		changed = removeAnnotation(n, karpv1.DoNotDisruptAnnotationKey) || changed
		changed = removeAnnotation(n, annotations.DoNotDisruptOwned) || changed
	}
	return applyUncordon(n) || changed
}

// applyUncordon lifts the controller's cordon alone — the cordoned marker plus
// Spec.Unschedulable — without touching do-not-disrupt or surge-for. The sweep
// uses it on a cordon-only node (one the controller cordoned but never froze)
// so an operator's do-not-disrupt on that node is preserved. A node without the
// cordoned marker is an operator cordon (or uncordoned) and is left untouched
// (spec §3.3, §5.3).
func applyUncordon(n *corev1.Node) bool {
	if !hasAnnotation(n, annotations.Cordoned) {
		return false
	}
	removeAnnotation(n, annotations.Cordoned)
	n.Spec.Unschedulable = false
	return true
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

// excludedNodeNames returns a map of node names that carry an operator's active
// do-not-disrupt protection and should be excluded from rotation (spec §3.2).
// A node is excluded only when the operator owns the do-not-disrupt annotation
// (value is exactly "true" and the controller's owned marker is absent);
// a node the controller marked itself (with the owned marker) is a mid-surge
// rotation target and is NOT excluded.
// Returns nil if no nodes are excluded (empty excluded set).
func excludedNodeNames(nodes []corev1.Node) map[string]bool {
	excluded := make(map[string]bool)
	for i := range nodes {
		if operatorOwnsDoNotDisrupt(&nodes[i]) {
			excluded[nodes[i].Name] = true
		}
	}
	if len(excluded) == 0 {
		return nil
	}
	return excluded
}

// excludedClaimNames returns a map of claim names scheduled on excluded nodes,
// or nil if the excluded set is empty. This enables the selection logic to skip
// claims whose nodes the operator has opted out of rotation.
func excludedClaimNames(claims []karpv1.NodeClaim, excluded map[string]bool) map[string]bool {
	if len(excluded) == 0 {
		return nil
	}
	result := make(map[string]bool)
	for i := range claims {
		if excluded[claims[i].Status.NodeName] {
			result[claims[i].Name] = true
		}
	}
	return result
}
