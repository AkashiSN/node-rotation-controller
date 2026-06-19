package surge_test

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Issue #80: the controller sizes the placeholder from the reschedulable Pod
// requests on the candidate node, which requires cluster-wide Pod visibility.
// Today that is a full cache-backed Pod list (allPods) handed to
// surge.ReschedulableRequests in the headroom check and placeholder creation,
// and to the reap guard's hostsRealPods. These benchmarks measure the pure
// scan/filter/aggregate cost of that path over synthetic cluster snapshots at
// representative Pod counts so the doc can record a decision.
//
// The slice is what allPods() returns; the informer-cache memory cost is
// proportional to the same Pod set and is discussed in docs/perf alongside
// these numbers. We benchmark the exported function so the result tracks the
// real hot path rather than a stand-in.

// clusterSnapshot builds podCount Pods spread across many namespaces, mimicking
// a real cluster: only a fraction land on the candidate node, and that fraction
// carries a realistic mix (plain workloads, a DaemonSet pod, a completed pod, a
// hostname-pinned pod). Everything else is scheduled on other nodes — the bulk
// the scan must skip on every call.
func clusterSnapshot(podCount int) []corev1.Pod {
	const (
		namespaces      = 200 // arbitrary-namespace spread (cross-namespace requirement)
		podsPerNode     = 30  // typical reschedulable density on the candidate node
		otherNodes      = 500
		standardRequest = "100m"
	)
	pods := make([]corev1.Pod, 0, podCount)

	// The candidate node's realistic workload mix. These are the only Pods that
	// pass the reschedulable filter (minus the excluded ones).
	onCandidate := podsPerNode
	if onCandidate > podCount {
		onCandidate = podCount
	}
	for i := 0; i < onCandidate; i++ {
		opts := []podOpt{
			onNode(candidateNode),
			reqs(rl("cpu", standardRequest, "memory", "128Mi")),
		}
		switch i % 10 {
		case 0:
			opts = append(opts, ownedBy("DaemonSet")) // excluded as infra
		case 1:
			opts = append(opts, phase(corev1.PodSucceeded)) // excluded as completed
		case 2:
			opts = append(opts, hostnamePinned()) // excluded as node-pinned
		default:
			opts = append(opts, ownedBy("ReplicaSet")) // reschedulable workload
		}
		p := pod(fmt.Sprintf("cand-pod-%d", i), opts...)
		p.Namespace = fmt.Sprintf("ns-%d", i%namespaces)
		pods = append(pods, p)
	}

	// The remaining Pods live on other nodes across many namespaces — the bulk
	// the scan rejects by spec.nodeName on the very first comparison.
	for i := onCandidate; i < podCount; i++ {
		p := pod(fmt.Sprintf("other-pod-%d", i),
			onNode(fmt.Sprintf("node-other-%d", i%otherNodes)),
			ownedBy("ReplicaSet"),
			reqs(rl("cpu", standardRequest, "memory", "128Mi")),
		)
		p.Namespace = fmt.Sprintf("ns-%d", i%namespaces)
		pods = append(pods, p)
	}
	return pods
}

var benchPodCounts = []int{1_000, 10_000, 50_000}

// BenchmarkReschedulableRequests measures the placeholder-sizing scan over a
// full cluster snapshot — the cost paid in the headroom check and on every
// placeholder creation.
func BenchmarkReschedulableRequests(b *testing.B) {
	for _, n := range benchPodCounts {
		pods := clusterSnapshot(n)
		b.Run(fmt.Sprintf("pods=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var sink corev1.ResourceList
			for i := 0; i < b.N; i++ {
				sink = surge.ReschedulableRequests(pods, candidateNode)
			}
			_ = sink
		})
	}
}

// BenchmarkIsInfraOrCompleted isolates the per-Pod exclusion predicate the scan
// applies, so the doc can separate filter cost from aggregation cost.
func BenchmarkIsInfraOrCompleted(b *testing.B) {
	pods := clusterSnapshot(1_000)
	b.ReportAllocs()
	b.ResetTimer()
	var hits int
	for i := 0; i < b.N; i++ {
		for j := range pods {
			if surge.IsInfraOrCompleted(&pods[j]) {
				hits++
			}
		}
	}
	_ = hits
}

// scopedSnapshot returns only the Pods on the candidate node — what a
// spec.nodeName field index would deliver to ReschedulableRequests. It lets the
// benchmark contrast the full-list scan against the would-be indexed path
// (issue #80, the field-index option) on identical inputs.
func scopedSnapshot(full []corev1.Pod) []corev1.Pod {
	scoped := make([]corev1.Pod, 0, 64)
	for i := range full {
		if full[i].Spec.NodeName == candidateNode {
			scoped = append(scoped, full[i])
		}
	}
	return scoped
}

// BenchmarkReschedulableRequestsScoped measures the same aggregation when the
// input is pre-filtered to the candidate node (the field-index option). The
// delta against BenchmarkReschedulableRequests is the cost a spec.nodeName index
// would remove from each call.
func BenchmarkReschedulableRequestsScoped(b *testing.B) {
	for _, n := range benchPodCounts {
		pods := scopedSnapshot(clusterSnapshot(n))
		b.Run(fmt.Sprintf("pods=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var sink corev1.ResourceList
			for i := 0; i < b.N; i++ {
				sink = surge.ReschedulableRequests(pods, candidateNode)
			}
			_ = sink
		})
	}
}

// approxSnapshotBytes is a coarse proxy for the informer-cache footprint of a
// snapshot: the controller-runtime cache holds the full Pod objects this slice
// represents. It is reported via a sub-benchmark metric so the doc can cite an
// order-of-magnitude memory figure alongside latency.
func BenchmarkSnapshotFootprint(b *testing.B) {
	for _, n := range benchPodCounts {
		b.Run(fmt.Sprintf("pods=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				pods := clusterSnapshot(n)
				// touch a field so the build is not optimized away
				if len(pods) > 0 {
					_ = pods[0].Name
				}
			}
		})
	}
}
