package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// The claim-side half of the family issue #304 opened on the NodePool (see
// staleanchor_internal_test.go). Every reconcile reads its NodeClaim through the
// informer cache too, so a pass can re-enter a handler on a claim view taken
// before an earlier pass wrote the terminal state — while the pool view it
// arrived on still shows the anchor. The two transitions INTO `expired` announce
// themselves right after that write, so a re-entry that rewrites the terminal
// state announces it a second time (issue #307).
//
// These tests hand the second pass copies of both objects taken before the first
// pass wrote — precisely what a lagging cache serves.

// claimViews returns n independent copies of the claim as it stands now, for
// handing to passes that must not see one another's writes.
func claimViews(c *karpv1.NodeClaim, n int) []*karpv1.NodeClaim {
	out := make([]*karpv1.NodeClaim, n)
	for i := range out {
		out[i] = c.DeepCopy()
	}
	return out
}

func expiringPendingRotation() (*karpv1.NodeClaim, *karpv1.NodePool, *corev1.Node, *corev1.Pod) {
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-2*time.Minute)),
	))
	cand.DeletionTimestamp = &dt
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	node := testK8sNode(candNode, true, map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.DoNotDisruptOwned:    "true",
		annotations.SurgeFor:             "nc-old",
	}, false)
	return cand, pool, node, placeholderPod("", corev1.PodPending)
}

// abortPendingExpiry: the candidate force-expires out of pending. A second pass
// on the pre-write claim view must find the transition already made and stay
// silent, exactly as advanceExpired — the handler for a claim ALREADY expired —
// deliberately does (spec §5.2).
func TestPendingForceExpiryIsCountedOnceOnAStaleClaimRead(t *testing.T) {
	cand, pool, node, ph := expiringPendingRotation()
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand, node, ph)
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	views := claimViews(cand, 2)
	for i, view := range views {
		if _, err := r.advancePending(context.Background(), getPool(t, r), res, view); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	if rec.expired != 1 {
		t.Errorf("expired counted %d times across a stale-claim re-entry, want exactly 1", rec.expired)
	}
	if rec.success != 0 {
		t.Errorf("a force-expiry must never count a success; got %d", rec.success)
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StateExpired {
		t.Errorf("the claim must still be terminally expired: %+v", c)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("anchor must be released, got %q", got)
	}
}

// advanceFailed's deletion branch: the backstop reached a rolled-back claim. Same
// transition into `expired`, same requirement.
func TestFailedBackstopExpiryIsCountedOnceOnAStaleClaimRead(t *testing.T) {
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	cand := testClaim("nc-old", 20*24*time.Hour, ncFinalizer(), ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, rfc(testNow.Add(-time.Minute)),
	))
	cand.DeletionTimestamp = &dt
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand)
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	for i, view := range claimViews(cand, 2) {
		if _, err := r.advanceFailed(context.Background(), getPool(t, r), res, view); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	if rec.expired != 1 {
		t.Errorf("expired counted %d times across a stale-claim re-entry, want exactly 1", rec.expired)
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StateExpired {
		t.Errorf("the claim must still be terminally expired: %+v", c)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("anchor must be released, got %q", got)
	}
}

// The other end of the veto: a claim that finalized away before the expiry write
// landed was never marked, so this pass cannot claim the transition — but the
// outcome still has to be counted once. Leaving the anchor set hands it to
// completeOrAbort, which owns exactly that case (no draining mirror ⇒ expired).
func TestPendingForceExpiryLeavesTheAnchorWhenTheClaimVanished(t *testing.T) {
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool) // nc-old already finalized away
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	gone := testClaim("nc-old", 20*24*time.Hour, ncAnn(annotations.State, annotations.StatePending))
	gone.DeletionTimestamp = &dt
	if _, err := r.advancePending(context.Background(), getPool(t, r), res, gone); err != nil {
		t.Fatalf("advancePending: %v", err)
	}

	if rec.expired != 0 {
		t.Errorf("a pass that never wrote the terminal state counted %d expiries, want 0", rec.expired)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Fatalf("the anchor must survive so completeOrAbort can own the outcome, got %q", got)
	}

	step(t, r, getPool(t, r))

	if rec.expired != 1 {
		t.Errorf("the vanished-claim abort counted %d expiries in total, want exactly 1", rec.expired)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("anchor must be released by the completing pass, got %q", got)
	}
}

// failPending reports the attempt number and the next-attempt instant an operator
// reads back off the annotations, so both must come from the value the increment
// actually wrote — not from the caller's claim copy, which may lag it. Here the
// durable retry-count is already 2 while the cached view still reads 0: the
// pre-#307 arithmetic would announce attempt 1 and a backoff expiry 90 minutes
// too early, while the gate reopens off the stored 3.
func TestFailureReportsTheRetryCountItActuallyWrote(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-20 * time.Minute)) // past the 15m readyTimeout
	ncAnn(annotations.RetryCount, "2")(cand)
	evs := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, node)
	r.Events = evs
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	stale := cand.DeepCopy()
	stale.Annotations[annotations.RetryCount] = "0" // the lagging cached view

	var lines []string
	ctx := log.IntoContext(context.Background(), captureLogger(&lines))
	if _, err := r.failPending(ctx, pool, res, stale); err != nil {
		t.Fatalf("failPending: %v", err)
	}

	// Stored retry-count 3 ⇒ EscalatedBackoff(3, 30m) = 30m << 2 = 2h.
	wantUntil := rfc3339(testNow.Add(2 * time.Hour))
	if !containsLine(lines, "rotation attempt failed", `"retryCount"=3`, `"backoffUntil"="`+wantUntil+`"`) {
		t.Errorf("the failure line must report the written retry count 3 and %s; lines = %v", wantUntil, lines)
	}
	if e := drain(evs); len(e) != 1 || !containsLine(e, "attempt 3", wantUntil) {
		t.Errorf("the Warning Event must report the written retry count 3 and %s; got %v", wantUntil, e)
	}
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.RetryCount] != "3" {
		t.Fatalf("the durable retry-count must advance from 2 to 3: %+v", c)
	}
	stored := parseInt(c.Annotations[annotations.RetryCount])
	failedAt, ok := parseTime(c.Annotations[annotations.FailedAt])
	if !ok {
		t.Fatal("failed-at must be stamped")
	}
	if reopensAt := failedAt.Add(selection.EscalatedBackoff(stored, testPolicy().Surge.RetryBackoff.Duration)); rfc3339(reopensAt) != wantUntil {
		t.Errorf("the reported backoffUntil %s must be the instant the gate reopens %s", wantUntil, rfc3339(reopensAt))
	}
}
