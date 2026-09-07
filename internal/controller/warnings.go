package controller

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/adapt"
	"github.com/AkashiSN/node-rotation-controller/internal/decide"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Event action verbs (the events.k8s.io "action" field — the machine-readable
// operation the controller was performing). The specific condition is carried by
// the reason (the finding code, or "ShortLead"); these name the operation.
const (
	actionEvaluateSchedule = "EvaluateSchedule"
	actionCheckExpiry      = "CheckExpiry"
	actionResolvePolicy    = "ResolvePolicy"
	actionReapRotation     = "ReapRotation"
	actionForcefulFallback = "RotateSurgeless"
	actionEvaluateNodePool = "EvaluateNodePool"
	reasonShortLead        = "ShortLead"
	reasonPolicyConflict   = "PolicyConflict"
	reasonGovernanceLost   = "GovernanceLost"
	reasonForcefulFallback = "ForcefulFallback"
	reasonStaticNodePool   = "StaticNodePool"
	reasonWindowMissed     = "WindowMissed"
	// reasonInsufficientHeadroom names the surge_headroom block (spec §5.2 step 3).
	// It is a Warning rather than a finding because the condition is a property of
	// the pool's budget against one candidate's footprint, not of the schedule
	// derivation (issue #326).
	reasonInsufficientHeadroom = "InsufficientHeadroom"
)

// warningEmitter surfaces non-fatal schedule findings and per-node short-lead
// conditions (issue #50) as logs and Kubernetes Events, deduplicated per
// NodePool so the once-per-reconcile recompute does not spam. It is kept separate
// from the metrics Recorder, which stays free of Kubernetes types.
//
// Dedup is by transition INTO the warning set: each pass recomputes the current
// set and stores it, so a finding/claim that clears and later returns re-fires (a
// genuinely new occurrence). State is in-memory only — on controller restart each
// active warning re-fires once, consistent with the Event recorder's own
// re-aggregation window.
type warningEmitter struct {
	events events.EventRecorder // nil disables events (log-only)
	mu     sync.Mutex
	state  map[string]*poolWarnState // key: NodePool name
}

type poolWarnState struct {
	findingCodes map[string]bool   // last-warned non-fatal finding codes
	shortLead    map[string]bool   // last-warned short-lead NodeClaim names
	conflict     string            // last-warned policy-conflict detail ("" = none)
	noCandidate  string            // last-logged no-candidate reason key ("" = none)
	staticPool   types.UID         // UID of the NodePool already warned as static ("" = none)
	phPending    map[string]string // NodeClaim name → last-logged "reason|message"
	headroom     string            // last-warned headroom block identity, "claim|resource|refused|refusedResource" ("" = none)
}

func newWarningEmitter(rec events.EventRecorder) *warningEmitter {
	return &warningEmitter{events: rec, state: map[string]*poolWarnState{}}
}

// poolStateLocked returns (creating if needed) the dedup state for pool. Callers
// must hold w.mu.
func (w *warningEmitter) poolStateLocked(pool string) *poolWarnState {
	s := w.state[pool]
	if s == nil {
		s = &poolWarnState{
			findingCodes: map[string]bool{},
			shortLead:    map[string]bool{},
			phPending:    map[string]string{},
		}
		w.state[pool] = s
	}
	return s
}

// EmitFindings logs and emits a Warning Event on the NodePool for each non-fatal
// finding code that is newly present since the last pass. Fatal findings are not
// handled here — they keep their existing §5.2 gate behavior.
func (w *warningEmitter) EmitFindings(ctx context.Context, pool *karpv1.NodePool, findings []schedule.Finding) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.poolStateLocked(pool.Name)
	l := log.FromContext(ctx).WithValues("nodepool", pool.Name)

	current := map[string]bool{}
	for _, f := range findings {
		if f.Severity != schedule.Warn {
			continue
		}
		current[f.Code] = true
		// Un-deduplicated debug record (issue #100): emit the finding every pass at
		// debug verbosity, independent of the transition dedup below, so raised -v /
		// -zap-devel shows every evaluation rather than only transitions. Reconcile
		// liveness must still be judged from the controller_runtime_reconcile_* /
		// workqueue_* metrics, not from this log (spec §4.2).
		l.V(1).Info("schedule feasibility warning (debug, per-pass)", "code", f.Code, "detail", f.Message)
		if s.findingCodes[f.Code] {
			continue // already warned and still active — no re-fire
		}
		l.Info("schedule feasibility warning", "code", f.Code, "detail", f.Message)
		if w.events != nil {
			// note is a format string in the events API; pass the message as an
			// arg so a literal % in it is never interpreted.
			w.events.Eventf(pool, nil, corev1.EventTypeWarning, f.Code, actionEvaluateSchedule, "%s", f.Message)
		}
	}
	s.findingCodes = current
}

