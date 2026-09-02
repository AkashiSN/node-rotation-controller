package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
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

// --- the write that never landed -------------------------------------------

// vanishOnFirstClaimUpdate answers the first NodeClaim Update carrying state by
// finalizing the claim away and returning the Conflict the API server would
// return for the now-stale resourceVersion. RetryOnConflict then re-enters with
// a Get that finds nothing — the attempt sequence a conditional write must not
// mistake for its own success.
func vanishOnFirstClaimUpdate(state string) interceptor.Funcs {
	first := true
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			nc, ok := obj.(*karpv1.NodeClaim)
			if !ok || !first || nc.Annotations[annotations.State] != state {
				return c.Update(ctx, obj, opts...)
			}
			first = false
			var live karpv1.NodeClaim
			if err := c.Get(ctx, client.ObjectKeyFromObject(nc), &live); err != nil {
				return err
			}
			live.Finalizers = nil
			if err := c.Update(ctx, &live); err != nil {
				return err
			}
			if err := client.IgnoreNotFound(c.Delete(ctx, &live)); err != nil {
				return err
			}
			return apierrors.NewConflict(
				schema.GroupResource{Group: karpapis.Group, Resource: "nodeclaims"}, nc.Name,
				errors.New("simulated stale resourceVersion"))
		},
	}
}

// A first attempt that reads a live claim and then loses its Update, followed by
// a retry whose Get finds the claim finalized away, wrote nothing at all. The
// outcome must be the vanished-claim one — no announcement, anchor retained —
// and not the first attempt's optimism carried across the retry.
func TestExpiryEmitsNothingWhenTheClaimVanishesUnderARetriedWrite(t *testing.T) {
	cand, pool, node, ph := expiringPendingRotation()
	rec := &fakeRecorder{}
	r := newFlakyReconciler(t, rec, vanishOnFirstClaimUpdate(annotations.StateExpired), pool, cand, node, ph)
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	if _, err := r.advancePending(context.Background(), getPool(t, r), res, cand.DeepCopy()); err != nil {
		t.Fatalf("advancePending: %v", err)
	}

	if getClaimOrNil(t, r, "nc-old") != nil {
		t.Fatal("test did not exercise the retry: the claim must be gone by the second attempt")
	}
	if rec.expired != 0 {
		t.Errorf("a pass whose write never landed counted %d expiries, want 0", rec.expired)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("the anchor must survive so completeOrAbort can own the outcome, got %q", got)
	}
}

// The veto must claim THIS handler's transition, not merely refuse a repeat. A
// claim deleted while still pending can reach `draining` through a pass that read
// it before the deletion; a later pass dispatched from the older `pending` view
// must not overwrite that with `expired`, announce it, and release the anchor —
// the rotation is draining and will complete.
func TestPendingForceExpiryRefusesAClaimThatHasReachedDraining(t *testing.T) {
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(), ncAnn(
		annotations.State, annotations.StateDraining,
	))
	cand.DeletionTimestamp = &dt
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-time.Minute)),
	}))
	surge := testK8sNode(surgeNode, true, map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.DoNotDisruptOwned:    "true",
		annotations.SurgeFor:             "nc-old",
	}, false)
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand, surge)
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	// The lagging view: the version this claim held before the pass that moved it on.
	stale := cand.DeepCopy()
	stale.Annotations[annotations.State] = annotations.StatePending
	if _, err := r.advancePending(context.Background(), getPool(t, r), res, stale); err != nil {
		t.Fatalf("advancePending: %v", err)
	}

	if rec.expired != 0 {
		t.Errorf("a stale pass counted %d expiries against a draining rotation, want 0", rec.expired)
	}
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateDraining {
		t.Fatalf("the durable draining state must survive the stale pass: %+v", c)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("the drain still owns the anchor, got %q", got)
	}
	if n := getNodeObj(t, r, surgeNode); n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Error("a stale pass must not unfreeze the surge node the live drain depends on")
	}
}

// failPending announces a failed attempt and stamps the pause anchor off a write
// that records it. A claim that finalized away mid-rollback recorded nothing, so
// there is no attempt to announce: the outcome is a force-expiry, which the
// anchor hands to completeOrAbort.
func TestFailPendingEmitsNothingWhenTheClaimVanished(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-20 * time.Minute))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, node) // cand never seeded: already gone
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	if _, err := r.failPending(context.Background(), getPool(t, r), res, cand); err != nil {
		t.Fatalf("failPending: %v", err)
	}

	if rec.failure != 0 {
		t.Errorf("a rollback that recorded nothing counted %d failures, want 0", rec.failure)
	}
	p := getPool(t, r)
	if p.Annotations[annotations.LastFailureAt] != "" {
		t.Error("no failed attempt was recorded, so no failure pause may be stamped")
	}
	if got := p.Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Fatalf("the anchor must survive so completeOrAbort can own the outcome, got %q", got)
	}

	step(t, r, getPool(t, r))

	if rec.expired != 1 || rec.failure != 0 {
		t.Errorf("a claim that vanished mid-rollback is a force-expiry, not a failure: %+v", rec)
	}
}

