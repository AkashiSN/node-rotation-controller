package selection_test

import (
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/adapt"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// views converts the karpv1 fixtures the helpers build into the pure views the
// package now takes. Keeping the fixtures on the CRD type is deliberate: it keeps
// every expectation below unchanged, so this file stays the proof that the view
// refactor is behaviour-preserving, and it exercises internal/adapt on the way.
func views(claims []karpv1.NodeClaim) []selection.Claim {
	v, _ := adapt.Claims(claims)
	return v
}

// now is the fixed reference instant for every age-based test.
var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

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

// tgp stamps the NodeClaim's own terminationGracePeriod — the authoritative
// per-node value the lead time must anchor on (spec §3.2).
func tgp(d time.Duration) claimOpt {
	return func(c *karpv1.NodeClaim) { c.Spec.TerminationGracePeriod = &metav1.Duration{Duration: d} }
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
// Base carries the whole 8d (DrainFallback 0); the test claims set no tGP, so
// LeadTime.For resolves to 8d for each.
func baseInputs() selection.Inputs {
	return selection.Inputs{
		Now:          now,
		LeadTime:     selection.LeadTime{Base: 8 * day},
		RetryBackoff: 30 * time.Minute,
	}
}

func TestPickEarliestDeadlineEligiblePicksTheOldest(t *testing.T) {
	claims := []karpv1.NodeClaim{
		claim("young-but-eligible", 10*day),
		claim("oldest", 20*day),
		claim("middle", 15*day),
	}
	got := selection.PickEarliestDeadlineEligible(views(claims), baseInputs())
	if got == nil || got.Name != "oldest" {
		t.Fatalf("want oldest, got %v", got)
	}
}

func TestPickEarliestDeadlineEligibleTieBreaksOnNameDeterministically(t *testing.T) {
	// metav1.Time is second-granular, so batch-provisioned claims share a
	// creationTimestamp; the pick must be the name-least one regardless of list
	// order (else selection drifts across reconciles).
	a := claim("a", 20*day)
	b := claim("b", 20*day)
	c := claim("c", 20*day)
	for _, order := range [][]karpv1.NodeClaim{{a, b, c}, {c, b, a}, {b, c, a}} {
		if got := selection.PickEarliestDeadlineEligible(views(order), baseInputs()); got == nil || got.Name != "a" {
			t.Fatalf("want a for any order, got %v", got)
		}
	}
}

func TestPickEarliestDeadlineEligibleNilWhenNoneEligible(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("too-young", 1*day)}
	if got := selection.PickEarliestDeadlineEligible(views(claims), baseInputs()); got != nil {
		t.Fatalf("want nil, got %v", got.Name)
	}
}

func TestPickEarliestDeadlineEligibleAutoTriggerBoundary(t *testing.T) {
	// threshold age = E(14d) − leadTime(8d) = 6d; trigger is age > threshold.
	in := baseInputs()
	justOver := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{claim("c", 6*day+time.Hour)}), in)
	if justOver == nil {
		t.Fatal("claim just past the trigger should be eligible")
	}
	justUnder := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{claim("c", 6*day-time.Hour)}), in)
	if justUnder != nil {
		t.Fatal("claim just under the trigger should not be eligible")
	}
}

func TestPickEarliestDeadlineEligibleAnchorsOnPerClaimExpireAfter(t *testing.T) {
	// Same age, different expireAfter: the shorter-lived claim triggers, the
	// longer-lived one does not — proving the per-claim anchor (§3.2).
	in := baseInputs()
	short := claim("short", 7*day, expireAfter(14*day)) // threshold 6d → eligible
	long := claim("long", 7*day, expireAfter(30*day))   // threshold 22d → not yet

	if got := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{long, short}), in); got == nil || got.Name != "short" {
		t.Fatalf("want short, got %v", got)
	}
}

