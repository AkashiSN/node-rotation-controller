package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/adapt"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

// A window that closes with every candidate inside its retryBackoff is the
// issue #303 shape: noderotation_candidates reads 0 the whole time, so the
// signal has to come from the census, and it must be emitted exactly once.
//
// The two evaluate calls model two racing reconciles that both read the pool
// while the stamp was still present: `live` performs the write, `stale` is the
// snapshot the other reader is still holding. Only the pass whose write lands
// may announce anything (issue #304) — this is what pins the `wrote` guard.
func TestWindowCloseMissedEmitsOnceAndClearsTheStamp(t *testing.T) {
	rec := &fakeRecorder{}
	cand := testClaim("nc-old", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(testNowOut.Add(-time.Minute)), // within backoff → InBackoff
		annotations.RetryCount, "1",
	))
	seed := withTGP(testNodePool(map[string]string{annotations.WindowOpenedAt: rfc(testNow)}))
	r := newReconciler(t, testNowOut, rec, seed, cand)
	evRec := events.NewFakeRecorder(16)
	r.Events = evRec
	ctx := context.Background()
	sched := mustSchedule(t)

	claims, err := r.poolClaims(ctx, seed)
	if err != nil {
		t.Fatalf("poolClaims: %v", err)
	}
	views, _ := adapt.Claims(claims)
	excluded, err := r.excludedClaims(ctx, seed, claims)
	if err != nil {
		t.Fatalf("excludedClaims: %v", err)
	}

	// Both readers observe the stamp still present before either writes.
	live := getPool(t, r)
	stale := getPool(t, r)

	resLive := r.resolve(live, testPolicy(), sched)
	if err := r.evaluateWindowEdge(ctx, live, resLive, testNowOut, views, excluded, sched.InWindow(testNowOut)); err != nil {
		t.Fatalf("evaluateWindowEdge (live pass): %v", err)
	}

	resStale := r.resolve(stale, testPolicy(), sched)
	if err := r.evaluateWindowEdge(ctx, stale, resStale, testNowOut, views, excluded, sched.InWindow(testNowOut)); err != nil {
		t.Fatalf("evaluateWindowEdge (stale pass): %v", err)
	}

	if rec.windowMissed != 1 {
		t.Errorf("windowMissed: got %d, want 1", rec.windowMissed)
	}
	if got := getPool(t, r).Annotations[annotations.WindowOpenedAt]; got != "" {
		t.Errorf("window-opened-at: got %q, want cleared", got)
	}

	evs := drain(evRec)
	if len(evs) != 1 {
		t.Fatalf("want exactly one Event, got %d: %v", len(evs), evs)
	}
	if !strings.Contains(evs[0], "Warning") {
		t.Errorf("the Event must be a Warning, got %q", evs[0])
	}
	// Hardcoded literal, not the reasonWindowMissed identifier: the point is to
	// pin the wire value independently of whatever the constant is set to.
	if !strings.Contains(evs[0], "WindowMissed") {
		t.Errorf("the Event must carry reason WindowMissed, got %q", evs[0])
	}
	if !strings.Contains(evs[0], "1 candidate") || !strings.Contains(evs[0], "0 eligible") || !strings.Contains(evs[0], "1 in retryBackoff") {
		t.Errorf("the Event message must name the counts, got %q", evs[0])
	}
}

