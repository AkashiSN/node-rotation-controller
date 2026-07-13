package sim_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/decide"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/sim"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

// basePolicy is a daily 4h window (02:00–06:00 UTC) with the surge knobs pinned so the
// derivation is exact: P = 24h, D = 4h.
func basePolicy() *policy.Policy {
	p := &policy.Policy{
		AgeThreshold: "auto",
		MaintenanceWindows: []policy.MaintenanceWindow{{
			Timezone: "UTC",
			Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
			Start:    "02:00",
			End:      "06:00",
		}},
		Surge: policy.Surge{
			ReadyTimeout:         dur(15 * time.Minute),
			CooldownAfter:        dur(10 * time.Minute),
			RetryBackoff:         dur(30 * time.Minute),
			DrainEstimate:        dur(10 * time.Minute),
			ProvisioningEstimate: dur(5 * time.Minute),
		},
	}
	p.ApplyDefaults()
	return p
}

func kinds(evs []sim.Event, k sim.Kind) []sim.Event {
	var out []sim.Event
	for _, e := range evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// TestRunGoldenSuccessPath pins the surge path's three instants exactly. It is the
// primary golden: a single node, one rotation, no ambiguity about when anything fires.
//
// The node is created 2026-03-01T00:00Z and the horizon starts well past its
// eligibility trigger, so the rotation starts the moment the window opens.
func TestRunGoldenSuccessPath(t *testing.T) {
	t.Parallel()

	pol := basePolicy()
	// E = 14d, tGP = 1h, K = 2, P = 24h, readyTimeout = 15m, Buffer = 2m.
	// leadTime = K*P + readyTimeout + Buffer + tGP = 48h + 15m + 2m + 1h = 49h17m.
	// A = E - leadTime = 14d - 49h17m = 286h43m; the node is eligible once it is older.
	tgp := time.Hour
	created := mustTime(t, "2026-03-01T00:00:00Z")
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: created}},
	}
	env := sim.Env{Provisioning: 3 * time.Minute, Drain: 7 * time.Minute}

	// Start the horizon at an instant where the node is already eligible (age 287h at
	// 2026-03-12T23:00Z > A = 286h43m) and the window is shut, so the first legal start
	// is the next window open: 2026-03-13T02:00Z.
	tl, err := sim.Run(pol, f, env, sim.Options{
		Start: mustTime(t, "2026-03-12T23:00:00Z"),
		End:   mustTime(t, "2026-03-13T06:00:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tl.Partial {
		t.Fatalf("Partial = true, diagnostics: %+v", tl.Diagnostics)
	}
	if len(tl.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", tl.Diagnostics)
	}

	wantA := 14*24*time.Hour - (48*time.Hour + 15*time.Minute + 2*time.Minute + time.Hour)
	if tl.Result.A != wantA {
		t.Errorf("A = %v, want %v", tl.Result.A, wantA)
	}
	// t_rot = readyTimeout + tGP + Buffer (the DEADLINE bound), t_rot_est =
	// provisioningEstimate + drainEstimate (the FORECAST) — never each other.
	if want := 15*time.Minute + time.Hour + schedule.Buffer; tl.Result.TRot != want {
		t.Errorf("TRot = %v, want %v", tl.Result.TRot, want)
	}
	if want := 15 * time.Minute; tl.Result.TRotEst != want {
		t.Errorf("TRotEst = %v, want %v", tl.Result.TRotEst, want)
	}

	windowOpen := mustTime(t, "2026-03-13T02:00:00Z")
	want := []sim.Event{
		{Kind: sim.KindRotationStart, At: windowOpen, Node: "node-a"},
		{Kind: sim.KindNodeReady, At: windowOpen.Add(3 * time.Minute), Node: "node-a"},
		{Kind: sim.KindRotationDone, At: windowOpen.Add(10 * time.Minute), Node: "node-a", Replacement: "node-a-r1"},
	}
	var got []sim.Event
	for _, e := range tl.Events {
		switch e.Kind {
		case sim.KindRotationStart, sim.KindNodeReady, sim.KindRotationDone:
			got = append(got, e)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("rotation events = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.Kind || !g.At.Equal(w.At) || g.Node != w.Node || g.Replacement != w.Replacement || g.Surgeless {
			t.Errorf("event[%d] = %+v, want %+v", i, g, w)
		}
	}

	// No node may breach: the whole point of the rotation.
	if b := kinds(tl.Events, sim.KindExpireAfterBreach); len(b) != 0 {
		t.Errorf("expire-after-breach events = %+v, want none", b)
	}
}

// alwaysOpenPolicy is basePolicy with an all-day window (00:00–23:59), so a rotation
// start is not snapped to a window edge and the exact instant a node crosses its
// eligibility trigger is observable. P is still 24h, so the derivation is unchanged:
// leadTime = K·P + readyTimeout + Buffer + tGP = 48h + 15m + 2m + 1h = 49h17m, and
// A = E − leadTime = 14d − 49h17m = 286h43m.
func alwaysOpenPolicy() *policy.Policy {
	p := basePolicy()
	p.MaintenanceWindows = []policy.MaintenanceWindow{{
		Timezone: "UTC",
		Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
		Start:    "00:00",
		End:      "23:59",
	}}
	return p
}

// wantA is the derived ageThreshold of basePolicy/alwaysOpenPolicy with E = 14d and
// tGP = 1h: 14d − (2·24h + 15m + 2m + 1h).
const wantA = 286*time.Hour + 43*time.Minute

// TestReplacementCreatedAtIsRotationStart is the acceptance criterion that will be
// silently reinterpreted if it is not pinned: on the SURGE path the replacement's
// CreatedAt is the rotation-start instant, NOT node-ready. Karpenter creates the
// replacement NodeClaim as soon as the placeholder Pod goes pending, and
// Env.Provisioning is the time from that to Ready — anchoring on node-ready would push
// every generation's deadline back by Env.Provisioning, compound across generations and
// under-report breaches.
//
// The anchor is observed through the next generation's eligibility trigger, which fires
// at CreatedAt + A: with an always-open window nothing rounds it to a window edge, so
// anchoring on node-ready instead would move the second rotation later by exactly
// Env.Provisioning.
func TestReplacementCreatedAtIsRotationStart(t *testing.T) {
	t.Parallel()

	pol := alwaysOpenPolicy()
	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	env := sim.Env{Provisioning: 9 * time.Minute, Drain: 11 * time.Minute}

	tl, err := sim.Run(pol, f, env, sim.Options{
		Start: mustTime(t, "2026-03-12T22:00:00Z"), // just before the trigger at 22:43
		End:   mustTime(t, "2026-03-24T23:00:00Z"), // past the replacement's own trigger
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tl.Result.A != wantA {
		t.Fatalf("A = %v, want %v", tl.Result.A, wantA)
	}

	starts := kinds(tl.Events, sim.KindRotationStart)
	if len(starts) != 2 {
		t.Fatalf("rotation-start events = %+v, want 2 (the node, then its replacement)", starts)
	}
	// Gen 1 crosses `age > A` at 22:43 exactly, so the first tick that triggers is 22:44.
	first := mustTime(t, "2026-03-12T22:44:00Z")
	if !starts[0].At.Equal(first) {
		t.Fatalf("first rotation-start = %s, want %s", starts[0].At, first)
	}
	// Gen 2's trigger is CreatedAt + A = 22:44 + 286h43m = 2026-03-24T21:27Z → 21:28.
	want := mustTime(t, "2026-03-24T21:28:00Z")
	if !starts[1].At.Equal(want) {
		t.Errorf("second rotation-start = %s, want %s — the replacement's CreatedAt must be the rotation-start instant %s, not node-ready %s (which would delay this to %s)",
			starts[1].At.Format(time.RFC3339), want.Format(time.RFC3339),
			first.Format(time.RFC3339), first.Add(env.Provisioning).Format(time.RFC3339),
			want.Add(env.Provisioning).Format(time.RFC3339))
	}
	if starts[1].Node != "node-a-r1" {
		t.Errorf("second rotation rotates %q, want the replacement node-a-r1", starts[1].Node)
	}
}

// TestSurgelessReplacementCreatedAtIsRotationDone pins the other half: the surge-less
// path stages no placeholder, so Karpenter provisions the replacement in response to the
// evicted Pods and its CreatedAt is the rotation-done instant. (An approximation —
// Karpenter may react during the drain — and conservative in the safe direction: an
// earlier real creation only makes the next deadline earlier.)
func TestSurgelessReplacementCreatedAtIsRotationDone(t *testing.T) {
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

	// Enter the horizon 30m before the node's deadline (2026-03-15T00:00Z). It is long
	// past its trigger, and the remaining 30m is under t_rot = readyTimeout + tGP +
	// Buffer = 1h17m, so a graceful surge cannot finish in time: the pick must take the
	// surge-less path. This is a deadline race, not a failure.
	tl, err := sim.Run(pol, f, env, sim.Options{
		Start: mustTime(t, "2026-03-14T23:30:00Z"),
		End:   mustTime(t, "2026-03-26T23:00:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	starts := kinds(tl.Events, sim.KindRotationStart)
	if len(starts) != 2 {
		t.Fatalf("rotation-start events = %+v, want 2 (the surge-less rotation, then its replacement)", starts)
	}
	if !starts[0].Surgeless {
		t.Fatalf("first rotation-start = %+v, want Surgeless", starts[0])
	}
	// The surge-less rotation stages no surge, so it emits no node-ready. (The second,
	// graceful rotation does — hence the window, not a bare count.)
	for _, e := range kinds(tl.Events, sim.KindNodeReady) {
		if e.At.Before(starts[1].At) {
			t.Errorf("node-ready %+v during the surge-less rotation, want none: it stages no surge", e)
		}
	}
	if b := kinds(tl.Events, sim.KindExpireAfterBreach); len(b) != 0 {
		t.Errorf("expire-after-breach = %+v, want none: the fallback rotated the node in time", b)
	}

	done := kinds(tl.Events, sim.KindRotationDone)
	if len(done) == 0 || !done[0].Surgeless {
		t.Fatalf("rotation-done events = %+v, want the first to be surge-less", done)
	}
	// Drain only, no provisioning: done = start + Env.Drain (11m, under tGP 1h).
	wantDone := starts[0].At.Add(env.Drain)
	if !done[0].At.Equal(wantDone) {
		t.Fatalf("rotation-done = %s, want %s (drain only)", done[0].At, wantDone)
	}
	// The replacement is anchored on rotation-done (23:41), so its trigger — and the
	// second rotation — is at 23:41 + A = 2026-03-26T22:24Z → 22:25. Anchoring on
	// rotation-start (23:30) would fire it 11m earlier.
	want := mustTime(t, "2026-03-26T22:25:00Z")
	if !starts[1].At.Equal(want) {
		t.Errorf("second rotation-start = %s, want %s — the surge-less replacement's CreatedAt must be rotation-done %s, not rotation-start %s",
			starts[1].At.Format(time.RFC3339), want.Format(time.RFC3339),
			wantDone.Format(time.RFC3339), starts[0].At.Format(time.RFC3339))
	}
	if starts[1].Surgeless {
		t.Errorf("second rotation-start = %+v, want graceful: the fresh replacement is nowhere near its deadline", starts[1])
	}
}

// TestEnvDrainClampsToPerClaimTGP: a drain longer than the node's own
// terminationGracePeriod is unreachable — Karpenter force-completes there. The clamp is
// per claim, so a heterogeneous fleet clamps per node.
func TestEnvDrainClampsToPerClaimTGP(t *testing.T) {
	t.Parallel()

	pol := basePolicy()
	tgp := 4 * time.Minute
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	env := sim.Env{Provisioning: 3 * time.Minute, Drain: 30 * time.Minute} // 30m > tGP 4m

	tl, err := sim.Run(pol, f, env, sim.Options{
		Start: mustTime(t, "2026-03-13T02:00:00Z"),
		End:   mustTime(t, "2026-03-13T06:00:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	starts := kinds(tl.Events, sim.KindRotationStart)
	done := kinds(tl.Events, sim.KindRotationDone)
	if len(starts) == 0 || len(done) == 0 {
		t.Fatalf("want at least one rotation, events: %+v", tl.Events)
	}
	// The drain is clamped to tGP: done == start + provisioning + tGP, not + 30m.
	if want := starts[0].At.Add(env.Provisioning + tgp); !done[0].At.Equal(want) {
		t.Errorf("rotation-done = %s, want %s (drain clamped to tGP %v)", done[0].At, want, tgp)
	}
	var found bool
	for _, d := range tl.Diagnostics {
		if d.Code == "EnvDrainAboveTGP" && d.Severity == schedule.Warn {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics = %+v, want an EnvDrainAboveTGP warning", tl.Diagnostics)
	}
}

// TestEnvProvisioningAboveReadyTimeoutIsFatal: the surge would be abandoned. That is the
// failure path, which sim does not model — so it must say so and refuse the timeline,
// never fake a successful rotation.
func TestEnvProvisioningAboveReadyTimeoutIsFatal(t *testing.T) {
	t.Parallel()

	pol := basePolicy() // readyTimeout 15m
	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	env := sim.Env{Provisioning: 20 * time.Minute, Drain: time.Minute}

	tl, err := sim.Run(pol, f, env, sim.Options{
		Start: mustTime(t, "2026-03-13T02:00:00Z"),
		End:   mustTime(t, "2026-03-20T06:00:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: unexpected error %v — an unmodelled path is a Diagnostic, not an error", err)
	}
	if !tl.Partial {
		t.Errorf("Partial = false, want true")
	}
	if len(kinds(tl.Events, sim.KindRotationDone)) != 0 {
		t.Errorf("rotation-done events = %+v, want none: the surge would be abandoned", tl.Events)
	}
	if tl.Result.A == 0 {
		t.Errorf("Result.A = 0: the header strip must still render")
	}
	var found bool
	for _, d := range tl.Diagnostics {
		if d.Code == "EnvProvisioningAboveReadyTimeout" && d.Severity == schedule.Fatal {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics = %+v, want a Fatal EnvProvisioningAboveReadyTimeout", tl.Diagnostics)
	}
}

// TestBlockedByGateIsEdgeTriggered: a long out-of-window stretch is ONE event carrying
// the interval, not one per tick. Without this the payload would dwarf the wasm binary.
func TestBlockedByGateIsEdgeTriggered(t *testing.T) {
	t.Parallel()

	pol := basePolicy() // window 02:00–06:00 daily
	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}

	// A whole out-of-window day: 06:00 → next 02:00 is 20h = 1200 ticks.
	start := mustTime(t, "2026-03-02T06:00:00Z")
	end := mustTime(t, "2026-03-03T01:00:00Z")
	tl, err := sim.Run(pol, f, sim.Env{}, sim.Options{Start: start, End: end})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	blocked := kinds(tl.Events, sim.KindBlockedByGate)
	if len(blocked) != 1 {
		t.Fatalf("blocked-by-gate events = %d, want exactly 1 coalesced event: %+v", len(blocked), blocked)
	}
	got := blocked[0]
	if got.Gate != decide.GateOutOfWindow {
		t.Errorf("gate = %q, want %q", got.Gate, decide.GateOutOfWindow)
	}
	if !got.At.Equal(start) || !got.Until.Equal(end) {
		t.Errorf("interval = [%s, %s], want [%s, %s]", got.At, got.Until, start, end)
	}
}

// TestNoEligibleClaimCarriesCensus: when the gates are open but nothing is old enough,
// the UI needs the reason — the census — and it too is edge-triggered.
func TestNoEligibleClaimCarriesCensus(t *testing.T) {
	t.Parallel()

	pol := basePolicy()
	tgp := time.Hour
	f := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-13T01:00:00Z")}}, // brand new
	}

	tl, err := sim.Run(pol, f, sim.Env{}, sim.Options{
		Start: mustTime(t, "2026-03-13T02:00:00Z"), // in-window, but the node is 1h old
		End:   mustTime(t, "2026-03-13T06:00:00Z"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	census := kinds(tl.Events, sim.KindNoEligibleClaim)
	if len(census) != 1 {
		t.Fatalf("no-eligible-claim events = %d, want exactly 1 coalesced event: %+v", len(census), census)
	}
	c := census[0].Census
	if c == nil {
		t.Fatalf("census is nil")
	}
	if c.Total != 1 || c.NotTriggered != 1 || c.Eligible != 0 {
		t.Errorf("census = %+v, want Total=1 NotTriggered=1 Eligible=0", *c)
	}
	if len(kinds(tl.Events, sim.KindRotationStart)) != 0 {
		t.Errorf("a rotation started for a node below the trigger")
	}
}

// TestRunRejectsUnrunnableInput: error is reserved for input that cannot run at all.
func TestRunRejectsUnrunnableInput(t *testing.T) {
	t.Parallel()

	tgp := time.Hour
	base := sim.Fleet{
		ExpireAfter: 14 * 24 * time.Hour,
		TGP:         &tgp,
		Nodes:       []sim.Node{{Name: "node-a", CreatedAt: mustTime(t, "2026-03-01T00:00:00Z")}},
	}
	okOpts := sim.Options{Start: mustTime(t, "2026-03-13T02:00:00Z"), End: mustTime(t, "2026-03-13T06:00:00Z")}

	tests := map[string]struct {
		fleet sim.Fleet
		env   sim.Env
		opts  sim.Options
	}{
		"inverted horizon":    {base, sim.Env{}, sim.Options{Start: okOpts.End, End: okOpts.Start}},
		"empty horizon":       {base, sim.Env{}, sim.Options{Start: okOpts.Start, End: okOpts.Start}},
		"negative env drain":  {base, sim.Env{Drain: -time.Minute}, okOpts},
		"non-positive expiry": {sim.Fleet{ExpireAfter: 0, TGP: &tgp, Nodes: base.Nodes}, sim.Env{}, okOpts},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := sim.Run(basePolicy(), tc.fleet, tc.env, tc.opts); err == nil {
				t.Errorf("Run: want error")
			}
		})
	}
}