func TestPickEarliestDeadlineUnderHeterogeneousExpireAfter(t *testing.T) {
	// Both eligible, but age order and deadline order disagree because
	// expireAfter is heterogeneous (§3.2, issue #157): the OLDER claim has a
	// LONGER expireAfter, so its forceful-expiration deadline is LATER; the
	// younger claim with the shorter expireAfter races its deadline first and
	// must be rotated first. leadTime = 8d.
	in := baseInputs()
	// older-long : age 20d, E 25d ⇒ deadline now+5d, trigger 20 > 25−8=17 ✓
	olderLong := claim("older-long", 20*day, expireAfter(25*day))
	// younger-short: age 10d, E 12d ⇒ deadline now+2d, trigger 10 > 12−8=4 ✓
	youngerShort := claim("younger-short", 10*day, expireAfter(12*day))

	for _, order := range [][]karpv1.NodeClaim{
		{olderLong, youngerShort},
		{youngerShort, olderLong},
	} {
		got := selection.PickEarliestDeadlineEligible(views(order), in)
		if got == nil || got.Name != "younger-short" {
			t.Fatalf("want younger-short (earliest deadline), got %v", got)
		}
	}
}

func TestPickEarliestDeadlineTieFallsBackToCreationThenName(t *testing.T) {
	// Equal deadlines must fall back to oldest creationTimestamp, then Name, so
	// selection stays deterministic across reconciles (§3.2).
	//
	// creationTimestamp precedes Name: both deadlines are now−6d (creationTimestamp
	// + expireAfter cancels the age/E offset), but the claims differ in
	// creationTimestamp. The older one wins even though its Name sorts LAST —
	// proving the creationTimestamp tiebreak is applied before Name.
	older := claim("z-older", 21*day, expireAfter(15*day))     // deadline now−6d, older by 1d
	younger := claim("a-younger", 20*day, expireAfter(14*day)) // deadline now−6d
	for _, order := range [][]karpv1.NodeClaim{{older, younger}, {younger, older}} {
		if got := selection.PickEarliestDeadlineEligible(views(order), baseInputs()); got == nil || got.Name != "z-older" {
			t.Fatalf("equal deadlines must tiebreak on oldest creationTimestamp (z-older), got %v", got)
		}
	}
	// Same deadline AND same creationTimestamp (same age + expireAfter): Name is
	// the final tiebreak, picking "a".
	a := claim("a", 20*day)
	b := claim("b", 20*day)
	for _, order := range [][]karpv1.NodeClaim{{a, b}, {b, a}} {
		if got := selection.PickEarliestDeadlineEligible(views(order), baseInputs()); got == nil || got.Name != "a" {
			t.Fatalf("equal deadline and creationTimestamp must tiebreak on name to a, got %v", got)
		}
	}
}

func TestPickEarliestDeadlineEligibleNeverExpireExcludedInAutoMode(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("immortal", 100*day, neverExpire())}
	if got := selection.PickEarliestDeadlineEligible(views(claims), baseInputs()); got != nil {
		t.Fatalf("a claim with expireAfter=Never has no deadline; want nil, got %v", got.Name)
	}
}

func TestPickEarliestDeadlineEligibleOverrideUsesAge(t *testing.T) {
	override := 5 * day
	in := baseInputs()
	in.Override = &override

	// expireAfter is large enough that the auto trigger would NOT fire; only the
	// age-based override makes these candidates.
	eligible := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{claim("c", 5*day+time.Hour, expireAfter(720*time.Hour))}), in)
	if eligible == nil {
		t.Fatal("claim older than the override should be eligible")
	}
	tooYoung := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{claim("c", 4*day, expireAfter(720*time.Hour))}), in)
	if tooYoung != nil {
		t.Fatal("claim younger than the override should not be eligible")
	}
}

func TestPickEarliestDeadlineEligibleExcludesNotReady(t *testing.T) {
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
			if got := selection.PickEarliestDeadlineEligible(views(claims), baseInputs()); got != nil {
				t.Fatalf("a NotReady claim must be skipped, got %v", got.Name)
			}
		})
	}
}

func TestPickEarliestDeadlineEligibleExcludesDeleting(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("c", 20*day, deleting())}
	if got := selection.PickEarliestDeadlineEligible(views(claims), baseInputs()); got != nil {
		t.Fatalf("a claim with deletionTimestamp must be skipped, got %v", got.Name)
	}
}

