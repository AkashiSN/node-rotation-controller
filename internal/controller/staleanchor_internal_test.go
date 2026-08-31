package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// Every reconcile reads its NodePool through the informer cache, and completion
// is exactly when watch events pile up on the pool: the old NodeClaim's delete
// and the surge node reaching Ready both map back to it. A reconcile enqueued by
// one of those can therefore start before the cache has observed the
// anchor-clearing write and re-enter completeOrAbort on a pool that still looks
// anchored, with the NodeClaim already gone — a second, complete pass through
// the completion side effects during ordinary operation (issue #304).
//
// The completion counters, the completion line, and the Event must therefore be
// owned by the pass that actually released the anchor, not by every pass that
// happens to read a pool where it is still set. These tests hand the second pass
// a copy of the pool taken before the first pass wrote — precisely what a lagging
// cache serves.

// passes runs reconcileNodePool once per supplied NodePool view, returning every
// log line the passes emitted.
func passes(t *testing.T, r *RotationReconciler, views ...*karpv1.NodePool) []string {
	t.Helper()
	var all []string
	for _, p := range views {
		var lines []string
		ctx := log.IntoContext(context.Background(), captureLogger(&lines))
		if _, err := r.reconcileNodePool(ctx, p, testPolicy(), mustSchedule(t)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		all = append(all, lines...)
	}
	return all
}

func TestRotationCompletionIsCountedOnceOnAStaleAnchorRead(t *testing.T) {
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-4 * time.Minute)),
		annotations.SurgeWait:           (90 * time.Second).String(),
	}))
	rec := &fakeRecorder{}
	evs := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, rec, pool) // nc-old already finalized away
	r.Events = evs

	// Both views are the pool as it was before the completion write; the second is
	// the stale one the lagging cache still serves after the first pass cleared it.
	fresh, stale := getPool(t, r), getPool(t, r)
	all := passes(t, r, fresh, stale)

	if rec.success != 1 {
		t.Errorf("success counted %d times across a stale-anchor re-entry, want exactly 1", rec.success)
	}
	if got := countDurations(rec, PhaseDrain); got != 1 {
		t.Errorf("drain observed %d times, want exactly 1", got)
	}
	if got := countLines(all, "rotation complete"); got != 1 {
		t.Errorf(`"rotation complete" logged %d times, want exactly 1`, got)
	}
	if e := drain(evs); len(e) != 1 {
		t.Errorf("want exactly 1 RotationCompleted Event, got %d: %v", len(e), e)
	}
}

// The test above enters stale and finds the truth on the inner read. The other
// half of the compare-and-swap is the inner read that is ITSELF stale: its Update
// then carries a resourceVersion older than the release it did not see, so the
// API server rejects it and RetryOnConflict re-runs the mutation against a newer
// read. This drives exactly that — the first anchor-releasing Update is answered
// with a Conflict after another pass has completed the rotation underneath — and
// pins the two properties the retry depends on: the closure's per-attempt state
// is reset (it must not carry `released` over from the losing attempt) and a lost
// race emits nothing at all.
func TestCompletionEmitsNothingWhenTheAnchorIsReleasedUnderneathIt(t *testing.T) {
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-4 * time.Minute)),
	}))
	rec := &fakeRecorder{}
	evs := events.NewFakeRecorder(16)

	// The interceptor plays the other pass: on the first anchor-releasing Update it
	// completes the rotation itself, then answers with the Conflict the API server
	// would return for the now-stale resourceVersion.
	first := true
	funcs := interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			p, ok := obj.(*karpv1.NodePool)
			if !ok || !first || p.Annotations[annotations.ActiveRotation] != "" {
				return c.Update(ctx, obj, opts...)
			}
			first = false
			var won karpv1.NodePool
			if err := c.Get(ctx, client.ObjectKeyFromObject(p), &won); err != nil {
				return err
			}
			won.Annotations[annotations.LastRotationAt] = rfc(testNow)
			clearRotationAnchorFields(won.Annotations)
			if err := c.Update(ctx, &won); err != nil {
				return err
			}
			return apierrors.NewConflict(
				schema.GroupResource{Group: karpapis.Group, Resource: "nodepools"}, p.Name,
				errors.New("simulated stale resourceVersion"))
		},
	}
	r := newFlakyReconciler(t, rec, funcs, pool) // nc-old already finalized away
	r.Events = evs

	all := passes(t, r, getPool(t, r))

	if rec.success != 0 {
		t.Errorf("the pass that lost the anchor counted %d successes, want 0", rec.success)
	}
	if rec.expired != 0 {
		t.Errorf("the pass that lost the anchor counted %d expiries, want 0", rec.expired)
	}
	if got := countDurations(rec, PhaseDrain); got != 0 {
		t.Errorf("drain observed %d times by the losing pass, want 0", got)
	}
	if got := countLines(all, "rotation complete"); got != 0 {
		t.Errorf(`"rotation complete" logged %d times by the losing pass, want 0`, got)
	}
	if e := drain(evs); len(e) != 0 {
		t.Errorf("the losing pass emitted %d Events, want none: %v", len(e), e)
	}
	// The winner's completion stands untouched.
	if got := getPool(t, r).Annotations; got[annotations.LastRotationAt] != rfc(testNow) ||
		got[annotations.ActiveRotation] != "" {
		t.Errorf("the winning pass's completion must survive the retry, got %v", got)
	}
}

// The force-expiry branch of the same completion point counts once for the same
// reason: nothing rotated, and a stale re-read must not report a second abort.
func TestForceExpiryIsCountedOnceOnAStaleAnchorRead(t *testing.T) {
	// No active-rotation-state mirror: the claim vanished out of pending.
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool) // nc-old already finalized away

	fresh, stale := getPool(t, r), getPool(t, r)
	passes(t, r, fresh, stale)

	if rec.expired != 1 {
		t.Errorf("expired counted %d times across a stale-anchor re-entry, want exactly 1", rec.expired)
	}
	if rec.success != 0 {
		t.Errorf("a force-expiry must never count a success; got %d", rec.success)
	}
}

// A completed rotation still records its cooldown anchor exactly where the
// pre-#304 code did: last-rotation-at is written by the completing pass and the
// rest of the anchor is cleared with it.
func TestStaleAnchorReEntryLeavesTheCompletedPoolState(t *testing.T) {
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-4 * time.Minute)),
	}))
	r := newReconciler(t, testNow, nil, pool)

	fresh, stale := getPool(t, r), getPool(t, r)
	passes(t, r, fresh, stale)

	got := getPool(t, r).Annotations
	if got[annotations.LastRotationAt] != rfc(testNow) {
		t.Errorf("last-rotation-at = %q, want %q", got[annotations.LastRotationAt], rfc(testNow))
	}
	for _, k := range []string{
		annotations.ActiveRotation, annotations.ActiveRotationState,
		annotations.DrainingAt, annotations.SurgeWait, annotations.RotationMode,
	} {
		if v, ok := got[k]; ok {
			t.Errorf("%s must be cleared at completion, got %q", k, v)
		}
	}
}
