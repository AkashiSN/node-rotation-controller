package controller

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// Sweep performs the spec §5.3 startup sweep: it repairs stale rotation
// artifacts a crash may have left behind, but only those that no NodePool's
// active-rotation anchor references. An anchored rotation is never stale — the
// reconcile loop resumes it on the first reconcile (that is the recovery path,
// not the sweep's job).
//
// It cleans:
//   - placeholder Pods whose surge-for claim is not anchored — deleted;
//   - node freeze markers (surge-for + the controller's own do-not-disrupt) and
//     controller cordons (the cordoned marker) that no anchor references —
//     reversed via applyUnfreeze for surge-frozen nodes and applyUncordon for
//     cordon-only nodes; neither strips an operator's do-not-disrupt (no surge-for
//     marker) or uncordons an operator's cordon (no cordoned marker);
//   - a pending/draining NodeClaim with no anchor — impossible from any crash
//     point, so it is set to failed and alerted;
//   - a NodePool active-rotation-state with no accompanying anchor — removed.
//
// failed/expired claims are kept (they drive backoff re-entry / mark a claim
// finalizing under the forceful drain). The sweep is best-effort: per-item
// errors are collected and returned joined so the caller can log them without
// aborting the rest of the sweep; the next controller restart re-attempts.
func (r *RotationReconciler) Sweep(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("startup-sweep")

	anchored, err := r.anchoredClaims(ctx)
	if err != nil {
		return err
	}

	var errs []error
	errs = append(errs, r.sweepPlaceholders(ctx, logger, anchored))
	errs = append(errs, r.sweepNodes(ctx, logger, anchored))
	errs = append(errs, r.sweepClaims(ctx, logger, anchored))
	return errors.Join(errs...)
}

// anchoredClaims returns the set of old-NodeClaim names a NodePool anchor
// references — the surge-for value of every live rotation. While listing, it
// repairs the torn case of an active-rotation-state with no anchor (impossible
// from any crash point, since the two are cleared in one update): the orphaned
// state annotation is removed.
func (r *RotationReconciler) anchoredClaims(ctx context.Context) (map[string]bool, error) {
	var pools karpv1.NodePoolList
	if err := r.List(ctx, &pools); err != nil {
		return nil, err
	}
	anchored := map[string]bool{}
	for i := range pools.Items {
		pool := &pools.Items[i]
		if claim := pool.Annotations[annotations.ActiveRotation]; claim != "" {
			anchored[claim] = true
			continue
		}
		if _, ok := pool.Annotations[annotations.ActiveRotationState]; ok {
			if err := r.patchPool(ctx, pool, func(m map[string]string) {
				delete(m, annotations.ActiveRotationState)
				delete(m, annotations.DrainingAt)
			}); err != nil {
				return nil, err
			}
		}
	}
	return anchored, nil
}

// sweepPlaceholders deletes every placeholder Pod whose surge-for claim is not
// anchored.
func (r *RotationReconciler) sweepPlaceholders(ctx context.Context, logger logr.Logger, anchored map[string]bool) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Namespace)); err != nil {
		return err
	}
	var errs []error
	for i := range pods.Items {
		claim := pods.Items[i].Labels[annotations.SurgeFor]
		if claim == "" || anchored[claim] {
			continue
		}
		if err := r.deletePlaceholder(ctx, claim); err != nil {
			errs = append(errs, err)
			continue
		}
		logger.Info("deleted orphaned placeholder", "claim", claim, "pod", pods.Items[i].Name)
	}
	return errors.Join(errs...)
}

// sweepNodes reverses the freeze/cordon on every node whose controller markers
// no anchor references. A surge-frozen node (surge-for marker) is fully
// unfrozen via applyUnfreeze; a cordon-only node (no surge-for) has just its
// cordon lifted via applyUncordon, so an operator's do-not-disrupt on it is
// preserved (spec §5.3). Both mutators leave an operator's unmarked cordon or
// do-not-disrupt untouched.
func (r *RotationReconciler) sweepNodes(ctx context.Context, logger logr.Logger, anchored map[string]bool) error {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}
	var errs []error
	for i := range nodes.Items {
		n := &nodes.Items[i]
		claim, surged := n.Annotations[annotations.SurgeFor]
		_, cordoned := n.Annotations[annotations.Cordoned]
		// Keep markers an anchor still references; a cordon-only node (no
		// surge-for) cannot be tied to a live rotation, so it is always orphaned.
		switch {
		case surged && anchored[claim]:
			continue
		case !surged && !cordoned:
			continue
		}
		// A surge-for node is fully unfrozen (its do-not-disrupt is the
		// controller's); a cordon-only node only has its cordon lifted.
		mutate := applyUnfreeze
		if !surged {
			mutate = applyUncordon
		}
		if err := r.patchNode(ctx, n.Name, mutate); err != nil {
			errs = append(errs, err)
			continue
		}
		logger.Info("unfroze orphaned node", "node", n.Name, "claim", claim)
	}
	return errors.Join(errs...)
}

// sweepClaims sets to failed any pending/draining NodeClaim with no anchor — a
// state no crash point can produce — and alerts. failed/expired claims are left
// in place.
func (r *RotationReconciler) sweepClaims(ctx context.Context, logger logr.Logger, anchored map[string]bool) error {
	var claims karpv1.NodeClaimList
	if err := r.List(ctx, &claims); err != nil {
		return err
	}
	var errs []error
	for i := range claims.Items {
		c := &claims.Items[i]
		state := c.Annotations[annotations.State]
		if state != annotations.StatePending && state != annotations.StateDraining {
			continue
		}
		if anchored[c.Name] {
			continue // the reconcile loop owns the live rotation
		}
		if err := r.patchClaim(ctx, c.Name, func(m map[string]string) {
			m[annotations.State] = annotations.StateFailed
			m[annotations.FailedAt] = rfc3339(r.now())
			delete(m, annotations.StartedAt)
			delete(m, annotations.SurgeClaim)
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		r.recorder().Failure(c.Labels[karpv1.NodePoolLabelKey], c.Name)
		logger.Info("failed un-anchored in-flight claim", "claim", c.Name, "state", state)
	}
	return errors.Join(errs...)
}
