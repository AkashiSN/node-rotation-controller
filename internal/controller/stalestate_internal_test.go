package controller

import (
	"context"
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// advance() dispatches on the state of a claim it reads through the informer
// cache, so a pass can land in a handler whose durable state has already moved
// on. #309 made the writes INTO the terminal `expired` state claim their
// transition; these tests cover the two writes that move a claim BACKWARDS, and
// which a stale dispatch would otherwise use to undo another pass's work
// (issue #307, item 3):
//
//   - advancePending asserts `pending` on entry. On a claim the rollback has
//     already written `failed`, that assertion restores `pending`, stamps a fresh
//     started-at — restarting the readyTimeout deadline — and so bypasses the
//     escalated backoff that only advanceFailed enforces, while retry-count keeps
//     its incremented value.
//   - advanceFailed's retry branch writes `pending` and then re-enters advance().
//     That re-entry reads through the same cache, so a read still lagging its own
//     write dispatches straight back into advanceFailed and starts the attempt
//     again.
//
// Each test hands the handler a view of the claim taken before the durable state
// moved — what a lagging cache serves.

func TestPendingAssertionRefusesAClaimThatIsDurablyFailed(t *testing.T) {
	// Durable: rolled back, and well inside its escalated backoff —
	// EscalatedBackoff(2, 30m) is 60m and the failure is a minute old.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "2",
		annotations.FailedAt, rfc(testNow.Add(-time.Minute)),
	))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand, testK8sNode(candNode, true, nil, false))
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	// The lagging view: the version the claim held before the rollback wrote failed.
	stale := cand.DeepCopy()
	stale.Annotations[annotations.State] = annotations.StatePending
	stale.Annotations[annotations.StartedAt] = rfc(testNow.Add(-2 * time.Minute))
	got, err := r.advancePending(context.Background(), getPool(t, r), res, stale)
	if err != nil {
		t.Fatalf("advancePending: %v", err)
	}
	if got.RequeueAfter != shortRequeue {
		t.Errorf("a pass that owns nothing stands down and comes back: RequeueAfter = %v, want %v", got.RequeueAfter, shortRequeue)
	}

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("a durably failed claim must not be reasserted as pending: %+v", c)
	}
	if s := c.Annotations[annotations.StartedAt]; s != "" {
		t.Errorf("started-at = %q: re-stamping it restarts the readyTimeout deadline", s)
	}
	if n := c.Annotations[annotations.RetryCount]; n != "2" {
		t.Errorf("retry-count = %q, want it untouched at 2", n)
	}
	if placeholderExists(t, r) {
		t.Error("no attempt may be started while the escalated backoff is still running")
	}
	if getNodeObj(t, r, candNode).Spec.Unschedulable {
		t.Error("a pass that owns nothing must not cordon the candidate node")
	}
	if p := getPool(t, r).Annotations[annotations.ActiveRotation]; p != "nc-old" {
		t.Errorf("the anchor belongs to the failed rotation, got %q", p)
	}
}

// The same assertion, against the other direction a claim can have moved: a pass
// wrote `draining` and has not yet issued the delete (the crash window
// advanceDraining repairs). Regressing that to `pending` would restart the surge
// for a rotation that is already draining.
func TestPendingAssertionRefusesAClaimThatHasReachedDraining(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(), ncAnn(
		annotations.State, annotations.StateDraining,
		annotations.StartedAt, rfc(testNow.Add(-3*time.Minute)),
	))
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-time.Minute)),
	}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, true))
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	stale := cand.DeepCopy()
	stale.Annotations[annotations.State] = annotations.StatePending
	if _, err := r.advancePending(context.Background(), getPool(t, r), res, stale); err != nil {
		t.Fatalf("advancePending: %v", err)
	}

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateDraining {
		t.Fatalf("a draining claim must not be regressed to pending: %+v", c)
	}
	if placeholderExists(t, r) {
		t.Error("a draining rotation already has its surge; no placeholder may be created")
	}
}

// advanceFailed's retry branch re-enters advance(), which re-reads the claim
// through the cache. A read that still lags the write this branch just made
// dispatches straight back here, and an unconditional write would start the
// attempt over — once per turn round the loop, each time with a real Update.
// Requiring the durable state to still be `failed` ends it: the attempt now
// belongs to whichever pass reached pending first.
func TestFailedRetryRefusesAClaimAnotherPassAlreadyReEntered(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-2*time.Minute)),
		annotations.RetryCount, "1",
		annotations.FailedAt, rfc(testNow.Add(-40*time.Minute)), // backoff 30m → elapsed
	))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	stale := cand.DeepCopy()
	stale.Annotations[annotations.State] = annotations.StateFailed
	got, err := r.advanceFailed(context.Background(), getPool(t, r), res, stale)
	if err != nil {
		t.Fatalf("advanceFailed: %v", err)
	}
	if got.RequeueAfter != shortRequeue {
		t.Errorf("a pass that owns nothing stands down and comes back: RequeueAfter = %v, want %v", got.RequeueAfter, shortRequeue)
	}

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Fatalf("the durable pending attempt must survive: %+v", c)
	}
	if s := c.Annotations[annotations.StartedAt]; s != rfc(testNow.Add(-2*time.Minute)) {
		t.Errorf("started-at = %q: the owning pass's deadline must not move", s)
	}
	if placeholderExists(t, r) {
		t.Error("this pass must not drive the attempt another pass owns")
	}
	if getNodeObj(t, r, candNode).Spec.Unschedulable {
		t.Error("a pass that owns nothing must not cordon the candidate node")
	}
}
