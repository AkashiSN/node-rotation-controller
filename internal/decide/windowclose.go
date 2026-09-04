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
	// attributable to the occurrence ever completing — clear the stamp AND
	// report. "Attributable to" is wider than "inside it": a window gates only
	// starts, so an attempt that began in-window and succeeded after the boundary
	// settles the occurrence too (WindowDefer holds the stamp until it lands).
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

// Outstanding is the number of claims the window that just closed left undone by
// AGE AND STATE: past the age trigger, and not held back by a reason of the
// claim's own.
//
// It is not "the claims the controller could have rotated". The window-close
// evaluation deliberately sits above the pool-level gates (static capacity §3.3,
// fatal feasibility §5.2), so a claim this counts may be one the controller
// would never have started — a static pool misses every occurrence that closes
// with age/state-outstanding claims, and that is the reported fact, not a
// contradiction. The signal states what happened to the window, not why the
// controller did not act.
//
// The backoff half is load-bearing. noderotation_candidates counts only
// Eligible, so a pool whose every candidate is inside its escalated retryBackoff
// reports zero candidates while the window drains away — the exact shape of
// issue #303, and the reason a "candidates > 0" alert stayed silent through it.
//
// It counts InBackoffTriggered rather than the InBackoff bucket, because the
// bucket files a claim by its FIRST disqualifier and state is checked before
// age: a claim whose age stopped being due after its attempt failed — a
// RotationPolicy edit raising the override or shortening the lead time (the
// trigger is age > expireAfter - leadTime, so a wider lead time triggers
// earlier), an extended expireAfter — sits in InBackoff while being owed
// nothing. Counting the bucket
// would report a lost window for a claim the schedule no longer asks for
// (issue #321 review).
//
// Every other bucket is excluded deliberately: NotTriggered was never owed this
// window, OptedOut is the operator's own karpenter.sh/do-not-disrupt, Deleting is
// already going away, NotReady belongs to Node Auto Repair, Terminal is already
// counted by completed_total{outcome="expired"}, and InFlight cannot reach the
// verdict because an in-flight rotation defers it.
//
// A frozen pool never reaches this count at all: WindowEdge checks the freeze
// annotation before calling Outstanding, because freeze is a pool-level
// instruction to stop rotating — the same operator choice OptedOut represents
// at the claim level. A window that closed under a freeze was declined, not
// lost, so the stamp is still cleared, and the next occurrence starts clean.
func Outstanding(c selection.Census) int { return c.Eligible + c.InBackoffTriggered }

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
//
// A pool frozen at close settles quietly too, but only once any in-flight
// rotation has been given its chance to defer: freeze is set only by the
// operator (spec: "Use freeze to suppress rotation"), so it is the pool-level
// twin of a Node's karpenter.sh/do-not-disrupt, which Outstanding already
// excludes for the same reason — this is the operator's own choice, not a
// controller failure. A window that closed under a freeze was declined, not
// lost. The stamp is still cleared, not held, so the next occurrence starts
// clean.
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
	case Frozen(in.Annotations, in.Now):
		return WindowSettled
	case Outstanding(in.Census) == 0:
		return WindowSettled
	}
	if rotated, ok := parseTime(in.Annotations[annotations.LastRotationAt]); ok && !rotated.Before(opened) {
		return WindowSettled
	}
	return WindowMissed
}
