package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// TestCensusOnNodeClassifiesEveryPodOnce is the counting counterpart of
// ReschedulableRequests: the same predicates, reported as a breakdown instead of
// a sum, so the controller can log WHY a placeholder is sized the way it is
// (issue #221). Every Pod on the node lands in exactly one bucket.
func TestCensusOnNodeClassifiesEveryPodOnce(t *testing.T) {
	pods := []corev1.Pod{
		pod("workload-a", reqs(rl("cpu", "500m"))),
		pod("workload-b", reqs(rl("cpu", "250m"))),
		pod("ds", ownedBy("DaemonSet")),
		pod("static", mirror()),
		pod("done", phase(corev1.PodSucceeded)),
		pod("crashed", phase(corev1.PodFailed)),
		pod("pinned", hostnamePinned()),
		pod("elsewhere", onNode("node-other")),
	}

	got := surge.CensusOnNode(pods, candidateNode)

	want := surge.PodCensus{Counted: 2, DaemonSet: 1, Mirror: 1, Completed: 2, NodePinned: 1}
	if got != want {
		t.Errorf("census: got %+v, want %+v", got, want)
	}
}

// TestCensusOnNodeCountsInfraBeforeCompleted pins the classification precedence
// to IsInfraOrCompleted's own order, so a Pod is never double-counted: a
// completed DaemonSet Pod is reported as a DaemonSet, not as completed.
func TestCensusOnNodeCountsInfraBeforeCompleted(t *testing.T) {
	pods := []corev1.Pod{pod("ds-done", ownedBy("DaemonSet"), phase(corev1.PodSucceeded))}

	got := surge.CensusOnNode(pods, candidateNode)

	want := surge.PodCensus{DaemonSet: 1}
	if got != want {
		t.Errorf("census: got %+v, want %+v", got, want)
	}
}

// TestCensusOnNodeCountedMatchesReschedulableRequests guards the invariant the
// log line depends on: Counted is exactly the number of Pods whose requests
// ReschedulableRequests summed. If the two predicates ever drift, the logged
// "reschedulable Pod count" would explain a placeholder size it did not produce.
func TestCensusOnNodeCountedMatchesReschedulableRequests(t *testing.T) {
	// Three Pods of 100m each are counted; the rest are excluded for one reason
	// each. cpu therefore lands on exactly Counted × 100m.
	pods := []corev1.Pod{
		pod("w1", reqs(rl("cpu", "100m"))),
		pod("w2", reqs(rl("cpu", "100m"))),
		pod("w3", reqs(rl("cpu", "100m"))),
		pod("ds", ownedBy("DaemonSet"), reqs(rl("cpu", "100m"))),
		pod("static", mirror(), reqs(rl("cpu", "100m"))),
		pod("done", phase(corev1.PodSucceeded), reqs(rl("cpu", "100m"))),
		pod("pinned", hostnameSelectorPinned(), reqs(rl("cpu", "100m"))),
	}

	c := surge.CensusOnNode(pods, candidateNode)
	sum := surge.ReschedulableRequests(pods, candidateNode)

	if c.Counted != 3 {
		t.Fatalf("Counted: got %d, want 3", c.Counted)
	}
	wantEqual(t, sum, rl("cpu", "300m"))
}
