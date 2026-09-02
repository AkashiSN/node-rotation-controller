package decide

import (
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// WindowAction is what the reconcile loop must do about the maintenance window
// edge on this pass (spec §4.2). The values are internal control flow, not a
// public surface: the operator-facing surface is the WindowMissed Event, the
// noderotation_window_missed_total counter and the log line the loop emits.
type WindowAction string

const (
	// WindowNothing: the observed state already matches the window; no write.
	WindowNothing WindowAction = ""
	// WindowStamp: in-window with no readable window-opened-at — record the open.
	WindowStamp WindowAction = "stamp"
	// WindowDefer: the window closed but a rotation is still in flight. It may yet
	// succeed, so the stamp stays and the verdict waits for the anchor to clear.
	WindowDefer WindowAction = "defer"
	// WindowSettled: the window closed with nothing to report — clear the stamp
	// silently.
	WindowSettled WindowAction = "settled"
	// WindowMissed: the window closed with candidates outstanding and no rotation
	// completed inside it — clear the stamp AND report.
	WindowMissed WindowAction = "missed"
)

// WindowInputs is the pure view of the window edge: the NodePool's own
// annotations plus the claim census the loop already computes.
type WindowInputs struct {
	Now         time.Time
	InWindow    bool              // window.Schedule.InWindow(Now), resolved by the caller
	Annotations map[string]string // the NodePool's: window-opened-at / last-rotation-at / active-rotation
	Census      selection.Census  // the pool's claims, classified (spec §3.2)
}

// Outstanding is the number of claims that were the controller's to rotate in
// the window that just closed: past the age trigger, and not held back by a
// reason of the claim's own.
//
// InBackoff is the load-bearing half. noderotation_candidates counts only
// Eligible, so a pool whose every candidate is inside its escalated retryBackoff
// reports zero candidates while the window drains away — the exact shape of
// issue #303, and the reason a "candidates > 0" alert stayed silent through it.
//
// Every other bucket is excluded deliberately: NotTriggered was never owed this
// window, OptedOut is the operator's own karpenter.sh/do-not-disrupt, Deleting is
// already going away, NotReady belongs to Node Auto Repair, Terminal is already
// counted by completed_total{outcome="expired"}, and InFlight cannot reach the
// verdict because an in-flight rotation defers it.
func Outstanding(c selection.Census) int { return c.Eligible + c.InBackoff }

// WindowEdge reports what to do about the maintenance window on this pass.
//
// The presence of window-opened-at is the occurrence's identity, so no
// occurrence start is derived from the schedule: internal/window projects
// entries onto a canonical weekly timeline with DST pinned to an anchor week,
// which would put a recovered wall-clock start up to an hour out and misjudge
// whether a rotation landed inside the occurrence.
//
// A stamp that does not parse is treated as no stamp in-window (so the state
// heals on the next pass) and as nothing to report out-of-window (so an
// unreadable annotation can never manufacture a WindowMissed). Both directions
// fail toward silence on the emitting side.
func WindowEdge(in WindowInputs) WindowAction {
	raw := in.Annotations[annotations.WindowOpenedAt]
	opened, parsed := parseTime(raw)

	if in.InWindow {
		if parsed {
			return WindowNothing
		}
		return WindowStamp
	}
	switch {
	case raw == "":
		return WindowNothing
	case !parsed:
		return WindowSettled
	case in.Annotations[annotations.ActiveRotation] != "":
		return WindowDefer
	case Outstanding(in.Census) == 0:
		return WindowSettled
	}
	if rotated, ok := parseTime(in.Annotations[annotations.LastRotationAt]); ok && !rotated.Before(opened) {
		return WindowSettled
	}
	return WindowMissed
}
