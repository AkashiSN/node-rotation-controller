package schedule

import (
	"testing"
	"time"
)

// codes collects the findings into a code→severity map for order-independent
// assertions.
func codes(r Result) map[string]Severity {
	m := make(map[string]Severity, len(r.Findings))
	for _, f := range r.Findings {
		m[f.Code] = f.Severity
	}
	return m
}

func has(t *testing.T, r Result, code string, want Severity) {
	t.Helper()
	c := codes(r)
	sev, ok := c[code]
	if !ok {
		t.Errorf("missing finding %q; got %v", code, c)
		return
	}
	if sev != want {
		t.Errorf("finding %q severity = %v, want %v", code, sev, want)
	}
}

func absent(t *testing.T, r Result, code string) {
	t.Helper()
	if _, ok := codes(r)[code]; ok {
		t.Errorf("unexpected finding %q present; got %v", code, codes(r))
	}
}

// baseAuto is the §3.2 worked example: a healthy auto-derived configuration.
// The window is {Wed,Sat} 02:00–06:00, so occurrence starts are 3d/4d apart and
// the shortest interval the window stays closed is 3d − 4h = 68h.
func baseAuto() Inputs {
	return Inputs{
		E:              14 * 24 * time.Hour, // 336h
		TGP:            time.Hour,
		P:              4 * 24 * time.Hour, // 96h
		WindowLen:      4 * time.Hour,
		IdleGap:        new(68 * time.Hour),
		ReadyTimeout:   15 * time.Minute,
		Cooldown:       10 * time.Minute,
		RetryBackoff:   30 * time.Minute,
		K:              2,
		MaxUnavailable: 1,
	}
}

func TestDeriveWorkedExample(t *testing.T) {
	r := Derive(baseAuto())

	if want := 90 * time.Minute; r.TRot != want {
		t.Errorf("TRot = %v, want %v", r.TRot, want)
	}
	if want := 142*time.Hour + 30*time.Minute; r.A != want { // 336h - (192h + 1.5h)
		t.Errorf("A = %v, want %v", r.A, want)
	}
	if r.G != 2 {
		t.Errorf("G = %d, want 2", r.G)
	}
	if r.C != 3 { // ceil(4h / 100m) = 3 — the near-edge start counts (§3.1)
		t.Errorf("C = %d, want 3", r.C)
	}
	if len(r.Findings) != 0 {
		t.Errorf("expected no findings, got %v", codes(r))
	}
}

func TestDeriveWeeklyOnlyFatal(t *testing.T) {
	in := baseAuto()
	in.P = 7 * 24 * time.Hour // 168h → A = 336 - (336 + 1.5) = -1.5h
	r := Derive(in)
	has(t, r, "ANonPositive", Fatal)
}

// TestDeriveACentralRaceZeroFatal pins the off-by-one boundary of the central
// race: A == 0 exactly is a Fatal ANonPositive, not a pass. The auto branch's
// guard is `A <= 0`, so the boundary value must trip it (between the A < 0 case
// in TestDeriveWeeklyOnlyFatal and the A > 0 healthy worked example). Here E is
// tuned so A = E − (K·P + t_rot) = 0 exactly: K·P = 2·96h = 192h, t_rot = 90m,
// so E = 193.5h gives A = 0.
func TestDeriveACentralRaceZeroFatal(t *testing.T) {
	in := baseAuto()
	in.E = 193*time.Hour + 30*time.Minute // A = 193.5h − (192h + 1.5h) = 0
	r := Derive(in)
	if r.A != 0 {
		t.Fatalf("A = %v, want exactly 0 (boundary)", r.A)
	}
	has(t, r, "ANonPositive", Fatal)
	absent(t, r, "AVeryAggressive") // the A <= 0 case must not also warn-aggressive
}

func TestDeriveKFloor(t *testing.T) {
	in := baseAuto()
	in.K = 0
	has(t, Derive(in), "KBelowOne", Fatal)

	in.K = 1
	r := Derive(in)
	has(t, r, "KBelowTwo", Warn)
	absent(t, r, "ANonPositive")
}

func TestDeriveAggressiveWarn(t *testing.T) {
	in := baseAuto()
	in.E = 240 * time.Hour // A = 240 - 193.5 = 46.5h, with P=96h → 0 < A < P
	r := Derive(in)
	has(t, r, "AVeryAggressive", Warn)
	absent(t, r, "ANonPositive")
}