// EmitShortLead logs and emits a Warning Event on each NodeClaim that is newly
// short-lead since the last pass — the spec §3.2 layer-3 "warned via an event".
// The Event must be raised against the actual Karpenter object (events.EventRecorder
// requires a runtime.Object, which the pure selection.Claim view is not), so this
// projects claims itself (internal/adapt) rather than taking a pure view: that
// guarantees the short-lead names resolved by selection.ShortLeadClaims and the
// byName lookup used to raise the Event come from the same List result.
func (w *warningEmitter) EmitShortLead(ctx context.Context, pool *karpv1.NodePool, claims []karpv1.NodeClaim, leadTime selection.LeadTime) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.poolStateLocked(pool.Name)
	l := log.FromContext(ctx).WithValues("nodepool", pool.Name)

	views, byName := adapt.Claims(claims)
	current := map[string]bool{}
	for _, c := range selection.ShortLeadClaims(views, leadTime) {
		current[c.Name] = true
		// Un-deduplicated debug record (issue #100): see EmitFindings — emitted every
		// pass at debug verbosity, independent of the transition dedup below.
		l.V(1).Info("short-lead NodeClaim (debug, per-pass)", "nodeclaim", c.Name)
		if s.shortLead[c.Name] {
			continue
		}
		msg := fmt.Sprintf("NodeClaim %s can no longer guarantee the configured rotation chances against its own expireAfter (short-lead, spec §3.2 layer 3); it will be rotated best-effort before forceful expiration", c.Name)
		l.Info("short-lead NodeClaim", "nodeclaim", c.Name)
		if w.events != nil {
			w.events.Eventf(byName[c.Name], nil, corev1.EventTypeWarning, reasonShortLead, actionCheckExpiry, "%s", msg)
		}
	}
	s.shortLead = current
}

// EmitConflict logs and emits a Warning Event on the NodePool when it is blocked
// from rotating by a RotationPolicy conflict (an equal-specificity tie or a
// runtime-invalid policy, spec §5.4). Deduplicated on the detail string: the same
// conflict warns once and stays silent until it changes or ClearConflict resets
// it, so the once-per-reconcile re-evaluation does not spam.
func (w *warningEmitter) EmitConflict(ctx context.Context, pool *karpv1.NodePool, detail string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.poolStateLocked(pool.Name)
	if s.conflict == detail {
		return // already warned for this exact conflict — no re-fire
	}
	s.conflict = detail
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("rotation policy conflict", "detail", detail)
	if w.events != nil {
		w.events.Eventf(pool, nil, corev1.EventTypeWarning, reasonPolicyConflict, actionResolvePolicy, "%s", detail)
	}
}

// EmitStaticNodePool logs and emits a Warning Event on a Karpenter static
// capacity NodePool (spec.replicas set), which the surge mechanism cannot rotate
// (issue #302): the placeholder is pinned to its own NodePool by a required node
// affinity, and Karpenter's provisioner does not consider static NodePools for
// pending pods, so the placeholder can never induce the replacement capacity.
// Deduplicated on the NodePool's UID rather than a bare flag: Karpenter rejects
// a transition between static and dynamic on an existing NodePool (a CEL rule on
// spec.replicas), so the only way out of the condition is to delete the pool and
// create a dynamic one. A delete and recreate under the same name may never
// surface a NotFound reconcile for Forget to act on, and the new object must
// still get its own warning.
func (w *warningEmitter) EmitStaticNodePool(ctx context.Context, pool *karpv1.NodePool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.poolStateLocked(pool.Name)
	if s.staticPool == pool.UID {
		return // already warned for this NodePool — no re-fire
	}
	s.staticPool = pool.UID
	var replicas int64
	if pool.Spec.Replicas != nil {
		replicas = *pool.Spec.Replicas
	}
	msg := fmt.Sprintf("NodePool %s sets spec.replicas (Karpenter static capacity), which the surge rotation cannot serve: the surge placeholder is pinned to this NodePool and Karpenter's provisioner does not consider static NodePools for pending pods, so no replacement capacity can be induced. No rotation will be started for this NodePool. Karpenter does not allow spec.replicas to be added to or removed from an existing NodePool, so the options are to migrate the workload to a dynamic NodePool or to exclude this one from the RotationPolicy selector. Its nodes remain subject to Karpenter's forceful expiration.", pool.Name)
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info(
		"static NodePool cannot be rotated by surge; not starting a rotation", "replicas", replicas)
	if w.events != nil {
		w.events.Eventf(pool, nil, corev1.EventTypeWarning, reasonStaticNodePool, actionEvaluateNodePool, "%s", msg)
	}
}

