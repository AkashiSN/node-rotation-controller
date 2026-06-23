package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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

// RotationPolicyStatusReconciler populates RotationPolicy.status as an
// observational, derived view (#119 deliverable 3). It is separate from the
// NodePool-keyed RotationReconciler and writes ONLY status — it never touches the
// rotation state machine, annotations, or markers, preserving the invariant that
// durable state lives on NodeClaim/NodePool annotations (spec §5.3).
type RotationPolicyStatusReconciler struct {
	client.Client
}

// Reconcile recomputes one policy's status from a fresh List of all policies and
// all pools, then writes it back only when it changed (the DeepEqual guard avoids
// a status-write hot loop, since a status Update re-fires this reconciler).
func (r *RotationPolicyStatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var target noderotationv1alpha1.RotationPolicy
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var policies noderotationv1alpha1.RotationPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return ctrl.Result{}, err
	}
	var pools karpv1.NodePoolList
	if err := r.List(ctx, &pools); err != nil {
		return ctrl.Result{}, err
	}

	desired := computeStatus(&target, policies.Items, pools.Items)
	if equality.Semantic.DeepEqual(target.Status, desired) {
		return ctrl.Result{}, nil
	}
	target.Status = desired
	if err := r.Status().Update(ctx, &target); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the status reconciler. Both a RotationPolicy spec change
// (a sibling can steal pools or create a tie) and a NodePool change (relabel, or
// an active-rotation anchor flip) can alter ANY policy's derived status, so both
// watches map to EVERY RotationPolicy — the conservative, always-correct mapping
// (spec §5.4). The RotationPolicy watch uses GenerationChangedPredicate so this
// reconciler's own status writes (which do not bump generation) do not re-trigger
// it; the NodePool watch is unfiltered because the anchor lives in annotations,
// not the generation.
func (r *RotationPolicyStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("rotationpolicy-status").
		Watches(&noderotationv1alpha1.RotationPolicy{}, handler.EnqueueRequestsFromMapFunc(r.allPolicies),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&karpv1.NodePool{}, handler.EnqueueRequestsFromMapFunc(r.allPolicies)).
		Complete(r)
}

// allPolicies enqueues a reconcile for every RotationPolicy — the effect of a
// policy or pool change on derived status is not local to a single policy.
func (r *RotationPolicyStatusReconciler) allPolicies(ctx context.Context, _ client.Object) []reconcile.Request {
	var list noderotationv1alpha1.RotationPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}
