package controller

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
//   - node freeze markers (surge-for + the controller's own do-not-disrupt,
//     attributed by the do-not-disrupt-owned marker) and controller cordons (the
//     cordoned marker) that no anchor references — reversed via applyUnfreeze for
//     surge-frozen nodes and applyUncordon for cordon-only nodes; neither strips
//     an operator's do-not-disrupt (no owned marker) or uncordons an operator's
//     cordon (no cordoned marker);
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
				delete(m, annotations.RotationMode)
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
		p := &pods.Items[i]
		claim := p.Labels[annotations.SurgeFor]
		if claim == "" || anchored[claim] {
			continue
		}
		// Delete the Pod the label selected, not one named from the label. The
		// reconcile paths address their placeholder by its deterministic name, but
		// the sweep's predicate is the label, and a Pod carrying it under any other
		// name — an operator's copy of the manifest, a leftover from a rename — is
		// exactly what the sweep exists to clean up. Rebuilding the canonical name
		// here would delete a different object, or none, while the line named the
		// one that was listed (issue #313).
		deleted, err := r.deleteSelected(ctx, p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !deleted {
			continue
		}
		logger.Info("deleted orphaned placeholder", "claim", claim, "pod", p.Name)
	}
	return errors.Join(errs...)
}

// deleteSelected deletes exactly the object the sweep listed and reports whether
// this call removed it.
//
// The List is a cache read and the Delete lands after it. A Pod already gone —
// deleted by a rollback or by an operator — was removed by someone else; a Pod
// replaced under the same name is a different object, which the UID precondition
// turns into a Conflict rather than a delete of something the sweep never
// selected. Neither is an error: nothing there needs repair any more, and the
// sweep runs once and is never retried.
func (r *RotationReconciler) deleteSelected(ctx context.Context, p *corev1.Pod) (bool, error) {
	var opts []client.DeleteOption
	if p.UID != "" {
		opts = append(opts, client.Preconditions{UID: &p.UID})
	}
	err := r.Delete(ctx, p, opts...)
	switch {
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// sweepNodes reverses the freeze/cordon on every node whose controller markers
// no anchor references. A surge-frozen node (surge-for marker) is unfrozen via
// applyUnfreeze; a cordon-only node (no surge-for) has just its cordon lifted via
// applyUncordon. Both mutators leave an operator's unmarked cordon or
// do-not-disrupt untouched: applyUnfreeze strips do-not-disrupt only when the
// controller's do-not-disrupt-owned marker is present (spec §3.3, §5.3).
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
		// The List above selected this node; only the read the write is validated
		// against says what there was to reverse. So the predicate is re-applied
		// there, the mutator is chosen there, and the line is described from there
		// (issue #313, mirroring the claim leg's #311 fix):
		//
		//   - an anchor taken during the window makes the markers current, not
		//     orphaned, and the rotation that owns them is not the sweep's to undo;
		//   - a node the List reported as surge-frozen can be cordon-only by then,
		//     and a cordon-only node was never frozen and belongs to no claim, so
		//     reporting it as an unfreeze names work of a kind the sweep did not do;
		//   - unfroze/frozenFor are set at the top of every attempt and read only
		//     when the write landed, so a losing attempt never describes the winning
		//     one (the shape of #307).
		var unfroze bool
		var frozenFor string
		wrote, err := r.patchNode(ctx, n.Name, func(fresh *corev1.Node) bool {
			unfroze, frozenFor = false, ""
			claim, surged := fresh.Annotations[annotations.SurgeFor]
			_, cordoned := fresh.Annotations[annotations.Cordoned]
			switch {
			case surged && anchored[claim]:
				return false
			case !surged && !cordoned:
				return false
			}
			// applyUnfreeze removes do-not-disrupt only if the owned marker attributes
			// it to the controller; a cordon-only node only has its cordon lifted.
			if !surged {
				return applyUncordon(fresh)
			}
			unfroze, frozenFor = true, claim
			return applyUnfreeze(fresh)
		})
		switch {
		case apierrors.IsNotFound(err):
			// Gone between patchNode's read and its Update — the same non-event as a
			// node already gone by the read, which patchNode absorbs, and the same
			// second half of the window the claim leg treats this way (issue #311).
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		case !wrote:
			continue
		}
		if unfroze {
			logger.Info("unfroze orphaned node", "node", n.Name, "claim", frozenFor)
		} else {
			logger.Info("uncordoned orphaned node", "node", n.Name)
		}
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
		// The List above is a cache read and the write lands some time after it. Two
		// things can happen in that window, neither of them this controller's to
		// order: Karpenter's termination controller can finalize the claim away, and
		// the claim's durable state can move past what the List saw. So re-apply the
		// selection predicate to the read the write is validated against, and let the
		// write itself report whether it landed (issue #311).
		// repaired is the state the write actually found, which the predicate allows
		// to differ from the one the List reported. It is read only on claimWritten,
		// so it always comes from the attempt that won.
		var repaired string
		wrote, err := r.patchClaimIf(ctx, c.Name, func(m map[string]string) bool {
			st := m[annotations.State]
			if st != annotations.StatePending && st != annotations.StateDraining {
				return false
			}
			repaired = st
			m[annotations.State] = annotations.StateFailed
			m[annotations.FailedAt] = rfc3339(r.now())
			delete(m, annotations.StartedAt)
			delete(m, annotations.SurgeClaim)
			return true
		})
		switch {
		case apierrors.IsNotFound(err):
			// Finalized away between the conditional write's read and its Update — the
			// other half of the window whose Get side patchClaimIf already treats as a
			// no-op, and the same non-event. A claim that needs no repair is not a
			// sweep error, and the sweep runs once and never retries.
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		case wrote != claimWritten:
			// Nothing was repaired, so there is no rollback to announce. Unlike the
			// reconcile paths, the sweep has no anchor to hand the outcome to — having
			// none is what selected this claim — so the outcome is simply that this
			// claim needed no sweeping.
			continue
		}
		r.recorder().Failure(c.Labels[karpv1.NodePoolLabelKey], c.Name)
		logger.Info("failed un-anchored in-flight claim", "claim", c.Name, "state", repaired)
	}
	return errors.Join(errs...)
}
