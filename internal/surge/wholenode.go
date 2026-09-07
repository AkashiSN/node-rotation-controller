package surge

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// WholeNode raises the placeholder's requests to a whole node's worth of what
// the drain needs — the opt-in surge.wholeNodeReservation mode (spec §3.3,
// ADR-0005, issue #326):
//
//	limit    = allocatable − DaemonSet overhead   (per resource, the Clamp ceiling)
//	requests = max(requests, limit)               (per resource)
//
// The aggregate reservation the placeholder makes is fungible with the
// individual evicted Pods' placement only on a host that can accept all of them,
// and a host already running other Pods is where that breaks: the hole is big
// enough while a Pod's own podAntiAffinity or hostPort still refuses the host, so
// Karpenter provisions for that Pod *after* the drain has started.
//
// Demanding a whole node's worth is what excludes exactly that class of host. It
// is NOT "force a new node": a genuinely empty host — DaemonSets only — still
// absorbs the placeholder, and should, because an empty host has nothing for a
// hostname-topology anti-affinity to bite on and is as good as a fresh one. It
// also never upsizes: limit + DaemonSet = allocatable, so the smallest instance
// type that fits is the candidate's own class.
//
// Only resources the drain actually requests are raised. A resource the evicted
// Pods do not request is not one they need reserved, and `pods` — which
// allocatable always carries — is not a resource a container may request at all.
// A drain that requests nothing therefore reserves nothing, which is correct: a
// candidate with no reschedulable Pods has nothing to guarantee.
//
// Three cases keep the drain rather than a raised value, each matching what
// Clamp does with the same input:
//
//   - allocatable absent or empty — no trustworthy ceiling to raise to;
//   - a resource allocatable does not report — its ceiling is unknown;
//   - a non-positive limit — raising to it would reserve nothing and satisfy
//     surge_ready with an empty placeholder, a silent break-before-make. Keeping
//     the drain leaves Clamp to refuse it and the rotation to roll back.
//
// A drain already above the limit is left alone: lowering it is Clamp's job,
// along with the shortfall reporting that goes with it (issue #224).
//
// requests is not modified; the caller reports the real drain alongside the
// raised value on the placeholder line.
func WholeNode(requests, allocatable, daemonSet corev1.ResourceList) corev1.ResourceList {
	if len(allocatable) == 0 || len(requests) == 0 {
		return requests
	}
	out := requests.DeepCopy()
	for name, want := range requests {
		limit, ok := provisionableLimit(allocatable, daemonSet, name)
		if !ok || limit.Sign() <= 0 {
			continue
		}
		if want.Cmp(limit) < 0 {
			out[name] = limit
		}
	}
	return out
}

// provisionableLimit is the ceiling Karpenter can actually provision for one
// resource on a fresh node of the candidate's instance type:
// NodeClaim.status.allocatable minus the DaemonSet overhead Karpenter adds to
// every node it creates (spec §3.3). ok is false when allocatable does not report
// the resource, which leaves it without a known ceiling.
//
// It is shared by Clamp, which caps requests at it, and WholeNode, which raises
// them to it — the two directions of the same ceiling, so they can never disagree
// about where it is.
func provisionableLimit(allocatable, daemonSet corev1.ResourceList, name corev1.ResourceName) (resource.Quantity, bool) {
	alloc, ok := allocatable[name]
	if !ok {
		return resource.Quantity{}, false
	}
	limit := alloc.DeepCopy()
	if ds, ok := daemonSet[name]; ok {
		limit.Sub(ds)
	}
	return limit, true
}
