package surge

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"
)

// DaemonSetRequests sums the effective requests of the DaemonSet Pods scheduled
// on nodeName — the overhead Karpenter adds to every node it provisions. The
// clamp subtracts this from NodeClaim.status.allocatable to find the largest
// placeholder Karpenter can actually fit onto a fresh node of the candidate's
// instance type (spec §3.3).
//
// It sizes each Pod with the same effective-request algorithm as
// ReschedulableRequests (resourcehelper.PodRequests). Only *running* DaemonSet
// Pods count: mirror/static Pods are not overhead Karpenter provisions for on a
// new node, and a Succeeded/Failed DaemonSet Pod is still bound to the node but
// consumes no allocatable, so kube-scheduler does not count it either.
//
// That exclusion is what keeps the clamp's shortfall bounded. kube-scheduler
// admitted every consuming Pod on the candidate, so
//
//	ReschedulableRequests <= Node.allocatable - DaemonSetRequests
//
// and with limit = NodeClaim.allocatable - DaemonSetRequests the DaemonSet term
// cancels, leaving shortfall <= Node.allocatable - NodeClaim.allocatable — the
// per-AZ band (see Band). Counting a Pod here that the scheduler does not count
// breaks that identity and lets the shortfall exceed the band.
func DaemonSetRequests(pods []corev1.Pod, nodeName string) corev1.ResourceList {
	total := corev1.ResourceList{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != nodeName || !isDaemonSet(p) || isCompleted(p) {
			continue
		}
		add(total, resourcehelper.PodRequests(p, resourcehelper.PodResourcesOptions{}))
	}
	return total
}

// Band is the per-resource capacity discrepancy the clamp trades away:
// Node.status.allocatable (what the kubelet reports, and what kube-scheduler
// packs against) minus NodeClaim.status.allocatable (the single value Karpenter
// caches per instance type, and what it plans a fresh node against). It is the
// upper bound on Clamp's shortfall (see DaemonSetRequests), measured per node
// rather than assumed — no constant to justify.
//
// A resource the claim reports at or above the node's value yields a zero band:
// there is nothing to give up, and the clamp does not fire on it anyway. Only
// resources both sides report have a measurable band — Clamp skips the rest, so
// they can never contribute a shortfall.
func Band(nodeAllocatable, claimAllocatable corev1.ResourceList) corev1.ResourceList {
	band := corev1.ResourceList{}
	for name, node := range nodeAllocatable {
		claim, ok := claimAllocatable[name]
		if !ok {
			continue
		}
		gap := node.DeepCopy()
		gap.Sub(claim)
		if gap.Sign() < 0 {
			gap.Set(0)
		}
		band[name] = gap
	}
	return band
}

// ExceedsBand reports the first resource whose shortfall exceeds its band, if
// any. Reaching it means the identity in DaemonSetRequests no longer holds — the
// controller's request accounting has diverged from the scheduler's — so the
// placeholder is giving up more than the per-AZ discrepancy. The rotation still
// proceeds (refusing it would trade a bounded, in-window, PDB-respecting drain
// for Karpenter's Forceful Expiration, which honours neither window nor PDB);
// the caller surfaces it so the divergence is observable rather than silent.
//
// A resource absent from band has no measurable discrepancy, so any positive
// shortfall on it exceeds zero.
func ExceedsBand(shortfall, band corev1.ResourceList) (corev1.ResourceName, bool) {
	for _, name := range slices.Sorted(maps.Keys(shortfall)) {
		short := shortfall[name]
		allowed := band[name] // zero value when absent
		if short.Cmp(allowed) > 0 {
			return name, true
		}
	}
	return "", false
}

// ClampResult is the outcome of Clamp: the requests to size the placeholder
// with, plus — only when the clamp actually lowered a resource — the details an
// operator needs to know the placeholder no longer reserves the full drain.
type ClampResult struct {
	// Requests is the (possibly reduced) sizing to hand the placeholder.
	Requests corev1.ResourceList
	// Clamped is true when at least one resource was lowered.
	Clamped bool
	// Limit is the per-resource ceiling (allocatable − DaemonSet) for exactly the
	// resources that were clamped. Nil when Clamped is false.
	Limit corev1.ResourceList
	// Shortfall is the per-resource amount given up (unclamped − clamped) for
	// exactly the resources that were clamped. Nil when Clamped is false.
	Shortfall corev1.ResourceList
	// Refused is true when a resource with positive demand has a non-positive
	// limit, so no clamp value under that ceiling would reserve any positive
	// amount of RefusedResource. It is per-resource — the loop returns on the
	// first such resource, and the drain's others may still be reservable — and
	// the ceiling is the candidate's CACHED allocatable minus the DaemonSet
	// overhead observed on it, so it says nothing about whether the full-drain
	// placeholder is schedulable anywhere. See Clamp (issue #328). Requests then
	// carries the full un-clamped drain and Clamped is false.
	Refused bool
	// RefusedResource names the resource that forced the refusal. Empty unless
	// Refused.
	RefusedResource corev1.ResourceName
}

