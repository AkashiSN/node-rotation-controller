package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/adapt"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// clampPolicy is the field-report shape from issue #303: a 90-minute window
// (05:00-06:30 UTC) and a 30-minute base backoff.
func clampPolicy() *policy.Policy {
	p := testPolicy()
	p.MaintenanceWindows = []policy.MaintenanceWindow{{
		Timezone: "UTC",
		Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
		Start:    "05:00",
		End:      "06:30",
	}}
	p.Surge.RetryBackoff = &metav1.Duration{Duration: 30 * time.Minute} // *metav1.Duration, not a value
	return p
}

// A claim that failed at 05:40 with retry-count 2 escalates to 60m, which lands
// at 06:40 -- past the 06:30 close. Under the clamp it falls to the 30m base and
// is selectable again at 06:10, inside the window that would otherwise have been
// spent waiting (issue #320).
func TestSelInputsClampsARetryThatWouldOutliveTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 4, 6, 15, 0, 0, time.UTC) // in window, 35m after the failure
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	failed := testClaim("nc-failed", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(time.Date(2026, 9, 4, 5, 40, 0, 0, time.UTC)),
		annotations.RetryCount, "2"))

	r := newReconciler(t, now, nil, pool, failed)
	sched := mustScheduleFor(t, clampPolicy())
	res := r.resolve(pool, clampPolicy(), sched)
	in := r.selInputs(res, now, nil)

	if in.WindowStart.IsZero() || in.WindowEnd.IsZero() {
		t.Fatalf("selInputs must carry the occurrence bounds; got [%v, %v)", in.WindowStart, in.WindowEnd)
	}
	if want := time.Date(2026, 9, 4, 6, 30, 0, 0, time.UTC); !in.WindowEnd.Equal(want) {
		t.Errorf("WindowEnd: got %s, want %s", in.WindowEnd, want)
	}
	if n := countEligibleClaims(t, r, pool, res, now); n != 1 {
		t.Errorf("eligible: got %d, want 1 — the clamped 30m backoff elapsed at 06:10", n)
	}
}

// The fresh-selection consumer at its exact boundary, mirroring the anchored
// consumer's just-before / exactly-at pair. A single mid-window instant would pass
// against several wrong backoff values; only the boundary pins the clamp itself.
func TestFreshSelectionReopensAtTheClampedInstant(t *testing.T) {
	failedAt := time.Date(2026, 9, 4, 5, 40, 0, 0, time.UTC)
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	failed := testClaim("nc-failed", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(failedAt),
		annotations.RetryCount, "2"))

	for name, tc := range map[string]struct {
		now  time.Time
		want int
	}{
		"just before the clamped backoff elapses": {failedAt.Add(29 * time.Minute), 0},
		"exactly at it": {failedAt.Add(30 * time.Minute), 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReconciler(t, tc.now, nil, pool.DeepCopy(), failed.DeepCopy())
			res := r.resolve(pool, clampPolicy(), mustScheduleFor(t, clampPolicy()))
			if n := countEligibleClaims(t, r, pool, res, tc.now); n != tc.want {
				t.Errorf("eligible at %s: got %d, want %d", tc.now.Format("15:04"), n, tc.want)
			}
		})
	}
}

// Out of window there is no occurrence, so no clamp: the same claim stays on its
// escalated 60m backoff and is not selectable until 06:40.
func TestSelInputsDoesNotClampOutOfWindow(t *testing.T) {
	now := time.Date(2026, 9, 4, 6, 35, 0, 0, time.UTC) // past the close, before 06:40
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	failed := testClaim("nc-failed", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(time.Date(2026, 9, 4, 5, 40, 0, 0, time.UTC)),
		annotations.RetryCount, "2"))

	r := newReconciler(t, now, nil, pool, failed)
	sched := mustScheduleFor(t, clampPolicy())
	res := r.resolve(pool, clampPolicy(), sched)
	in := r.selInputs(res, now, nil)

	if !in.WindowStart.IsZero() || !in.WindowEnd.IsZero() {
		t.Errorf("out of window the bounds must be zero; got [%v, %v)", in.WindowStart, in.WindowEnd)
	}
	if n := countEligibleClaims(t, r, pool, res, now); n != 0 {
		t.Errorf("eligible: got %d, want 0 — the escalated 60m backoff runs to 06:40", n)
	}
}