// A rotation still draining when the window closes may yet succeed, so the
// verdict waits for the anchor rather than reporting a loss that did not happen.
//
// The interceptor simulates the anchor clearing between decide.WindowEdge's
// verdict (computed from the pool object the caller already holds, which still
// shows the rotation in flight) and the authoritative re-read inside
// patchPoolIf: it strips active-rotation on the FIRST Get of the pool. A
// correct WindowDefer handling never performs any write for this pass — it
// takes no patchPoolIf branch at all — so the property this test pins is zero
// Update calls on the pool, counted independently of whatever the interceptor
// does to Gets. Only a handler that (incorrectly) routes WindowDefer into the
// WindowMissed write path would ever observe the cleared anchor, pass its
// veto, and issue an Update — which is exactly the class of bug the
// WindowMissed veto alone cannot catch whenever the anchor is genuinely still
// held (that veto re-checks the same field the race just cleared).
func TestWindowCloseDefersWhileARotationIsInFlight(t *testing.T) {
	rec := &fakeRecorder{}
	active := testClaim("nc-active", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNowOut.Add(-2*time.Minute))))
	backoff := testClaim("nc-old", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(testNowOut.Add(-time.Minute)),
		annotations.RetryCount, "1",
	))
	pool := withTGP(testNodePool(map[string]string{
		annotations.WindowOpenedAt: rfc(testNow),
		annotations.ActiveRotation: "nc-active",
	}))

	var cleared bool
	var updates int
	cl := fake.NewClientBuilder().WithScheme(scheme.New()).
		WithObjects(pool, active, backoff).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if np, ok := obj.(*karpv1.NodePool); ok && np.Name == testPoolName && !cleared {
					cleared = true
					delete(np.Annotations, annotations.ActiveRotation)
				}
				return nil
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if np, ok := obj.(*karpv1.NodePool); ok && np.Name == testPoolName {
					updates++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	evRec := events.NewFakeRecorder(16)
	r := &RotationReconciler{
		Client:            cl,
		Namespace:         testNS,
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
		Recorder:          rec,
		Events:            evRec,
		Clock:             func() time.Time { return testNowOut },
	}
	ctx := context.Background()
	sched := mustSchedule(t)

	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		t.Fatalf("poolClaims: %v", err)
	}
	views, _ := adapt.Claims(claims)
	excluded, err := r.excludedClaims(ctx, pool, claims)
	if err != nil {
		t.Fatalf("excludedClaims: %v", err)
	}

	res := r.resolve(pool, testPolicy(), sched)
	if err := r.evaluateWindowEdge(ctx, pool, res, testNowOut, views, excluded, sched.InWindow(testNowOut)); err != nil {
		t.Fatalf("evaluateWindowEdge: %v", err)
	}

	if rec.windowMissed != 0 {
		t.Errorf("windowMissed: got %d, want 0", rec.windowMissed)
	}
	if got := getPool(t, r).Annotations[annotations.WindowOpenedAt]; got != rfc(testNow) {
		t.Errorf("window-opened-at: got %q, want the stamp still present", got)
	}
	if updates != 0 {
		t.Errorf("pool Update calls: got %d, want 0 (a deferred verdict must perform no write)", updates)
	}
	if evs := drain(evRec); len(evs) != 0 {
		t.Errorf("want no Event while the verdict is deferred, got %v", evs)
	}
}

// The open is recorded once per occurrence, and re-entering the same open
// window does not re-stamp it.
//
// Like the missed-emission test, this races two readers that both observe no
// stamp yet before either writes: `live` writes first, at `now`; `stale` is
// the delayed reader still holding the pre-write snapshot, evaluated later, at
// `now+5m`. Both passes independently conclude WindowStamp from what they each
// saw — that part is not in question — so this pins the write-side veto: the
// stale pass's own computed stamp (rfc3339 of a DIFFERENT time than live's)
// must lose to the fresh read showing live's stamp already parseable, or the
// clock advance would make an unconditional overwrite visible as a changed
// value rather than a silently-identical rewrite.
func TestWindowOpenStampsOnceAndIsIdempotent(t *testing.T) {
	rec := &fakeRecorder{}
	seed := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, rec, seed)
	ctx := context.Background()
	sched := mustSchedule(t)

	claims, err := r.poolClaims(ctx, seed)
	if err != nil {
		t.Fatalf("poolClaims: %v", err)
	}
	views, _ := adapt.Claims(claims)
	excluded, err := r.excludedClaims(ctx, seed, claims)
	if err != nil {
		t.Fatalf("excludedClaims: %v", err)
	}

	// Both readers observe no stamp yet before either writes.
	live := getPool(t, r)
	stale := getPool(t, r)

	resLive := r.resolve(live, testPolicy(), sched)
	if err := r.evaluateWindowEdge(ctx, live, resLive, testNow, views, excluded, sched.InWindow(testNow)); err != nil {
		t.Fatalf("evaluateWindowEdge (live pass): %v", err)
	}
	first := getPool(t, r).Annotations[annotations.WindowOpenedAt]
	if first == "" {
		t.Fatal("window-opened-at must be stamped on the live pass")
	}

	later := testNow.Add(5 * time.Minute)
	resStale := r.resolve(stale, testPolicy(), sched)
	if err := r.evaluateWindowEdge(ctx, stale, resStale, later, views, excluded, sched.InWindow(later)); err != nil {
		t.Fatalf("evaluateWindowEdge (stale pass): %v", err)
	}
	second := getPool(t, r).Annotations[annotations.WindowOpenedAt]
	if second != first {
		t.Errorf("window-opened-at changed on the stale pass: got %q, want unchanged %q", second, first)
	}
	if rec.windowMissed != 0 {
		t.Errorf("windowMissed: got %d, want 0", rec.windowMissed)
	}
}