// Clamp caps requests at what Karpenter can actually provision for a fresh node
// of the candidate's instance type: NodeClaim.status.allocatable minus the
// DaemonSet overhead Karpenter adds to every node it creates (spec §3.3).
//
//	limit    = allocatable − daemonSet   (per resource, floored at zero)
//	requests = min(requests, limit)      (per resource)
//
// Karpenter caches one estimated allocatable per instance type while EC2 reports
// slightly different memory for the same type across AZs, so a node the
// scheduler filled past that estimate yields a placeholder Karpenter refuses to
// provision ("no instance type has enough resources"). The clamp trades the full
// capacity guarantee — the shortfall is bounded by that per-AZ band — for a
// placeholder Karpenter can always fit; the drain absorbs the shortfall through
// placeholder preemption (priority −10) plus Karpenter follow-up provisioning
// (issue #224).
//
// When allocatable is empty/absent (a NodeClaim that has not registered yet, or
// a nil map) there is no trustworthy ceiling, so the clamp is a no-op: it returns
// the full requests unchanged rather than clamping toward zero, which would
// silently reserve no capacity and break make-before-break. A resource absent
// from allocatable is likewise left untouched — its ceiling is unknown.
//
// A non-positive limit on a resource with positive demand is refused, not
// clamped. Every clamp value under that ceiling is non-positive, so a clamped
// placeholder would reserve no amount of that resource at all — and one that
// reserves nothing could satisfy surge_ready while holding nothing. That is
// break-before-make, which v1 exposes only as the opt-in, window-bounded
// surge.forcefulFallback (ADR-0001) — the clamp must not become it silently.
// Refusing preserves the full drain instead. (This is the reason at limit == 0
// too, where the arithmetic alone would admit a zero-sized placeholder beside
// the overhead: what rules it out is that it holds nothing, not that it fails to
// fit. Nothing here says where such a Pod would land, either — that is the same
// question the next paragraph refuses to answer.)
//
// Refusing establishes NOTHING about schedulability, and the caller must not
// report it as if it did. What it establishes is arithmetic about ONE ceiling on
// ONE resource: under the candidate's CACHED per-type allocatable minus the
// DaemonSet overhead observed running on it, no clamp value reserves any
// positive amount of the resource this returned on. It does not extend to the
// rest of the drain — the loop returns on the first such resource, and the
// others may have positive ceilings and be reservable.
//
// Real scheduling happens elsewhere: Node.status.allocatable can exceed the
// cached estimate, which is the whole premise of this clamp and the gap Band
// measures, so even a node of the SAME instance type carrying the SAME
// DaemonSets can have room for the full drain. A larger allowed instance type
// and a node carrying less applicable overhead are two further ordinary cases,
// since the placeholder pins the NodePool and the replicated requirements but
// never the instance type. Those are examples, not an exhaustive set: the
// rollback that follows when no node can take the placeholder is an outcome,
// never the complement of a list (issue #328).
func Clamp(requests, allocatable, daemonSet corev1.ResourceList) ClampResult {
	if len(allocatable) == 0 {
		return ClampResult{Requests: requests}
	}
	res := ClampResult{Requests: requests.DeepCopy()}
	if res.Requests == nil {
		res.Requests = corev1.ResourceList{}
	}
	// Sorted so a refusal names the same resource on every pass, whatever the map
	// iteration order.
	for _, name := range slices.Sorted(maps.Keys(requests)) {
		want := requests[name]
		limit, ok := provisionableLimit(allocatable, daemonSet, name)
		if !ok {
			continue // no ceiling reported for this resource — leave it unclamped
		}
		if limit.Sign() <= 0 {
			if want.Sign() <= 0 {
				continue // nothing demanded here; nothing to refuse
			}
			return ClampResult{Requests: requests, Refused: true, RefusedResource: name}
		}
		if want.Cmp(limit) <= 0 {
			continue // already fits
		}
		if !res.Clamped {
			res.Clamped = true
			res.Limit = corev1.ResourceList{}
			res.Shortfall = corev1.ResourceList{}
		}
		short := want.DeepCopy()
		short.Sub(limit)
		res.Shortfall[name] = short
		res.Limit[name] = limit
		res.Requests[name] = limit
	}
	return res
}
