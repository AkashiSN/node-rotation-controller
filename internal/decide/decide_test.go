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
