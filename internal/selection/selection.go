// Package selection implements the read path of the reconcile loop (spec §5.2):
// which NodePools are in scope, and which NodeClaim is the next rotation
// candidate (spec §3.2). It has no side effects — pure predicates over Karpenter
// types and resolved durations — so the caller derives the per-NodePool inputs
// (leadTime from the schedule, the ageThreshold override from policy) and passes
// plain values, mirroring the layering of internal/schedule and internal/window.
package selection

import (
	"strconv"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
)

// maxBackoffShift caps the escalated re-selection backoff at 8× the base
// (2^3, spec §5.3).
const maxBackoffShift = 3

// InScopeNodePools returns the NodePools matched by the configured selectors.
// Within one selector every matchLabels entry must match (AND); across selectors
// any match qualifies (OR) — spec §3.2, §5.4.
func InScopeNodePools(pools []karpv1.NodePool, selectors []policy.Selector) []karpv1.NodePool {
	var out []karpv1.NodePool
	for _, p := range pools {
		if matchesAny(p.Labels, selectors) {
			out = append(out, p)
		}
	}
	return out
}

func matchesAny(labels map[string]string, selectors []policy.Selector) bool {
	for _, s := range selectors {
		if matchesAll(labels, s.MatchLabels) {
			return true
		}
	}
	return false
}

