package selection_test

import (
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// now is the fixed reference instant for every age-based test.
var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func pool(name string, labels map[string]string) karpv1.NodePool {
	return karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func names(pools []karpv1.NodePool) []string {
	out := make([]string, len(pools))
	for i, p := range pools {
		out[i] = p.Name
	}
	return out
}

func TestInScopeNodePoolsMatchesAllLabelsWithinSelector(t *testing.T) {
	pools := []karpv1.NodePool{
		pool("a", map[string]string{"workload": "api", "team": "core"}),
		pool("b", map[string]string{"workload": "api"}), // missing team → no match
	}
	sel := []policy.Selector{{MatchLabels: map[string]string{"workload": "api", "team": "core"}}}

	got := names(selection.InScopeNodePools(pools, sel))
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("want [a], got %v", got)
	}
}

func TestInScopeNodePoolsOrsAcrossSelectors(t *testing.T) {
	pools := []karpv1.NodePool{
		pool("a", map[string]string{"workload": "api"}),
		pool("b", map[string]string{"workload": "batch"}),
		pool("c", map[string]string{"workload": "system"}),
	}
	sel := []policy.Selector{
		{MatchLabels: map[string]string{"workload": "api"}},
		{MatchLabels: map[string]string{"workload": "batch"}},
	}

	got := names(selection.InScopeNodePools(pools, sel))
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("want [a b], got %v", got)
	}
}

func TestInScopeNodePoolsEmptyWhenNothingMatches(t *testing.T) {
	pools := []karpv1.NodePool{pool("a", map[string]string{"workload": "api"})}
	sel := []policy.Selector{{MatchLabels: map[string]string{"workload": "batch"}}}

	if got := selection.InScopeNodePools(pools, sel); len(got) != 0 {
		t.Fatalf("want none, got %v", names(got))
	}
}

// claimOpt mutates a NodeClaim during construction.
type claimOpt func(*karpv1.NodeClaim)

func ready(v bool) claimOpt {
	s := metav1.ConditionTrue
	if !v {
		s = metav1.ConditionFalse
	}
	return func(c *karpv1.NodeClaim) {
		c.Status.Conditions = []status.Condition{{Type: status.ConditionReady, Status: s}}
	}
}

func noReadyCondition() claimOpt {
	return func(c *karpv1.NodeClaim) { c.Status.Conditions = nil }
}

func unknownReady() claimOpt {
	return func(c *karpv1.NodeClaim) {
		c.Status.Conditions = []status.Condition{{Type: status.ConditionReady, Status: metav1.ConditionUnknown}}
	}
}

func expireAfter(d time.Duration) claimOpt {
	return func(c *karpv1.NodeClaim) { c.Spec.ExpireAfter = karpv1.NillableDuration{Duration: &d} }
}

func neverExpire() claimOpt {
	return func(c *karpv1.NodeClaim) { c.Spec.ExpireAfter = karpv1.NillableDuration{Duration: nil} }
}

func deleting() claimOpt {
	return func(c *karpv1.NodeClaim) {
		t := metav1.NewTime(now)
		c.DeletionTimestamp = &t
		c.Finalizers = []string{"karpenter.sh/termination"}
	}
}

func ann(kv ...string) claimOpt {
	return func(c *karpv1.NodeClaim) {
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		for i := 0; i+1 < len(kv); i += 2 {
			c.Annotations[kv[i]] = kv[i+1]
		}
	}
}

// claim builds a NodeClaim of the given age. Defaults: Ready=true, expireAfter
// 14d, no state annotation — i.e. an eligible fresh candidate under baseInputs.
func claim(name string, age time.Duration, opts ...claimOpt) karpv1.NodeClaim {
	c := karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(now.Add(-age)),
		},
	}
	expireAfter(14 * day)(&c)
	ready(true)(&c)
	for _, o := range opts {
		o(&c)
	}
	return c
}

// baseInputs: auto mode, leadTime 8d ⇒ trigger age threshold = 14d − 8d = 6d.
func baseInputs() selection.Inputs {
	return selection.Inputs{
		Now:          now,
		LeadTime:     8 * day,
		RetryBackoff: 30 * time.Minute,
	}
}

