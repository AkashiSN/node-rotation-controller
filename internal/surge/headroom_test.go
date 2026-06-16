package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// poolWith builds a NodePool with the given spec.limits and status.resources.
func poolWith(limits, provisioned corev1.ResourceList) *karpv1.NodePool {
	p := &karpv1.NodePool{}
	p.Spec.Limits = karpv1.Limits(limits)
	p.Status.Resources = provisioned
	return p
}

func TestFitsHeadroomWithinBudget(t *testing.T) {
	pool := poolWith(rl("cpu", "100", "memory", "200Gi"), rl("cpu", "90", "memory", "180Gi"))
	if !surge.FitsHeadroom(pool, rl("cpu", "8", "memory", "16Gi")) {
		t.Error("request within remaining budget should fit")
	}
}

func TestFitsHeadroomExactFit(t *testing.T) {
	pool := poolWith(rl("cpu", "100"), rl("cpu", "92"))
	if !surge.FitsHeadroom(pool, rl("cpu", "8")) {
		t.Error("a request exactly equal to remaining headroom should fit")
	}
}

func TestFitsHeadroomOverBudget(t *testing.T) {
	pool := poolWith(rl("cpu", "100"), rl("cpu", "95"))
	if surge.FitsHeadroom(pool, rl("cpu", "8")) {
		t.Error("a request exceeding remaining headroom must not fit")
	}
}

func TestFitsHeadroomAnyResourceOverBudgetFails(t *testing.T) {
	// cpu fits but memory does not → the whole request must be rejected.
	pool := poolWith(rl("cpu", "100", "memory", "10Gi"), rl("cpu", "10", "memory", "9Gi"))
	if surge.FitsHeadroom(pool, rl("cpu", "1", "memory", "4Gi")) {
		t.Error("if any resource is over budget the request must not fit")
	}
}

func TestFitsHeadroomUnlimitedResourcePasses(t *testing.T) {
	// No limit on memory → memory is unbounded; only cpu is checked.
	pool := poolWith(rl("cpu", "100"), rl("cpu", "10"))
	if !surge.FitsHeadroom(pool, rl("cpu", "5", "memory", "999Gi")) {
		t.Error("a resource with no NodePool limit is unbounded and should pass")
	}
}

func TestFitsHeadroomNoLimitsPasses(t *testing.T) {
	// An empty/absent limits map means no budget ceiling at all (spec §3.3).
	pool := poolWith(nil, rl("cpu", "1000"))
	if !surge.FitsHeadroom(pool, rl("cpu", "8")) {
		t.Error("with no limits configured every request fits")
	}
}

func TestFitsHeadroomProvisionedAbsentTreatedAsZero(t *testing.T) {
	pool := poolWith(rl("cpu", "10"), nil)
	if !surge.FitsHeadroom(pool, rl("cpu", "10")) {
		t.Error("absent status.resources means zero provisioned")
	}
	if surge.FitsHeadroom(pool, rl("cpu", "11")) {
		t.Error("request above the full limit with nothing provisioned must not fit")
	}
}
