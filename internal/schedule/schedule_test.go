package schedule

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
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
	if want := 40 * time.Minute; r.TRotEst != want { // 15m + drainEstimate 10m + 15m
		t.Errorf("TRotEst = %v, want %v", r.TRotEst, want)
	}
	if want := 10 * time.Minute; r.DrainEstimate != want { // min(tGP 1h, default 10m)
		t.Errorf("DrainEstimate = %v, want %v", r.DrainEstimate, want)
	}
	if r.C != 5 { // ceil(4h / 50m) = 5 — the near-edge start counts (§3.1)
		t.Errorf("C = %d, want 5", r.C)
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
	in := pinnedEstimate() // denom = t_rot_est 90m + cooldown 10m = 100m (estimate pinned to tGP)

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

// TestDeriveAutoModeDefaultHasNoThroughputZero pins D1+D2 (issue #211) and the
// #212 split: on the stock Auto Mode tGP = 24h the DEADLINE bound t_rot ≈ 24.5h
// dwarfs any realistic window, but layer 2 forecasts with t_rot_est = 40m (the
// default drainEstimate), so C = 5. The old ThroughputZero finding asserted a
// positive-length window rotates nothing and is gone.
func TestDeriveAutoModeDefaultHasNoThroughputZero(t *testing.T) {
	in := baseAuto()
	in.TGP = 24 * time.Hour // Auto Mode stock default → tRot ≈ 24.5h, tRotEst = 40m
	r := Derive(in)
	absent(t, r, "ThroughputZero")
	absent(t, r, "ANonPositive") // A = 336 - (192 + 24.5) = 119.5h > 0
	if r.C != 5 {
		t.Errorf("C = %d, want 5 (ceil(4h / 50m))", r.C)
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
	in := pinnedEstimate() // C=3, A=142.5h, P=96h
	in.NodeCount = 5       // C·A = 427.5h < N·P = 480h
	has(t, Derive(in), "ThroughputBelowArrival", Warn)

	in.NodeCount = 4 // N·P = 384h < C·A = 427.5h
	absent(t, Derive(in), "ThroughputBelowArrival")
}

func TestDeriveThroughputBurstShortfall(t *testing.T) {
	in := pinnedEstimate() // C=3, K=2 → K·C=6
	in.NodeCount = 7       // a synchronized batch of 7 exceeds K·C=6
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
	in.TGP = 24 * time.Hour         // tRot = 24h30m, tRotEst = 40m
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
	if r.C != 2 { // ceil(90m / 50m); the deadline bound would say 1
		t.Fatalf("C = %d, want 2", r.C)
	}
	// Layer 1 stays clean — the findings below are layer 2's alone.
	absent(t, r, "ANonPositive")
	absent(t, r, "AVeryAggressive") // A 167.5h > P 144h
	absent(t, r, "HardCapExceeded") // E + tGP = 21d exactly, cap is a strict >

	absent(t, r, "ThroughputZero")
	has(t, r, "ThroughputBelowArrival", Warn) // C·A = 335h < N·P = 432h
	absent(t, r, "ThroughputBurstShortfall")  // N=3 <= K·C = 4
	absent(t, r, "RotationSpansNextWindow")   // 50m < idle gap 22h30m
}

// TestDeriveShortWindowDeadlineBoundView pins what the pre-#212 model reported for
// the same schedule: feeding the force-kill deadline back in as the estimate
// reproduces C = 1 and all three layer-2 warnings. It is the before/after contrast
// that makes the split visible, and it guards against t_rot leaking back into
// layer 2.
func TestDeriveShortWindowDeadlineBoundView(t *testing.T) {
	in := baseAuto()
	in.E = 20 * 24 * time.Hour
	in.TGP = 24 * time.Hour
	in.DrainEstimate = new(24 * time.Hour) // estimate == deadline: the old model
	in.P = 6 * 24 * time.Hour
	in.WindowLen = 90 * time.Minute
	in.IdleGap = new(22*time.Hour + 30*time.Minute)
	in.NodeCount = 3

	r := Derive(in)
	if r.TRotEst != r.TRot {
		t.Fatalf("TRotEst = %v, want it to equal TRot %v", r.TRotEst, r.TRot)
	}
	if r.C != 1 {
		t.Fatalf("C = %d, want 1", r.C)
	}
	has(t, r, "ThroughputBelowArrival", Warn)
	has(t, r, "ThroughputBurstShortfall", Warn) // N=3 > K·C = 2
	has(t, r, "RotationSpansNextWindow", Warn)  // 24h40m > idle gap 22h30m
}

// TestDeriveRotationSpansNextWindow pins D3 (issue #211). The gate a rotation
// holds is t_rot + cooldownAfter — the same denom that spaces starts inside an
// occurrence — and the check is a strict `denom > idleGap`: a gate that frees
// exactly as the next occurrence opens does not consume it.
func TestDeriveRotationSpansNextWindow(t *testing.T) {
	in := pinnedEstimate() // denom = tRot 90m + cooldown 10m = 100m

	in.IdleGap = new(99 * time.Minute)
	has(t, Derive(in), "RotationSpansNextWindow", Warn)

	in.IdleGap = new(100 * time.Minute) // exactly denom: the gate is free again
	absent(t, Derive(in), "RotationSpansNextWindow")
}

// TestDeriveRotationSpansNextWindowCountsCooldown guards the false negative a
// t_rot-only predicate leaves: cooldownAfter is stamped at *completion* and keeps
// the per-NodePool start gate shut afterwards, so a rotation whose drain finishes
// before the next occurrence opens can still block that occurrence's first start.
// Here t_rot = 90m ends 10m before the window reopens, but the 30m cooldown runs
// 20m into it — no start is legal there, yet t_rot alone would stay silent.
func TestDeriveRotationSpansNextWindowCountsCooldown(t *testing.T) {
	in := pinnedEstimate()
	in.Cooldown = 30 * time.Minute
	in.RetryBackoff = 30 * time.Minute // keep RetryBackoffShort out of the picture
	in.IdleGap = new(100 * time.Minute)

	r := Derive(in)
	if r.TRotEst > *in.IdleGap {
		t.Fatalf("precondition: t_rot_est %v must not exceed the idle gap %v on its own", r.TRotEst, *in.IdleGap)
	}
	has(t, r, "RotationSpansNextWindow", Warn)
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
	in := pinnedEstimate() // C=3, K=2 → K·C=6
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
	in := pinnedEstimate() // C = ceil(4h / 100m) = 3 at m=1
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

// pinnedEstimate is baseAuto with drainEstimate set explicitly equal to tGP, so
// t_rot_est == t_rot and denom stays 100m. Tests about the layer-2 *checks*
// rather than about the estimate itself use it, so a future change to
// DrainEstimateDefault cannot silently move their thresholds.
func pinnedEstimate() Inputs {
	in := baseAuto()
	in.DrainEstimate = new(time.Hour) // == in.TGP
	return in
}

// layer1Codes is the closed set of layer-1 finding codes (spec §3.2). Layer 1 vs
// layer 2 is classified by CODE, not severity: every layer-2 finding is Warn, and
// so are several layer-1 ones. NoWindows is included: it is emitted directly by
// Derive (not via the layer1 helper) before the P<=0 short-circuit, but it is a
// scheduling-feasibility Fatal like the rest of layer 1.
var layer1Codes = map[string]bool{
	"KBelowOne": true, "KBelowTwo": true,
	"ANonPositive": true, "AVeryAggressive": true,
	"OverrideNonPositive": true, "OverrideGBelowOne": true, "OverrideGBelowK": true,
	"TGPUnset": true, "HardCapExceeded": true, "RetryBackoffShort": true,
	"NoWindows": true,
}

// forecastCodes is the closed set of forecast-side codes: the layer-2 throughput
// warnings plus the layer-2-adjacent input-validity warning DrainEstimateAboveTGP
// emitted from Derive itself. Together with layer1Codes it must cover every code
// the package emits (see TestFindingCodesAreClassified).
var forecastCodes = map[string]bool{
	"ThroughputBelowArrival": true, "ThroughputBurstShortfall": true, "RotationSpansNextWindow": true,
	"DrainEstimateAboveTGP": true,
}

func layer1Set(r Result) map[string]Severity {
	m := map[string]Severity{}
	for _, f := range r.Findings {
		if layer1Codes[f.Code] {
			m[f.Code] = f.Severity
		}
	}
	return m
}

// TestFindingCodesAreClassified guards layer1Codes/forecastCodes against going
// stale: it parses schedule.go's own source for every Finding{Code: "..."}
// literal and requires each one to be registered in layer1Codes or
// forecastCodes. Without this, a new layer-1 finding added without a matching
// entry would make TestDeriveDrainEstimateContainment's layer-1 invariance
// check silently ignore it — the containment assertion would pass vacuously
// even if drainEstimate started leaking into that finding.
//
// If this test fails: add the reported code to layer1Codes (if it can gate a
// NodePool out of rotating) or forecastCodes (if it is throughput/forecast-only
// advisory), matching schedule.go's own layer-1/layer-2 split.
func TestFindingCodesAreClassified(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "schedule.go", nil, 0)
	if err != nil {
		t.Fatalf("parse schedule.go: %v", err)
	}

	seen := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if ident, ok := kv.Key.(*ast.Ident); !ok || ident.Name != "Code" {
			return true
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		code, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("unquote Code literal %s: %v", lit.Value, err)
		}
		seen[code] = true
		return true
	})
	if len(seen) == 0 {
		t.Fatal("found no Finding Code literals in schedule.go; did the field name or parse target change?")
	}

	for code := range seen {
		if !layer1Codes[code] && !forecastCodes[code] {
			t.Errorf("finding code %q is emitted by schedule.go but not classified in layer1Codes or forecastCodes (schedule_test.go); add it to whichever set matches its containment side", code)
		}
	}
}

