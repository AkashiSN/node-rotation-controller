// Package schedule implements the ageThreshold derivation and its layered
// feasibility validation (spec §3.2). It is pure arithmetic over resolved
// duration inputs — no Karpenter or Kubernetes types — so callers extract E and
// tGP from the NodeClaim/NodePool (applying the nil-tGP fallback) and pass plain
// values. Layer 1 (scheduling feasibility) and layer 2 (throughput) are returned
// as structured Findings; layer 3 (per-node runtime) is the caller's job.
//
// One layer-1 row from spec §3.2 is intentionally NOT evaluated here because it
// needs cluster context this pure-logic layer does not have; it belongs to the
// reconcile/startup wiring: the NodePool spec.limits headroom check (the
// authoritative, candidate-dependent surge_headroom check runs at rotation start,
// §5.2 step 3). The Auto Mode "E + tGP > 21d" cap is evaluated here — it is pure
// arithmetic over E and tGP, which Derive already has (issue #59).
package schedule

import (
	"fmt"
	"time"
)

// Buffer is the fixed slack added to t_rot beyond readyTimeout + tGP (spec §3.2
// symbol table). 15m makes the worked example's t_rot land at 1.5h.
const Buffer = 15 * time.Minute

// HardCap is the EKS Auto Mode ceiling on a node's true end-of-life: Auto Mode
// enforces expireAfter + terminationGracePeriod ≤ 21d (§1.1), a limit operators
// can lower but not remove. A NodePool whose representative E + tGP exceeds it is
// surfaced as a non-fatal HardCapExceeded warning (spec §3.2 layer 1). Self-
// managed Karpenter is not subject to the cap, but the sum is evaluated
// unconditionally (Auto Mode is not reliably detectable from the NodePool API and
// is the primary target, §1.1); the warning is advisory and never changes A.
const HardCap = 21 * 24 * time.Hour

// DrainFallback is the fixed bound substituted for tGP when it is unset
// (self-managed Karpenter allows nil); it matches the §5.2 stuck-drain bound.
// Exported so callers resolve a nil tGP to the same value Derive expects.
const DrainFallback = time.Hour

// Severity classifies a Finding.
type Severity int

const (
	// Warn surfaces a risk; the NodePool can still be managed.
	Warn Severity = iota
	// Fatal means the schedule cannot guarantee the requested rotation chances.
	Fatal
)

func (s Severity) String() string {
	if s == Fatal {
		return "fatal"
	}
	return "warn"
}

// Finding is one structured validation result (spec §3.2 layers 1 & 2). Code is
// a stable machine identifier; Message is human-readable English.
type Finding struct {
	Severity Severity
	Code     string
	Message  string
}

// Inputs are the resolved per-NodePool derivation inputs. Durations are already
// resolved (the caller substitutes DrainFallback for an unset tGP and sets
// TGPWasUnset).
type Inputs struct {
	E              time.Duration  // expireAfter (representative template value)
	TGP            time.Duration  // terminationGracePeriod, fallback already applied
	TGPWasUnset    bool           // true when DrainFallback was substituted for tGP
	P              time.Duration  // worst-case window period (window.WorstCasePeriod)
	WindowLen      time.Duration  // D: window occurrence duration, for layer-2 throughput
	IdleGap        *time.Duration // shortest interval the window stays closed between occurrences (window.ShortestIdleGap); nil skips the layer-2 carry-over check
	ReadyTimeout   time.Duration  // surge.readyTimeout
	Cooldown       time.Duration  // surge.cooldownAfter
	RetryBackoff   time.Duration  // surge.retryBackoff
	K              int            // minRotationChances
	MaxUnavailable int            // m; surge parallelism, fixed at 1 in v1 (spec §3.3). 0 is treated as 1.
	NodeCount      int            // N; 0 skips the arrival-rate sub-check
	Override       *time.Duration // explicit ageThreshold override; nil => auto
}

// Result carries the derived values plus all findings.
type Result struct {
	TRot     time.Duration // t_rot
	A        time.Duration // ageThreshold (derived, or the override echoed back)
	G        int           // guaranteed chances (== K in auto mode; recomputed for an override)
	C        int           // throughput per window occurrence (layer 2); >= 1 whenever WindowLen > 0
	Findings []Finding
}

