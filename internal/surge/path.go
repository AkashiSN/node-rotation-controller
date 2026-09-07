package surge

import (
	"time"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// The two surge paths (spec §3.3). They are reported on the "surge node ready"
// and "rotation complete" lines so a short surge_wait is self-explanatory: the
// capacity-absorb path reserves aggregate capacity on a host that is already
// running other Pods, and therefore does not bound the time until the evicted
// Pods are running (issue #305).
const (
	// PathProvisioned means the surge host's NodeClaim came into existence during
	// this rotation attempt. That is what is observed; "Karpenter launched it for
	// the placeholder" is the usual cause but not a claim this value makes (see
	// HostPath).
	PathProvisioned = "provisioned"
	// PathAbsorbed means the surge host predates this attempt — the scheduler
	// bin-packed the placeholder onto pre-existing capacity.
	PathAbsorbed = "absorbed"
)

// CreatedByAttempt reports whether claim came into existence during the rotation
// attempt that started at startedAt. It is the ONE definition of "this attempt's
// node", shared by the rollback's reap guard (which must never delete a host it
// did not induce) and the surge-path reporting, so the two can never disagree
// about the same host.
//
// The comparison is strict, and the granularity makes that a real boundary
// rather than a formality: NodeClaim.CreationTimestamp is stored to the second
// and started-at is an RFC3339 string, so a node launched inside the attempt's
// first second reads as pre-existing. Both callers err the same way — the reap
// leaves such a host alone, the report calls it absorbed.
func CreatedByAttempt(claim *karpv1.NodeClaim, startedAt time.Time) bool {
	return claim != nil && claim.CreationTimestamp.After(startedAt)
}

// HostPath names how the surge host came to be, given the NodeClaim that owns it
// and the attempt's started-at. It returns "" when the host's claim could not be
// resolved; callers omit the field rather than print a value the predicate did
// not establish.
//
// It reports what it can observe — the host's claim came into existence during
// this attempt — not causality. A node Karpenter provisioned for some other
// pending Pod inside this attempt's window, which then absorbed the placeholder,
// reads as PathProvisioned. The rollback path accepts the same imprecision in
// its created-after leg (such a claim passes it; the separate occupancy guard
// may still spare it), and sharing CreatedByAttempt keeps that one accepted
// imprecision from becoming two divergent ones.
func HostPath(hostClaim *karpv1.NodeClaim, startedAt time.Time) string {
	if hostClaim == nil {
		return ""
	}
	if CreatedByAttempt(hostClaim, startedAt) {
		return PathProvisioned
	}
	return PathAbsorbed
}