// A rotation that completed inside the occurrence settles it: the stamp clears
// and nothing is reported.
func TestWindowCloseAfterASuccessIsSilent(t *testing.T) {
	rec := &fakeRecorder{}
	cand := testClaim("nc-old", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(testNowOut.Add(-time.Minute)), // within backoff → InBackoff
		annotations.RetryCount, "1",
	))
	pool := withTGP(testNodePool(map[string]string{
		annotations.WindowOpenedAt: rfc(testNow),
		annotations.LastRotationAt: rfc(testNow.Add(20 * time.Minute)), // inside the occurrence
	}))
	r := newReconciler(t, testNowOut, rec, pool, cand)
	evRec := events.NewFakeRecorder(16)
	r.Events = evRec
	ctx := context.Background()
	sched := mustSchedule(t)

	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		t.Fatalf("poolClaims: %v", err)
	}
	views, _ := adapt.Claims(claims)
	excluded, err := r.excludedClaims(ctx, pool, claims)
	if err != nil {
		t.Fatalf("excludedClaims: %v", err)
	}

	res := r.resolve(pool, testPolicy(), sched)
	if err := r.evaluateWindowEdge(ctx, pool, res, testNowOut, views, excluded, sched.InWindow(testNowOut)); err != nil {
		t.Fatalf("evaluateWindowEdge: %v", err)
	}

	if rec.windowMissed != 0 {
		t.Errorf("windowMissed: got %d, want 0", rec.windowMissed)
	}
	if got := getPool(t, r).Annotations[annotations.WindowOpenedAt]; got != "" {
		t.Errorf("window-opened-at: got %q, want cleared", got)
	}
	if evs := drain(evRec); len(evs) != 0 {
		t.Errorf("want no Event after a settled success, got %v", evs)
	}
}

// poolRaceReconciler builds a reconciler whose FIRST Get of the test NodePool
// applies mutate to the object it hands back — exactly how an annotation written
// between this pass's cached read and patchPoolIf's authoritative re-read is
// observed — and counts Updates on that pool into *updates. The stored object is
// never touched, so getPool still reports what is actually persisted. The
// returned raced() says whether the mutation was in fact applied, so a test can
// never pass by silently failing to stage the race.
func poolRaceReconciler(
	t *testing.T,
	rec *fakeRecorder,
	now time.Time,
	updates *int,
	mutate func(*karpv1.NodePool),
	objs ...client.Object,
) (*RotationReconciler, *events.FakeRecorder, func() bool) {
	t.Helper()
	var raced bool
	cl := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if np, ok := obj.(*karpv1.NodePool); ok && np.Name == testPoolName && !raced {
					raced = true
					mutate(np)
				}
				return nil
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if np, ok := obj.(*karpv1.NodePool); ok && np.Name == testPoolName {
					*updates++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	evRec := events.NewFakeRecorder(16)
	r := &RotationReconciler{
		Client:            cl,
		Namespace:         testNS,
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
		Recorder:          rec,
		Events:            evRec,
		Clock:             func() time.Time { return now },
	}
	return r, evRec, func() bool { return raced }
}

// runWindowEdge drives one window-close evaluation the way reconcileNodePool
// does: list the pool's claims once, project them, then evaluate.
func runWindowEdge(t *testing.T, r *RotationReconciler, pool *karpv1.NodePool, now time.Time) {
	t.Helper()
	ctx := context.Background()
	sched := mustSchedule(t)
	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		t.Fatalf("poolClaims: %v", err)
	}
	views, _ := adapt.Claims(claims)
	excluded, err := r.excludedClaims(ctx, pool, claims)
	if err != nil {
		t.Fatalf("excludedClaims: %v", err)
	}
	res := r.resolve(pool, testPolicy(), sched)
	if err := r.evaluateWindowEdge(ctx, pool, res, now, views, excluded, sched.InWindow(now)); err != nil {
		t.Fatalf("evaluateWindowEdge: %v", err)
	}
}