// Derive computes t_rot, A, G, C and returns all layer-1/2 findings. It never
// errors: feasibility problems are Findings (Fatal/Warn). The caller decides
// what a Fatal means for a given NodePool.
func Derive(in Inputs) Result {
	r := Result{}
	r.TRot = in.ReadyTimeout + in.TGP + Buffer

	if in.P <= 0 {
		// Without a window period the derivation is undefined; everything below
		// would divide by zero. Surface it and return what we can.
		r.Findings = append(r.Findings, Finding{
			Severity: Fatal,
			Code:     "NoWindows",
			Message:  "no maintenance window occurrences: worst-case period P is zero, so ageThreshold cannot be derived",
		})
		return r
	}

	if in.Override != nil {
		r.A = *in.Override
		r.G = int(floorDiv(in.E-r.TRot-r.A, in.P))
	} else {
		r.A = in.E - (time.Duration(in.K)*in.P + r.TRot)
		r.G = in.K
	}

	if denom := r.TRot + in.Cooldown; denom > 0 && in.WindowLen > 0 {
		// spec §3.2 layer-2: C = m·ceil(D/(t_rot+cooldown)). The window gates only
		// rotation *starts* (§3.1), so the legal starts in an occurrence of length D
		// are k·denom < D for k = 0, 1, …, and their count is ceil, not floor — the
		// final near-edge start completes past the window's close (issue #211). m is
		// fixed at 1 in v1 (policy validates surge.maxUnavailable == 1); kept explicit
		// so v2 surge parallelism needs no formula change.
		m := max(in.MaxUnavailable, 1)
		r.C = m * int(ceilDiv(in.WindowLen, denom))
	}

	r.Findings = append(r.Findings, layer1(in, r)...)
	r.Findings = append(r.Findings, layer2(in, r)...)
	return r
}

// layer1 covers scheduling feasibility (spec §3.2 layer-1 table).
func layer1(in Inputs, r Result) []Finding {
	var fs []Finding

	switch {
	case in.K < 1:
		fs = append(fs, Finding{Severity: Fatal, Code: "KBelowOne",
			Message: fmt.Sprintf("minRotationChances must be >= 1, got %d", in.K)})
	case in.K == 1:
		fs = append(fs, Finding{Severity: Warn, Code: "KBelowTwo",
			Message: "minRotationChances = 1 leaves no retry before Forceful Expiration; >= 2 is recommended"})
	}

	if in.Override != nil {
		// The override is normally validated positive by policy.AgeThresholdOverride,
		// but Derive is a pure function that must surface a broken input rather than
		// trust it — keep the A <= 0 guard symmetric with the auto branch below.
		switch {
		case r.A <= 0:
			fs = append(fs, Finding{Severity: Fatal, Code: "OverrideNonPositive",
				Message: fmt.Sprintf("ageThreshold override A = %v <= 0; must be a positive duration", r.A)})
		case r.G < 1:
			fs = append(fs, Finding{Severity: Fatal, Code: "OverrideGBelowOne",
				Message: fmt.Sprintf("ageThreshold override guarantees %d completable window occurrences before expireAfter (need >= 1)", r.G)})
		case r.G < in.K:
			fs = append(fs, Finding{Severity: Warn, Code: "OverrideGBelowK",
				Message: fmt.Sprintf("ageThreshold override guarantees only %d chances, weaker than the requested minRotationChances %d", r.G, in.K)})
		}
		if r.A > 0 && r.A < in.P {
			fs = append(fs, Finding{Severity: Warn, Code: "AVeryAggressive",
				Message: fmt.Sprintf("ageThreshold A = %v is below one window period P = %v: nodes rotate very young, maximizing churn", r.A, in.P)})
		}
	} else {
		switch {
		case r.A <= 0:
			fs = append(fs, Finding{Severity: Fatal, Code: "ANonPositive",
				Message: fmt.Sprintf("schedule cannot guarantee %d rotation chances: ageThreshold A = %v <= 0; raise expireAfter, add window occurrences, or lower minRotationChances", in.K, r.A)})
		case r.A < in.P:
			fs = append(fs, Finding{Severity: Warn, Code: "AVeryAggressive",
				Message: fmt.Sprintf("ageThreshold A = %v is below one window period P = %v: nodes rotate very young, maximizing churn", r.A, in.P)})
		}
	}

	if in.TGPWasUnset {
		fs = append(fs, Finding{Severity: Warn, Code: "TGPUnset",
			Message: fmt.Sprintf("terminationGracePeriod is unset; drain is unbounded by Karpenter and t_rot falls back to %v", DrainFallback)})
	}

	if in.E+in.TGP > HardCap {
		fs = append(fs, Finding{Severity: Warn, Code: "HardCapExceeded",
			Message: fmt.Sprintf("expireAfter %v + terminationGracePeriod %v = %v exceeds the EKS Auto Mode 21d hard cap (§1.1); under Auto Mode this NodePool is rejected — lower expireAfter or terminationGracePeriod", in.E, in.TGP, in.E+in.TGP)})
	}

	if in.RetryBackoff < in.ReadyTimeout {
		fs = append(fs, Finding{Severity: Warn, Code: "RetryBackoffShort",
			Message: fmt.Sprintf("retryBackoff %v is shorter than readyTimeout %v; retries repeat the failed-surge cost faster than one attempt lasts", in.RetryBackoff, in.ReadyTimeout)})
	}

	return fs
}