// The retried half of the same case: the failure write is attempted against a
// live claim, loses its Update, and finds nothing on the retry.
func TestFailPendingEmitsNothingWhenTheClaimVanishesUnderARetriedWrite(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-20 * time.Minute))
	rec := &fakeRecorder{}
	r := newFlakyReconciler(t, rec, vanishOnFirstClaimUpdate(annotations.StateFailed), pool, cand, node)
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	if _, err := r.failPending(context.Background(), getPool(t, r), res, cand.DeepCopy()); err != nil {
		t.Fatalf("failPending: %v", err)
	}

	if getClaimOrNil(t, r, "nc-old") != nil {
		t.Fatal("test did not exercise the retry: the claim must be gone by the second attempt")
	}
	if rec.failure != 0 {
		t.Errorf("a rollback whose write never landed counted %d failures, want 0", rec.failure)
	}
	p := getPool(t, r)
	if p.Annotations[annotations.LastFailureAt] != "" {
		t.Error("no failed attempt was recorded, so no failure pause may be stamped")
	}
	if got := p.Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("the anchor must survive so completeOrAbort can own the outcome, got %q", got)
	}
}

// --- the announcement must outlive a failed cleanup -------------------------

// failFirstPodDelete makes the placeholder delete fail once with a transient API
// error — the ordinary kind a reconcile retries — after the terminal write has
// already landed.
func failFirstPodDelete() interceptor.Funcs {
	first := true
	return interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Pod); ok && first {
				first = false
				return apierrors.NewInternalError(errors.New("simulated transient API error"))
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// Once the terminal write lands the outcome is decided, and the announcement
// belongs to that pass — it cannot wait behind fallible cleanup. A cleanup error
// sends the next reconcile to advanceExpired, which repairs the cleanup and
// deliberately never emits, so an expiry announced after the cleanup would be
// lost from the counter for good rather than merely delayed.
func TestPendingForceExpiryCountsTheExpiryWhenCleanupFails(t *testing.T) {
	cand, pool, node, ph := expiringPendingRotation()
	rec := &fakeRecorder{}
	r := newFlakyReconciler(t, rec, failFirstPodDelete(), pool, cand, node, ph)
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	if _, err := r.advancePending(context.Background(), getPool(t, r), res, cand.DeepCopy()); err == nil {
		t.Fatal("test did not exercise the cleanup failure: the pass must return the error")
	}
	if rec.expired != 1 {
		t.Errorf("the pass that wrote the terminal state counted %d expiries, want 1", rec.expired)
	}

	// Recovery runs through advanceExpired, which repairs the cleanup in silence.
	step(t, r, getPool(t, r))

	if rec.expired != 1 {
		t.Errorf("recovery re-announced the expiry: counted %d in total, want 1", rec.expired)
	}
	if placeholderExists(t, r) {
		t.Error("recovery must complete the cleanup the failed pass left behind")
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("recovery must release the anchor, got %q", got)
	}
}

// The mirror of TestPendingForceExpiryRefusesAClaimThatHasReachedDraining, on the
// other call site: advanceFailed reads a claim as failed with no deletionTimestamp,
// an external delete stamps one, and its own retry branch writes pending back over
// it. A pass still lagging at the failed view must not expire what is now a fresh
// pending attempt — that call site has to pass its own pre-state, not any state.
func TestFailedBackstopExpiryRefusesAClaimThatHasReturnedToPending(t *testing.T) {
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-time.Minute)),
		annotations.RetryCount, "1",
	))
	cand.DeletionTimestamp = &dt
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand)
	res := r.resolve(pool, testPolicy(), mustSchedule(t))

	stale := cand.DeepCopy()
	stale.Annotations[annotations.State] = annotations.StateFailed
	stale.Annotations[annotations.FailedAt] = rfc(testNow.Add(-time.Hour))
	if _, err := r.advanceFailed(context.Background(), getPool(t, r), res, stale); err != nil {
		t.Fatalf("advanceFailed: %v", err)
	}

	if rec.expired != 0 {
		t.Errorf("a stale failed pass counted %d expiries against a fresh pending attempt, want 0", rec.expired)
	}
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Fatalf("the durable pending state must survive the stale pass: %+v", c)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("the pending attempt still owns the anchor, got %q", got)
	}
}