// backoffCandidate is the issue #303 shape: a claim past the age trigger whose
// last attempt failed and is still inside its escalated retryBackoff, so it is
// outstanding without ever being counted by noderotation_candidates.
func backoffCandidate(failedAt time.Time) *karpv1.NodeClaim {
	return testClaim("nc-old", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(failedAt),
		annotations.RetryCount, "1",
	))
}

// An operator freeze that lands between the cached read the verdict was computed
// from and the authoritative read inside the write loop must veto the report: the
// fresh object says the occurrence was declined, not lost, and a pass may only
// write what the object it is writing to still justifies.
//
// The verdict here is genuinely WindowMissed — the pool the pass holds carries no
// freeze — so nothing but re-running the verdict against the fresh annotations
// can catch it. Removing that re-assertion makes this test clear the stamp,
// increment the counter and raise the Event.
func TestWindowCloseMissedVetoedByAFreezeThatLandsFirst(t *testing.T) {
	rec := &fakeRecorder{}
	pool := withTGP(testNodePool(map[string]string{annotations.WindowOpenedAt: rfc(testNow)}))
	var updates int
	r, evRec, raced := poolRaceReconciler(t, rec, testNowOut, &updates,
		func(np *karpv1.NodePool) {
			np.Annotations[annotations.Freeze] = rfc(testNowOut.Add(time.Hour))
		},
		pool, backoffCandidate(testNowOut.Add(-time.Minute)))

	runWindowEdge(t, r, pool, testNowOut)

	if !raced() {
		t.Fatal("the freeze was never staged: the interceptor saw no NodePool Get")
	}
	if updates != 0 {
		t.Errorf("pool Update calls: got %d, want 0 (a frozen pool must not have its stamp cleared by this pass)", updates)
	}
	if rec.windowMissed != 0 {
		t.Errorf("windowMissed: got %d, want 0", rec.windowMissed)
	}
	if got := getPool(t, r).Annotations[annotations.WindowOpenedAt]; got != rfc(testNow) {
		t.Errorf("window-opened-at: got %q, want the stamp still present", got)
	}
	if evs := drain(evRec); len(evs) != 0 {
		t.Errorf("want no Event when the fresh object is frozen, got %v", evs)
	}
}

// The other direction: a settled verdict rests on annotations too, so a clear
// must be justified by the object it is written to. Here the cached map showed a
// last-rotation-at inside the occurrence (settle silently), but the fresh read no
// longer does — and with a candidate still outstanding the fresh object says the
// window was LOST. Clearing the stamp anyway would drop that loss forever: the
// stamp is the occurrence's only identity, and nothing re-creates it.
//
// So the pass writes nothing and the stamp survives for the next pass to judge.
// Removing the re-assertion makes this test clear the stamp silently.
func TestWindowCloseSettledVetoedWhenTheRotationRecordIsGone(t *testing.T) {
	rec := &fakeRecorder{}
	pool := withTGP(testNodePool(map[string]string{
		annotations.WindowOpenedAt: rfc(testNow),
		annotations.LastRotationAt: rfc(testNow.Add(20 * time.Minute)), // inside the occurrence
	}))
	var updates int
	r, evRec, raced := poolRaceReconciler(t, rec, testNowOut, &updates,
		func(np *karpv1.NodePool) { delete(np.Annotations, annotations.LastRotationAt) },
		pool, backoffCandidate(testNowOut.Add(-time.Minute)))

	runWindowEdge(t, r, pool, testNowOut)

	if !raced() {
		t.Fatal("the vanished last-rotation-at was never staged: the interceptor saw no NodePool Get")
	}
	if updates != 0 {
		t.Errorf("pool Update calls: got %d, want 0 (a settle the fresh object no longer justifies must not be written)", updates)
	}
	if got := getPool(t, r).Annotations[annotations.WindowOpenedAt]; got != rfc(testNow) {
		t.Errorf("window-opened-at: got %q, want the stamp preserved for the next pass to judge", got)
	}
	if rec.windowMissed != 0 {
		t.Errorf("windowMissed: got %d, want 0 (this pass announces nothing; the next pass judges)", rec.windowMissed)
	}
	if evs := drain(evRec); len(evs) != 0 {
		t.Errorf("want no Event from a vetoed settle, got %v", evs)
	}
}

