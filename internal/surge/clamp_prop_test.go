package surge_test

import (
	"math/rand"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Property: for any candidate the scheduler admitted (reschedulable + daemonSet
// <= nodeAllocatable), a fired clamp's shortfall never exceeds the band. This is
// the identity the spec asserts; fuzz it rather than trust the worked example.
func TestClampShortfallNeverExceedsBandProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	q := func(v int64) resource.Quantity { return *resource.NewQuantity(v, resource.BinarySI) }
	for range 20000 {
		nodeAlloc := int64(rng.Intn(20000) + 1)   // real node allocatable
		claimAlloc := int64(rng.Intn(20000) + 1)  // Karpenter cached estimate
		ds := int64(rng.Intn(int(nodeAlloc) + 1)) // DS <= node
		// reschedulable is whatever the scheduler let land beside DS on the node
		maxResched := nodeAlloc - ds
		if maxResched < 0 {
			continue
		}
		resched := int64(rng.Intn(int(maxResched) + 1))

		req := corev1.ResourceList{corev1.ResourceMemory: q(resched)}
		alloc := corev1.ResourceList{corev1.ResourceMemory: q(claimAlloc)}
		dsl := corev1.ResourceList{corev1.ResourceMemory: q(ds)}
		res := surge.Clamp(req, alloc, dsl)
		if !res.Clamped {
			continue
		}
		band := surge.Band(
			corev1.ResourceList{corev1.ResourceMemory: q(nodeAlloc)},
			alloc,
		)
		if name, ok := surge.ExceedsBand(res.Shortfall, band); ok {
			short := res.Shortfall[corev1.ResourceMemory]
			b := band[corev1.ResourceMemory]
			t.Fatalf("identity broken: shortfall %s > band %s on %s (node=%d claim=%d ds=%d resched=%d)",
				short.String(), b.String(), name, nodeAlloc, claimAlloc, ds, resched)
		}
	}
}