// EmitWindowMissed logs and raises the Warning Event for a maintenance window
// occurrence that closed with candidates outstanding and no rotation
// attributable to it ever completing (spec §4.2, issue #303). Unlike the other
// emitters this one carries no dedup state: the caller has already proven it
// owned the transition by clearing the window-opened-at stamp, so it fires at
// most once per occurrence — the clear lands before this call, so a controller
// that stops in between drops the Event rather than inventing one.
func (w *warningEmitter) EmitWindowMissed(ctx context.Context, pool *karpv1.NodePool, openedAt string, c selection.Census) {
	msg := fmt.Sprintf(
		"maintenance window first observed open at %s closed with %d candidate(s) unrotated (%d eligible, %d still due and in retryBackoff): even after allowing an in-flight attempt to finish past the boundary, no rotation attributable to that occurrence completed. The guaranteed rotation chance for those NodeClaims was consumed without a graceful replacement; they remain subject to Karpenter's forceful expiration.",
		openedAt, decide.Outstanding(c), c.Eligible, c.InBackoffTriggered)
	// inBackoffTriggered, not inBackoff: the "no rotation candidate" census line
	// logs the raw first-disqualifier bucket under inBackoff, which also holds
	// claims the schedule stopped asking for. Two different numbers under one key
	// would read as an inconsistency, so this one carries the qualified name.
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info(
		"maintenance window closed with candidates unrotated",
		"windowOpenedAt", openedAt, "eligible", c.Eligible, "inBackoffTriggered", c.InBackoffTriggered)
	if w.events != nil {
		w.events.Eventf(pool, nil, corev1.EventTypeWarning, reasonWindowMissed, actionEvaluateSchedule, "%s", msg)
	}
}

// ClearStaticNodePool resets the static-capacity dedup state for a NodePool that
// is not static, so a dynamic pool recreated as a static one under the same name
// warns again.
func (w *warningEmitter) ClearStaticNodePool(pool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.state[pool]; s != nil {
		s.staticPool = ""
	}
}

