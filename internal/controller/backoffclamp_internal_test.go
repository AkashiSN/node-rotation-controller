package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