// TestDeriveOverrideAggressiveWarn covers the override branch of the
// AVeryAggressive warning (only the auto branch is exercised by
// TestDeriveAggressiveWarn). An explicit override A with 0 < A < P warns that
// nodes rotate very young — and must still guarantee >= 1 chance so no fatal
// fires. Override = 50h with P = 96h ⇒ 0 < A < P; G = floor((334.5−50)/96) = 2.
func TestDeriveOverrideAggressiveWarn(t *testing.T) {
	in := baseAuto()
	in.Override = new(50 * time.Hour) // 0 < 50h < P(96h)
	r := Derive(in)
	if r.A != 50*time.Hour {
		t.Fatalf("A = %v, want override 50h", r.A)
	}
	has(t, r, "AVeryAggressive", Warn)
	absent(t, r, "OverrideNonPositive")
	absent(t, r, "OverrideGBelowOne")
	absent(t, r, "OverrideGBelowK") // G = 2 == K
}

func TestDeriveOverrideGBelowOneFatal(t *testing.T) {
	in := baseAuto()
	in.Override = new(300 * time.Hour) // G = floor((334.5 - 300)/96) = 0
	r := Derive(in)
	has(t, r, "OverrideGBelowOne", Fatal)
	if r.G != 0 {
		t.Errorf("G = %d, want 0", r.G)
	}
	if r.A != 300*time.Hour {
		t.Errorf("A = %v, want override 300h", r.A)
	}
}

// TestDeriveOverrideGFloorNegative locks the floor (not truncate-toward-zero)
// semantics: with A > E − t_rot the numerator is negative, so floor((E−tRot−A)/P)
// must be -1, not 0.
func TestDeriveOverrideGFloorNegative(t *testing.T) {
	in := baseAuto()
	in.Override = new(400 * time.Hour) // E−tRot = 334.5h; (334.5−400)/96 = -0.68 → floor -1
	r := Derive(in)
	if r.G != -1 {
		t.Errorf("G = %d, want -1 (floor of negative numerator)", r.G)
	}
	has(t, r, "OverrideGBelowOne", Fatal)
}

func TestDeriveOverrideGBelowKWarn(t *testing.T) {
	in := baseAuto()
	in.Override = new(200 * time.Hour) // G = floor((334.5 - 200)/96) = 1, K=2
	r := Derive(in)
	has(t, r, "OverrideGBelowK", Warn)
	absent(t, r, "OverrideGBelowOne")
	if r.G != 1 {
		t.Errorf("G = %d, want 1", r.G)
	}
}

func TestDeriveTGPUnsetWarn(t *testing.T) {
	in := baseAuto()
	in.TGP = DrainFallback // caller substitutes the fallback…
	in.TGPWasUnset = true  // …and flags it
	r := Derive(in)
	has(t, r, "TGPUnset", Warn)
	if want := 90 * time.Minute; r.TRot != want { // 15m + 1h + 15m
		t.Errorf("TRot = %v, want %v", r.TRot, want)
	}
}

func TestDeriveHardCapExceededWarn(t *testing.T) {
	in := baseAuto()
	in.E = 20 * 24 * time.Hour  // 20d
	in.TGP = 2 * 24 * time.Hour // 2d → E + tGP = 22d > 21d
	r := Derive(in)
	has(t, r, "HardCapExceeded", Warn)
	// Non-fatal: the cap warning must not change the derived A. tRot = 15m + 48h +
	// 15m = 48.5h; A = 480h − (2·96h + 48.5h) = 239.5h.
	if want := 239*time.Hour + 30*time.Minute; r.A != want {
		t.Errorf("A = %v, want %v (cap warning must not change A)", r.A, want)
	}
}

func TestDeriveHardCapAtBoundaryNoWarn(t *testing.T) {
	// E + tGP == 21d exactly is at the cap, not over it (the cap is a strict >).
	in := baseAuto()
	in.E = 20 * 24 * time.Hour // 20d
	in.TGP = 24 * time.Hour    // 1d → E + tGP = 21d exactly
	absent(t, Derive(in), "HardCapExceeded")
}

func TestDeriveHardCapUnderCapNoWarn(t *testing.T) {
	// The worked example (E = 14d, tGP = 1h) is well under the cap.
	absent(t, Derive(baseAuto()), "HardCapExceeded")
}

