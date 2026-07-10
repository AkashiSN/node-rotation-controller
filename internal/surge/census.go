package surge

import (
	corev1 "k8s.io/api/core/v1"
)

// PodCensus is the breakdown of the Pods on a candidate node by the reason each
// did or did not contribute to the placeholder's sizing. Counted is the
// reschedulable workload ReschedulableRequests summed; the remaining buckets are
// the exclusions it applied (spec §3.3).
//
// It exists so the controller can state, at placeholder creation, both the
// computed requests and how they were arrived at (issue #221). Karpenter's own
// FailedScheduling message reports the total capacity it must find — the
// placeholder's requests PLUS the DaemonSet overhead it adds to any new node —
// which reads like a double-count of the DaemonSet Pods unless the controller
// says what it actually excluded.
type PodCensus struct {
	Counted    int // reschedulable workload; sized into the placeholder
	DaemonSet  int
	Mirror     int
	Completed  int
	NodePinned int
}

// CensusOnNode classifies every Pod scheduled on nodeName into exactly one
// PodCensus bucket, applying the same predicates as ReschedulableRequests in the
// same order: IsInfraOrCompleted (DaemonSet, then mirror, then completed) before
// the node-pin check. Pods on other nodes are ignored.
func CensusOnNode(pods []corev1.Pod, nodeName string) PodCensus {
	var c PodCensus
	for i := range pods {
		p := &pods[i]
		switch {
		case p.Spec.NodeName != nodeName:
			continue
		case isDaemonSet(p):
			c.DaemonSet++
		case isMirror(p):
			c.Mirror++
		case isCompleted(p):
			c.Completed++
		case isNodePinned(p, nodeName):
			c.NodePinned++
		default:
			c.Counted++
		}
	}
	return c
}
