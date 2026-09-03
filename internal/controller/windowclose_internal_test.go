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