func TestPickOldestEligiblePicksTheOldest(t *testing.T) {
	claims := []karpv1.NodeClaim{
		claim("young-but-eligible", 10*day),
		claim("oldest", 20*day),
		claim("middle", 15*day),
	}
	got := selection.PickOldestEligible(claims, baseInputs())
	if got == nil || got.Name != "oldest" {
		t.Fatalf("want oldest, got %v", got)
	}
}

func TestPickOldestEligibleTieBreaksOnNameDeterministically(t *testing.T) {
	// metav1.Time is second-granular, so batch-provisioned claims share a
	// creationTimestamp; the pick must be the name-least one regardless of list
	// order (else selection drifts across reconciles).
	a := claim("a", 20*day)
	b := claim("b", 20*day)
	c := claim("c", 20*day)
	for _, order := range [][]karpv1.NodeClaim{{a, b, c}, {c, b, a}, {b, c, a}} {
		if got := selection.PickOldestEligible(order, baseInputs()); got == nil || got.Name != "a" {
			t.Fatalf("want a for any order, got %v", got)
		}
	}
}

func TestPickOldestEligibleNilWhenNoneEligible(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("too-young", 1*day)}
	if got := selection.PickOldestEligible(claims, baseInputs()); got != nil {
		t.Fatalf("want nil, got %v", got.Name)
	}
}

func TestPickOldestEligibleAutoTriggerBoundary(t *testing.T) {
	// threshold age = E(14d) − leadTime(8d) = 6d; trigger is age > threshold.
	in := baseInputs()
	justOver := selection.PickOldestEligible([]karpv1.NodeClaim{claim("c", 6*day+time.Hour)}, in)
	if justOver == nil {
		t.Fatal("claim just past the trigger should be eligible")
	}
	justUnder := selection.PickOldestEligible([]karpv1.NodeClaim{claim("c", 6*day-time.Hour)}, in)
	if justUnder != nil {
		t.Fatal("claim just under the trigger should not be eligible")
	}
}

func TestPickOldestEligibleAnchorsOnPerClaimExpireAfter(t *testing.T) {
	// Same age, different expireAfter: the shorter-lived claim triggers, the
	// longer-lived one does not — proving the per-claim anchor (§3.2).
	in := baseInputs()
	short := claim("short", 7*day, expireAfter(14*day)) // threshold 6d → eligible
	long := claim("long", 7*day, expireAfter(30*day))   // threshold 22d → not yet

	if got := selection.PickOldestEligible([]karpv1.NodeClaim{long, short}, in); got == nil || got.Name != "short" {
		t.Fatalf("want short, got %v", got)
	}
}

func TestPickOldestEligibleNeverExpireExcludedInAutoMode(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("immortal", 100*day, neverExpire())}
	if got := selection.PickOldestEligible(claims, baseInputs()); got != nil {
		t.Fatalf("a claim with expireAfter=Never has no deadline; want nil, got %v", got.Name)
	}
}

func TestPickOldestEligibleOverrideUsesAge(t *testing.T) {
	override := 5 * day
	in := baseInputs()
	in.Override = &override

	// expireAfter is large enough that the auto trigger would NOT fire; only the
	// age-based override makes these candidates.
	eligible := selection.PickOldestEligible(
		[]karpv1.NodeClaim{claim("c", 5*day+time.Hour, expireAfter(720*time.Hour))}, in)
	if eligible == nil {
		t.Fatal("claim older than the override should be eligible")
	}
	tooYoung := selection.PickOldestEligible(
		[]karpv1.NodeClaim{claim("c", 4*day, expireAfter(720*time.Hour))}, in)
	if tooYoung != nil {
		t.Fatal("claim younger than the override should not be eligible")
	}
}

func TestPickOldestEligibleExcludesNotReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  claimOpt
	}{
		{"ready-false", ready(false)},
		{"ready-unknown", unknownReady()},
		{"no-condition", noReadyCondition()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := []karpv1.NodeClaim{claim("c", 20*day, tc.opt)}
			if got := selection.PickOldestEligible(claims, baseInputs()); got != nil {
				t.Fatalf("a NotReady claim must be skipped, got %v", got.Name)
			}
		})
	}
}