func TestResolveDrainEstimate(t *testing.T) {
	tests := []struct {
		name        string
		tgp         time.Duration
		tgpWasUnset bool
		cfg         *time.Duration
		want        time.Duration
		wantWarn    bool
	}{
		{"unset falls back to the default", 24 * time.Hour, false, nil, 10 * time.Minute, false},
		{"unset clamps to a tighter tGP", 5 * time.Minute, false, nil, 5 * time.Minute, false},
		{"explicit below tGP is kept", 24 * time.Hour, false, new(2 * time.Hour), 2 * time.Hour, false},
		{"explicit equal to tGP is kept", time.Hour, false, new(time.Hour), time.Hour, false},
		{"explicit above tGP warns and clamps", time.Hour, false, new(25 * time.Hour), time.Hour, true},
		{"unset tGP falls back to the default", DrainFallback, true, nil, 10 * time.Minute, false},
		{"unset tGP does not clamp an explicit estimate", DrainFallback, true, new(25 * time.Hour), 25 * time.Hour, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, fs := resolveDrainEstimate(tc.tgp, tc.tgpWasUnset, tc.cfg)
			if got != tc.want {
				t.Errorf("estimate = %v, want %v", got, tc.want)
			}
			warned := false
			for _, f := range fs {
				if f.Code == "DrainEstimateAboveTGP" {
					warned = true
					if f.Severity != Warn {
						t.Errorf("DrainEstimateAboveTGP severity = %v, want Warn", f.Severity)
					}
				}
			}
			if warned != tc.wantWarn {
				t.Errorf("DrainEstimateAboveTGP present = %v, want %v", warned, tc.wantWarn)
			}
		})
	}
}

