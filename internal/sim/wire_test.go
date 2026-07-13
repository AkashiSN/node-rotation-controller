package sim_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/sim"
)

// The derived structure of a run — generations, rotations and window intervals — is what a
// consumer draws instead of re-deriving policy semantics from the event stream. These tests
// pin the facts the events cannot carry, and the invariants that make the two agree.

// leadTime of basePolicy/alwaysOpenPolicy for a node whose tGP is 1h:
// K·P + readyTimeout + Buffer + tGP = 48h + 15m + 2m + 1h.
const leadTime1h = 48*time.Hour + 15*time.Minute + schedule.Buffer + time.Hour

func genOf(t *testing.T, tl sim.Timeline, slot, gen int) sim.Generation {
	t.Helper()
	for _, g := range tl.Generations {
		if g.Slot == slot && g.Gen == gen {
			return g
		}
	}
	t.Fatalf("no generation (slot=%d gen=%d) in %+v", slot, gen, tl.Generations)
	return sim.Generation{}
}

func wantInstant(t *testing.T, field string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %s, want %s", field, got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestGenerationsRecordTheSurgeRelay is the golden record of a completed surge. It pins
// what the event stream cannot say: which generation replaced which (node-ready is emitted
// under the OLD node's name, so the new generation's ready instant is unrecoverable from
// the events), and each generation's own deadline, drain cap and eligibility boundary.
func TestGenerationsRecordTheSurgeRelay(t *testing.T) {
	t.Parallel()

	pol := basePolicy() // window 02:00–06:00 daily
	tgp := time.Hour
	created := mustTime(t, "2026-03-01T00:00:00Z")
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: created}},
	}
	env := sim.Env{Provisioning: 3 * time.Minute, Drain: 7 * time.Minute}
	end := mustTime(t, "2026-03-13T06:00:00Z")

	tl, err := sim.Run(pol, f, env, sim.Options{
		Start: mustTime(t, "2026-03-12T23:00:00Z"),
		End:   end,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantInstant(t, "SimulatedThrough", tl.SimulatedThrough, end)

	if len(tl.Generations) != 2 {
		t.Fatalf("generations = %+v, want 2 (the node and its replacement)", tl.Generations)
	}

	// The initial node: no predecessor, no provisioning, on its declared CreatedAt.
	g0 := genOf(t, tl, 0, 0)
	if g0.Name != "node-a" || g0.BirthMode != sim.BirthInitial {
		t.Errorf("gen 0 = %+v, want node-a born initial", g0)
	}
	if g0.PredecessorGen != nil {
		t.Errorf("gen 0 predecessor = %d, want none: an initial node replaced nothing", *g0.PredecessorGen)
	}
	if !g0.ReadyAt.IsZero() {
		t.Errorf("gen 0 readyAt = %s, want absent: an initial node stages no surge", g0.ReadyAt)
	}
	if g0.Provisional {
		t.Error("gen 0 is provisional, want settled")
	}
	wantInstant(t, "gen 0 createdAt", g0.CreatedAt, created)
	wantInstant(t, "gen 0 deadline", g0.Deadline, created.Add(f.ExpireAfter))
	// The boundary is the node's own deadline minus ITS OWN lead time, which includes its
	// own tGP — not Result.A, the template-derived representative.
	wantInstant(t, "gen 0 eligibilityBoundary", g0.EligibilityBoundary, created.Add(f.ExpireAfter).Add(-leadTime1h))
	if g0.DrainCap != tgp || g0.DrainCapSource != sim.DrainCapExplicit {
		t.Errorf("gen 0 drain cap = %v (%s), want %v (explicit)", g0.DrainCap, g0.DrainCapSource, tgp)
	}

	// The replacement: born AT the rotation-start (not at node-ready — that anchor sets
	// every later deadline), carrying its own ready instant.
	start := mustTime(t, "2026-03-13T02:00:00Z")
	g1 := genOf(t, tl, 0, 1)
	if g1.Name != "node-a-r1" || g1.BirthMode != sim.BirthSurge {
		t.Errorf("gen 1 = %+v, want node-a-r1 born surge", g1)
	}
	if g1.PredecessorGen == nil || *g1.PredecessorGen != 0 {
		t.Errorf("gen 1 predecessor = %v, want 0", g1.PredecessorGen)
	}
	if g1.Provisional {
		t.Error("gen 1 is still provisional although its rotation completed")
	}
	wantInstant(t, "gen 1 createdAt", g1.CreatedAt, start)
	wantInstant(t, "gen 1 readyAt", g1.ReadyAt, start.Add(env.Provisioning))
	wantInstant(t, "gen 1 deadline", g1.Deadline, start.Add(f.ExpireAfter))

	// The rotation relates the two, with all three instants settled.
	if len(tl.Rotations) != 1 {
		t.Fatalf("rotations = %+v, want 1", tl.Rotations)
	}
	rot := tl.Rotations[0]
	if rot.Slot != 0 || rot.FromGen != 0 || rot.ToGen == nil || *rot.ToGen != 1 || rot.Mode != sim.RotationSurge {
		t.Errorf("rotation = %+v, want slot 0, gen 0 → 1, surge", rot)
	}
	wantInstant(t, "rotation start", rot.Start, start)
	wantInstant(t, "rotation ready", rot.Ready, start.Add(env.Provisioning))
	wantInstant(t, "rotation done", rot.Done, start.Add(env.Provisioning+env.Drain))
	// The relay's keystone: the replacement's readyAt IS the rotation's ready instant. The
	// events cannot say this — node-ready carries the old node's name.
	wantInstant(t, "gen 1 readyAt == rotation ready", g1.ReadyAt, rot.Ready)
}

// TestSurgeInFlightAtHorizonEndIsProvisional: a surge still running when the simulation
// ends must still produce its replacement's generation. Without it the make-before-break
// overlap — the provisioning of generation n+1 against the drain of generation n, the whole
// point of the timeline — would vanish in exactly the case a reader most wants to see,
// because sim increments the generation only at rotation-done.
func TestSurgeInFlightAtHorizonEndIsProvisional(t *testing.T) {
	t.Parallel()

	pol := alwaysOpenPolicy()
	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	env := sim.Env{Provisioning: 9 * time.Minute, Drain: 30 * time.Minute}

	// The rotation starts at 22:44 (the first tick past the trigger at 22:43), is Ready at
	// 22:53 and would be done at 23:23. The horizon ends at 23:00: mid-drain, with the
	// replacement provisioned and the old node still draining.
	end := mustTime(t, "2026-03-12T23:00:00Z")
	tl, err := sim.Run(pol, f, env, sim.Options{Start: mustTime(t, "2026-03-12T22:00:00Z"), End: end})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(kinds(tl.Events, sim.KindRotationDone)) != 0 {
		t.Fatalf("the rotation completed inside the horizon; this test needs it in flight")
	}

	start := mustTime(t, "2026-03-12T22:44:00Z")
	g1 := genOf(t, tl, 0, 1) // fails the test if the in-flight replacement is missing
	if !g1.Provisional {
		t.Error("the replacement of an in-flight rotation is not marked provisional")
	}
	if g1.BirthMode != sim.BirthSurge {
		t.Errorf("gen 1 birth mode = %q, want surge", g1.BirthMode)
	}
	wantInstant(t, "gen 1 createdAt", g1.CreatedAt, start)
	// It became Ready inside the horizon, so that instant is established even though the
	// rotation is not done — this is what draws the overlap.
	wantInstant(t, "gen 1 readyAt", g1.ReadyAt, start.Add(env.Provisioning))

	if len(tl.Rotations) != 1 {
		t.Fatalf("rotations = %+v, want 1", tl.Rotations)
	}
	rot := tl.Rotations[0]
	if rot.ToGen == nil || *rot.ToGen != 1 {
		t.Errorf("rotation toGen = %v, want 1: the surged replacement exists from the start", rot.ToGen)
	}
	wantInstant(t, "rotation ready", rot.Ready, start.Add(env.Provisioning))
	if !rot.Done.IsZero() {
		t.Errorf("rotation done = %s, want absent: the drain has not finished", rot.Done)
	}
}

// TestSurgelessInFlightNamesNoReplacement is the other half, and the one that is easy to get
// wrong. The surge-less (forceful-fallback) path stages no placeholder: its replacement is
// born at rotation-done. While such a rotation is still draining at the horizon's end, no
// replacement exists — so the rotation must name none. Naming one would require inventing a
// CreatedAt from a drain end the simulation never reached: reporting time it never
// simulated, the very defect this wire exists to fix.
func TestSurgelessInFlightNamesNoReplacement(t *testing.T) {
	t.Parallel()

	pol := alwaysOpenPolicy()
	pol.Surge.ForcefulFallback.Enabled = true

	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	env := sim.Env{Provisioning: 9 * time.Minute, Drain: 45 * time.Minute}

	// 30m before the deadline (2026-03-15T00:00Z) the remaining time is under t_rot, so the
	// pick takes the surge-less path — a deadline race, not a failure. The 45m drain runs
	// past the horizon's end.
	end := mustTime(t, "2026-03-14T23:50:00Z")
	tl, err := sim.Run(pol, f, env, sim.Options{Start: mustTime(t, "2026-03-14T23:30:00Z"), End: end})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tl.Rotations) != 1 {
		t.Fatalf("rotations = %+v, want 1", tl.Rotations)
	}
	rot := tl.Rotations[0]
	if rot.Mode != sim.RotationSurgeless {
		t.Fatalf("rotation mode = %q, want surgeless; this test needs the fallback path", rot.Mode)
	}
	if !rot.Ready.IsZero() {
		t.Errorf("rotation ready = %s, want absent: the surge-less path stages no surge", rot.Ready)
	}
	if !rot.Done.IsZero() {
		t.Errorf("rotation done = %s, want absent: the drain has not finished", rot.Done)
	}
	if rot.ToGen != nil {
		t.Errorf("rotation toGen = %d, want absent: the surge-less replacement is born at done, and this drain never finished — no such generation exists",
			*rot.ToGen)
	}
	if len(tl.Generations) != 1 {
		t.Errorf("generations = %+v, want only the initial node: the replacement does not exist yet", tl.Generations)
	}
}

