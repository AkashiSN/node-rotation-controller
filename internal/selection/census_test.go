package selection_test

import (
	"testing"
	"time"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// TestTakeCensusClassifiesEachClaimByItsFirstDisqualifier is the counting
// counterpart of the eligibility predicate: it reports WHY no candidate was
// picked, so the controller can log a reason instead of falling silent
// (issue #221). Each claim lands in exactly one bucket, chosen by the first
// check that rejects it — the same order eligible() applies.
func TestTakeCensusClassifiesEachClaimByItsFirstDisqualifier(t *testing.T) {
	in := baseInputs()
	in.Excluded = map[string]bool{"opted-out": true}

	claims := []karpv1.NodeClaim{
		claim("fresh-eligible", 7*day),
		claim("opted-out", 7*day),
		claim("expiring", 7*day, deleting()),
		claim("broken", 7*day, ready(false)),
		claim("in-flight", 7*day, ann(annotations.State, annotations.StatePending)),
		claim("draining", 7*day, ann(annotations.State, annotations.StateDraining)),
		claim("terminal", 7*day, ann(annotations.State, annotations.StateExpired)),
		claim("backing-off", 7*day,
			ann(annotations.State, annotations.StateFailed,
				annotations.FailedAt, now.Add(-time.Minute).Format(time.RFC3339),
				annotations.RetryCount, "1")),
		claim("young", 1*day),
	}

	got := selection.TakeCensus(views(claims), in)

	want := selection.Census{
		Total: 9, Eligible: 1, OptedOut: 1, Deleting: 1, NotReady: 1,
		InFlight: 2, Terminal: 1, InBackoff: 1, NotTriggered: 1,
	}
	if got != want {
		t.Errorf("census: got %+v, want %+v", got, want)
	}
}

// TestTakeCensusEligibleMatchesCountEligible guards the invariant the logged
// reason depends on: the census's Eligible bucket is exactly the candidates
// gauge. If the two predicates drift, the controller would log "no candidate"
// while reporting a positive noderotation_candidates.
func TestTakeCensusEligibleMatchesCountEligible(t *testing.T) {
	in := baseInputs()
	claims := []karpv1.NodeClaim{
		claim("a", 7*day),
		claim("b", 7*day),
		claim("c", 1*day),
		claim("d", 7*day, ready(false)),
	}

	c := selection.TakeCensus(views(claims), in)

	if want := selection.CountEligible(views(claims), in); c.Eligible != want {
		t.Errorf("Eligible: got %d, want CountEligible = %d", c.Eligible, want)
	}
}

// TestTakeCensusBucketsSumToTotal: every claim is classified exactly once, so a
// future predicate added to eligible() without a matching bucket cannot silently
// vanish from the log.
func TestTakeCensusBucketsSumToTotal(t *testing.T) {
	in := baseInputs()
	in.Excluded = map[string]bool{"x": true}
	claims := []karpv1.NodeClaim{
		claim("a", 7*day), claim("x", 7*day), claim("y", 1*day),
		claim("z", 7*day, deleting()), claim("w", 7*day, ready(false)),
		claim("v", 7*day, ann(annotations.State, annotations.StateDraining)),
	}

	c := selection.TakeCensus(views(claims), in)

	sum := c.Eligible + c.OptedOut + c.Deleting + c.NotReady + c.InFlight + c.Terminal + c.InBackoff + c.NotTriggered
	if sum != c.Total || c.Total != len(claims) {
		t.Errorf("buckets %d must sum to Total %d = len(claims) %d (census %+v)", sum, c.Total, len(claims), c)
	}
}

// TestTakeCensusFailedPastBackoffIsEligibleNotInBackoff: InBackoff means "failed
// and still paused". A failed claim whose escalated backoff has elapsed is a
// genuine candidate again and must not be reported as blocked.
func TestTakeCensusFailedPastBackoffIsEligibleNotInBackoff(t *testing.T) {
	in := baseInputs() // RetryBackoff 30m, retry 0 ⇒ escalated backoff 30m
	claims := []karpv1.NodeClaim{
		claim("retryable", 7*day,
			ann(annotations.State, annotations.StateFailed,
				annotations.FailedAt, now.Add(-2*time.Hour).Format(time.RFC3339),
				annotations.RetryCount, "0")),
	}

	got := selection.TakeCensus(views(claims), in)

	want := selection.Census{Total: 1, Eligible: 1}
	if got != want {
		t.Errorf("census: got %+v, want %+v", got, want)
	}
}
