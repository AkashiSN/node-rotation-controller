package controller

import (
	"context"
	"testing"
	"time"

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
}

// A rotation still draining when the window closes may yet succeed, so the
// verdict waits for the anchor rather than reporting a loss that did not happen.
//
// The interceptor simulates the anchor clearing between decide.WindowEdge's
// verdict (computed from the pool object the caller already holds, which still
// shows the rotation in flight) and the authoritative re-read inside
// patchPoolIf: it strips active-rotation on the FIRST Get of the pool. A
// correct WindowDefer handling never performs that Get at all — it writes
// nothing and defers — so the race is never even reached; only a handler that
// (incorrectly) routes WindowDefer into the WindowMissed write path would ever
// observe the cleared anchor and act on it. This is what makes the branch
// routing itself load-bearing, independent of the WindowMissed veto that would
// otherwise mask the same bug whenever the anchor is genuinely still held.
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
		}).Build()
	r := &RotationReconciler{
		Client:            cl,
		Namespace:         testNS,
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
		Recorder:          rec,
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
}

// The open is recorded once per occurrence, and re-entering the same open
// window does not re-stamp it.
func TestWindowOpenStampsOnceAndIsIdempotent(t *testing.T) {
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, nil, pool)
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
	if err := r.evaluateWindowEdge(ctx, pool, res, testNow, views, excluded, sched.InWindow(testNow)); err != nil {
		t.Fatalf("evaluateWindowEdge (first pass): %v", err)
	}
	first := getPool(t, r).Annotations[annotations.WindowOpenedAt]
	if first == "" {
		t.Fatal("window-opened-at must be stamped on the first pass")
	}

	if err := r.evaluateWindowEdge(ctx, pool, res, testNow, views, excluded, sched.InWindow(testNow)); err != nil {
		t.Fatalf("evaluateWindowEdge (second pass): %v", err)
	}
	second := getPool(t, r).Annotations[annotations.WindowOpenedAt]
	if second != first {
		t.Errorf("window-opened-at changed on the second pass: got %q, want unchanged %q", second, first)
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
}
