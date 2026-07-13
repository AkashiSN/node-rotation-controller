package decide_test

import (
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/decide"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func rfc(t time.Time) string { return t.Format(time.RFC3339) }

// TestStartGateOrder pins both the verdict and the REASON STRING, which is emitted
// to logs and events — a rename here is a user-visible change (§5.2).
func TestStartGateOrder(t *testing.T) {
	tests := []struct {
		name     string
		in       decide.Inputs
		wantOK   bool
		wantGate decide.Gate
	}{
		{
			name:     "out of window loses first",
			in:       decide.Inputs{Now: now, InWindow: false},
			wantOK:   false,
			wantGate: decide.GateOutOfWindow,
		},
		{
			name: "frozen beats cooldown",
			in: decide.Inputs{
				Now: now, InWindow: true,
				Annotations: map[string]string{annotations.Freeze: rfc(now.Add(time.Hour))},
			},
			wantOK:   false,
			wantGate: decide.GateFrozen,
		},
		{
			name: "inside cooldownAfter",
			in: decide.Inputs{
				Now: now, InWindow: true, Cooldown: 10 * time.Minute,
				Annotations: map[string]string{annotations.LastRotationAt: rfc(now.Add(-5 * time.Minute))},
			},
			wantOK:   false,
			wantGate: decide.GateCooldownAfterSuccess,
		},
		{
			name: "inside failurePause",
			in: decide.Inputs{
				Now: now, InWindow: true, FailurePause: 30 * time.Minute,
				Annotations: map[string]string{annotations.LastFailureAt: rfc(now.Add(-5 * time.Minute))},
			},
			wantOK:   false,
			wantGate: decide.GateFailurePause,
		},
		{
			name: "all gates open",
			in: decide.Inputs{
				Now: now, InWindow: true, Cooldown: 10 * time.Minute, FailurePause: 30 * time.Minute,
				Annotations: map[string]string{
					annotations.LastRotationAt: rfc(now.Add(-2 * time.Hour)),
					annotations.LastFailureAt:  rfc(now.Add(-2 * time.Hour)),
				},
			},
			wantOK:   true,
			wantGate: decide.GateNone,
		},
		{
			name:     "unset timestamps never block",
			in:       decide.Inputs{Now: now, InWindow: true, Cooldown: time.Hour, FailurePause: time.Hour},
			wantOK:   true,
			wantGate: decide.GateNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, gate := decide.StartGate(tt.in)
			if ok != tt.wantOK || gate != tt.wantGate {
				t.Fatalf("StartGate = (%v, %q), want (%v, %q)", ok, gate, tt.wantOK, tt.wantGate)
			}
		})
	}
}

// TestStartGateFailurePauseIndependentOfCooldown pins ADR-0004: gate B
// (post-failure) reads surge.failurePause and gate A (post-success) reads
// surge.cooldownAfter, so the two are independent — a success 5m ago no longer
// blocks a 1m cooldown while a failure 5m ago still blocks a 10m failurePause, the
// exact case a shared value could not express (issue #216). Moved from
// internal/controller/transitionlog_internal_test.go's
// TestStartGatesFailurePauseSplitFromCooldown with #238: that test called
// RotationReconciler.startGates directly, a method that no longer exists now that
// this logic lives in decide.StartGate. The resolve()-side half of that test — that
// r.resolve() itself derives failurePause as max(FailurePauseFloor, cooldownAfter),
// overridable — is controller-specific and stayed there, renamed
// TestResolveFailurePauseDefaultsToMaxFloorCooldown.
func TestStartGateFailurePauseIndependentOfCooldown(t *testing.T) {
	// cooldownAfter 1m, failurePause 10m (== max(FailurePauseFloor, cooldownAfter)
	// resolved by the controller for cooldownAfter 1m; see
	// TestResolveFailurePauseDefaultsToMaxFloorCooldown).
	base := decide.Inputs{Now: now, InWindow: true, Cooldown: time.Minute, FailurePause: 10 * time.Minute}

	// success 5m ago: gate A (cooldown 1m) is satisfied → open.
	open := base
	open.Annotations = map[string]string{annotations.LastRotationAt: rfc(now.Add(-5 * time.Minute))}
	if ok, gate := decide.StartGate(open); !ok {
		t.Errorf("post-success 5m > cooldown 1m: got (false, %q), want open", gate)
	}

	// failure 5m ago: gate B (failurePause 10m) still blocks, and names its own gate.
	blocked := base
	blocked.Annotations = map[string]string{annotations.LastFailureAt: rfc(now.Add(-5 * time.Minute))}
	if ok, gate := decide.StartGate(blocked); ok || gate != decide.GateFailurePause {
		t.Errorf("post-failure 5m < failurePause 10m: got (%v, %q), want (false, %q)", ok, gate, decide.GateFailurePause)
	}

	// An explicit failurePause (2m) overrides the max(floor, cooldownAfter) default.
	blocked.FailurePause = 2 * time.Minute
	if ok, _ := decide.StartGate(blocked); !ok {
		t.Error("post-failure 5m > explicit failurePause 2m: want open")
	}
}