// TestDeriveDrainEstimateContainment is the safety argument, pinned as a test
// (issue #212, ADR-0002): drainEstimate reaches the forecast side and nothing else.
// Holding every other input fixed, TRot / A / G and the ENTIRE layer-1 finding set
// must be invariant under any drainEstimate — so a wrong estimate can never make a
// node race its own Forceful Expiration. Only C, TRotEst, DrainEstimate and the
// non-layer-1 (forecast-side) findings may move: the layer-2 warnings, plus
// DrainEstimateAboveTGP, which is an input-validity warning about the estimate
// rather than a term in the capacity model.
func TestDeriveDrainEstimateContainment(t *testing.T) {
	base := baseAuto()
	base.TGP = 24 * time.Hour // stock Auto Mode; leaves room above and below
	base.NodeCount = 3
	// Force one layer-1 finding so the "layer-1 set is invariant" assertion below
	// compares a NON-EMPTY set. Without this the invariance holds vacuously and the
	// test would still pass if layer 1 stopped emitting findings entirely.
	base.RetryBackoff = 10 * time.Minute // < readyTimeout 15m → RetryBackoffShort

	ref := Derive(base) // nil estimate
	refLayer1 := layer1Set(ref)
	if len(refLayer1) == 0 {
		t.Fatal("precondition: the layer-1 reference set must be non-empty, else invariance is vacuous")
	}

	// C = ceil(WindowLen 240m / (readyTimeout 15m + est + buffer 15m + cooldown 10m))
	cases := []struct {
		name    string
		cfg     *time.Duration
		wantEst time.Duration
		wantC   int
		wantAbv bool // DrainEstimateAboveTGP expected
	}{
		{"unset", nil, 10 * time.Minute, 5, false},                          // denom 50m
		{"1m", new(time.Minute), time.Minute, 6, false},                     // denom 41m
		{"10m", new(10 * time.Minute), 10 * time.Minute, 5, false},          // denom 50m
		{"1h", new(time.Hour), time.Hour, 3, false},                         // denom 100m
		{"25h clamps to tGP", new(25 * time.Hour), 24 * time.Hour, 1, true}, // denom 24h40m
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.DrainEstimate = tc.cfg
			r := Derive(in)

			// Invariant: the deadline side.
			if r.TRot != ref.TRot {
				t.Errorf("TRot = %v, want %v (must not depend on drainEstimate)", r.TRot, ref.TRot)
			}
			if r.A != ref.A {
				t.Errorf("A = %v, want %v (must not depend on drainEstimate)", r.A, ref.A)
			}
			if r.G != ref.G {
				t.Errorf("G = %d, want %d (must not depend on drainEstimate)", r.G, ref.G)
			}
			if got := layer1Set(r); !reflect.DeepEqual(got, refLayer1) {
				t.Errorf("layer-1 findings = %v, want %v (must not depend on drainEstimate)", got, refLayer1)
			}

			// Variant: the forecast side.
			if r.DrainEstimate != tc.wantEst {
				t.Errorf("DrainEstimate = %v, want %v", r.DrainEstimate, tc.wantEst)
			}
			if want := in.ReadyTimeout + tc.wantEst + Buffer; r.TRotEst != want {
				t.Errorf("TRotEst = %v, want %v", r.TRotEst, want)
			}
			if r.C != tc.wantC {
				t.Errorf("C = %d, want %d", r.C, tc.wantC)
			}
			if tc.wantAbv {
				has(t, r, "DrainEstimateAboveTGP", Warn)
			} else {
				absent(t, r, "DrainEstimateAboveTGP")
			}
		})
	}
}

