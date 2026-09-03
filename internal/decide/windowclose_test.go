package decide_test

import (
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/decide"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

func ts(t time.Time) string { return t.Format(time.RFC3339) }

// The branch matrix of the §4.2 window-close evaluation. Every case names the
// operational situation it stands for, because the action alone does not say why.
func TestWindowEdge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 6, 31, 0, 0, time.UTC)
	opened := now.Add(-91 * time.Minute) // the window ran 05:00–06:30
	outstanding := selection.Census{Total: 2, InBackoff: 2}

	tests := map[string]struct {
		inWindow bool
		ann      map[string]string
		census   selection.Census
		want     decide.WindowAction
	}{
		"in window, no stamp yet: record the open": {
			inWindow: true,
			ann:      map[string]string{},
			want:     decide.WindowStamp,
		},
		"in window, stamp already present: nothing to do": {
			inWindow: true,
			ann:      map[string]string{annotations.WindowOpenedAt: ts(opened)},
			want:     decide.WindowNothing,
		},
		"in window, corrupt stamp: re-stamp so the state self-heals": {
			inWindow: true,
			ann:      map[string]string{annotations.WindowOpenedAt: "not-a-time"},
			want:     decide.WindowStamp,
		},
		"out of window, never observed one: nothing to do": {
			inWindow: false,
			ann:      map[string]string{},
			want:     decide.WindowNothing,
		},
		"closed with candidates in backoff and no rotation: missed": {
			inWindow: false,
			ann:      map[string]string{annotations.WindowOpenedAt: ts(opened)},
			census:   outstanding,
			want:     decide.WindowMissed,
		},
		"closed with eligible candidates and no rotation: missed": {
			inWindow: false,
			ann:      map[string]string{annotations.WindowOpenedAt: ts(opened)},
			census:   selection.Census{Total: 1, Eligible: 1},
			want:     decide.WindowMissed,
		},
		"closed with nothing outstanding: settle quietly": {
			inWindow: false,
			ann:      map[string]string{annotations.WindowOpenedAt: ts(opened)},
			census:   selection.Census{Total: 3, NotTriggered: 3},
			want:     decide.WindowSettled,
		},
		"closed after a rotation succeeded inside it: settle quietly": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: ts(opened),
				annotations.LastRotationAt: ts(opened.Add(20 * time.Minute)),
			},
			census: outstanding,
			want:   decide.WindowSettled,
		},
		// A window gates only STARTS: an attempt that began in-window keeps
		// running past the boundary (the stamp is held for it by WindowDefer),
		// and a success that lands after the close is still attributable to this
		// occurrence. That is why the operator-facing wording says "attributable
		// to the occurrence", not "inside it".
		"closed, then the in-flight rotation succeeded after the boundary: settle quietly": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: ts(opened),
				annotations.LastRotationAt: ts(now.Add(-time.Minute)), // after the 06:30 close
			},
			census: outstanding,
			want:   decide.WindowSettled,
		},
		"closed, last success predates this occurrence: missed": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: ts(opened),
				annotations.LastRotationAt: ts(opened.Add(-24 * time.Hour)),
			},
			census: outstanding,
			want:   decide.WindowMissed,
		},
		"closed with a rotation still in flight: defer the verdict": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: ts(opened),
				annotations.ActiveRotation: "nc-1",
			},
			census: outstanding,
			want:   decide.WindowDefer,
		},
		"closed with a corrupt stamp: clear it, claim nothing": {
			inWindow: false,
			ann:      map[string]string{annotations.WindowOpenedAt: "not-a-time"},
			census:   outstanding,
			want:     decide.WindowSettled,
		},
		// An unreadable stamp names no occurrence, so there is nothing for the
		// in-flight rotation to defer TO — the garbage is cleared and nothing is
		// claimed, even though a rotation is running.
		"closed with a corrupt stamp while a rotation is in flight: clear it, claim nothing": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: "not-a-time",
				annotations.ActiveRotation: "nc-1",
			},
			census: outstanding,
			want:   decide.WindowSettled,
		},
		// The claim being rotated sits in the census's InFlight bucket, which
		// Outstanding deliberately does not count, so a zero outstanding count is
		// exactly what an in-flight rotation looks like. Deferring on the anchor
		// rather than settling on the count is what lets the verdict be taken
		// correctly once the rotation ends.
		"closed with a rotation in flight and nothing else outstanding: still defer": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: ts(opened),
				annotations.ActiveRotation: "nc-1",
			},
			census: selection.Census{Total: 1, InFlight: 1},
			want:   decide.WindowDefer,
		},
		"closed under an operator freeze: settle quietly": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: ts(opened),
				annotations.Freeze:         ts(now.Add(time.Hour)),
			},
			census: outstanding,
			want:   decide.WindowSettled,
		},
		"closed after the freeze expired: still missed": {
			inWindow: false,
			ann: map[string]string{
				annotations.WindowOpenedAt: ts(opened),
				annotations.Freeze:         ts(now.Add(-time.Hour)),
			},
			census: outstanding,
			want:   decide.WindowMissed,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := decide.WindowEdge(decide.WindowInputs{
				Now:         now,
				InWindow:    tc.inWindow,
				Annotations: tc.ann,
				Census:      tc.census,
			})
			if got != tc.want {
				t.Errorf("WindowEdge = %q, want %q", got, tc.want)
			}
		})
	}
}

// Outstanding counts the claims the window left undone by AGE AND STATE: past
// the age trigger and not excluded for a reason of their own. It is not "the
// claims the controller could have rotated" — a pool-level gate (static
// capacity, fatal infeasibility) sits BELOW this evaluation and is deliberately
// not consulted. Every other census bucket is deliberately not outstanding — a
// claim the operator opted out of, one Node Auto Repair owns, or one already
// counted as expired must never make a window look lost.
func TestOutstandingCountsOnlyRotatableClaims(t *testing.T) {
	t.Parallel()

	full := selection.Census{
		Total: 8, Eligible: 1, InBackoff: 1,
		OptedOut: 1, Deleting: 1, NotReady: 1, InFlight: 1, Terminal: 1, NotTriggered: 1,
	}
	if got := decide.Outstanding(full); got != 2 {
		t.Errorf("Outstanding = %d, want 2 (Eligible + InBackoff only)", got)
	}
	for name, c := range map[string]selection.Census{
		"opted out":     {Total: 1, OptedOut: 1},
		"deleting":      {Total: 1, Deleting: 1},
		"not ready":     {Total: 1, NotReady: 1},
		"in flight":     {Total: 1, InFlight: 1},
		"terminal":      {Total: 1, Terminal: 1},
		"not triggered": {Total: 1, NotTriggered: 1},
	} {
		if got := decide.Outstanding(c); got != 0 {
			t.Errorf("%s: Outstanding = %d, want 0", name, got)
		}
	}
}