// A maintenance window gates only rotation STARTS: an attempt that began inside
// the occurrence runs past the boundary, and the stamp is held for it
// (WindowDefer). When it finally succeeds, last-rotation-at lands AFTER the
// window closed — later than window-opened-at — and the occurrence settles
// silently. That success is attributable to the occurrence even though it did
// not complete inside it, which is why the operator-facing wording says
// "attributable to" rather than "inside".
func TestWindowCloseAfterALateSuccessIsSilent(t *testing.T) {
	rec := &fakeRecorder{}
	// The window is [00:00, 23:59); testNowOut is its close to the minute. Both
	// the success and this pass are after it.
	rotated := testNowOut.Add(10 * time.Second)
	now := testNowOut.Add(30 * time.Second)
	pool := withTGP(testNodePool(map[string]string{
		annotations.WindowOpenedAt: rfc(testNow),
		annotations.LastRotationAt: rfc(rotated),
	}))
	r := newReconciler(t, now, rec, pool, backoffCandidate(now.Add(-time.Minute)))
	evRec := events.NewFakeRecorder(16)
	r.Events = evRec

	if mustSchedule(t).InWindow(now) {
		t.Fatalf("test setup: %s must be outside the maintenance window", rfc(now))
	}
	runWindowEdge(t, r, pool, now)

	if rec.windowMissed != 0 {
		t.Errorf("windowMissed: got %d, want 0 (a success after the boundary still settles the occurrence)", rec.windowMissed)
	}
	if got := getPool(t, r).Annotations[annotations.WindowOpenedAt]; got != "" {
		t.Errorf("window-opened-at: got %q, want cleared", got)
	}
	if evs := drain(evRec); len(evs) != 0 {
		t.Errorf("want no Event after a late success, got %v", evs)
	}
}

// The verdict alone does not identify the occurrence. A pass still holding the
// PREVIOUS occurrence's stamp recomputes WindowMissed just as happily against a
// pool that has since opened — and stamped — a new one, so re-running the verdict
// cannot catch this on its own: the exact stamp the pass evaluated has to be
// re-asserted too. Otherwise the stale pass consumes the live occurrence's stamp
// and reports it under the earlier windowOpenedAt.
func TestWindowCloseMissedVetoedByANewerOccurrencesStamp(t *testing.T) {
	rec := &fakeRecorder{}
	newer := rfc(testNow.Add(12 * time.Hour))
	live := withTGP(testNodePool(map[string]string{annotations.WindowOpenedAt: newer}))
	r := newReconciler(t, testNowOut, rec, live, backoffCandidate(testNowOut.Add(-time.Minute)))
	evRec := events.NewFakeRecorder(16)
	r.Events = evRec

	// The delayed reader: same pool, but the stamp of the occurrence BEFORE the
	// one the object now carries.
	stale := live.DeepCopy()
	stale.Annotations[annotations.WindowOpenedAt] = rfc(testNow)

	runWindowEdge(t, r, stale, testNowOut)

	if got := getPool(t, r).Annotations[annotations.WindowOpenedAt]; got != newer {
		t.Errorf("window-opened-at: got %q, want the live occurrence's stamp %q untouched", got, newer)
	}
	if rec.windowMissed != 0 {
		t.Errorf("windowMissed: got %d, want 0 (the stale pass may not consume another occurrence)", rec.windowMissed)
	}
	if evs := drain(evRec); len(evs) != 0 {
		t.Errorf("want no Event from the stale pass, got %v", evs)
	}
}