// TestSurgelessReplacementIsBornAtDone pins the completed surge-less relay: the replacement
// appears at rotation-done (Karpenter provisions in response to the evicted Pods), carries
// no ready instant, and the rotation names it only then.
func TestSurgelessReplacementIsBornAtDone(t *testing.T) {
	t.Parallel()

	pol := alwaysOpenPolicy()
	pol.Surge.ForcefulFallback.Enabled = true

	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	env := sim.Env{Provisioning: 9 * time.Minute, Drain: 11 * time.Minute}

	tl, err := sim.Run(pol, f, env, sim.Options{
		Start: mustTime(t, "2026-03-14T23:30:00Z"),
		End:   mustTime(t, "2026-03-15T00:30:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rot := tl.Rotations[0]
	if rot.Mode != sim.RotationSurgeless || rot.Done.IsZero() {
		t.Fatalf("rotation = %+v, want a completed surge-less one", rot)
	}
	if rot.ToGen == nil || *rot.ToGen != 1 {
		t.Fatalf("rotation toGen = %v, want 1 once the replacement exists", rot.ToGen)
	}

	g1 := genOf(t, tl, 0, 1)
	if g1.BirthMode != sim.BirthSurgeless {
		t.Errorf("gen 1 birth mode = %q, want surgeless — an initial node and a surged replacement are both %q with a mere boolean",
			g1.BirthMode, "not surgeless")
	}
	if !g1.ReadyAt.IsZero() {
		t.Errorf("gen 1 readyAt = %s, want absent: no surge was staged", g1.ReadyAt)
	}
	if g1.Provisional {
		t.Error("gen 1 is provisional although its rotation completed")
	}
	// Born at done, not at start: this anchor is what sets its own deadline.
	wantInstant(t, "gen 1 createdAt", g1.CreatedAt, rot.Done)
	wantInstant(t, "gen 1 deadline", g1.Deadline, rot.Done.Add(f.ExpireAfter))
}

// TestEligibilityBoundaryIsPerNodeAndExclusive: the boundary follows the node's OWN tGP
// (LeadTime.For adds it), and the trigger is a strict inequality — a node exactly at the
// boundary is not yet eligible, which is why the field is a boundary and not an "eligibleAt".
func TestEligibilityBoundaryIsPerNodeAndExclusive(t *testing.T) {
	t.Parallel()

	pol := alwaysOpenPolicy()
	tgp := time.Hour
	long := 3 * time.Hour // node-b's own tGP: a longer lead time, an earlier boundary
	created := mustTime(t, "2026-03-01T00:00:00Z")
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes: []sim.Node{
			{Name: "node-a", CreatedAt: created},
			{Name: "node-b", CreatedAt: created, TGP: &long},
		},
	}

	// A horizon that ends before either node is eligible: nothing rotates, and the boundary
	// records stand alone.
	tl, err := sim.Run(pol, f, sim.Env{}, sim.Options{
		Start: mustTime(t, "2026-03-12T20:00:00Z"),
		End:   mustTime(t, "2026-03-12T20:30:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(kinds(tl.Events, sim.KindRotationStart)) != 0 {
		t.Fatalf("a rotation started before any boundary was crossed")
	}

	deadline := created.Add(f.ExpireAfter)
	a := genOf(t, tl, 0, 0)
	wantInstant(t, "node-a eligibilityBoundary", a.EligibilityBoundary, deadline.Add(-leadTime1h))

	// node-b's own 3h tGP lengthens its lead time by 2h, so its boundary is 2h EARLIER —
	// the template-derived Result.A would report both nodes the same and be wrong for one.
	b := genOf(t, tl, 1, 0)
	wantInstant(t, "node-b eligibilityBoundary", b.EligibilityBoundary, deadline.Add(-leadTime1h-2*time.Hour))
	if b.DrainCap != long || b.DrainCapSource != sim.DrainCapExplicit {
		t.Errorf("node-b drain cap = %v (%s), want %v (explicit)", b.DrainCap, b.DrainCapSource, long)
	}

	// Exclusive: the trigger is `age > boundary`, so a horizon ending exactly AT the
	// boundary must not rotate the node. The tick grid lands on the boundary (a whole
	// minute), so this is observable rather than rounded away. node-b is dropped here: its
	// own boundary is 2h earlier, so it would rotate first and mask the check.
	alone := sim.Fleet{ExpireAfter: f.ExpireAfter, TGP: &tgp, Nodes: f.Nodes[:1]}
	tl2, err := sim.Run(pol, alone, sim.Env{}, sim.Options{
		Start: a.EligibilityBoundary.Add(-time.Minute),
		End:   a.EligibilityBoundary,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := kinds(tl2.Events, sim.KindRotationStart); len(got) != 0 {
		t.Errorf("rotation-start = %+v at or before the eligibility boundary %s, want none: the trigger is strict (age > boundary)",
			got, a.EligibilityBoundary.Format(time.RFC3339))
	}
}

// TestDrainCapFallbackWhenTGPUnset: a fleet with no terminationGracePeriod is bounded by the
// fixed fallback, and the record says so — the consumer must never re-derive that constant.
func TestDrainCapFallbackWhenTGPUnset(t *testing.T) {
	t.Parallel()

	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour, // TGP unset
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	tl, err := sim.Run(basePolicy(), f, sim.Env{}, sim.Options{
		Start: mustTime(t, "2026-03-13T02:00:00Z"),
		End:   mustTime(t, "2026-03-13T03:00:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	g := genOf(t, tl, 0, 0)
	if g.DrainCap != schedule.DrainFallback || g.DrainCapSource != sim.DrainCapFallback {
		t.Errorf("drain cap = %v (%s), want %v (fallback)", g.DrainCap, g.DrainCapSource, schedule.DrainFallback)
	}
}

// TestWindowIntervalsClipping pins the clipped flags, which are what keep a horizon artifact
// from being drawn as a real transition of the schedule.
func TestWindowIntervalsClipping(t *testing.T) {
	t.Parallel()

	pol := basePolicy() // 02:00–06:00 daily, UTC
	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-13T01:00:00Z")}}, // never eligible
	}

	tests := map[string]struct {
		start, end               string
		wantStart, wantEnd       string
		startClipped, endClipped bool
		wantCloseEvent           bool
	}{
		// The horizon opens exactly at a union boundary: a REAL opening, not a clip.
		// InWindow(start) alone cannot tell this apart from the mid-window case below.
		"starts exactly at the window open": {
			start: "2026-03-13T02:00:00Z", end: "2026-03-13T06:00:00Z",
			wantStart: "2026-03-13T02:00:00Z", wantEnd: "2026-03-13T06:00:00Z",
			startClipped: false, endClipped: false, wantCloseEvent: true,
		},
		// Mid-window: the occurrence began before the simulation did.
		"starts inside an already-open window": {
			start: "2026-03-13T03:00:00Z", end: "2026-03-13T06:00:00Z",
			wantStart: "2026-03-13T03:00:00Z", wantEnd: "2026-03-13T06:00:00Z",
			startClipped: true, endClipped: false, wantCloseEvent: true,
		},
		// Still open when the simulation stops: the interval records where observation
		// ended — and NO window-close event is emitted for a window that never closed.
		"ends inside an open window": {
			start: "2026-03-13T02:00:00Z", end: "2026-03-13T05:00:00Z",
			wantStart: "2026-03-13T02:00:00Z", wantEnd: "2026-03-13T05:00:00Z",
			startClipped: false, endClipped: true, wantCloseEvent: false,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tl, err := sim.Run(pol, f, sim.Env{}, sim.Options{
				Start: mustTime(t, tc.start), End: mustTime(t, tc.end),
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(tl.Windows) != 1 {
				t.Fatalf("windows = %+v, want exactly 1 occurrence", tl.Windows)
			}
			w := tl.Windows[0]
			wantInstant(t, "window start", w.Start, mustTime(t, tc.wantStart))
			wantInstant(t, "window end", w.End, mustTime(t, tc.wantEnd))
			if w.StartClipped != tc.startClipped || w.EndClipped != tc.endClipped {
				t.Errorf("clipping = (start %v, end %v), want (%v, %v)",
					w.StartClipped, w.EndClipped, tc.startClipped, tc.endClipped)
			}
			if got := len(kinds(tl.Events, sim.KindWindowClose)) > 0; got != tc.wantCloseEvent {
				t.Errorf("window-close event present = %v, want %v — the event means a GENUINE union transition; a synthetic one would make a consumer that has only the events draw a close for a window that never closed",
					got, tc.wantCloseEvent)
			}
		})
	}
}

// TestPartialRunSweepsAtLastProcessed is the defect this wire depends on. When the step
// budget runs out, the final sweep must run at the last instant actually PROCESSED — not at
// the requested end. Sweeping at the requested end reports breaches and interval ends drawn
// from time the simulator never reached, and no consumer can tell them from real ones.
//
// The loop cursor at break is not a safe stand-in either: the step guard runs before the
// body, so that instant was never processed.
func TestPartialRunSweepsAtLastProcessed(t *testing.T) {
	t.Parallel()

	pol := basePolicy() // window 02:00–06:00 daily
	tgp := time.Hour
	// A node whose deadline is deep in the un-simulated tail: if the sweep ran at the
	// requested end, it would report this breach — from time that was never simulated.
	created := mustTime(t, "2030-01-01T00:00:00Z")
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: created}},
	}

	// maxSteps is 2,000,000 ticks of one minute ≈ 3.8 years; a 10-year horizon exhausts it
	// well before the node above is even created.
	start := mustTime(t, "2026-01-01T00:00:00Z")
	end := mustTime(t, "2036-01-01T00:00:00Z")
	tl, err := sim.Run(pol, f, sim.Env{}, sim.Options{Start: start, End: end})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !tl.Partial {
		t.Fatalf("Partial = false: this test needs the step budget to be exhausted")
	}
	if !tl.SimulatedThrough.Before(end) {
		t.Fatalf("SimulatedThrough = %s, want an instant before the requested end %s",
			tl.SimulatedThrough.Format(time.RFC3339), end.Format(time.RFC3339))
	}

	// Every instant in the response is one the simulator actually reached.
	for _, e := range tl.Events {
		if e.At.After(tl.SimulatedThrough) {
			t.Errorf("event %+v is at %s, past simulatedThrough %s", e.Kind, e.At, tl.SimulatedThrough)
		}
		if e.Until.After(tl.SimulatedThrough) {
			t.Errorf("event %+v runs until %s, past simulatedThrough %s", e.Kind, e.Until, tl.SimulatedThrough)
		}
	}
	for _, w := range tl.Windows {
		if w.End.After(tl.SimulatedThrough) {
			t.Errorf("window %+v ends past simulatedThrough %s", w, tl.SimulatedThrough)
		}
	}
	if b := kinds(tl.Events, sim.KindExpireAfterBreach); len(b) != 0 {
		t.Errorf("expire-after-breach = %+v: the node's deadline lies in the un-simulated tail; sweeping the REQUESTED end invented this", b)
	}
	// The diagnostic must name the instant the run actually stopped at.
	var msg string
	for _, d := range tl.Diagnostics {
		if d.Code == "StepBudgetExhausted" {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("diagnostics = %+v, want StepBudgetExhausted", tl.Diagnostics)
	}
	if want := tl.SimulatedThrough.Format(time.RFC3339); !strings.Contains(msg, want) {
		t.Errorf("StepBudgetExhausted says %q, want it to name simulatedThrough %s", msg, want)
	}
}

// TestWireInvariants sweeps a range of runs and asserts the contract a consumer is entitled
// to rely on. Defensive handling in the page guards against a truncated response; it is not
// a licence for the producer to emit an inconsistent one.
func TestWireInvariants(t *testing.T) {
	t.Parallel()

	tgp := time.Hour
	created := mustTime(t, "2026-03-01T00:00:00Z")
	fleet := func(n int) sim.Fleet {
		f := sim.Fleet{ExpireAfter: 14 * 24 * time.Hour, TGP: &tgp}
		for i := range n {
			f.Nodes = append(f.Nodes, sim.Node{
				Name:      string(rune('a'+i)) + "-node",
				CreatedAt: created.Add(time.Duration(i) * time.Hour),
			})
		}
		return f
	}

	cases := []struct {
		name       string
		surgeless  bool
		alwaysOpen bool
		nodes      int
		env        sim.Env
		start, end string
	}{
		{"windowed fleet", false, false, 3, sim.Env{Provisioning: 5 * time.Minute, Drain: 10 * time.Minute},
			"2026-03-12T23:00:00Z", "2026-03-16T06:00:00Z"},
		{"always open, several generations", false, true, 2, sim.Env{Provisioning: 9 * time.Minute, Drain: 11 * time.Minute},
			"2026-03-12T00:00:00Z", "2026-04-10T00:00:00Z"},
		{"forceful fallback", true, true, 2, sim.Env{Provisioning: 9 * time.Minute, Drain: 20 * time.Minute},
			"2026-03-14T23:30:00Z", "2026-03-30T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pol := basePolicy()
			if tc.alwaysOpen {
				pol = alwaysOpenPolicy()
			}
			pol.Surge.ForcefulFallback.Enabled = tc.surgeless

			tl, err := sim.Run(pol, fleet(tc.nodes), tc.env, sim.Options{
				Start: mustTime(t, tc.start), End: mustTime(t, tc.end),
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tl.SimulatedThrough.IsZero() {
				t.Fatal("SimulatedThrough is zero")
			}

			// 1. Nothing in the response lies beyond the instant actually simulated.
			for _, e := range tl.Events {
				if e.At.After(tl.SimulatedThrough) || e.Until.After(tl.SimulatedThrough) {
					t.Errorf("event %+v lies past simulatedThrough %s", e, tl.SimulatedThrough)
				}
			}
			for _, w := range tl.Windows {
				if w.End.After(tl.SimulatedThrough) || w.End.Before(w.Start) {
					t.Errorf("window %+v is not a sane interval within [start, simulatedThrough]", w)
				}
			}

			// 2. Every rotation refers to generations that exist in ITS OWN slot.
			index := map[[2]int]sim.Generation{}
			for _, g := range tl.Generations {
				k := [2]int{g.Slot, g.Gen}
				if _, dup := index[k]; dup {
					t.Errorf("duplicate generation (slot=%d gen=%d)", g.Slot, g.Gen)
				}
				index[k] = g
			}
			for _, rot := range tl.Rotations {
				from, ok := index[[2]int{rot.Slot, rot.FromGen}]
				if !ok {
					t.Fatalf("rotation %+v starts from a generation that does not exist", rot)
				}
				if from.Provisional {
					t.Errorf("rotation %+v rotates a provisional generation", rot)
				}

				// 3. A completed surge's replacement carries the rotation's own ready
				//    instant; an in-flight one has no done.
				switch rot.Mode {
				case sim.RotationSurge:
					to, ok := index[[2]int{rot.Slot, *rot.ToGen}]
					if rot.ToGen == nil || !ok {
						t.Fatalf("surge %+v names no existing replacement: it is born at the start", rot)
					}
					if to.BirthMode != sim.BirthSurge || to.PredecessorGen == nil || *to.PredecessorGen != rot.FromGen {
						t.Errorf("replacement %+v does not record the relay from generation %d", to, rot.FromGen)
					}
					if !rot.Ready.IsZero() && !to.ReadyAt.Equal(rot.Ready) {
						t.Errorf("replacement readyAt %s != rotation ready %s", to.ReadyAt, rot.Ready)
					}
					if to.Provisional == !rot.Done.IsZero() {
						t.Errorf("rotation %+v done=%v but its replacement provisional=%v — they must be opposites", rot, !rot.Done.IsZero(), to.Provisional)
					}
				case sim.RotationSurgeless:
					if !rot.Ready.IsZero() {
						t.Errorf("surge-less rotation %+v carries a ready instant, but it stages no surge", rot)
					}
					if rot.Done.IsZero() && rot.ToGen != nil {
						t.Errorf("in-flight surge-less rotation %+v names a replacement that cannot exist yet", rot)
					}
					if !rot.Done.IsZero() {
						to, ok := index[[2]int{rot.Slot, *rot.ToGen}]
						if rot.ToGen == nil || !ok {
							t.Fatalf("completed surge-less rotation %+v names no existing replacement", rot)
						}
						if to.BirthMode != sim.BirthSurgeless || !to.CreatedAt.Equal(rot.Done) {
							t.Errorf("surge-less replacement %+v is not born at its rotation's done %s", to, rot.Done)
						}
					}
				default:
					t.Errorf("rotation %+v has an unknown mode", rot)
				}
			}

			// 4. Parity with the event stream, wherever the two overlap.
			if got, want := len(tl.Rotations), len(kinds(tl.Events, sim.KindRotationStart)); got != want {
				t.Errorf("rotations = %d, rotation-start events = %d", got, want)
			}
			var done int
			for _, rot := range tl.Rotations {
				if !rot.Done.IsZero() {
					done++
				}
			}
			if want := len(kinds(tl.Events, sim.KindRotationDone)); done != want {
				t.Errorf("completed rotations = %d, rotation-done events = %d", done, want)
			}
			if got, want := len(tl.Windows), len(kinds(tl.Events, sim.KindWindowOpen)); got != want {
				t.Errorf("window intervals = %d, window-open events = %d", got, want)
			}
		})
	}
}