// TestDeriveCeilCountsTheNearEdgeStart pins D1 (issue #211): the window gates
// only rotation *starts* (§3.1), so the legal starts in an occurrence of length D
// are k·denom < D, and their count is ceil(D/denom) — the final near-edge start
// completes past the window's close and must be counted. floor and ceil agree
// only when D is an exact multiple of denom.
func TestDeriveCeilCountsTheNearEdgeStart(t *testing.T) {
	in := baseAuto() // denom = tRot 90m + cooldown 10m = 100m

	in.WindowLen = 200 * time.Minute // exact multiple: starts at 0m, 100m
	if r := Derive(in); r.C != 2 {
		t.Errorf("C = %d, want 2 (D = 2·denom exactly)", r.C)
	}

	in.WindowLen = 201 * time.Minute // one more legal start at 200m
	if r := Derive(in); r.C != 3 {
		t.Errorf("C = %d, want 3 (ceil must count the near-edge start)", r.C)
	}

	in.WindowLen = time.Minute // a start at 0m is always legal in a non-empty window
	if r := Derive(in); r.C != 1 {
		t.Errorf("C = %d, want 1 (every non-empty occurrence admits one start)", r.C)
	}
}

// TestDeriveAutoModeDefaultHasNoThroughputZero pins D1+D2 (issue #211): on the
// stock Auto Mode tGP = 24h, t_rot ≈ 24.5h dwarfs any realistic window, but a
// positive-length window still admits one rotation start. The old ThroughputZero
// finding asserted otherwise and is gone; C = 1, not 0.
func TestDeriveAutoModeDefaultHasNoThroughputZero(t *testing.T) {
	in := baseAuto()
	in.TGP = 24 * time.Hour // Auto Mode stock default → tRot ≈ 24.5h
	r := Derive(in)
	absent(t, r, "ThroughputZero")
	absent(t, r, "ANonPositive") // A = 336 - (192 + 24.5) = 119.5h > 0
	if r.C != 1 {
		t.Errorf("C = %d, want 1 (ceil(4h / 24h40m))", r.C)
	}
}

// TestDeriveDegenerateWindowLenSkipsLayer2 locks the defensive guard D2 keeps: a
// non-positive D is the one input that can still drive C below 1, and no layer-2
// finding is meaningful there. Layer 1 owns that case (NoWindows) whenever the
// schedule genuinely has no occurrences.
func TestDeriveDegenerateWindowLenSkipsLayer2(t *testing.T) {
	in := baseAuto()
	in.WindowLen = 0
	in.NodeCount = 100 // would trip both throughput checks at any positive C
	r := Derive(in)
	if r.C != 0 {
		t.Errorf("C = %d, want 0 for a zero-length window", r.C)
	}
	absent(t, r, "ThroughputBelowArrival")
	absent(t, r, "ThroughputBurstShortfall")
	absent(t, r, "RotationSpansNextWindow")
}

func TestDeriveRetryBackoffShortWarn(t *testing.T) {
	in := baseAuto()
	in.RetryBackoff = 10 * time.Minute // < readyTimeout 15m
	has(t, Derive(in), "RetryBackoffShort", Warn)
}

func TestDeriveThroughputBelowArrival(t *testing.T) {
	in := baseAuto() // C=3, A=142.5h, P=96h
	in.NodeCount = 5 // C·A = 427.5h < N·P = 480h
	has(t, Derive(in), "ThroughputBelowArrival", Warn)

	in.NodeCount = 4 // N·P = 384h < C·A = 427.5h
	absent(t, Derive(in), "ThroughputBelowArrival")
}

func TestDeriveThroughputBurstShortfall(t *testing.T) {
	in := baseAuto() // C=3, K=2 → K·C=6
	in.NodeCount = 7 // a synchronized batch of 7 exceeds K·C=6
	has(t, Derive(in), "ThroughputBurstShortfall", Warn)

	in.NodeCount = 6 // 6 == K·C: fits within the K guaranteed windows
	absent(t, Derive(in), "ThroughputBurstShortfall")
}