// A claim that failed in a PREVIOUS occurrence must not be clamped by this one.
// It failed at 05:40 the day before with retry-count 4 (240m, due 09:40 that day),
// so by the next window it is past its escalated backoff and eligible for that
// reason -- not because the clamp fired. The guard is pinned in
// selection.TestEffectiveBackoff; this test pins that the controller passes the
// bounds that make the guard reachable.
func TestSelInputsBoundsBelongToTheCurrentOccurrence(t *testing.T) {
	now := time.Date(2026, 9, 5, 5, 10, 0, 0, time.UTC)
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	r := newReconciler(t, now, nil, pool)
	sched := mustScheduleFor(t, clampPolicy())
	res := r.resolve(pool, clampPolicy(), sched)
	in := r.selInputs(res, now, nil)

	wantStart := time.Date(2026, 9, 5, 5, 0, 0, 0, time.UTC)
	if !in.WindowStart.Equal(wantStart) {
		t.Errorf("WindowStart: got %s, want %s — the bounds must be today's occurrence, not the failure's", in.WindowStart, wantStart)
	}
}

// mustScheduleFor builds a window.Schedule from a policy, failing the test on a
// malformed one. testPolicy's own schedule comes from mustSchedule; this variant
// takes the 90-minute clamp policy.
func mustScheduleFor(t *testing.T, p *policy.Policy) *window.Schedule {
	t.Helper()
	s, err := window.New(p.MaintenanceWindows)
	if err != nil {
		t.Fatalf("window.New: %v", err)
	}
	return s
}

// countEligibleClaims lists the pool's claims and counts those the selection
// predicate accepts, going through the same selInputs the reconcile loop uses.
func countEligibleClaims(t *testing.T, r *RotationReconciler, pool *karpv1.NodePool, res resolved, now time.Time) int {
	t.Helper()
	claims, err := r.poolClaims(context.Background(), pool)
	if err != nil {
		t.Fatalf("poolClaims: %v", err)
	}
	views, _ := adapt.Claims(claims)
	return selection.CountEligible(views, r.selInputs(res, now, nil))
}

// getClaim fetches a NodeClaim by name, failing the test if it is not found.
func getClaim(t *testing.T, r *RotationReconciler, name string) *karpv1.NodeClaim {
	t.Helper()
	var c karpv1.NodeClaim
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &c); err != nil {
		t.Fatalf("get claim %s: %v", name, err)
	}
	return &c
}

// newReconcilerWithClock is newReconciler with a caller-supplied clock AND an
// Event recorder. newReconciler sets neither: it fixes the clock to one instant
// and leaves Events nil, and failPending only raises the Event when Events != nil
// — so without this the announcement assertion below would never run and the
// negative controls would pin nothing.
func newReconcilerWithClock(t *testing.T, clock func() time.Time, rec *fakeRecorder, objs ...client.Object) *RotationReconciler {
	t.Helper()
	r := newReconciler(t, time.Time{}, rec, objs...)
	r.Clock = clock
	r.Events = events.NewFakeRecorder(16)
	return r
}

// lastBackoffUntil parses the next-attempt instant out of the RotationFailed
// Event the emitter raised, which carries the same value as the log's
// backoffUntil field.
func lastBackoffUntil(t *testing.T, r *RotationReconciler) time.Time {
	t.Helper()
	rec, ok := r.Events.(*events.FakeRecorder)
	if !ok {
		t.Fatal("the reconciler must be built with a FakeRecorder to read the Event")
	}
	select {
	case ev := <-rec.Events:
		// Step 3a's wording is "... next attempt <RFC3339> under the current
		// schedule ..." — the timestamp is bounded by the next space, not by the
		// end of the string, so only the token is parsed, not the trailing clause.
		const marker = "next attempt "
		idx := strings.LastIndex(ev, marker)
		if idx < 0 {
			t.Fatalf("the RotationFailed Event must name the next attempt instant; got %q", ev)
		}
		rest := ev[idx+len(marker):]
		end := strings.IndexByte(rest, ' ')
		if end < 0 {
			end = len(rest)
		}
		ts, err := time.Parse(time.RFC3339, rest[:end])
		if err != nil {
			t.Fatalf("unparseable next-attempt instant in %q: %v", ev, err)
		}
		return ts
	default:
		t.Fatal("no RotationFailed Event was raised")
		return time.Time{}
	}
}

