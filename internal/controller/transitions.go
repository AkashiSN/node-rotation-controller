package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// Event reasons and actions for the rotation state machine (issue #221). The
// one-shot transitions are logged inline at their transition point; the two
// LEVEL-TRIGGERED conditions below are emitted through the warningEmitter's
// dedup so a condition that persists across reconciles reports once.
const (
	reasonRotationStarted    = "RotationStarted"
	reasonRotationCompleted  = "RotationCompleted"
	reasonRotationFailed     = "RotationFailed"
	reasonSurgeUnschedulable = "SurgeUnschedulable"

	actionRotateNode     = "RotateNode"
	actionProvisionSurge = "ProvisionSurge"
)

// EmitNoCandidate logs, once per transition, that a start gate is holding the
// NodePool. reconcileNodePool self-requeues every longRequeue, so an
// undeduplicated line would print every minute for as long as the pool idles —
// which is most of the time, most pools being outside their maintenance window.
// No Event: an idle pool is not a condition an operator needs pushed to them.
func (w *warningEmitter) EmitNoCandidate(ctx context.Context, pool *karpv1.NodePool, gate string) {
	w.emitNoCandidate(ctx, pool, "gate:"+gate, "reason", gate)
}

// EmitNoCandidateCensus logs, once per transition, why the pick came back empty
// with the gates open. The key is the whole census, so the line re-fires when the
// population changes (a node ages into the trigger, a backoff expires) but stays
// silent while it does not.
func (w *warningEmitter) EmitNoCandidateCensus(ctx context.Context, pool *karpv1.NodePool, c selection.Census) {
	w.emitNoCandidate(ctx, pool, fmt.Sprintf("census:%+v", c),
		"reason", "noEligibleClaim",
		"claims", c.Total,
		"notTriggered", c.NotTriggered,
		"inBackoff", c.InBackoff,
		"inFlight", c.InFlight,
		"optedOut", c.OptedOut,
		"deleting", c.Deleting,
		"notReady", c.NotReady,
		"terminal", c.Terminal)
}

func (w *warningEmitter) emitNoCandidate(ctx context.Context, pool *karpv1.NodePool, key string, kv ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.poolStateLocked(pool.Name)
	if s.noCandidate == key {
		return // same reason as last pass — no re-fire
	}
	s.noCandidate = key
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("no rotation candidate", kv...)
}

// ClearNoCandidate resets the dedup state once the pool starts a rotation, so the
// next idle period reports its reason afresh.
func (w *warningEmitter) ClearNoCandidate(pool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.state[pool]; s != nil {
		s.noCandidate = ""
	}
}

// EmitPlaceholderPending surfaces the scheduler's own PodScheduled=False reason
// and message for a placeholder that has not been placed, as a log line and a
// Warning Event on the candidate NodeClaim. This is the highest-value line in
// issue #221: without it a placeholder that no instance type can fit stalls for
// the full readyTimeout with nothing but Karpenter's FailedScheduling Event —
// whose `resources` field reports the capacity Karpenter must find (placeholder
// PLUS the DaemonSet overhead it adds to a fresh node), not the placeholder's
// own requests, and is therefore easy to misread as a double-count.
//
// advancePending re-enters every shortRequeue, so the pair is deduplicated on
// (reason, message): a stall reports once, and again only if the scheduler's
// explanation changes.
func (w *warningEmitter) EmitPlaceholderPending(ctx context.Context, pool string, cand *karpv1.NodeClaim, ph *corev1.Pod) {
	cond := podScheduledFalse(ph)
	if cond == nil {
		return // placed, or the scheduler has not weighed in yet — nothing to explain
	}
	key := cond.Reason + "|" + cond.Message

	w.mu.Lock()
	s := w.poolStateLocked(pool)
	if s.phPending[cand.Name] == key {
		w.mu.Unlock()
		return
	}
	s.phPending[cand.Name] = key
	w.mu.Unlock()

	log.FromContext(ctx).WithValues("nodepool", pool).Info("surge placeholder is not schedulable",
		"nodeclaim", cand.Name, "placeholder", ph.Name, "reason", cond.Reason, "detail", cond.Message)
	if w.events != nil {
		w.events.Eventf(cand, nil, corev1.EventTypeWarning, reasonSurgeUnschedulable, actionProvisionSurge,
			"the surge placeholder cannot be scheduled (%s): %s", cond.Reason, cond.Message)
	}
}

// ClearPlaceholderPending drops a claim's unschedulable dedup state once its
// attempt ends (surge ready, rollback, or abort), so a later attempt on the same
// claim reports its own stall.
func (w *warningEmitter) ClearPlaceholderPending(pool, claim string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.state[pool]; s != nil {
		delete(s.phPending, claim)
	}
}

// podScheduledFalse returns the placeholder's PodScheduled condition when the
// scheduler has rejected it, else nil.
func podScheduledFalse(ph *corev1.Pod) *corev1.PodCondition {
	if ph == nil {
		return nil
	}
	for i := range ph.Status.Conditions {
		c := &ph.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return c
		}
	}
	return nil
}
