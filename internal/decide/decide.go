// Package decide holds the pure rotation decisions the reconcile loop makes: may a
// rotation start now (the §5.2 start gates), and must the picked candidate take the
// window-bounded surge-less path (ADR-0001). They read nothing but annotations,
// resolved durations and the clock — no client, no recorder, no metrics — so the
// policy simulator (which compiles to wasm) calls the very same functions the
// controller does, and the two cannot drift.
package decide

import (
	"math"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// Gate names the first start gate that blocked a rotation. The values are emitted
// to logs and events — treat them as a public surface.
type Gate string

const (
	GateNone                 Gate = ""
	GateOutOfWindow          Gate = "outOfWindow"
	GateFrozen               Gate = "frozen"
	GateCooldownAfterSuccess Gate = "cooldownAfterSuccess"
	GateFailurePause         Gate = "failurePause" // post-failure inter-attempt pause (gate B, §4.4, ADR-0004)
)

// Inputs is the resolved per-NodePool view the decisions read. It deliberately does
// not take the controller's `resolved` struct: that would drag controller-internal
// types into the pure layer.
type Inputs struct {
	Now         time.Time
	InWindow    bool              // window.Schedule.InWindow(Now), resolved by the caller
	Annotations map[string]string // the NodePool's: freeze / last-rotation-at / last-failure-at

	Cooldown     time.Duration // surge.cooldownAfter — gate A (post-success settle); may be 0 (ADR-0004)
	FailurePause time.Duration // surge.failurePause — gate B (post-failure pause)

	// Forceful-fallback (ADR-0001) inputs. t_rot = ReadyTimeout + DrainBound (§3.2).
	FallbackEnabled bool
	ReadyTimeout    time.Duration
	DrainBound      time.Duration // tGP + buffer; DrainFallback when tGP is unset (§5.2)
}

// StartGate reports whether a fresh rotation may start, and the first gate that said
// no. The order is load-bearing: the reason reported is the one that actually blocked.
func StartGate(in Inputs) (bool, Gate) {
	switch {
	case !in.InWindow:
		return false, GateOutOfWindow
	case Frozen(in.Annotations, in.Now):
		return false, GateFrozen
	case since(in.Annotations[annotations.LastRotationAt], in.Now) < in.Cooldown:
		return false, GateCooldownAfterSuccess
	case since(in.Annotations[annotations.LastFailureAt], in.Now) < in.FailurePause:
		return false, GateFailurePause
	}
	return true, GateNone
}

// Frozen reports whether the NodePool's freeze annotation is in the future.
func Frozen(ann map[string]string, now time.Time) bool {
	until, ok := parseTime(ann[annotations.Freeze])
	return ok && now.Before(until)
}

// SurgelessFallback reports whether the candidate must rotate via the opt-in
// window-bounded surge-less path (spec §3.3): the fallback is enabled and a graceful
// surge started now cannot finish before the candidate's own deadline
// (deadline − now < t_rot), so the surge would only lose the race to Forceful
// Expiration. The pool is already confirmed in-window by StartGate. A candidate with
// expireAfter: Never has no deadline and never qualifies.
func SurgelessFallback(c *selection.Claim, in Inputs) bool {
	if !in.FallbackEnabled {
		return false
	}
	if c.ExpireAfter == nil {
		return false
	}
	deadline := c.CreatedAt.Add(*c.ExpireAfter)
	tRot := in.ReadyTimeout + in.DrainBound
	return deadline.Sub(in.Now) < tRot
}

// since returns now − the RFC3339 timestamp, or +∞ when it is unset/unparseable.
func since(ts string, now time.Time) time.Duration {
	t, ok := parseTime(ts)
	if !ok {
		return time.Duration(math.MaxInt64)
	}
	return now.Sub(t)
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
