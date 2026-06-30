package controller

import "time"

// Recorder captures the §4.2 observability surface of the rotation state machine.
// It is an interface so the Prometheus wiring (internal/metrics) lives outside
// the state machine, which stays free of a metrics-library dependency; the no-op
// default lets the reconciler run and be unit-tested in isolation.
//
// The surface is split by emission shape:
//   - Completion counters (Success/Expired/Failure) fire once at a decision
//     point. Emission is intentionally not transactional with the annotation
//     writes — spec §5.2 documents the at-least-once / at-most-once skews the
//     alert rules (built on increase(...)) tolerate.
//   - ObservePool sets the per-NodePool gauges that reflect current state; they
//     are recomputed and re-set on every reconcile so they reset correctly (a
//     resolved drain stops alerting, a successful pool reports zero retries).
//   - ObserveWindow sets the per-NodePool window-active gauge (each pool resolves
//     its own governing-policy schedule, spec §5.4).
//   - ObserveDuration records a completed phase duration into the histogram.
type Recorder interface {
	// Success records a controller-driven rotation that completed (cooldown consumed).
	Success(nodePool string)
	// Expired records a rotation aborted by a force-expiry — nothing was rotated.
	Expired(nodePool, nodeClaim string)
	// Failure records a surge attempt that timed out and rolled back.
	Failure(nodePool, nodeClaim string)
	// ForcefulFallback records a surge-less, window-bounded forceful fallback
	// rotation that was initiated (spec §3.3): a candidate that could not complete
	// a graceful surge before its deadline, deleted in-window via the voluntary path.
	ForcefulFallback(nodePool, nodeClaim string)
	// ObservePool sets the §4.2 reconcile-time gauges for one NodePool.
	ObservePool(nodePool string, o PoolObservation)
	// ObserveWindow sets the per-NodePool window-active gauge (§4.2): whether the
	// pool's governing-policy maintenance window is currently open (spec §5.4).
	ObserveWindow(nodePool string, active bool)
	// ObservePolicyConflict sets the noderotation_policy_conflict gauge for one
	// NodePool: 1 while the pool is blocked from rotating by an equal-specificity
	// RotationPolicy tie or a runtime-invalid policy (spec §5.4), 0 once a single
	// governing policy resolves cleanly. ForgetPool drops the series.
	ObservePolicyConflict(nodePool string, blocked bool)
	// ObserveDuration records one completed phase duration (§4.2 duration_seconds).
	ObserveDuration(nodePool, phase string, d time.Duration)
	// ForgetPool drops every per-NodePool series when the NodePool is deleted, so
	// the recomputed gauges do not latch at their last value once its reconciles
	// stop (§4.2). The cluster-wide window-active gauge is unaffected.
	ForgetPool(nodePool string)
}

// duration_seconds phase labels (§4.2): surge_wait spans started-at →
// surge_ready; drain spans the NodePool draining-at anchor → old-NodeClaim
// finalization. The drain phase needs that durable anchor because the old
// NodeClaim's deletionTimestamp is gone by the completion point where the
// histogram is observed once (spec §4.2, §5.3).
const (
	PhaseSurgeWait = "surge_wait"
	PhaseDrain     = "drain"
)

// PoolObservation is the set of §4.2 reconcile-time gauges for one NodePool,
// recomputed from live state on every reconcile.
type PoolObservation struct {
	// Candidates is the eligible NodeClaim count (noderotation_candidates).
	Candidates int
	// InProgress is the active rotation count (noderotation_in_progress); 0 or 1
	// in v1 (serial per NodePool).
	InProgress int
	// FreezeUntil is the active freeze deadline; the zero time means no freeze
	// (noderotation_freeze_until_timestamp).
	FreezeUntil time.Time
	// AgeThreshold is the derived ageThreshold A (noderotation_age_threshold_seconds).
	AgeThreshold time.Duration
	// RotationChances is the guaranteed chances G (noderotation_rotation_chances).
	RotationChances int
	// WindowPeriod is the worst-case window period P (noderotation_window_period_seconds).
	WindowPeriod time.Duration
	// ShortLeadNodes counts claims whose own expireAfter can no longer guarantee K
	// chances (noderotation_short_lead_nodes; §3.2 layer 3).
	ShortLeadNodes int
	// DrainStuck is true when the in-flight drain has exceeded tGP + buffer
	// (noderotation_drain_stuck).
	DrainStuck bool
	// RetryCount is the highest retry-count across the pool's claims
	// (noderotation_retry_count); 0 when none.
	RetryCount int
}

// noopRecorder is the default when no Recorder is supplied.
type noopRecorder struct{}

func (noopRecorder) Success(string)                                {}
func (noopRecorder) Expired(string, string)                        {}
func (noopRecorder) Failure(string, string)                        {}
func (noopRecorder) ForcefulFallback(string, string)               {}
func (noopRecorder) ObservePool(string, PoolObservation)           {}
func (noopRecorder) ObserveWindow(string, bool)                    {}
func (noopRecorder) ObservePolicyConflict(string, bool)            {}
func (noopRecorder) ObserveDuration(string, string, time.Duration) {}
func (noopRecorder) ForgetPool(string)                             {}
