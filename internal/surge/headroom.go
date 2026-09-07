package surge

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// HeadroomResult is the outcome of Headroom: whether the placeholder fits the
// NodePool's remaining budget and, when it does not, the resource that blocked
// it together with the three numbers that decide it.
//
// The detail exists because the block used to be actionable only by reading the
// NodePool by hand: the log line said the surge could not proceed and nothing
// said which resource, by how much, or against which ceiling (issue #326).
type HeadroomResult struct {
	// Fits is true when every limited resource has room for the request.
	Fits bool
	// Resource names the first resource (in sorted order) that did not fit.
	// Empty when Fits.
	Resource corev1.ResourceName
	// Want is the request on Resource; Remaining is limit − provisioned; Limit is
	// the configured ceiling. Zero values when Fits.
	Want, Remaining, Limit resource.Quantity
}

// Headroom reports whether requests fit within the NodePool's remaining resource
// budget — spec.limits minus the already-provisioned status.resources (spec §5.2
// step 3). A resource with no configured limit is unbounded and never blocks; the
// request fits only when every limited resource has room.
//
// This is the candidate-dependent surge_headroom gate: it is checked after the
// candidate is picked, because the request sum that sizes it is the placeholder's
// (spec §3.3). It is conservative — the capacity-absorb path consumes no new
// budget, but v1 still requires the headroom before starting.
//
// Resources are examined in sorted order so a block names the same resource on
// every pass whatever the map iteration order. The Event that reports it is
// deduplicated on its message, so an unstable name would re-fire it forever —
// the same reason Clamp sorts before it refuses.
func Headroom(pool *karpv1.NodePool, requests corev1.ResourceList) HeadroomResult {
	limits := corev1.ResourceList(pool.Spec.Limits)
	provisioned := pool.Status.Resources
	for _, name := range slices.Sorted(maps.Keys(requests)) {
		want := requests[name]
		limit, ok := limits[name]
		if !ok {
			continue // no ceiling on this resource — unbounded
		}
		remaining := limit.DeepCopy()
		if used, ok := provisioned[name]; ok {
			remaining.Sub(used)
		}
		if want.Cmp(remaining) > 0 {
			return HeadroomResult{Resource: name, Want: want, Remaining: remaining, Limit: limit}
		}
	}
	return HeadroomResult{Fits: true}
}