func TestPickEarliestDeadlineEligibleExcludesInFlightAndTerminalStates(t *testing.T) {
	for _, state := range []string{
		annotations.StatePending,
		annotations.StateDraining,
		annotations.StateExpired,
	} {
		t.Run(state, func(t *testing.T) {
			claims := []karpv1.NodeClaim{claim("c", 20*day, ann(annotations.State, state))}
			if got := selection.PickEarliestDeadlineEligible(views(claims), baseInputs()); got != nil {
				t.Fatalf("state %q must not be re-selected, got %v", state, got.Name)
			}
		})
	}
}

func TestPickEarliestDeadlineEligibleFailedRespectsBackoff(t *testing.T) {
	in := baseInputs() // RetryBackoff 30m
	// retry-count 1 ⇒ backoff = 30m·2^0 = 30m.
	withinBackoff := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, now.Add(-20*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{withinBackoff}), in); got != nil {
		t.Fatalf("failed claim within backoff must not be re-selected, got %v", got.Name)
	}

	pastBackoff := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, now.Add(-40*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{pastBackoff}), in); got == nil {
		t.Fatal("failed claim past its backoff must be re-selectable")
	}
}

func TestPickEarliestDeadlineEligibleFailedBackoffEscalates(t *testing.T) {
	in := baseInputs() // base 30m
	// retry-count 3 ⇒ backoff = 30m·2^2 = 120m. At 90m elapsed it is still held.
	c := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "3",
		annotations.FailedAt, now.Add(-90*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{c}), in); got != nil {
		t.Fatalf("escalated backoff (120m) should still hold at 90m, got %v", got.Name)
	}
}

func TestPickEarliestDeadlineEligibleFailedWithoutFailedAtIsReselectable(t *testing.T) {
	// A torn failed write (state=failed but no failed-at, §5.2 crash recovery)
	// must not strand the claim: the backoff is treated as elapsed so the
	// case-failed handler can re-enter it.
	c := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "2",
	))
	if got := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{c}), baseInputs()); got == nil {
		t.Fatal("failed claim with no failed-at must remain re-selectable")
	}
}

func TestPickEarliestDeadlineEligibleFailedWithUnparseableRetryCountUsesBase(t *testing.T) {
	// A non-numeric retry-count parses to 0, so the backoff falls back to the
	// base (30m), not a stranded claim. failed-at is 20m ago → within base → held.
	in := baseInputs()
	withinBase := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "garbage",
		annotations.FailedAt, now.Add(-20*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{withinBase}), in); got != nil {
		t.Fatalf("unparseable retry-count must fall back to the base backoff (30m), got %v", got.Name)
	}
	pastBase := claim("c", 20*day, ann(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "garbage",
		annotations.FailedAt, now.Add(-40*time.Minute).Format(time.RFC3339),
	))
	if got := selection.PickEarliestDeadlineEligible(views([]karpv1.NodeClaim{pastBase}), in); got == nil {
		t.Fatal("past the base backoff the claim must be re-selectable")
	}
}