// TestDeriveDrainEstimateContainmentTGPUnset covers the branch the spec forks on
// and the one most likely to break: with tGP unset the DrainFallback substitution
// applies to the leadTime side only, so an explicit estimate is NOT clamped and no
// DrainEstimateAboveTGP is emitted (issue #212 validation table, row 3).
func TestDeriveDrainEstimateContainmentTGPUnset(t *testing.T) {
	base := baseAuto()
	base.TGP = DrainFallback
	base.TGPWasUnset = true

	ref := Derive(base)
	refLayer1 := layer1Set(ref)
	if ref.DrainEstimate != 10*time.Minute {
		t.Fatalf("unset estimate = %v, want 10m (min(DrainFallback, default))", ref.DrainEstimate)
	}

	in := base
	in.DrainEstimate = new(25 * time.Hour)
	r := Derive(in)

	if r.TRot != ref.TRot || r.A != ref.A || r.G != ref.G {
		t.Errorf("deadline side moved: TRot=%v A=%v G=%d, want %v/%v/%d", r.TRot, r.A, r.G, ref.TRot, ref.A, ref.G)
	}
	if got := layer1Set(r); !reflect.DeepEqual(got, refLayer1) {
		t.Errorf("layer-1 findings = %v, want %v", got, refLayer1)
	}
	if r.DrainEstimate != 25*time.Hour {
		t.Errorf("DrainEstimate = %v, want 25h (no clamp without a tGP deadline)", r.DrainEstimate)
	}
	absent(t, r, "DrainEstimateAboveTGP")
}

// TestDeriveCarryOverUsesEstimate is the D2 regression guard (issue #212): layer 2
// evaluates the carry-over predicate on t_rot_est, the same denominator that spaces
// starts inside an occurrence — not on the deadline bound t_rot. A gap that sits
// between the two must stay silent.
func TestDeriveCarryOverUsesEstimate(t *testing.T) {
	in := baseAuto()
	in.TGP = 24 * time.Hour // t_rot = 24h30m; t_rot_est = 40m (default estimate)
	in.IdleGap = new(12 * time.Hour)

	r := Derive(in)
	if want := 40 * time.Minute; r.TRotEst != want {
		t.Fatalf("TRotEst = %v, want %v", r.TRotEst, want)
	}
	if r.TRot+in.Cooldown <= *in.IdleGap {
		t.Fatalf("precondition: the deadline bound must exceed the gap, else this proves nothing")
	}
	absent(t, r, "RotationSpansNextWindow")

	// Feeding the deadline back in as the estimate restores the old behavior.
	in.DrainEstimate = new(24 * time.Hour)
	has(t, Derive(in), "RotationSpansNextWindow", Warn)
}

// TestDeriveCeilBoundaryOnEstimate re-pins the #211 ceil boundary on the NEW
// denominator: D == denom admits exactly one start, D == denom + ε admits two.
func TestDeriveCeilBoundaryOnEstimate(t *testing.T) {
	in := baseAuto() // default estimate 10m → t_rot_est 40m, denom = 50m
	in.IdleGap = nil // keep the carry-over check out of the picture

	in.WindowLen = 50 * time.Minute
	if r := Derive(in); r.C != 1 {
		t.Errorf("C = %d, want 1 (D == denom exactly)", r.C)
	}
	in.WindowLen = 51 * time.Minute
	if r := Derive(in); r.C != 2 {
		t.Errorf("C = %d, want 2 (ceil must count the near-edge start)", r.C)
	}
}
