package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Issue #326: the aggregate reservation is fungible with the individual Pods'
// placement only on a host that can accept all of them, and a partially-filled
// host is where that breaks. WholeNode raises the placeholder to a whole node's
// worth of the dimensions bin-packing turns on, so a host carrying enough other
// workload to matter cannot take it.

func TestWholeNodeRaisesRequestsToTheProvisionableLimit(t *testing.T) {
	// allocatable 3770m/14570Mi − DaemonSet 300m/1082Mi = limit 3470m/13488Mi.
	got := surge.WholeNode(
		rl("cpu", "800m", "memory", "9400Mi"),
		rl("cpu", "3770m", "memory", "14570Mi"),
		rl("cpu", "300m", "memory", "1082Mi"),
		1,
	)

	if got.Cpu().String() != "3470m" {
		t.Errorf("cpu = %v, want 3470m (allocatable − DaemonSet)", got.Cpu())
	}
	if got.Memory().String() != "13488Mi" {
		t.Errorf("memory = %v, want 13488Mi", got.Memory())
	}
}

// The counterexample that made "a partially-filled host cannot take it" false
// for a drain-shaped projection: a drain that requests only cpu reserved only
// cpu, so a host filled with memory-only Pods still had the cpu free to absorb
// the placeholder — and one of those Pods can be exactly the one an evicted
// Pod's hostname anti-affinity or hostPort refuses. Both bin-packing dimensions
// are raised whenever there is workload to re-land, whatever the drain happens
// to declare.
func TestWholeNodeRaisesBothBinPackingDimensionsEvenWhenTheDrainNamesOne(t *testing.T) {
	got := surge.WholeNode(
		rl("cpu", "800m"),
		rl("cpu", "3770m", "memory", "14570Mi"),
		rl("cpu", "300m", "memory", "1082Mi"),
		1,
	)

	if got.Cpu().String() != "3470m" {
		t.Errorf("cpu = %v, want 3470m", got.Cpu())
	}
	if got.Memory().String() != "13488Mi" {
		t.Errorf("memory = %v, want 13488Mi — a cpu-only drain must still reserve the node's memory", got.Memory())
	}
}

// `pods` is a node allocatable dimension the scheduler counts, not a resource a
// container may request: inventing it would make the placeholder invalid.
func TestWholeNodeNeverRequestsPods(t *testing.T) {
	got := surge.WholeNode(
		rl("cpu", "800m"),
		rl("cpu", "3770m", "memory", "14570Mi", "pods", "110"),
		nil,
		1,
	)

	if _, ok := got[corev1.ResourcePods]; ok {
		t.Errorf("pods is not a container request and must never appear: %v", got)
	}
}

// Resources outside the two bin-packing dimensions are not invented either: a
// node's ephemeral storage or accelerators are not what an evicted Pod needs
// reserved unless it asked for them, and demanding all of a node's accelerators
// would block far more than the rotation.
func TestWholeNodeDoesNotInventNonBinPackingResources(t *testing.T) {
	got := surge.WholeNode(
		rl("cpu", "800m"),
		rl("cpu", "3770m", "memory", "14570Mi", "ephemeral-storage", "100Gi", "nvidia.com/gpu", "4"),
		nil,
		1,
	)

	if _, ok := got[corev1.ResourceEphemeralStorage]; ok {
		t.Errorf("ephemeral-storage must not be invented: %v", got)
	}
	if _, ok := got["nvidia.com/gpu"]; ok {
		t.Errorf("accelerators must not be invented: %v", got)
	}
}

// A resource the drain DOES request is still raised, so a Pod that asked for an
// accelerator gets the node's worth reserved with it.
func TestWholeNodeRaisesResourcesTheDrainRequests(t *testing.T) {
	got := surge.WholeNode(
		rl("cpu", "800m", "nvidia.com/gpu", "1"),
		rl("cpu", "3770m", "memory", "14570Mi", "nvidia.com/gpu", "4"),
		nil,
		1,
	)

	if q := got["nvidia.com/gpu"]; q.String() != "4" {
		t.Errorf("a requested accelerator must be raised to the node's worth, got %v", q)
	}
}

