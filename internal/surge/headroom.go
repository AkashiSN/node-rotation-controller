package surge

import (
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// FitsHeadroom reports whether requests fit within the NodePool's remaining
// resource budget — spec.limits minus the already-provisioned status.resources
// (spec §5.2 step 3). A resource with no configured limit is unbounded and never
// blocks; the request fits only when every limited resource has room.
//
// This is the candidate-dependent surge_headroom gate: it is checked after the
// candidate is picked, because the request sum that sizes it is the placeholder's
// (spec §3.3). It is conservative — the capacity-absorb path consumes no new
// budget, but v1 still requires the headroom before starting.
func FitsHeadroom(pool *karpv1.NodePool, requests corev1.ResourceList) bool {
	limits := corev1.ResourceList(pool.Spec.Limits)
	provisioned := pool.Status.Resources
	for name, want := range requests {
		limit, ok := limits[name]
		if !ok {
			continue // no ceiling on this resource — unbounded
		}
		remaining := limit.DeepCopy()
		if used, ok := provisioned[name]; ok {
			remaining.Sub(used)
		}
		if want.Cmp(remaining) > 0 {
			return false
		}
	}
	return true
}
