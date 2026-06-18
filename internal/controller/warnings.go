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
	reasonShortLead        = "ShortLead"
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

// Forget drops a NodePool's dedup state, called when the NodePool is deleted so a
// recreated pool re-warns from a clean slate.
func (w *warningEmitter) Forget(pool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.state, pool)
}
