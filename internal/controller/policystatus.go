package controller

import (
	"fmt"
	"slices"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/resolve"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// computeStatus derives the observational status for target from the current
// cluster state (all policies + all pools). It is pure: the reconciler does the
// I/O and the conditional write. matchedNodePools counts the pools target wins by
// selector specificity (independent of spec validity); rotatingNodePools counts
// those with an in-flight rotation anchor; the Ready condition reports why the
// policy is or is not effective. Invalid (intrinsic) takes precedence over
// Conflict (relational). See spec §5.3/§5.4 and #119 §3.
func computeStatus(
	target *noderotationv1alpha1.RotationPolicy,
	policies []noderotationv1alpha1.RotationPolicy,
	pools []karpv1.NodePool,
) noderotationv1alpha1.RotationPolicyStatus {
	st := noderotationv1alpha1.RotationPolicyStatus{
		ObservedGeneration: target.Generation,
		// Seed from the existing conditions so meta.SetStatusCondition preserves
		// lastTransitionTime when the Ready status does not flip (no-op stability).
		Conditions: append([]metav1.Condition(nil), target.Status.Conditions...),
	}

	// Intrinsic validity: does the spec resolve at reconcile time? (The OpenAPI
	// schema cannot reject an overnight window or a non-positive surge duration.)
	valid, invalidMsg := true, ""
	if pol, err := resolve.ToPolicy(target.Spec); err != nil {
		valid, invalidMsg = false, err.Error()
	} else if _, err := window.New(pol.MaintenanceWindows); err != nil {
		valid, invalidMsg = false, err.Error()
	}

	var matched, rotating int32
	var conflictedPools []string
	tiedNames := map[string]bool{}
	for i := range pools {
		pool := &pools[i]
		winner, outcome, tied := resolve.Governing(pool, policies)
		switch outcome {
		case resolve.Matched:
			if winner.Name == target.Name {
				matched++
				if pool.Annotations[annotations.ActiveRotation] != "" {
					rotating++
				}
			}
		case resolve.Conflict:
			if slices.Contains(tied, target.Name) {
				conflictedPools = append(conflictedPools, pool.Name)
				for _, n := range tied {
					if n != target.Name {
						tiedNames[n] = true
					}
				}
			}
		}
	}
	st.MatchedNodePools = matched
	st.RotatingNodePools = rotating

	cond := metav1.Condition{Type: noderotationv1alpha1.ConditionTypeReady, ObservedGeneration: target.Generation}
	switch {
	case !valid:
		cond.Status = metav1.ConditionFalse
		cond.Reason = noderotationv1alpha1.ReasonInvalid
		cond.Message = invalidMsg
	case len(conflictedPools) > 0:
		sort.Strings(conflictedPools)
		cond.Status = metav1.ConditionFalse
		cond.Reason = noderotationv1alpha1.ReasonConflict
		cond.Message = fmt.Sprintf("ties with %v for NodePool(s) %v; refusing to govern until resolved", sortedKeys(tiedNames), conflictedPools)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = noderotationv1alpha1.ReasonAccepted
		cond.Message = fmt.Sprintf("policy is valid and governs %d NodePool(s)", matched)
	}
	meta.SetStatusCondition(&st.Conditions, cond)
	return st
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
