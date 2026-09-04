package selection

import (
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// Census is the breakdown of a NodePool's claims by the first eligibility check
// each one fails — the reason no candidate was picked (spec §3.2). Eligible is
// exactly what CountEligible returns; the other buckets partition the rest.
//
// The reconcile loop logs it when the pick comes back empty (issue #221): the
// noderotation_candidates gauge reports that the count is zero but never why,
// and "excluded because its drain began" reads identically to "excluded because
// it entered retryBackoff".
type Census struct {
	Total    int
	Eligible int
	// OptedOut: the Node carries an operator-set karpenter.sh/do-not-disrupt.
	OptedOut int
	// Deleting: already being deleted, typically Forceful Expiration.
	Deleting int
	// NotReady: left to Node Auto Repair and the backstop.
	NotReady int
	// InFlight: pending or draining under §5.2 step 1.
	InFlight int
	// Terminal: expired.
	Terminal int
	// InBackoff: failed, still inside its escalated retryBackoff.
	InBackoff int
	// NotTriggered: healthy and selectable, but its age has not crossed the trigger.
	NotTriggered int

	// InBackoffTriggered is the subset of InBackoff whose age HAS crossed the
	// trigger — the claims that would be eligible the moment their backoff
	// elapses. It is not a bucket: it overlaps InBackoff and is excluded from the
	// partition above.
	//
	// The partition files a claim under its FIRST disqualifier, and state is
	// checked before age, so InBackoff also holds claims that would fall to
	// NotTriggered if the backoff were lifted — reachable when a RotationPolicy
	// edit raises ageThresholdOverride or SHORTENS the lead time (the trigger is
	// age > expireAfter - leadTime, so a wider lead time triggers earlier, not
	// later), or when a claim's own expireAfter is extended, after its attempt
	// failed. That is the right answer
	// for the issue-#221 log line, which reports what is blocking the claim now,
	// but the wrong one for "what did this window leave undone": a claim whose age
	// is no longer due was owed nothing. decide.Outstanding therefore counts this
	// field, not InBackoff (issue #321 review).
	InBackoffTriggered int
}

// TakeCensus classifies every claim into exactly one Census bucket, applying the
// eligibility checks in the same order as eligible() so the reported reason is
// the one that actually rejected the claim. It also fills the non-bucket
// InBackoffTriggered tally, which re-reads the age trigger for the claims it
// filed under InBackoff.
func TakeCensus(claims []Claim, in Inputs) Census {
	c := Census{Total: len(claims)}
	for i := range claims {
		cl := &claims[i]
		switch {
		case in.Excluded[cl.Name]:
			c.OptedOut++
		case cl.Deleting:
			c.Deleting++
		case !cl.Ready:
			c.NotReady++
		case !stateAllows(cl, in):
			switch cl.Annotations[annotations.State] {
			case annotations.StateExpired:
				c.Terminal++
			case annotations.StateFailed:
				c.InBackoff++
				if triggered(cl, in) {
					c.InBackoffTriggered++
				}
			default: // pending, draining, or any future in-flight state
				c.InFlight++
			}
		case !triggered(cl, in):
			c.NotTriggered++
		default:
			c.Eligible++
		}
	}
	return c
}
