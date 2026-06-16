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
func baseAuto() Inputs {
	return Inputs{
		E:              14 * 24 * time.Hour, // 336h
		TGP:            time.Hour,
		P:              4 * 24 * time.Hour, // 96h
		WindowLen:      4 * time.Hour,
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
	if r.C != 2 {
		t.Errorf("C = %d, want 2", r.C)
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

func TestDeriveThroughputZeroWarn(t *testing.T) {
	in := baseAuto()
	in.TGP = 24 * time.Hour // Auto Mode stock default → tRot ≈ 24.5h
	r := Derive(in)
	has(t, r, "ThroughputZero", Warn)
	absent(t, r, "ANonPositive") // A = 336 - (192 + 24.5) = 119.5h > 0
	if r.C != 0 {
		t.Errorf("C = %d, want 0", r.C)
	}
}

func TestDeriveRetryBackoffShortWarn(t *testing.T) {
	in := baseAuto()
	in.RetryBackoff = 10 * time.Minute // < readyTimeout 15m
	has(t, Derive(in), "RetryBackoffShort", Warn)
}

func TestDeriveThroughputBelowArrival(t *testing.T) {
	in := baseAuto() // C=2, A=142.5h, P=96h
	in.NodeCount = 3 // N*P/A = 288/142.5 ≈ 2.02 > C(2)
	has(t, Derive(in), "ThroughputBelowArrival", Warn)

	in.NodeCount = 2 // 192/142.5 ≈ 1.35 < C(2)
	absent(t, Derive(in), "ThroughputBelowArrival")
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

// TestDeriveMaxUnavailableScalesC pins the spec §3.2 C = m·floor(...) factor:
// m = 2 doubles the per-occurrence throughput. (v1 fixes m at 1; this guards
// the formula for the v2 surge-parallelism expansion point.)
func TestDeriveMaxUnavailableScalesC(t *testing.T) {
	in := baseAuto() // C = floor(4h / 100m) = 2 at m=1
	in.MaxUnavailable = 2
	if r := Derive(in); r.C != 4 {
		t.Errorf("C = %d, want 4 (m=2 · floor(4h/100m))", r.C)
	}

	in.MaxUnavailable = 0 // unset is treated as 1
	if r := Derive(in); r.C != 2 {
		t.Errorf("C = %d, want 2 (m=0 treated as 1)", r.C)
	}
}

func TestDeriveNoWindowsFatal(t *testing.T) {
	in := baseAuto()
	in.P = 0
	has(t, Derive(in), "NoWindows", Fatal)
}