// matchesAll reports whether labels contain every want entry. An empty want
// matches vacuously; policy validation (internal/policy) guarantees non-empty
// matchLabels per selector, so that case is unreachable in practice.
func matchesAll(labels, want map[string]string) bool {
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// LeadTime resolves the per-NodeClaim rotation lead time K·P + t_rot, where
// t_rot = readyTimeout + tGP + buffer (spec §3.2). Base is the tGP-independent
// part (K·P + readyTimeout + buffer); the tGP term is read from each NodeClaim's
// own spec.terminationGracePeriod — the authoritative per-node value — so a
// NodePool template shortened after a claim was stamped cannot under-estimate the
// lead time. DrainFallback is substituted when a claim leaves tGP unset
// (self-managed Karpenter allows nil), matching the §3.2 derivation. The NodePool
// template tGP stays the representative input for per-NodePool validation/logging
// (schedule.Derive), not the per-node trigger.
type LeadTime struct {
	// Base is K·P + readyTimeout + buffer — everything in K·P + t_rot except tGP.
	Base time.Duration
	// DrainFallback is the fixed bound used when a NodeClaim's tGP is unset.
	DrainFallback time.Duration
}

// For returns the lead time for one claim: Base plus the claim's own
// terminationGracePeriod (DrainFallback when unset).
func (lt LeadTime) For(c *karpv1.NodeClaim) time.Duration {
	return lt.Base + claimTGP(c, lt.DrainFallback)
}

// claimTGP reads a NodeClaim's own spec.terminationGracePeriod, substituting the
// fallback bound when Karpenter leaves it nil (spec §3.2).
func claimTGP(c *karpv1.NodeClaim, fallback time.Duration) time.Duration {
	if d := c.Spec.TerminationGracePeriod; d != nil {
		return d.Duration
	}
	return fallback
}

// Inputs are the resolved per-NodePool selection inputs.
type Inputs struct {
	// Now is the evaluation instant.
	Now time.Time
	// LeadTime resolves K·P + t_rot per claim. In auto mode the trigger is
	// now > deadline − LeadTime.For(claim), where deadline =
	// NodeClaim.creationTimestamp + NodeClaim.spec.expireAfter.
	LeadTime LeadTime
	// Override is the explicit ageThreshold; when set, the trigger is age > *Override
	// (purely age-based, ignoring the per-claim expireAfter) — spec §3.2.
	Override *time.Duration
	// RetryBackoff is the base backoff; a failed claim is re-selectable once
	// now − failed-at ≥ EscalatedBackoff(retry-count, RetryBackoff).
	RetryBackoff time.Duration
}

// PickOldestEligible returns the oldest eligible candidate, or nil when none
// qualify (spec §3.2). "Oldest" is the earliest creationTimestamp, ties broken
// by NodeClaim name (see isOlder). The returned pointer aliases an element of
// claims.
func PickOldestEligible(claims []karpv1.NodeClaim, in Inputs) *karpv1.NodeClaim {
	var best *karpv1.NodeClaim
	for i := range claims {
		c := &claims[i]
		if !eligible(c, in) {
			continue
		}
		if best == nil || isOlder(c, best) {
			best = c
		}
	}
	return best
}

// CountEligible returns how many claims currently pass the rotation-eligibility
// predicate — the §4.2 noderotation_candidates gauge. It applies the same
// predicate as PickOldestEligible (ready, in-scope state, triggered, not
// deleting), so an in-flight or terminal claim is excluded.
func CountEligible(claims []karpv1.NodeClaim, in Inputs) int {
	n := 0
	for i := range claims {
		if eligible(&claims[i], in) {
			n++
		}
	}
	return n
}

// ShortLeadClaims returns the claims that can no longer guarantee K rotation
// chances against their own spec.expireAfter — the short-lead set (§3.2 layer 3,
// surfaced as noderotation_short_lead_nodes and a per-claim warning event). A
// claim is short-lead when its expireAfter ≤ leadTime (K·P + t_rot, i.e. per-node
// A ≤ 0), the lead time resolved against the claim's own terminationGracePeriod
// (LeadTime.For). A nil (Never) expireAfter never races forceful expiration, and a
// claim already on the forceful path (deletionTimestamp set) is excluded — both
// mirror selection eligibility. The returned pointers alias the input slice.
func ShortLeadClaims(claims []karpv1.NodeClaim, leadTime LeadTime) []*karpv1.NodeClaim {
	var out []*karpv1.NodeClaim
	for i := range claims {
		c := &claims[i]
		if c.DeletionTimestamp != nil {
			continue
		}
		e := c.Spec.ExpireAfter.Duration
		if e == nil {
			continue
		}
		if *e <= leadTime.For(c) {
			out = append(out, c)
		}
	}
	return out
}

// CountShortLead returns how many claims are short-lead (§4.2
// noderotation_short_lead_nodes gauge); see ShortLeadClaims for the predicate.
func CountShortLead(claims []karpv1.NodeClaim, leadTime LeadTime) int {
	return len(ShortLeadClaims(claims, leadTime))
}

// isOlder orders candidates oldest-first by creationTimestamp, breaking ties on
// Name so selection is deterministic across reconciles. metav1.Time is
// second-granular, so claims batch-provisioned by Karpenter routinely share a
// timestamp; without a stable tiebreak the pick would follow nondeterministic
// list order. Spec §3.2 specifies oldest-first and leaves the tiebreak open.
func isOlder(a, b *karpv1.NodeClaim) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

func eligible(c *karpv1.NodeClaim, in Inputs) bool {
	if c.DeletionTimestamp != nil {
		return false // already being deleted — typically Forceful Expiration (§3.2)
	}
	if !isReady(c) {
		return false // NotReady is left to Node Auto Repair + the backstop (§3.2)
	}
	if !stateAllows(c, in) {
		return false
	}
	return triggered(c, in)
}

// stateAllows reports whether the claim's noderotation.io/state permits a fresh
// selection: empty (fresh) always, failed only past its escalated backoff;
// pending/draining (in-flight, driven by §5.2 step 1) and expired (terminal)
// never.
func stateAllows(c *karpv1.NodeClaim, in Inputs) bool {
	switch c.Annotations[annotations.State] {
	case "":
		return true
	case annotations.StateFailed:
		return failedPastBackoff(c, in)
	default:
		return false
	}
}

func failedPastBackoff(c *karpv1.NodeClaim, in Inputs) bool {
	failedAt, ok := parseTime(c.Annotations[annotations.FailedAt])
	if !ok {
		// A failed claim with no parseable failed-at is a torn write; treat the
		// backoff as elapsed so the §5.2 case-failed handler can re-enter it.
		return true
	}
	retry := parseInt(c.Annotations[annotations.RetryCount])
	return in.Now.Sub(failedAt) >= EscalatedBackoff(retry, in.RetryBackoff)
}

// Triggered reports whether the claim's age has crossed the rotation trigger
// (spec §3.2) — the same age/deadline predicate PickOldestEligible applies,
// exported so the reconcile loop can compute the near-deadline host set for the
// placeholder's hostname exclusion (spec §3.3) without duplicating the formula.
// Unlike eligibility it considers age alone: a near-deadline node should be
// avoided regardless of its Ready/state condition.
func Triggered(c *karpv1.NodeClaim, in Inputs) bool { return triggered(c, in) }

// triggered evaluates the age/deadline trigger (spec §3.2).
func triggered(c *karpv1.NodeClaim, in Inputs) bool {
	age := in.Now.Sub(c.CreationTimestamp.Time)
	if in.Override != nil {
		return age > *in.Override
	}
	// Auto mode: anchored on this claim's own expireAfter. A nil (Never)
	// expireAfter has no deadline — the node never races forceful expiration, so
	// it is never a candidate (§3.2).
	e := c.Spec.ExpireAfter.Duration
	if e == nil {
		return false
	}
	return age > *e-in.LeadTime.For(c)
}

// isReady reports whether the NodeClaim's Ready condition is True.
func isReady(c *karpv1.NodeClaim) bool {
	for _, cond := range c.Status.Conditions {
		if cond.Type == status.ConditionReady {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}

// EscalatedBackoff returns the re-selection backoff for a failed claim:
// base · 2^(retryCount − 1), capped at 8× (spec §5.3). A retryCount below 1
// (defensive — a failed claim always carries ≥ 1) yields the base.
func EscalatedBackoff(retryCount int, base time.Duration) time.Duration {
	shift := min(max(retryCount-1, 0), maxBackoffShift)
	return base << shift
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func parseInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
