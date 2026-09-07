package surge

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// binPackingResources are the dimensions kube-scheduler and Karpenter actually
// pack against on every node, and the two the whole-node reservation raises
// whatever the drain happens to declare. Both are always reported in
// NodeClaim.status.allocatable and both are ordinary container requests.
//
// Nothing else is invented. `pods` is a node dimension the scheduler counts, not
// a resource a container may request. Ephemeral storage and accelerators are not
// what an evicted Pod needs reserved unless it asked for them — and demanding
// all of a node's accelerators would block far more than the rotation — so those
// are raised only when the drain itself requests them.
var binPackingResources = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

// WholeNode raises the placeholder's requests towards a whole node's worth of
// capacity — the opt-in surge.wholeNodeReservation mode (spec §3.3, ADR-0005,
// issue #326):
//
//	limit    = allocatable − DaemonSet overhead   (per resource, the Clamp ceiling)
//	requests = max(requests, limit)               (per resource)
//
// applied to cpu and memory whenever the candidate has reschedulable Pods, plus
// every other resource the drain itself requests.
//
// The aggregate reservation the placeholder makes is fungible with the
// individual evicted Pods' placement only on a host that can accept all of them,
// and a host already running other Pods is where that breaks: the hole is big
// enough while a Pod's own podAntiAffinity or hostPort still refuses the host, so
// Karpenter provisions for that Pod *after* the drain has started.
//
// # What this does and does not establish
//
// Demanding a whole node's worth of both bin-packing dimensions excludes every
// host whose free cpu or memory is short of a whole node's — which is most
// occupied hosts, and the reason the mode raises the bar substantially.
//
// It does NOT prove a host is empty, and must not be described as if it did.
// Pods that request no resources occupy nothing the scheduler counts, so a host
// running any number of them still has a whole node's worth free and can absorb
// the placeholder — and such a Pod can be exactly the one whose anti-affinity or
// hostPort then refuses an evicted Pod. Kubernetes offers no way to express
// "a host with no other Pods" as a requirement here: a required
// kubernetes.io/hostname NotIn term makes Karpenter's provisioner refuse to
// provision at all (issue #96), and a required podAntiAffinity matching every
// Pod would also exclude the DaemonSets every node carries. The mode is a
// substantial, bounded reduction of the failure mode, not a guarantee against it.
//
// Admitting a genuinely empty host is deliberate and correct: an empty host has
// nothing for a hostname-topology anti-affinity to bite on, so it is as good as
// a fresh one.
//
// # Cases that keep the drain
//
// Each matches what Clamp does with the same input:
//
//   - reschedulablePods == 0 — nothing has to re-land, so nothing is reserved;
//   - allocatable absent or empty — no trustworthy ceiling to raise to;
//   - a resource allocatable does not report — its ceiling is unknown;
//   - a non-positive limit — raising to it would reserve nothing and satisfy
//     surge_ready with an empty placeholder, a silent break-before-make. Keeping
//     the drain leaves Clamp to refuse it and the rotation to roll back.
//
// A drain already above the limit is left alone: Clamp lowers it and reports the
// shortfall (issue #224).
//
// reschedulablePods is the count, not the sum, because Pods that request nothing
// still have to land somewhere: a candidate carrying only zero-request Pods has
// an empty drain and real workload.
//
// requests is not modified; the caller reports the real drain alongside the
// raised value on the placeholder line.
func WholeNode(requests, allocatable, daemonSet corev1.ResourceList, reschedulablePods int) corev1.ResourceList {
	if reschedulablePods == 0 || len(allocatable) == 0 {
		return requests
	}
	out := requests.DeepCopy()
	if out == nil {
		out = corev1.ResourceList{}
	}
	for _, name := range binPackingResources {
		raise(out, name, allocatable, daemonSet, true)
	}
	for name := range requests {
		raise(out, name, allocatable, daemonSet, false)
	}
	return out
}

// raise lifts one resource in out to the provisionable limit, unless the limit is
// unknown or the existing request already meets it.
//
// A non-positive limit means the DaemonSet overhead leaves nothing to reserve on
// this instance type, which is Clamp's refusal case — but Clamp only examines
// resources the requests carry, so on a mandatory dimension the demand has to be
// left there for it to refuse on. Without that, workload whose Pods request
// nothing produces an EMPTY placeholder: one that binds anywhere and satisfies
// surge_ready with nothing reserved, the silent break-before-make this mode must
// never introduce. The value asked for is the node's whole allocatable — what a
// whole node would provide — so the refusal names the resource that cannot
// supply it.
//
// On a non-mandatory dimension (one the drain itself requested) the drain's own
// positive demand is already there for Clamp to refuse on, so nothing is added.
func raise(out corev1.ResourceList, name corev1.ResourceName, allocatable, daemonSet corev1.ResourceList, mandatory bool) {
	limit, ok := provisionableLimit(allocatable, daemonSet, name)
	if !ok {
		return
	}
	if limit.Sign() <= 0 {
		if mandatory {
			refusable(out, name, allocatable)
		}
		return
	}
	if want, ok := out[name]; ok && want.Cmp(limit) >= 0 {
		return
	}
	out[name] = limit
}

// refusable ensures out carries a positive demand for name, so Clamp sees the
// resource and refuses on it rather than passing an empty request through.
func refusable(out corev1.ResourceList, name corev1.ResourceName, allocatable corev1.ResourceList) {
	if want, ok := out[name]; ok && want.Sign() > 0 {
		return // the drain already demands it; Clamp will refuse on that
	}
	alloc := allocatable[name]
	if alloc.Sign() <= 0 {
		return // the node reports nothing here either — no demand to make
	}
	out[name] = alloc.DeepCopy()
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