// The failure announcement must report the instant the claim actually reopens:
// the clamped backoff, measured from the failed-at this write persisted.
//
// The clock advances between the mutator and the report AND crosses an RFC3339
// second boundary, which is what catches a second r.now(): RFC3339 drops
// sub-second precision, so a report derived from a later clock reading disagrees
// with the value failedPastBackoff parses.
func TestFailureReportsTheClampedInstantFromTheDurableFailedAt(t *testing.T) {
	base := time.Date(2026, 9, 4, 5, 40, 0, 900000000, time.UTC) // .9s
	tick := 0
	clock := func() time.Time {
		t := base.Add(time.Duration(tick) * 200 * time.Millisecond) // crosses :41 on the 1st tick
		tick++
		return t
	}
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.RetryCount, "1"))
	r := newReconcilerWithClock(t, clock, nil, pool, cand, testK8sNode(candNode, true, nil, false))
	res := r.resolve(pool, clampPolicy(), mustScheduleFor(t, clampPolicy()))

	if _, err := r.failPending(context.Background(), pool, res, cand); err != nil {
		t.Fatalf("failPending: %v", err)
	}

	// retry becomes 2 => escalated 60m from 05:40 lands at 06:40, past the 06:30
	// close, so the clamp falls to the 30m base: reopen at 06:10.
	got := getClaim(t, r, "nc-old")
	failedAt, ok := parseTime(got.Annotations[annotations.FailedAt])
	if !ok {
		t.Fatalf("unparseable failed-at %q", got.Annotations[annotations.FailedAt])
	}
	want := failedAt.Add(30 * time.Minute)
	if reported := lastBackoffUntil(t, r); !reported.Equal(want) {
		t.Errorf("reported next attempt %s, want %s (the clamped backoff off the persisted failed-at)", reported, want)
	}
}

// advanceFailed's re-entry gate is a SEPARATE consumer: the anchor sends the
// reconcile into advance() before the fresh-start path is reached. Driving only
// the fresh path would pass while this one is still on the unclamped value.
func TestAnchoredRetryReentersOnTheClampedBackoff(t *testing.T) {
	failedAt := time.Date(2026, 9, 4, 5, 40, 0, 0, time.UTC)
	pool := withExpireAfter(withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateFailed,
	})))
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(failedAt),
		annotations.RetryCount, "2"))

	for name, tc := range map[string]struct {
		now       time.Time
		wantState string
	}{
		// One minute before the clamped 30m backoff elapses.
		"just before reopening": {failedAt.Add(29 * time.Minute), annotations.StateFailed},
		// Exactly at it. Without the clamp the gate would wait until 06:40.
		"exactly at reopening": {failedAt.Add(30 * time.Minute), annotations.StatePending},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReconciler(t, tc.now, nil, pool.DeepCopy(), cand.DeepCopy(),
				testK8sNode(candNode, true, nil, false))
			res := r.resolve(pool, clampPolicy(), mustScheduleFor(t, clampPolicy()))
			if _, err := r.advanceFailed(context.Background(), pool.DeepCopy(), res, getClaim(t, r, "nc-old")); err != nil {
				t.Fatalf("advanceFailed: %v", err)
			}
			if got := getClaim(t, r, "nc-old").Annotations[annotations.State]; got != tc.wantState {
				t.Errorf("state: got %q, want %q", got, tc.wantState)
			}
		})
	}
}