func TestPickOldestEligibleExcludesDeleting(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("c", 20*day, deleting())}
	if got := selection.PickOldestEligible(claims, baseInputs()); got != nil {
		t.Fatalf("a claim with deletionTimestamp must be skipped, got %v", got.Name)
	}
}

func TestPickOldestEligibleExcludesInFlightAndTerminalStates(t *testing.T) {
	for _, state := range []string{
		annotations.StatePending,
		annotations.StateDraining,
		annotations.StateExpired,
	} {
		t.Run(state, func(t *testing.T) {
			claims := []karpv1.NodeClaim{claim("c", 20*day, ann(annotations.State, state))}
			if got := selection.PickOldestEligible(claims, baseInputs()); got != nil {
				t.Fatalf("state %q must not be re-selected, got %v", state, got.Name)
			}
		})
	}
}

func TestPickOldestEligibleFailedRespectsBackoff(t *testing.T) {
	in := baseInputs() // RetryBackoff 30m
	// retry-count 1 ⇒ backoff = 30m·2^0 = 30m.
	withinBackoff := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, now.Add(-20*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickOldestEligible([]karpv1.NodeClaim{withinBackoff}, in); got != nil {
		t.Fatalf("failed claim within backoff must not be re-selected, got %v", got.Name)
	}

	pastBackoff := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, now.Add(-40*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickOldestEligible([]karpv1.NodeClaim{pastBackoff}, in); got == nil {
		t.Fatal("failed claim past its backoff must be re-selectable")
	}
}

func TestPickOldestEligibleFailedBackoffEscalates(t *testing.T) {
	in := baseInputs() // base 30m
	// retry-count 3 ⇒ backoff = 30m·2^2 = 120m. At 90m elapsed it is still held.
	c := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "3",
		annotations.FailedAt, now.Add(-90*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickOldestEligible([]karpv1.NodeClaim{c}, in); got != nil {
		t.Fatalf("escalated backoff (120m) should still hold at 90m, got %v", got.Name)
	}
}

func TestPickOldestEligibleFailedWithoutFailedAtIsReselectable(t *testing.T) {
	// A torn failed write (state=failed but no failed-at, §5.2 crash recovery)
	// must not strand the claim: the backoff is treated as elapsed so the
	// case-failed handler can re-enter it.
	c := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "2",
	))
	if got := selection.PickOldestEligible([]karpv1.NodeClaim{c}, baseInputs()); got == nil {
		t.Fatal("failed claim with no failed-at must remain re-selectable")
	}
}

func TestPickOldestEligibleFailedWithUnparseableRetryCountUsesBase(t *testing.T) {
	// A non-numeric retry-count parses to 0, so the backoff falls back to the
	// base (30m), not a stranded claim. failed-at is 20m ago → within base → held.
	in := baseInputs()
	withinBase := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "garbage",
		annotations.FailedAt, now.Add(-20*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickOldestEligible([]karpv1.NodeClaim{withinBase}, in); got != nil {
		t.Fatalf("unparseable retry-count must fall back to the base backoff (30m), got %v", got.Name)
	}
	pastBase := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "garbage",
		annotations.FailedAt, now.Add(-40*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickOldestEligible([]karpv1.NodeClaim{pastBase}, in); got == nil {
		t.Fatal("past the base backoff the claim must be re-selectable")
	}
}

func TestEscalatedBackoff(t *testing.T) {
	base := 30 * time.Minute
	for _, tc := range []struct {
		retry int
		want  time.Duration
	}{
		{0, base},      // defensive: treated as the base
		{1, base},      // 2^0
		{2, 2 * base},  // 2^1
		{3, 4 * base},  // 2^2
		{4, 8 * base},  // 2^3 (cap)
		{5, 8 * base},  // capped at 8×
		{10, 8 * base}, // capped at 8×
	} {
		if got := selection.EscalatedBackoff(tc.retry, base); got != tc.want {
			t.Errorf("EscalatedBackoff(%d) = %v, want %v", tc.retry, got, tc.want)
		}
	}
}
