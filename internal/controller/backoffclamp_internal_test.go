package controller

import (
	"context"
	"fmt"
	"strconv"
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
	"github.com/AkashiSN/node-rotation-controller/internal/decide"
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

// runWindow replays one maintenance-window occurrence minute by minute and
// returns the instants at which a rotation would start. Every decision is
// production code; the test only applies the state writes failPending makes when
// an attempt times out.
//
// clamped=false zeroes the occurrence bounds, reproducing the behaviour before
// issue #320 so the two can be compared in one test.
func runWindow(t *testing.T, pol *policy.Policy, claimCount int, from, to time.Time, readyTimeout time.Duration, clamped bool) []time.Time {
	t.Helper()
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	objs := []client.Object{pool}
	for i := range claimCount {
		objs = append(objs, testClaim(fmt.Sprintf("nc-%d", i), time.Duration(20+i)*24*time.Hour))
	}
	r := newReconciler(t, from, nil, objs...)
	res := r.resolve(pool, pol, mustScheduleFor(t, pol))

	var starts []time.Time
	var inFlight string
	var startedAt time.Time
	for now := from; now.Before(to); now = now.Add(time.Minute) {
		if inFlight != "" {
			if !now.Before(startedAt.Add(readyTimeout)) {
				failClaimInPlace(t, r, pool, inFlight, now) // what failPending writes
				inFlight = ""
			}
			continue
		}
		if open, _ := decide.StartGate(r.gateInputs(pool, res, now)); !open {
			continue
		}
		in := r.selInputs(res, now, nil)
		if !clamped {
			in.WindowStart, in.WindowEnd = time.Time{}, time.Time{}
		}
		claims, err := r.poolClaims(context.Background(), pool)
		if err != nil {
			t.Fatalf("poolClaims: %v", err)
		}
		views, _ := adapt.Claims(claims)
		pick := selection.PickEarliestDeadlineEligible(views, in)
		if pick == nil {
			continue
		}
		starts = append(starts, now)
		inFlight, startedAt = pick.Name, now
	}
	return starts
}

// failClaimInPlace applies the durable state failPending writes when an attempt
// times out: the claim goes failed with this instant as its failed-at and its
// retry-count incremented, and the pool records the failure for gate B. It makes
// no decision — every decision in runWindow is production code.
func failClaimInPlace(t *testing.T, r *RotationReconciler, pool *karpv1.NodePool, name string, now time.Time) {
	t.Helper()
	c := getClaim(t, r, name)
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[annotations.State] = annotations.StateFailed
	c.Annotations[annotations.FailedAt] = rfc(now)
	c.Annotations[annotations.RetryCount] = strconv.Itoa(parseInt(c.Annotations[annotations.RetryCount]) + 1)
	if err := r.Update(context.Background(), c); err != nil {
		t.Fatalf("update %s: %v", name, err)
	}
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[annotations.LastFailureAt] = rfc(now)
}

// The failure MODE from the field report on issue #303: a 90-minute window, base
// 30m, two claims, every attempt timing out, and a window that stops starting
// rotations well before it closes.
//
// This reproduces the shape and the count, not the incident's exact timestamps —
// StartGate opens at equality with failurePause and this replay is on a one-minute
// grid, so the starts land near but not on the reported 05:00/05:15/05:35/05:51.
// The unclamped run producing exactly four is what makes the clamped run's number
// mean something: it anchors the harness to the incident's outcome before the
// harness is used to measure the fix.
func TestClampKeepsTheWindowWorkingAfterTheFourthFailure(t *testing.T) {
	pol := clampPolicy()
	from := time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 4, 6, 30, 0, 0, time.UTC)

	before := runWindow(t, pol, 2, from, to, 5*time.Minute, false)
	after := runWindow(t, pol, 2, from, to, 5*time.Minute, true)

	if len(before) != 4 {
		t.Fatalf("harness does not reproduce the field report: got %d starts %v, want 4", len(before), before)
	}
	if len(after) <= len(before) {
		t.Errorf("clamped run made %d starts, want more than the unclamped %d", len(after), len(before))
	}
	// The idle tail is the point: unclamped, the window stops starting work with a
	// long stretch still open.
	if !after[len(after)-1].After(before[len(before)-1]) {
		t.Errorf("last start: clamped %s, unclamped %s — the clamp must push work later into the window",
			after[len(after)-1].Format("15:04"), before[len(before)-1].Format("15:04"))
	}
}