func TestTriggeredConsidersAgeAloneIgnoringReadyAndState(t *testing.T) {
	in := baseInputs() // threshold 6d
	// Past the trigger but NotReady and in-flight: not *eligible*, yet still
	// near-deadline, so Triggered (used for the placeholder host exclusion) is true.
	c := views([]karpv1.NodeClaim{claim("c", 10*day, ready(false), ann(annotations.State, annotations.StatePending))})[0]
	if !selection.Triggered(&c, in) {
		t.Error("a past-deadline claim must be Triggered regardless of Ready/state")
	}
	young := views([]karpv1.NodeClaim{claim("young", 1*day)})[0]
	if selection.Triggered(&young, in) {
		t.Error("a young claim must not be Triggered")
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

func TestCountEligibleCountsEveryEligibleClaim(t *testing.T) {
	// baseInputs: auto mode, leadTime 8d, expireAfter 14d ⇒ trigger age > 6d.
	claims := []karpv1.NodeClaim{
		claim("old1", 10*day), // eligible
		claim("old2", 20*day), // eligible
		claim("young", 1*day), // not triggered
		claim("notready", 20*day, ready(false)),
		claim("pending", 20*day, ann(annotations.State, annotations.StatePending)),
		claim("deleting", 20*day, deleting()),
	}
	if got := selection.CountEligible(views(claims), baseInputs()); got != 2 {
		t.Fatalf("want 2 eligible, got %d", got)
	}
}

func TestCountEligibleZeroWhenNoneEligible(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("young", 1*day)}
	if got := selection.CountEligible(views(claims), baseInputs()); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}

func TestPickEarliestDeadlineEligibleSkipsOptedOut(t *testing.T) {
	claims := []karpv1.NodeClaim{
		claim("kept", 20*day), // oldest — would be picked
		claim("other", 15*day),
	}
	in := baseInputs()
	in.Excluded = map[string]bool{"kept": true}
	got := selection.PickEarliestDeadlineEligible(views(claims), in)
	if got == nil || got.Name != "other" {
		t.Fatalf("opted-out oldest must be skipped; want other, got %v", got)
	}
}

func TestPickEarliestDeadlineEligibleNilWhenAllOptedOut(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("only", 20*day)}
	in := baseInputs()
	in.Excluded = map[string]bool{"only": true}
	if got := selection.PickEarliestDeadlineEligible(views(claims), in); got != nil {
		t.Fatalf("want nil, got %v", got.Name)
	}
}

func TestCountEligibleSkipsOptedOut(t *testing.T) {
	claims := []karpv1.NodeClaim{claim("a", 20*day), claim("b", 20*day)}
	in := baseInputs()
	in.Excluded = map[string]bool{"a": true}
	if n := selection.CountEligible(views(claims), in); n != 1 {
		t.Fatalf("want 1 eligible (b), got %d", n)
	}
}

func TestShortLeadClaims(t *testing.T) {
	claims := []karpv1.NodeClaim{
		claim("ample", 1*day, expireAfter(30*day)), // not short-lead
		claim("short-a", 1*day, expireAfter(1*time.Hour)),
		claim("short-b", 1*day, expireAfter(1*time.Hour)),
		claim("never", 1*day, neverExpire()), // nil expireAfter excluded
	}
	got := selection.ShortLeadClaims(views(claims), selection.LeadTime{Base: 24 * time.Hour})
	if len(got) != 2 {
		t.Fatalf("want 2 short-lead claims, got %d", len(got))
	}
	names := map[string]bool{got[0].Name: true, got[1].Name: true}
	if !names["short-a"] || !names["short-b"] {
		t.Fatalf("unexpected short-lead set: %v", names)
	}
}

func TestCountShortLeadCountsClaimsThatCannotGuaranteeKChances(t *testing.T) {
	// A claim whose own expireAfter ≤ leadTime (K·P + t_rot) has per-node A ≤ 0
	// and can no longer guarantee K chances (§3.2 layer 3).
	leadTime := selection.LeadTime{Base: 8 * day}
	claims := []karpv1.NodeClaim{
		claim("short", 1*day, expireAfter(7*day)),                // 7d ≤ 8d → counted
		claim("exact", 1*day, expireAfter(8*day)),                // 8d ≤ 8d (A == 0) → counted
		claim("ample", 1*day, expireAfter(30*day)),               // not short-lead
		claim("never", 1*day, neverExpire()),                     // nil expireAfter → never
		claim("expiring", 1*day, expireAfter(7*day), deleting()), // forceful path begun → excluded
	}
	if got := selection.CountShortLead(views(claims), leadTime); got != 2 {
		t.Fatalf("want 2 short-lead, got %d", got)
	}
}

