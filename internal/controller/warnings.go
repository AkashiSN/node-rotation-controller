package controller

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// Event action verbs (the events.k8s.io "action" field — the machine-readable
// operation the controller was performing). The specific condition is carried by
// the reason (the finding code, or "ShortLead"); these name the operation.
const (
	actionEvaluateSchedule = "EvaluateSchedule"
	actionCheckExpiry      = "CheckExpiry"
	actionResolvePolicy    = "ResolvePolicy"
	reasonShortLead        = "ShortLead"
	reasonPolicyConflict   = "PolicyConflict"
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
	findingCodes map[string]bool // last-warned non-fatal finding codes
	shortLead    map[string]bool // last-warned short-lead NodeClaim names
	conflict     string          // last-warned policy-conflict detail ("" = none)
}

func newWarningEmitter(rec events.EventRecorder) *warningEmitter {
	return &warningEmitter{events: rec, state: map[string]*poolWarnState{}}
}

// poolStateLocked returns (creating if needed) the dedup state for pool. Callers
// must hold w.mu.
func (w *warningEmitter) poolStateLocked(pool string) *poolWarnState {
	s := w.state[pool]
	if s == nil {
		s = &poolWarnState{findingCodes: map[string]bool{}, shortLead: map[string]bool{}}
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
func (w *warningEmitter) EmitShortLead(ctx context.Context, pool *karpv1.NodePool, claims []karpv1.NodeClaim, leadTime selection.LeadTime) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.poolStateLocked(pool.Name)
	l := log.FromContext(ctx).WithValues("nodepool", pool.Name)

	current := map[string]bool{}
	for _, c := range selection.ShortLeadClaims(claims, leadTime) {
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
			w.events.Eventf(c, nil, corev1.EventTypeWarning, reasonShortLead, actionCheckExpiry, "%s", msg)
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