// TestGateStringsAreStable: these exact strings reach logs and events today.
func TestGateStringsAreStable(t *testing.T) {
	for gate, want := range map[decide.Gate]string{
		decide.GateOutOfWindow:          "outOfWindow",
		decide.GateFrozen:               "frozen",
		decide.GateCooldownAfterSuccess: "cooldownAfterSuccess",
		decide.GateFailurePause:         "failurePause",
		decide.GateNone:                 "",
	} {
		if string(gate) != want {
			t.Errorf("gate = %q, want %q", string(gate), want)
		}
	}
}

// TestSurgelessFallback pins the ADR-0001 deadline race: the fallback fires only
// when a graceful surge started now cannot finish before the claim's own deadline
// (deadline − now < t_rot, where t_rot = readyTimeout + drainBound).
func TestSurgelessFallback(t *testing.T) {
	e := 100 * time.Hour
	// t_rot = 15m + 35m = 50m.
	in := decide.Inputs{
		Now: now, FallbackEnabled: true,
		ReadyTimeout: 15 * time.Minute, DrainBound: 35 * time.Minute,
	}
	racing := selection.Claim{Name: "racing", CreatedAt: now.Add(-e + 40*time.Minute), ExpireAfter: &e}
	safe := selection.Claim{Name: "safe", CreatedAt: now.Add(-e + 60*time.Minute), ExpireAfter: &e}
	never := selection.Claim{Name: "never", CreatedAt: now.Add(-1000 * time.Hour)}

	if !decide.SurgelessFallback(&racing, in) {
		t.Error("deadline 40m out with t_rot 50m must take the surge-less path")
	}
	if decide.SurgelessFallback(&safe, in) {
		t.Error("deadline 60m out with t_rot 50m must surge normally")
	}
	if decide.SurgelessFallback(&never, in) {
		t.Error("expireAfter: Never has no deadline and never qualifies")
	}
	off := in
	off.FallbackEnabled = false
	if decide.SurgelessFallback(&racing, off) {
		t.Error("fallback disabled must never fire (ADR-0001 is opt-in)")
	}
}

// ffTRot is readyTimeout (15m) + drainBound (32m) = 47m — the same t_rot
// internal/controller's forcefulfallback_internal_test.go fixtures used before this
// boundary-precision coverage moved here with #238 (the method it called,
// RotationReconciler.surgelessFallback, no longer exists; the reconcile-level tests
// in that file, which drive the same decision through reconcileNodePool, still pin
// it at that layer and were left untouched).
const ffTRot = 47 * time.Minute

// ffClaim builds a candidate whose Forceful Expiration deadline sits gap after now
// (deadline = CreatedAt + *ExpireAfter, expireAfter fixed at 14d as the original
// fixture had it). A negative gap puts the deadline in the past.
func ffClaim(gap time.Duration) selection.Claim {
	e := 14 * 24 * time.Hour
	return selection.Claim{Name: "nc-old", CreatedAt: now.Add(gap - e), ExpireAfter: &e}
}

// ffNeverClaim builds a candidate with expireAfter: Never — no deadline at all.
func ffNeverClaim(age time.Duration) selection.Claim {
	return selection.Claim{Name: "nc-old", CreatedAt: now.Add(-age)}
}

// TestSurgelessFallbackThreshold pins the decision function on both sides of the
// boundary, on the boundary itself, and for the deadline-less candidate.
func TestSurgelessFallbackThreshold(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		cand    selection.Claim
		want    bool
	}{{
		// The fallback is opt-in: a candidate that would otherwise qualify surges.
		name:    "disabled policy never falls back",
		enabled: false,
		cand:    ffClaim(ffTRot - time.Second),
		want:    false,
	}, {
		name:    "gap strictly below t_rot falls back",
		enabled: true,
		cand:    ffClaim(ffTRot - time.Second),
		want:    true,
	}, {
		// deadline − now == t_rot: a graceful surge started now finishes exactly at
		// the deadline, so it still wins the race. The inequality is strict.
		name:    "gap exactly t_rot keeps surging",
		enabled: true,
		cand:    ffClaim(ffTRot),
		want:    false,
	}, {
		name:    "gap just above t_rot keeps surging",
		enabled: true,
		cand:    ffClaim(ffTRot + time.Second),
		want:    false,
	}, {
		name:    "far deadline keeps surging",
		enabled: true,
		cand:    ffClaim(7 * 24 * time.Hour),
		want:    false,
	}, {
		// Already past its deadline: Karpenter is force-expiring it or is about to,
		// so a surge cannot possibly win.
		name:    "deadline already passed falls back",
		enabled: true,
		cand:    ffClaim(-time.Hour),
		want:    true,
	}, {
		name:    "expireAfter Never never falls back",
		enabled: true,
		cand:    ffNeverClaim(20 * 24 * time.Hour),
		want:    false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := decide.Inputs{
				Now: now, FallbackEnabled: tc.enabled,
				ReadyTimeout: 15 * time.Minute, DrainBound: 32 * time.Minute, // t_rot = 47m = ffTRot
			}
			if got := decide.SurgelessFallback(&tc.cand, in); got != tc.want {
				t.Errorf("SurgelessFallback = %v, want %v", got, tc.want)
			}
		})
	}
}
