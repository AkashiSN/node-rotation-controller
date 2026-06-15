// Package annotations is the registry of the controller's own annotation and
// label keys — the durable on-object state described in spec §5.3. Keys are part
// of the project's compatibility surface (§6.1), so they live in one place
// rather than scattered as string literals across the handlers.
//
// Only the keys read by candidate selection (spec §3.2) are defined so far; the
// surge, drain, and completion handlers add the remaining keys as they land.
package annotations

// Prefix is the common namespace for every controller-owned key (spec §5.3).
const Prefix = "noderotation.io/"

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
)

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