// The accepted cost, measured rather than asserted in prose. retryBackoff does not
// bound the pool-wide attempt rate: it is per claim, and failurePause has a
// 10-minute floor only as its DEFAULT. With a long window and a small explicit
// failurePause the ceiling is readyTimeout + failurePause per start, which is what
// the runbook tells operators to reach for.
func TestClampCostIsBoundedByReadyTimeoutPlusFailurePause(t *testing.T) {
	pol := clampPolicy()
	pol.MaintenanceWindows[0].End = "17:00" // a 12-hour window
	pol.Surge.FailurePause = &metav1.Duration{Duration: time.Minute}
	from := time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC)
	readyTimeout := 5 * time.Minute

	starts := runWindow(t, pol, 2, from, to, readyTimeout, true)

	ceiling := 1 + int(to.Sub(from)/(readyTimeout+time.Minute))
	if len(starts) > ceiling {
		t.Errorf("made %d starts, above the readyTimeout+failurePause ceiling of %d", len(starts), ceiling)
	}
	// And it really is many more than a short failurePause would suggest is free —
	// this number is the cost the spec and runbook disclose.
	if len(starts) < 10 {
		t.Errorf("made only %d starts in a 12-hour window with a 1-minute failurePause; the cost discussion assumes many", len(starts))
	}
}

// The design accepts that "the same occurrence" is resolved under the schedule as
// it stands on the current read, not as it stood when the claim failed. An edit
// that bridges two occurrences therefore lets a failure from the earlier one be
// clamped by the merged run. Pin both directions so the behaviour is chosen rather
// than discovered.
func TestScheduleEditRedefinesTheOccurrenceIdentity(t *testing.T) {
	failedAt := time.Date(2026, 9, 4, 5, 40, 0, 0, time.UTC)
	// Inside the SECOND occurrence, and AFTER 07:40. That instant matters: under the
	// merged schedule the 240m step does not fit [05:00, 08:00) but the 120m one
	// does, landing at 07:40 — so at 07:30 the bridged case would still be
	// ineligible and this test would assert the wrong thing about a correct clamp.
	now := time.Date(2026, 9, 4, 7, 45, 0, 0, time.UTC)
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	failed := testClaim("nc-failed", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(failedAt),
		annotations.RetryCount, "4")) // 8x = 240m: due 09:40, past both occurrences

	split := clampPolicy() // 05:00-06:30
	split.MaintenanceWindows = append(split.MaintenanceWindows, policy.MaintenanceWindow{
		Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
		Start: "07:00", End: "08:00",
	})
	merged := clampPolicy()
	merged.MaintenanceWindows[0].End = "08:00" // the bridging edit: one 05:00-08:00 run

	for name, tc := range map[string]struct {
		pol  *policy.Policy
		want int
	}{
		// Two occurrences: the failure is in the earlier one, so the guard refuses
		// to clamp and the claim stays on its 240m backoff until 09:40.
		"separate occurrences": {split, 0},
		// One merged run containing both the failure and now: the clamp applies, the
		// ladder falls to the 120m step (05:40 + 120m = 07:40, inside [05:00, 08:00)),
		// and the claim is selectable again from 07:40.
		"bridged into one occurrence": {merged, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReconciler(t, now, nil, pool.DeepCopy(), failed.DeepCopy())
			res := r.resolve(pool, tc.pol, mustScheduleFor(t, tc.pol))
			if n := countEligibleClaims(t, r, pool, res, now); n != tc.want {
				t.Errorf("eligible: got %d, want %d", n, tc.want)
			}
		})
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
