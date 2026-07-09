package controller

import (
	"context"
	"testing"
	"time"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
)

// The surge-less fallback triggers on a STRICT inequality, deadline − now < t_rot
// (spec §3.3). With testPolicy's default readyTimeout (15m) and withTGP's
// terminationGracePeriod (30m) plus schedule.Buffer (15m), t_rot is exactly 1h
// across this file, so a candidate's gap can be positioned on either side of the
// boundary — and on it.
const (
	ffTRot        = time.Hour
	ffClaimExpire = 14 * 24 * time.Hour
)

// ffEnabledPolicy is testPolicy with the opt-in surge-less fallback turned on.
func ffEnabledPolicy() *policy.Policy {
	p := testPolicy()
	p.Surge.ForcefulFallback.Enabled = true
	return p
}

// ffResolved is a resolved whose t_rot is readyTimeout + drainBound = 1h, matching
// what resolve() derives for testPolicy over a withTGP NodePool.
func ffResolved(pol *policy.Policy) resolved {
	return resolved{pol: pol, readyTimeout: 15 * time.Minute, drainBound: 45 * time.Minute}
}

// ffClaim builds a candidate whose Forceful Expiration deadline sits gap after
// testNow (deadline = creationTimestamp + expireAfter). A negative gap puts the
// deadline in the past.
func ffClaim(gap time.Duration, opts ...ncOpt) *karpv1.NodeClaim {
	return testClaim("nc-old", ffClaimExpire-gap, opts...)
}

// ffNeverClaim builds a candidate with expireAfter: Never — no deadline at all.
// Karpenter never force-expires it, so the surge-less fallback must never fire for
// it. Auto mode cannot select such a claim (it has no deadline to work back from),
// but an explicit ageThreshold override can, so the decision function must handle
// it rather than assume a deadline exists.
func ffNeverClaim(age time.Duration, opts ...ncOpt) *karpv1.NodeClaim {
	c := testClaim("nc-old", age, opts...)
	c.Spec.ExpireAfter = karpv1.NillableDuration{}
	return c
}

// TestSurgelessFallbackThreshold pins the decision function on both sides of the
// boundary, on the boundary itself, and for the deadline-less candidate.
func TestSurgelessFallbackThreshold(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		cand    *karpv1.NodeClaim
		want    bool
	}{{
		// The fallback is opt-in: a candidate that would otherwise qualify surges.
		name:    "disabled policy never falls back",
		enabled: false,
		cand:    ffClaim(ffTRot - time.Second),
		want:    false,
	}, {
		name:    "gap strictly below t_rot falls back",
		enabled: true,
		cand:    ffClaim(ffTRot - time.Second),
		want:    true,
	}, {
		// deadline − now == t_rot: a graceful surge started now finishes exactly at
		// the deadline, so it still wins the race. The inequality is strict.
		name:    "gap exactly t_rot keeps surging",
		enabled: true,
		cand:    ffClaim(ffTRot),
		want:    false,
	}, {
		name:    "gap just above t_rot keeps surging",
		enabled: true,
		cand:    ffClaim(ffTRot + time.Second),
		want:    false,
	}, {
		name:    "far deadline keeps surging",
		enabled: true,
		cand:    ffClaim(7 * 24 * time.Hour),
		want:    false,
	}, {
		// Already past its deadline: Karpenter is force-expiring it or is about to,
		// so a surge cannot possibly win.
		name:    "deadline already passed falls back",
		enabled: true,
		cand:    ffClaim(-time.Hour),
		want:    true,
	}, {
		name:    "expireAfter Never never falls back",
		enabled: true,
		cand:    ffNeverClaim(20 * 24 * time.Hour),
		want:    false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pol := testPolicy()
			if tc.enabled {
				pol = ffEnabledPolicy()
			}
			r := newReconciler(t, testNow, nil)
			if got := r.surgelessFallback(tc.cand, ffResolved(pol), testNow); got != tc.want {
				t.Errorf("surgelessFallback = %v, want %v", got, tc.want)
			}
		})
	}
}

