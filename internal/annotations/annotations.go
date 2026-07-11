// Package annotations is the registry of the controller's own annotation and
// label keys — the durable on-object state described in spec §5.3. Keys are part
// of the project's compatibility surface (§6.1), so they live in one place
// rather than scattered as string literals across the handlers.
//
// Beyond the keys read by candidate selection (spec §3.2), the surge builder
// (spec §3.3) defines SurgeFor; the rotation state machine (spec §5.2) adds the
// NodePool anchor, the per-attempt NodeClaim timestamps, and the node Cordoned
// marker that complete the §5.3 state model.
package annotations

// Prefix is the common namespace for every controller-owned key (spec §5.3).
const Prefix = "noderotation.io/"

// SurgeFor pairs the surge runtime objects with the rotation that owns them
// (spec §3.3, §5.3). Its value is the old NodeClaim's metadata.name. It is set
// as a label on the placeholder Pod and as an annotation on each
// controller-frozen node; the marker is what finds the placeholder and resolves
// the surge target after the old NodeClaim is gone. It is not an ownership
// marker for karpenter.sh/do-not-disrupt; DoNotDisruptOwned carries that.
const SurgeFor = Prefix + "surge-for"

// Per-NodeClaim progress-state keys (spec §5.3 State Model). The selector reads
// State (to exclude in-flight/terminal claims) and, for a Failed claim, FailedAt
// and RetryCount (to apply the escalated backoff before re-selection).
const (
	// State records where the anchored rotation is: one of the State* values.
	State = Prefix + "state"
	// FailedAt is the RFC3339 backoff anchor stamped when an attempt fails.
	FailedAt = Prefix + "failed-at"
	// RetryCount is the integer count of consecutive failures of this claim; it
	// escalates the re-selection backoff (spec §5.3).
	RetryCount = Prefix + "retry-count"
	// StartedAt is the RFC3339 readyTimeout deadline anchor, written once per
	// attempt and cleared by the failed write so a retry re-stamps its own
	// deadline (spec §5.3).
	StartedAt = Prefix + "started-at"
	// SurgeClaim records the induced surge NodeClaim's name, persisted by the
	// pending handler as soon as the placeholder's bind target is observable so
	// the rollback path can reap it even after the placeholder is gone (spec §5.3).
	SurgeClaim = Prefix + "surge-claim"
)

// Per-NodePool rotation keys (spec §5.3 State Model). The anchor outlives the old
// NodeClaim's deletion on success, so the durable record of an in-flight rotation
// — and the per-NodePool serial gate — lives here, not on the claim.
const (
	// ActiveRotation is the durable anchor: the in-flight rotation's old
	// NodeClaim name. Written before any other side effect at start, cleared last
	// at completion/failure; also the per-NodePool serial gate (spec §5.2).
	ActiveRotation = Prefix + "active-rotation"
	// ActiveRotationState mirrors whether the anchored rotation reached draining;
	// its absence after the old NodeClaim is gone marks a force-expiry, not a
	// success (spec §5.2). Its only value is StateDraining.
	ActiveRotationState = Prefix + "active-rotation-state"
	// DrainingAt is the RFC3339 drain-start anchor, stamped at the pending →
	// draining transition in the same update that writes ActiveRotationState. It
	// is the durable start time for the §4.2 drain-phase duration_seconds
	// histogram: the old NodeClaim's deletionTimestamp (the natural drain start)
	// has finalized away by the single completion point where the histogram is
	// observed once, so the duration needs its own pool-side anchor (spec §5.3).
	// Cleared with the anchor at completion.
	DrainingAt = Prefix + "draining-at"
	// SurgeWait is the surge-phase duration (started-at → surge_ready) carried
	// forward from the pending → draining transition to the single completion
	// point, where the old NodeClaim — and its started-at — has finalized away.
	// It lets the "rotation complete" line report the whole rotation's
	// total = surge_wait + drain on one self-contained line rather than a join
	// back to the earlier "surge node ready" line (spec §4.2, §5.3). Stamped
	// write-once in the same update as DrainingAt at the pending → draining
	// transition; read and cleared with the anchor at completion. Absent on the
	// surge-less forceful-fallback path, which has no surge phase.
	SurgeWait = Prefix + "surge-wait"
	// LastRotationAt is the RFC3339 completion time of the last successful
	// rotation; the cooldownAfter start-gate anchor (spec §5.2 step 2).
	LastRotationAt = Prefix + "last-rotation-at"
	// LastFailureAt is the RFC3339 anchor for the pool-level inter-attempt pause
	// after a failed attempt (spec §4.4, §5.2 step 2).
	LastFailureAt = Prefix + "last-failure-at"
	// Freeze is an RFC3339 freeze-until timestamp that suppresses rotation starts
	// and holds escalation of an in-flight pending rotation (spec §3.1).
	Freeze = Prefix + "freeze"
	// RotationMode records how the in-flight rotation is being performed, stamped
	// on the NodePool anchor so it survives the candidate NodeClaim's deletion on
	// success. Absent = the default surge (make-before-break) rotation.
	RotationMode = Prefix + "rotation-mode"
)

// Cordoned marks a node's cordon (spec.unschedulable) as controller-applied, so
// rollback and the startup sweep uncordon only what the controller cordoned —
// an operator's pre-existing cordon (no marker) is never touched (spec §5.3).
const Cordoned = Prefix + "cordoned"

// DoNotDisruptOwned marks a node's karpenter.sh/do-not-disrupt as
// controller-applied, so rollback and the startup sweep remove only the
// protection the controller itself set — an operator's pre-existing
// do-not-disrupt (no marker) is never touched (spec §5.3). It is the
// do-not-disrupt analogue of Cordoned: freeze() sets it only when the
// controller actually applies do-not-disrupt, never when do-not-disrupt is
// already present without this marker. SurgeFor alone cannot attribute
// ownership, because the controller still freezes (and so labels with SurgeFor)
// a node an operator had already protected.
const DoNotDisruptOwned = Prefix + "do-not-disrupt-owned"

// State annotation values (spec §5.3). An empty/absent State means a fresh claim.
const (
	// StatePending: a rotation is in flight, surge not yet ready (driven by §5.2 step 1).
	StatePending = "pending"
	// StateDraining: the old NodeClaim is being drained (driven by §5.2 step 1).
	StateDraining = "draining"
	// StateFailed: an attempt rolled back; re-selectable after its escalated backoff.
	StateFailed = "failed"
	// StateExpired: terminal — caught force-expiring mid-rotation; never re-selected.
	StateExpired = "expired"
)

// RotationMode annotation values (spec §5.3). An absent RotationMode means the
// default surge (make-before-break) rotation.
const (
	// RotationModeForcefulFallback marks a surge-less, window-bounded forceful
	// fallback rotation (spec §3.3): the NodeClaim is deleted in-window without a
	// surge, draining via the voluntary path (PDBs apply).
	RotationModeForcefulFallback = "forceful-fallback"
)