// layer2 covers throughput (spec §3.2 layer-2). It only warns and never changes A.
func layer2(in Inputs, r Result) []Finding {
	var fs []Finding

	// With C = m·ceil(D/(t_rot+cooldown)), every occurrence of positive length admits
	// at least one rotation start, so C >= 1 and the checks below always compare
	// against a positive capacity. A non-positive D is degenerate — layer 1 reports
	// NoWindows for a schedule with no occurrences — and is the only input that can
	// still drive C below 1; nothing here is meaningful there (issue #211).
	if in.WindowLen <= 0 || r.C < 1 {
		return nil
	}

	// Candidate arrival rate exceeds capacity: C < N·P/A ⟺ C·A < N·P (A > 0).
	// float64 avoids int64 overflow on the ns-scale duration products; the
	// comparison tolerates the rounding.
	if in.NodeCount > 0 && r.A > 0 {
		if float64(r.C)*float64(r.A) < float64(in.NodeCount)*float64(in.P) {
			fs = append(fs, Finding{Severity: Warn, Code: "ThroughputBelowArrival",
				Message: fmt.Sprintf("candidate arrival (N=%d, P=%v, A=%v) can exceed capacity C=%d per window; candidates may accumulate", in.NodeCount, in.P, r.A, r.C)})
		}
	}

	// spec §3.2 layer-2 (synchronized batch): the steady-state arrival check above
	// assumes a uniform age distribution. A batch of N nodes created together shares
	// one deadline and contends for the same windows; its leadTime guarantees K
	// window occurrences of capacity C each, so it is fully rotated only when
	// K·C ≥ N. N > K·C leaves a surplus that misses every window and reaches
	// Forceful Expiration — a case the average above does not detect.
	if in.NodeCount > 0 && in.K > 0 && in.NodeCount > in.K*r.C {
		fs = append(fs, Finding{Severity: Warn, Code: "ThroughputBurstShortfall",
			Message: fmt.Sprintf("a synchronized batch of N=%d nodes exceeds K·C=%d (K=%d windows × C=%d per window): the surplus cannot rotate gracefully before a shared deadline and may reach Forceful Expiration", in.NodeCount, in.K*r.C, in.K, r.C)})
	}

	// spec §3.2 layer-2 (carry-over): C counts the legal starts within one occurrence
	// but does not establish that consecutive occurrences are independent. Because the
	// window gates only starts (§3.1), a rotation begun near an occurrence's close runs
	// for up to t_rot and still holds the per-NodePool serial gate (§5.2 step 2) when
	// the next occurrence opens, consuming part or all of it. K·C above therefore reads
	// as an upper bound whenever this warns. IdleGap is nil when the union never closes
	// (a continuously-open window has no next occurrence to carry into).
	if in.IdleGap != nil && r.TRot > *in.IdleGap {
		fs = append(fs, Finding{Severity: Warn, Code: "RotationSpansNextWindow",
			Message: fmt.Sprintf("t_rot %v exceeds the %v the maintenance window stays closed between occurrences: a rotation started near a window's close still holds the serial rotation gate when the next occurrence opens, so adjacent occurrences do not each deliver a full C=%d — read K·C as an upper bound; space the occurrences further apart or lower terminationGracePeriod", r.TRot, *in.IdleGap, r.C)})
	}

	return fs
}

// floorDiv returns floor(a/b) for b > 0 (Go integer division truncates toward
// zero, which differs from floor for negative numerators).
func floorDiv(a, b time.Duration) time.Duration {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// ceilDiv returns ceil(a/b) for a >= 0 and b > 0. It divides before adjusting
// (rather than the (a+b-1)/b idiom) so a week-scale numerator cannot overflow.
func ceilDiv(a, b time.Duration) time.Duration {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}