// EmitHeadroomBlocked logs and raises the Warning Event for a rotation the
// surge_headroom gate is holding back (spec §5.2 step 3, issue #326.)
//
// Both call sites are level-triggered — the start gate re-evaluates every
// longRequeue and the failed-retry gate once per effective backoff — so this
// deduplicates on the block's IDENTITY: the candidate and the resource that did
// not fit. A different candidate or a different blocking resource is a new
// occurrence and re-fires; the same block stays silent however its numbers move.
//
// The numbers are deliberately not part of the key. `remaining` is derived from
// NodePool.status.resources, which tracks the pool's provisioned capacity, so any
// scale or consolidation elsewhere in the pool changes it while the block itself
// is unchanged — keying on the rendered message would re-fire the Event on every
// one of those, which on a busy pool means every longRequeue. They stay in the
// message and the log line, where they are what an operator acts on.
//
// The identity is still only as stable as the resource name, which is why
// surge.Headroom examines resources in sorted order.
//
// The Event is raised on the NodePool with the claim as the related object: what
// is stuck is the pool's rotation, and the pool is where an operator looks to ask
// why nothing is rotating.
func (w *warningEmitter) EmitHeadroomBlocked(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim, hr surge.HeadroomResult, clamp surge.ClampResult) {
	msg := fmt.Sprintf(
		"NodeClaim %s cannot be rotated: the surge placeholder needs %s %s but the NodePool has %s remaining of its spec.limits ceiling of %s (%s already provisioned). No rotation will start for this NodePool while that holds — the surge reserves replacement capacity before draining, so it consumes budget the limit does not allow. ",
		cand.Name, hr.Want.String(), hr.Resource, hr.Remaining.String(), hr.Limit.String(), provisionedString(hr))
	// The promise is scoped to the gate this Event is about. Saying "to let the
	// rotation proceed" and then, on the refused path, "may not be sufficient"
	// would retract its own sentence; clearing THIS gate is what raising the
	// budget does, and whether anything else stands behind it is stated next
	// (issue #326).
	msg += "Raise spec.limits, or reduce the pool's provisioned capacity, to clear this headroom gate; until then these nodes remain subject to Karpenter's forceful expiration."
	// A refused clamp is a second, independent condition, and it is arithmetic
	// about ONE ceiling: the candidate's cached status.allocatable minus the
	// DaemonSet overhead OBSERVED running on it. All it settles is that no clamp
	// value under that ceiling reserves any of the drain. It does not reach
	// schedulability — kube-scheduler decides that against real nodes, whose
	// Node.status.allocatable can exceed the cached per-type estimate (the band
	// the clamp is built on), so even a node of the SAME type carrying the SAME
	// DaemonSets can have room. A larger allowed type and a host with less
	// applicable overhead are two more ordinary ways, and Karpenter's estimate of
	// the set for a fresh node is not provably the set observed here either.
	//
	// The list is therefore EXAMPLES, and says so: naming two and treating the
	// rollback as what is left over would be the same overclaim this caveat exists
	// to avoid, one box smaller (issue #328). The operator is told what was
	// measured and which questions to ask, never which of them is the case.
	if clamp.Refused {
		msg += fmt.Sprintf(
			" Note that the budget may not be the only thing in the way: on this candidate's own values — its instance type's cached allocatable minus the DaemonSet overhead observed on it — %s has no positive ceiling left, so under that ceiling no clamp value reserves any of the drain. That does not decide whether the full-drain placeholder is schedulable. Ordinary ways it still is include a node reporting more allocatable than Karpenter's cached estimate for the type, a larger instance type the NodePool allows, and a node carrying less applicable DaemonSet overhead; which of these apply is not something this controller can determine, and they are examples rather than the full set. Widening the allowed instance types or reducing the DaemonSet footprint addresses the ones you control — or opt into surge.forcefulFallback for surge-less rotation.",
			clamp.RefusedResource)
	}

	// The refusal is part of the identity, including WHICH resource it names: a
	// refusal that moves from cpu to memory is a different diagnosis with a
	// different remedy even when the budget still runs out on the same resource
	// first, and must re-announce rather than hide under the earlier key.
	key := fmt.Sprintf("%s|%s|%t|%s", cand.Name, hr.Resource, clamp.Refused, clamp.RefusedResource)

	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.poolStateLocked(pool.Name)
	if s.headroom == key {
		return // same block, already announced — no re-fire
	}
	s.headroom = key
	kv := []any{
		"candidate", cand.Name, "resource", hr.Resource,
		"want", hr.Want.String(), "remaining", hr.Remaining.String(), "limit", hr.Limit.String(),
	}
	if clamp.Refused {
		kv = append(kv, "clampRefused", clamp.RefusedResource)
	}
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info(
		"insufficient limits headroom; cannot surge", kv...)
	if w.events != nil {
		w.events.Eventf(pool, cand, corev1.EventTypeWarning, reasonInsufficientHeadroom, actionEvaluateNodePool, "%s", msg)
	}
}

// provisionedString renders limit − remaining, the amount already provisioned
// against the ceiling. It is derived rather than carried so HeadroomResult keeps
// only the three numbers the gate itself compares.
func provisionedString(hr surge.HeadroomResult) string {
	used := hr.Limit.DeepCopy()
	used.Sub(hr.Remaining)
	return used.String()
}

// ClearHeadroomBlocked resets a NodePool's headroom dedup once the budget admits
// the surge again, so a block that returns later is announced as the new
// occurrence it is.
func (w *warningEmitter) ClearHeadroomBlocked(pool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.state[pool]; s != nil {
		s.headroom = ""
	}
}

// ClearConflict resets a NodePool's conflict dedup state once it is governed by a
// single valid policy again, so a conflict that recurs later re-fires its Event.
func (w *warningEmitter) ClearConflict(pool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.state[pool]; s != nil {
		s.conflict = ""
	}
}

// Forget drops a NodePool's dedup state, called when the NodePool is deleted so a
// recreated pool re-warns from a clean slate.
func (w *warningEmitter) Forget(pool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.state, pool)
}
