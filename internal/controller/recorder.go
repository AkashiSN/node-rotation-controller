package controller

// Recorder captures the §4.2 metric/alert emission points of the rotation state
// machine. It is an interface so the actual Prometheus wiring (a later v0.3 PR)
// can be supplied without the state machine depending on a metrics library; the
// no-op default lets the reconciler run and be unit-tested in isolation.
//
// The nodePool/nodeClaim arguments are label values; emission is intentionally
// not transactional with the annotation writes (spec §5.2 documents the
// at-least-once / at-most-once skews the alert rules tolerate).
type Recorder interface {
	// Success records a controller-driven rotation that completed (cooldown consumed).
	Success(nodePool string)
	// Expired records a rotation aborted by a force-expiry — nothing was rotated.
	Expired(nodePool, nodeClaim string)
	// Failure records a surge attempt that timed out and rolled back.
	Failure(nodePool, nodeClaim string)
	// DrainStuck records a drain that exceeded its bound (operator remediation).
	DrainStuck(nodePool, nodeClaim string)
	// RetryCount surfaces the consecutive-failure gauge feeding the systematic-failure alert.
	RetryCount(nodePool, nodeClaim string, count int)
}

// noopRecorder is the default when no Recorder is supplied.
type noopRecorder struct{}

func (noopRecorder) Success(string)                 {}
func (noopRecorder) Expired(string, string)         {}
func (noopRecorder) Failure(string, string)         {}
func (noopRecorder) DrainStuck(string, string)      {}
func (noopRecorder) RetryCount(string, string, int) {}