// ffStep drives one reconcile with an explicit policy (step() hardcodes testPolicy,
// whose fallback is off).
func ffStep(t *testing.T, r *RotationReconciler, pool *karpv1.NodePool, pol *policy.Policy) {
	t.Helper()
	if _, err := r.reconcileNodePool(context.Background(), pool, pol, mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
}

// ffAssertSurged asserts the reconcile took the default make-before-break path:
// a placeholder was created, the anchor carries no rotation-mode, the candidate
// still exists, and the fallback counter never moved.
func ffAssertSurged(t *testing.T, r *RotationReconciler, rec *fakeRecorder) {
	t.Helper()
	if !placeholderExists(t, r) {
		t.Error("the surge placeholder must be created")
	}
	if got := getPool(t, r).Annotations[annotations.RotationMode]; got != "" {
		t.Errorf("rotation-mode must be absent on the surge path; got %q", got)
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.DeletionTimestamp != nil {
		t.Error("the candidate must not be deleted on the surge path")
	}
	if rec.forcefulFallback != 0 {
		t.Errorf("forceful_fallback_total: got %d, want 0", rec.forcefulFallback)
	}
}

// TestReconcileExactThresholdKeepsSurging is the reconcile-level counterpart of the
// "gap exactly t_rot" row: an enabled policy whose candidate sits exactly on the
// boundary must still surge, not delete the node out from under the workload.
func TestReconcileExactThresholdKeepsSurging(t *testing.T) {
	rec := &fakeRecorder{}
	cand := ffClaim(ffTRot, ncNode(candNode), ncFinalizer())
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, rec, pool, cand, testK8sNode(candNode, true, nil, false))

	ffStep(t, r, pool, ffEnabledPolicy())

	ffAssertSurged(t, r, rec)
}

// TestReconcileFarDeadlineKeepsSurging: enabling the fallback must not turn every
// rotation surge-less. A candidate eligible on age but far from its deadline
// surges as usual.
func TestReconcileFarDeadlineKeepsSurging(t *testing.T) {
	rec := &fakeRecorder{}
	cand := ffClaim(24*time.Hour, ncNode(candNode), ncFinalizer())
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, rec, pool, cand, testK8sNode(candNode, true, nil, false))

	ffStep(t, r, pool, ffEnabledPolicy())

	ffAssertSurged(t, r, rec)
}

// TestReconcileNeverExpireKeepsSurging: expireAfter: Never is unreachable in auto
// mode (no deadline to derive a trigger from) but selectable under an explicit
// ageThreshold. Such a candidate has no deadline to lose a race to, so it surges.
func TestReconcileNeverExpireKeepsSurging(t *testing.T) {
	rec := &fakeRecorder{}
	pol := ffEnabledPolicy()
	pol.AgeThreshold = "120h" // override mode: select on age alone
	cand := ffNeverClaim(20*24*time.Hour, ncNode(candNode), ncFinalizer())
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, rec, pool, cand, testK8sNode(candNode, true, nil, false))

	ffStep(t, r, pool, pol)

	ffAssertSurged(t, r, rec)
}

// TestReconcileBelowThresholdFallsBack is the near side of the boundary: one second
// inside t_rot, the same fixture rotates surge-less — no placeholder, the anchor
// records the mode, the candidate is deleted, and the counter moves. Together with
// TestReconcileExactThresholdKeepsSurging this pins the strict inequality from both
// sides at the reconcile layer.
func TestReconcileBelowThresholdFallsBack(t *testing.T) {
	rec := &fakeRecorder{}
	cand := ffClaim(ffTRot-time.Second, ncNode(candNode), ncFinalizer())
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, rec, pool, cand, testK8sNode(candNode, true, nil, false))

	ffStep(t, r, pool, ffEnabledPolicy())

	p := getPool(t, r)
	if got := p.Annotations[annotations.RotationMode]; got != annotations.RotationModeForcefulFallback {
		t.Errorf("rotation-mode: got %q, want %q", got, annotations.RotationModeForcefulFallback)
	}
	if got := p.Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Errorf("active-rotation-state: got %q, want draining", got)
	}
	if placeholderExists(t, r) {
		t.Error("the surge-less path must not create a placeholder")
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.DeletionTimestamp == nil {
		t.Error("the candidate must be deleted on the surge-less path")
	}
	if rec.forcefulFallback != 1 {
		t.Errorf("forceful_fallback_total: got %d, want 1", rec.forcefulFallback)
	}
}