// A candidate with nothing to re-land has nothing to guarantee, so the mode
// reserves nothing rather than a whole node — the cheap, correct outcome. The
// count decides this, not the request sum: Pods with no resource requests are
// still workload that has to land somewhere.
func TestWholeNodeReservesNothingWithNoReschedulablePods(t *testing.T) {
	got := surge.WholeNode(nil, rl("cpu", "3770m", "memory", "14570Mi"), rl("cpu", "300m"), 0)

	if len(got) != 0 {
		t.Errorf("no reschedulable Pods must reserve nothing, got %v", got)
	}
}

// Pods that request nothing sum to nothing, but they still have to re-land — and
// a host that accepts them is exactly what the mode is for. The count, not the
// sum, is what says there is workload.
func TestWholeNodeReservesForZeroRequestPods(t *testing.T) {
	got := surge.WholeNode(nil, rl("cpu", "3770m", "memory", "14570Mi"), rl("cpu", "300m"), 3)

	if got.Cpu().String() != "3470m" {
		t.Errorf("cpu = %v, want 3470m for zero-request workload", got.Cpu())
	}
	if got.Memory().String() != "14570Mi" {
		t.Errorf("memory = %v, want 14570Mi", got.Memory())
	}
}

// Without a trustworthy ceiling there is nothing to raise to. Leaving the drain
// alone matches Clamp's rule for the same input and keeps make-before-break at
// the sum, rather than reserving zero.
func TestWholeNodeIsANoOpWithoutAllocatable(t *testing.T) {
	drain := rl("cpu", "800m", "memory", "9400Mi")

	got := surge.WholeNode(drain, nil, rl("cpu", "300m"), 1)

	if got.Cpu().String() != "800m" || got.Memory().String() != "9400Mi" {
		t.Errorf("with no allocatable the drain must pass through unchanged, got %v", got)
	}
}

// A resource the claim does not report has no known ceiling; raising it would be
// a guess, so it keeps the drain's own value.
func TestWholeNodeLeavesResourcesWithNoReportedCeiling(t *testing.T) {
	got := surge.WholeNode(
		rl("cpu", "800m", "example.com/fpga", "2"),
		rl("cpu", "3770m"),
		nil,
		1,
	)

	if got.Cpu().String() != "3770m" {
		t.Errorf("cpu = %v, want 3770m", got.Cpu())
	}
	if q := got["example.com/fpga"]; q.String() != "2" {
		t.Errorf("a resource with no reported ceiling keeps the drain value, got %v", q)
	}
}

// DaemonSet overhead can exhaust allocatable. Raising to a non-positive limit
// would reserve nothing at all and satisfy surge_ready with an empty placeholder
// — a silent break-before-make. The drain is kept so Clamp refuses it and the
// rotation rolls back, exactly as it does without this mode.
func TestWholeNodeKeepsTheDrainWhenTheLimitIsNonPositive(t *testing.T) {
	got := surge.WholeNode(
		rl("cpu", "800m"),
		rl("cpu", "300m"),
		rl("cpu", "300m"),
		1,
	)

	if got.Cpu().String() != "800m" {
		t.Errorf("a non-positive limit must leave the drain for Clamp to refuse, got %v", got.Cpu())
	}
}

// The drain can already exceed the limit — the #224 case. Whole-node must not
// lower it here: Clamp owns that reduction and the shortfall reporting with it.
func TestWholeNodeNeverLowersADrainAboveTheLimit(t *testing.T) {
	got := surge.WholeNode(
		rl("memory", "13600Mi"),
		rl("memory", "14570Mi"),
		rl("memory", "1082Mi"),
		1,
	)

	if got.Memory().String() != "13600Mi" {
		t.Errorf("whole-node must leave a drain above the limit to Clamp, got %v", got.Memory())
	}
}

// The input must not be mutated: the caller reports the real drain alongside the
// raised request on the placeholder line.
func TestWholeNodeDoesNotMutateTheDrain(t *testing.T) {
	drain := rl("cpu", "800m")

	surge.WholeNode(drain, rl("cpu", "3770m", "memory", "14570Mi"), nil, 1)

	if drain.Cpu().String() != "800m" {
		t.Errorf("the drain must be left intact for reporting, got %v", drain.Cpu())
	}
	if _, ok := drain[corev1.ResourceMemory]; ok {
		t.Errorf("the drain must not gain the memory the reservation added: %v", drain)
	}
}