// TestDeriveShortWindowStillEvaluatesThroughput is the issue #211 reproduction:
// E = 20d, tGP = 1d, {Sat,Sun} 02:00–03:30 Asia/Tokyo (P = 144h, D = 90m, idle
// gap 22h30m). Layer 1 is clean. Before the fix, C = 0 emitted a lone
// ThroughputZero and returned early, hiding both throughput checks — which is
// every realistic maintenance window on stock Auto Mode. They must now evaluate.
func TestDeriveShortWindowStillEvaluatesThroughput(t *testing.T) {
	in := baseAuto()
	in.E = 20 * 24 * time.Hour      // 480h
	in.TGP = 24 * time.Hour         // tRot = 24h30m
	in.P = 6 * 24 * time.Hour       // 144h (Sun → next Sat)
	in.WindowLen = 90 * time.Minute // D
	in.IdleGap = new(22*time.Hour + 30*time.Minute)
	in.NodeCount = 3

	r := Derive(in)
	if want := 24*time.Hour + 30*time.Minute; r.TRot != want {
		t.Fatalf("TRot = %v, want %v", r.TRot, want)
	}
	if want := 167*time.Hour + 30*time.Minute; r.A != want { // 480h − (288h + 24.5h)
		t.Fatalf("A = %v, want %v", r.A, want)
	}
	if r.C != 1 {
		t.Fatalf("C = %d, want 1", r.C)
	}
	// Layer 1 stays clean — the findings below are layer 2's alone.
	absent(t, r, "ANonPositive")
	absent(t, r, "AVeryAggressive") // A 167.5h > P 144h
	absent(t, r, "HardCapExceeded") // E + tGP = 21d exactly, cap is a strict >

	absent(t, r, "ThroughputZero")
	has(t, r, "ThroughputBelowArrival", Warn)   // C·A = 167.5h < N·P = 432h
	has(t, r, "ThroughputBurstShortfall", Warn) // N=3 > K·C = 2
	has(t, r, "RotationSpansNextWindow", Warn)  // tRot 24h30m > idle gap 22h30m
}

// TestDeriveRotationSpansNextWindow pins D3 (issue #211). The check is a strict
// `t_rot > idleGap`: a rotation that finishes exactly as the next occurrence
// opens has released the serial gate and does not consume it.
func TestDeriveRotationSpansNextWindow(t *testing.T) {
	in := baseAuto() // tRot = 90m

	in.IdleGap = new(89 * time.Minute)
	has(t, Derive(in), "RotationSpansNextWindow", Warn)

	in.IdleGap = new(90 * time.Minute) // exactly t_rot: the gate is free again
	absent(t, Derive(in), "RotationSpansNextWindow")
}

// TestDeriveRotationSpansNextWindowSkippedWhenGapUndefined: a continuously-open
// union never closes (window.ShortestIdleGap reports ok=false), so there is no
// "next occurrence" to carry into and the check must not fire — not even with a
// t_rot that dwarfs the whole week.
func TestDeriveRotationSpansNextWindowSkippedWhenGapUndefined(t *testing.T) {
	in := baseAuto()
	in.IdleGap = nil
	in.TGP = 30 * 24 * time.Hour
	absent(t, Derive(in), "RotationSpansNextWindow")
}

// TestDeriveRotationSpansNextWindowDoesNotDiscountBurst: D3 and
// ThroughputBurstShortfall stay orthogonal — the carry-over warn is emitted
// alongside K·C rather than discounting it (the operator composes the two).
// N = 6 == K·C, so the burst check stays quiet even while carry-over fires.
func TestDeriveRotationSpansNextWindowDoesNotDiscountBurst(t *testing.T) {
	in := baseAuto() // C=3, K=2 → K·C=6
	in.IdleGap = new(time.Minute)
	in.NodeCount = 6
	r := Derive(in)
	has(t, r, "RotationSpansNextWindow", Warn)
	absent(t, r, "ThroughputBurstShortfall")
}

// TestDeriveOverrideNonPositive locks the defensive guard: Derive is pure and
// must surface a non-positive override (which policy normally rejects upstream)
// rather than silently echo it back.
func TestDeriveOverrideNonPositive(t *testing.T) {
	in := baseAuto()
	in.Override = new(-1 * time.Hour)
	r := Derive(in)
	has(t, r, "OverrideNonPositive", Fatal)
	absent(t, r, "OverrideGBelowOne") // A <= 0 short-circuits the G checks
}

// TestDeriveMaxUnavailableScalesC pins the spec §3.2 C = m·ceil(...) factor:
// m = 2 doubles the per-occurrence throughput. (v1 fixes m at 1; this guards
// the formula for the v2 surge-parallelism expansion point.)
func TestDeriveMaxUnavailableScalesC(t *testing.T) {
	in := baseAuto() // C = ceil(4h / 100m) = 3 at m=1
	in.MaxUnavailable = 2
	if r := Derive(in); r.C != 6 {
		t.Errorf("C = %d, want 6 (m=2 · ceil(4h/100m))", r.C)
	}

	in.MaxUnavailable = 0 // unset is treated as 1
	if r := Derive(in); r.C != 3 {
		t.Errorf("C = %d, want 3 (m=0 treated as 1)", r.C)
	}
}

func TestDeriveNoWindowsFatal(t *testing.T) {
	in := baseAuto()
	in.P = 0
	has(t, Derive(in), "NoWindows", Fatal)
}
