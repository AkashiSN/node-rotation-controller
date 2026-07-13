package sim

import (
	"fmt"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/decide"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// loop advances the virtual clock from Start to End.
//
// At every instant the order mirrors the reconcile loop (§5.2): drive the in-flight
// rotation first (step 1), then — only when none is in flight — the fatal-feasibility
// gate (step 1b), the candidate-independent start gates (step 2), and the pick
// (step 3). Nothing here decides anything itself: decide.StartGate,
// selection.PickEarliestDeadlineEligible and decide.SurgelessFallback do.
func (r *run) loop() {
	// A schedule whose findings are Fatal must not start ANY rotation (§3.2 layer 1,
	// §5.2 step 1b). That is real controller behaviour, not an unmodelled path, so it is
	// a Diagnostic and the clock still runs: the timeline shows the nodes marching into
	// their expireAfter backstop, which is exactly what would happen.
	fatal, blocked := firstFatal(r.res.Derived.Findings)
	if blocked {
		r.diagOnce(Diagnostic{
			Severity: schedule.Fatal,
			Code:     fatal.Code,
			Message:  fmt.Sprintf("%s — the controller refuses to start a rotation while a feasibility finding is fatal (spec §3.2 layer 1), so no node rotates", fatal.Message),
		})
	}

	now := r.opts.Start
	inWindow := r.sched.InWindow(now)
	if inWindow {
		r.emit(Event{Kind: KindWindowOpen, At: now})
	}

	for steps := 0; !now.After(r.opts.End); steps++ {
		if steps >= maxSteps {
			r.tl.Partial = true
			r.diagOnce(Diagnostic{
				Severity: schedule.Warn,
				Code:     "StepBudgetExhausted",
				Message: fmt.Sprintf("the simulation stopped at %s: the horizon needs more than %d steps of the %v decision cadence. Shorten the horizon",
					now.Format(time.RFC3339), maxSteps, tick),
			})
			break
		}

		if open := r.sched.InWindow(now); open != inWindow {
			inWindow = open
			if open {
				r.emit(Event{Kind: KindWindowOpen, At: now})
			} else {
				r.emit(Event{Kind: KindWindowClose, At: now})
			}
		}

		r.advance(now)
		r.breachCheck(now)
		if r.rot == nil {
			r.maybeStart(now, inWindow)
		}

		next := r.next(now)
		if !next.After(now) { // defensive: the clock must always move forward
			break
		}
		now = next
	}

	// A deadline does not fall on the tick grid, so the last tick of the horizon can lie
	// before it: a breach at (lastTick, End] — including End itself, the horizon an
	// operator asking "does every node make it?" would naturally choose — would otherwise
	// go unreported. Sweep once more at End. A rotation that completed before its deadline
	// re-created the node with a fresh CreatedAt, so this cannot invent a breach.
	r.breachCheck(r.opts.End)

	// Close any interval still open at the horizon so the UI is never left with a
	// dangling "blocked since …" it has to guess the end of.
	r.flushBlocked(r.opts.End)
	r.flushCensus(r.opts.End)
}

// next is the following decision instant: the next tick of the grid, or an in-flight
// rotation's provisioning/drain completion if that lands sooner. Completions are
// processed at their exact instant so a sub-minute Env is not rounded away.
func (r *run) next(now time.Time) time.Time {
	next := now.Truncate(tick).Add(tick)
	if r.rot != nil {
		if !r.rot.surgeless && r.rot.readyAt.After(now) && r.rot.readyAt.Before(next) {
			next = r.rot.readyAt
		}
		if r.rot.doneAt.After(now) && r.rot.doneAt.Before(next) {
			next = r.rot.doneAt
		}
	}
	return next
}

// advance drives the in-flight rotation (§5.2 step 1): surge Ready, then completion.
func (r *run) advance(now time.Time) {
	rot := r.rot
	if rot == nil {
		return
	}
	n := &r.nodes[rot.slot]
	if !rot.surgeless && n.state == annotations.StatePending && !now.Before(rot.readyAt) {
		// The surge node is Ready; the old NodeClaim is deleted and its drain begins.
		n.state = annotations.StateDraining
		r.emit(Event{Kind: KindNodeReady, At: rot.readyAt, Node: n.name()})
	}
	if now.Before(rot.doneAt) {
		return
	}

	old := n.name()
	// Replacement materialization. The replacement's CreatedAt sets the NEXT
	// generation's deadline (selection.deadlineOf = CreatedAt + expireAfter), so it is
	// pinned, not left to chance:
	//   - surge path: the rotation-start instant. Karpenter creates the replacement
	//     NodeClaim as soon as the low-priority placeholder Pod goes pending, and
	//     Env.Provisioning is the time from THAT to Ready. Anchoring on node-ready would
	//     push every generation's deadline back by Env.Provisioning, compound across
	//     generations and UNDER-REPORT breaches — the one error class this simulator
	//     must not make.
	//   - surge-less path: the rotation-done instant. There is no placeholder, so
	//     Karpenter provisions in response to the evicted Pods. An approximation (it may
	//     react during the drain), conservative in the safe direction: an earlier real
	//     creation only makes the next deadline earlier.
	createdAt := rot.start
	if rot.surgeless {
		createdAt = rot.doneAt
	}
	// A replacement is provisioned from the NodePool template, so it inherits the
	// template's expireAfter/tGP — not the old node's per-node overrides.
	n.gen++
	n.createdAt = createdAt
	n.expireAfter = r.template.ExpireAfter
	n.tgp = r.template.TGP
	n.state = ""
	n.breached = false

	r.poolAnn[annotations.LastRotationAt] = rot.doneAt.Format(time.RFC3339)
	r.emit(Event{Kind: KindRotationDone, At: rot.doneAt, Node: old, Replacement: n.name(), Surgeless: rot.surgeless})
	r.rot = nil
}

// breachCheck reports a node that reached its expireAfter deadline without having
// completed a rotation — the outcome the controller exists to prevent, and the red mark
// on the timeline. Karpenter's Forceful Expiration takes the node here, at an instant
// nobody chose.
//
// A node whose rotation is IN FLIGHT is checked too, deliberately: a rotation that is
// still draining when the deadline passes has lost the race, and suppressing that mark
// would under-report breaches. sim leaves the node in the fleet — it still completes
// its rotation, and the fresh generation clears the mark.
func (r *run) breachCheck(now time.Time) {
	for i := range r.nodes {
		n := &r.nodes[i]
		if n.breached {
			continue
		}
		if !now.Before(n.deadline()) {
			n.breached = true
			r.emit(Event{Kind: KindExpireAfterBreach, At: n.deadline(), Node: n.name()})
		}
	}
}

// maybeStart runs the start gates and the pick, and starts a rotation when both pass.
func (r *run) maybeStart(now time.Time, inWindow bool) {
	if _, fatal := firstFatal(r.res.Derived.Findings); fatal {
		return // §5.2 step 1b: a fatal finding gates every start. The Diagnostic said so.
	}

	gi := decide.Inputs{
		Now:             now,
		InWindow:        inWindow,
		Annotations:     r.poolAnn,
		Cooldown:        r.res.Cooldown,
		FailurePause:    r.res.FailurePause,
		FallbackEnabled: r.pol.Surge.ForcefulFallback.Enabled,
		ReadyTimeout:    r.res.ReadyTimeout,
		DrainBound:      r.res.DrainBound,
	}
	if open, gate := decide.StartGate(gi); !open {
		r.flushCensus(now)
		r.recordBlocked(now, gate)
		return
	}
	r.flushBlocked(now)

	views := r.views()
	sel := selection.Inputs{
		Now:          now,
		LeadTime:     r.res.LeadTime,
		Override:     r.res.Override,
		RetryBackoff: r.res.RetryBackoff,
	}
	pick := selection.PickEarliestDeadlineEligible(views, sel)
	if pick == nil {
		r.recordCensus(now, selection.TakeCensus(views, sel))
		return
	}
	r.flushCensus(now)

	slot := r.slotOf(pick.Name)
	n := &r.nodes[slot]
	// A candidate that cannot complete a graceful surge before its own deadline rotates
	// surge-less when the opt-in fallback is enabled (ADR-0001): no placeholder, no
	// surge, drain only. This is a deadline-race branch, not a failure branch.
	surgeless := decide.SurgelessFallback(pick, gi)
	rot := &rotation{slot: slot, start: now, surgeless: surgeless}
	drain := r.drainFor(n)
	if surgeless {
		n.state = annotations.StateDraining
		rot.doneAt = now.Add(drain)
	} else {
		n.state = annotations.StatePending
		rot.readyAt = now.Add(r.env.Provisioning)
		rot.doneAt = rot.readyAt.Add(drain)
	}
	r.rot = rot
	r.emit(Event{Kind: KindRotationStart, At: now, Node: n.name(), Surgeless: surgeless})
}

// views projects the fleet onto the pure claim view selection reads. Every node is
// Ready (sim models no NotReady nodes) and none is Deleting: the drain of a rotating
// node is modelled by its state annotation, which is what excludes it from selection.
func (r *run) views() []selection.Claim {
	out := make([]selection.Claim, len(r.nodes))
	for i := range r.nodes {
		n := &r.nodes[i]
		ea := n.expireAfter
		c := selection.Claim{
			Name:        n.name(),
			CreatedAt:   n.createdAt,
			ExpireAfter: &ea,
			TGP:         n.tgp,
			Ready:       true,
		}
		if n.state != "" {
			c.Annotations = map[string]string{annotations.State: n.state}
		}
		out[i] = c
	}
	return out
}

func (r *run) slotOf(name string) int {
	for i := range r.nodes {
		if r.nodes[i].name() == name {
			return i
		}
	}
	return -1 // unreachable: the pick always aliases a view built from r.nodes
}

// recordBlocked / recordCensus / flush* implement the edge-triggered coalescing
// contract: one event per REASON change, carrying the interval it covers, instead of
// one per tick. A week-long out-of-window stretch is one event, not ten thousand.
func (r *run) recordBlocked(now time.Time, gate decide.Gate) {
	if r.blockGate == gate && !r.blockFrom.IsZero() {
		return
	}
	r.flushBlocked(now)
	r.blockGate, r.blockFrom = gate, now
}

func (r *run) flushBlocked(at time.Time) {
	if r.blockFrom.IsZero() {
		return
	}
	r.emit(Event{Kind: KindBlockedByGate, At: r.blockFrom, Until: at, Gate: r.blockGate})
	r.blockFrom, r.blockGate = time.Time{}, decide.GateNone
}

func (r *run) recordCensus(now time.Time, c selection.Census) {
	if r.hasCensus && r.census == c {
		return
	}
	r.flushCensus(now)
	r.census, r.censusFrom, r.hasCensus = c, now, true
}

func (r *run) flushCensus(at time.Time) {
	if !r.hasCensus {
		return
	}
	c := r.census
	r.emit(Event{Kind: KindNoEligibleClaim, At: r.censusFrom, Until: at, Census: &c})
	r.hasCensus = false
}

func firstFatal(fs []schedule.Finding) (schedule.Finding, bool) {
	for _, f := range fs {
		if f.Severity == schedule.Fatal {
			return f, true
		}
	}
	return schedule.Finding{}, false
}