func TestLeadTimeForUsesPerClaimTGPWithFallback(t *testing.T) {
	lt := selection.LeadTime{Base: 48 * time.Hour, DrainFallback: time.Hour}
	// A claim with its own tGP: lead = Base + tGP.
	withTGP := views([]karpv1.NodeClaim{claim("with", 1*day, tgp(3*time.Hour))})[0]
	if got := lt.For(&withTGP); got != 51*time.Hour {
		t.Errorf("For(withTGP) = %v, want 51h (Base 48h + tGP 3h)", got)
	}
	// A claim that leaves tGP unset falls back to DrainFallback.
	noTGP := views([]karpv1.NodeClaim{claim("none", 1*day)})[0]
	if got := lt.For(&noTGP); got != 49*time.Hour {
		t.Errorf("For(noTGP) = %v, want 49h (Base 48h + fallback 1h)", got)
	}
}

// A NodePool template tGP shortened after a NodeClaim was stamped with a longer
// value must not shrink that claim's lead time: the trigger anchors on the
// claim's own (longer) tGP, so it becomes a candidate earlier (spec §3.2). This
// is the regression #54 guards — using the template tGP would select too late.
func TestTriggeredAnchorsOnPerClaimTGPNotTemplate(t *testing.T) {
	// Base = K·P + readyTimeout + buffer with no tGP term. The candidate carries a
	// 4d tGP, so its lead = 8d + 4d = 12d ⇒ trigger age threshold = 14d − 12d = 2d.
	in := selection.Inputs{Now: now, LeadTime: selection.LeadTime{Base: 8 * day, DrainFallback: time.Hour}}
	c := views([]karpv1.NodeClaim{claim("long-tgp", 3*day, tgp(4*day))})[0] // age 3d > 2d ⇒ triggered
	if !selection.Triggered(&c, in) {
		t.Error("claim with a long per-claim tGP must trigger on its own (longer) lead time")
	}
	// Same claim without the long tGP: lead = 8d + 1h fallback, threshold ≈ 6d, so
	// at age 3d it would NOT yet be triggered — proving the tGP drove the result.
	short := views([]karpv1.NodeClaim{claim("short-tgp", 3*day)})[0]
	if selection.Triggered(&short, in) {
		t.Error("claim relying on the short fallback tGP must not trigger at age 3d")
	}
}

// ShortLeadClaims must flag a claim whose own (longer) tGP pushes its per-node
// lead time above its expireAfter, even when the tGP-independent base alone would
// not (spec §3.2 layer 3).
func TestShortLeadClaimsUsesPerClaimTGP(t *testing.T) {
	lt := selection.LeadTime{Base: 6 * day, DrainFallback: time.Hour}
	claims := []karpv1.NodeClaim{
		// E = 7d. Base alone (6d) < 7d, but Base + 2d tGP = 8d ≥ 7d ⇒ short-lead.
		claim("long-tgp", 1*day, expireAfter(7*day), tgp(2*day)),
		// E = 7d, no tGP ⇒ lead = 6d + 1h < 7d ⇒ not short-lead.
		claim("short-tgp", 1*day, expireAfter(7*day)),
	}
	got := selection.ShortLeadClaims(views(claims), lt)
	if len(got) != 1 || got[0].Name != "long-tgp" {
		t.Fatalf("want only long-tgp short-lead, got %v", got)
	}
}

// TestClaimViewIsPure pins the view's field set: everything selection reads off a
// NodeClaim and nothing more. It is the compile-time guard that keeps karpv1 out.
func TestClaimViewIsPure(t *testing.T) {
	e := 10 * day
	g := 30 * time.Minute
	c := selection.Claim{
		Name:        "n1",
		CreatedAt:   now.Add(-11 * day),
		Deleting:    false,
		ExpireAfter: &e,
		TGP:         &g,
		Ready:       true,
		Annotations: map[string]string{},
	}
	in := selection.Inputs{
		Now:      now,
		LeadTime: selection.LeadTime{Base: time.Hour, DrainFallback: time.Hour},
	}
	if !selection.Triggered(&c, in) {
		t.Fatalf("claim aged past its deadline minus lead time must be triggered")
	}
	if got, want := in.LeadTime.For(&c), time.Hour+g; got != want {
		t.Fatalf("LeadTime.For = %v, want %v (Base + the claim's own TGP)", got, want)
	}
}
