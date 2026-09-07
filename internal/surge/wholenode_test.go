package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Issue #326: the aggregate reservation is fungible with the individual Pods'
// placement only on a host that can accept all of them, and a partially-filled
// host is where that breaks. WholeNode raises the placeholder to a whole node's
// worth of what the drain needs, so only a host with that much free space can
// take it.

func TestWholeNodeRaisesRequestsToTheProvisionableLimit(t *testing.T) {
	// allocatable 3770m/14570Mi − DaemonSet 300m/1082Mi = limit 3470m/13488Mi.
	got := surge.WholeNode(
		rl("cpu", "800m", "memory", "9400Mi"),
		rl("cpu", "3770m", "memory", "14570Mi"),
		rl("cpu", "300m", "memory", "1082Mi"),
	)

	if got.Cpu().String() != "3470m" {
		t.Errorf("cpu = %v, want 3470m (allocatable − DaemonSet)", got.Cpu())
	}
	if got.Memory().String() != "13488Mi" {
		t.Errorf("memory = %v, want 13488Mi", got.Memory())
	}
}

// The mode demands a whole node's worth of WHAT THE DRAIN NEEDS, not of every
// resource the node reports. A resource the evicted Pods do not request is not
// one they need reserved — and `pods`, which allocatable always carries, is not
// a resource a container can request at all.
func TestWholeNodeLeavesResourcesTheDrainDoesNotRequest(t *testing.T) {
	got := surge.WholeNode(
		rl("cpu", "800m"),
		rl("cpu", "3770m", "memory", "14570Mi", "pods", "110"),
		rl("cpu", "300m"),
	)

	if _, ok := got[corev1.ResourceMemory]; ok {
		t.Errorf("memory must not be invented into the request: %v", got)
	}
	if _, ok := got[corev1.ResourcePods]; ok {
		t.Errorf("pods is not a container request and must never appear: %v", got)
	}
	if got.Cpu().String() != "3470m" {
		t.Errorf("cpu = %v, want 3470m", got.Cpu())
	}
}

// A candidate with nothing to re-land has nothing to guarantee, so the mode
// reserves nothing rather than a whole node — the cheap, correct outcome.
func TestWholeNodeReservesNothingForAnEmptyDrain(t *testing.T) {
	got := surge.WholeNode(nil, rl("cpu", "3770m"), rl("cpu", "300m"))

	if len(got) != 0 {
		t.Errorf("an empty drain must reserve nothing, got %v", got)
	}
}

// Without a trustworthy ceiling there is nothing to raise to. Leaving the drain
// alone matches Clamp's rule for the same input and keeps make-before-break at
// the sum, rather than reserving zero.
func TestWholeNodeIsANoOpWithoutAllocatable(t *testing.T) {
	drain := rl("cpu", "800m", "memory", "9400Mi")

	got := surge.WholeNode(drain, nil, rl("cpu", "300m"))

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
		nil, // no DaemonSet overhead here, so the cpu ceiling is allocatable itself
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
	)

	if got.Cpu().String() != "800m" {
		t.Errorf("a non-positive limit must leave the drain for Clamp to refuse, got %v", got.Cpu())
	}
}

// The drain can already exceed the limit — the #224 case. Whole-node must not
// lower it here: Clamp owns that reduction and its reporting.
func TestWholeNodeNeverLowersADrainAboveTheLimit(t *testing.T) {
	got := surge.WholeNode(
		rl("memory", "13600Mi"),
		rl("memory", "14570Mi"),
		rl("memory", "1082Mi"),
	)

	if got.Memory().String() != "13600Mi" {
		t.Errorf("whole-node must leave a drain above the limit to Clamp, got %v", got.Memory())
	}
}

// The input must not be mutated: the caller reports the real drain alongside the
// raised request on the placeholder line.
func TestWholeNodeDoesNotMutateTheDrain(t *testing.T) {
	drain := rl("cpu", "800m")

	surge.WholeNode(drain, rl("cpu", "3770m"), nil)

	if drain.Cpu().String() != "800m" {
		t.Errorf("the drain must be left intact for reporting, got %v", drain.Cpu())
	}
}
