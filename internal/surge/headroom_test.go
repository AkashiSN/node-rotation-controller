package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// mustQ parses a quantity for comparison in the assertions below.
func mustQ(s string) resource.Quantity { return resource.MustParse(s) }

// poolWith builds a NodePool with the given spec.limits and status.resources.
func poolWith(limits, provisioned corev1.ResourceList) *karpv1.NodePool {
	p := &karpv1.NodePool{}
	p.Spec.Limits = karpv1.Limits(limits)
	p.Status.Resources = provisioned
	return p
}

func TestHeadroomWithinBudget(t *testing.T) {
	pool := poolWith(rl("cpu", "100", "memory", "200Gi"), rl("cpu", "90", "memory", "180Gi"))
	if !surge.Headroom(pool, rl("cpu", "8", "memory", "16Gi")).Fits {
		t.Error("request within remaining budget should fit")
	}
}

func TestHeadroomExactFit(t *testing.T) {
	pool := poolWith(rl("cpu", "100"), rl("cpu", "92"))
	if !surge.Headroom(pool, rl("cpu", "8")).Fits {
		t.Error("a request exactly equal to remaining headroom should fit")
	}
}

func TestHeadroomOverBudget(t *testing.T) {
	pool := poolWith(rl("cpu", "100"), rl("cpu", "95"))
	if surge.Headroom(pool, rl("cpu", "8")).Fits {
		t.Error("a request exceeding remaining headroom must not fit")
	}
}

func TestHeadroomAnyResourceOverBudgetFails(t *testing.T) {
	// cpu fits but memory does not → the whole request must be rejected.
	pool := poolWith(rl("cpu", "100", "memory", "10Gi"), rl("cpu", "10", "memory", "9Gi"))
	if surge.Headroom(pool, rl("cpu", "1", "memory", "4Gi")).Fits {
		t.Error("if any resource is over budget the request must not fit")
	}
}

func TestHeadroomUnlimitedResourcePasses(t *testing.T) {
	// No limit on memory → memory is unbounded; only cpu is checked.
	pool := poolWith(rl("cpu", "100"), rl("cpu", "10"))
	if !surge.Headroom(pool, rl("cpu", "5", "memory", "999Gi")).Fits {
		t.Error("a resource with no NodePool limit is unbounded and should pass")
	}
}

func TestHeadroomNoLimitsPasses(t *testing.T) {
	// An empty/absent limits map means no budget ceiling at all (spec §3.3).
	pool := poolWith(nil, rl("cpu", "1000"))
	if !surge.Headroom(pool, rl("cpu", "8")).Fits {
		t.Error("with no limits configured every request fits")
	}
}

func TestHeadroomProvisionedAbsentTreatedAsZero(t *testing.T) {
	pool := poolWith(rl("cpu", "10"), nil)
	if !surge.Headroom(pool, rl("cpu", "10")).Fits {
		t.Error("absent status.resources means zero provisioned")
	}
	if surge.Headroom(pool, rl("cpu", "11")).Fits {
		t.Error("request above the full limit with nothing provisioned must not fit")
	}
}

// A block an operator cannot act on is most of why this gate was invisible
// (issue #326): the result names the resource that did not fit and the two
// numbers that decide it, so the Event can say what to raise and by how much.
func TestHeadroomNamesTheBlockingResourceAndItsNumbers(t *testing.T) {
	pool := poolWith(rl("cpu", "100"), rl("cpu", "95"))

	got := surge.Headroom(pool, rl("cpu", "8"))

	if got.Fits {
		t.Fatal("the request must not fit")
	}
	if got.Resource != corev1.ResourceCPU {
		t.Errorf("blocking resource = %q, want cpu", got.Resource)
	}
	if got.Want.Cmp(mustQ("8")) != 0 {
		t.Errorf("want = %v, want 8", got.Want)
	}
	if got.Remaining.Cmp(mustQ("5")) != 0 {
		t.Errorf("remaining = %v, want 5 (limit 100 − provisioned 95)", got.Remaining)
	}
	if got.Limit.Cmp(mustQ("100")) != 0 {
		t.Errorf("limit = %v, want 100", got.Limit)
	}
}

// Two resources over budget must name the same one on every pass, whatever the
// map iteration order — the dedup that decides whether to re-fire an Event
// compares the message, so an unstable name would re-fire it forever.
func TestHeadroomNamesTheSameResourceOnEveryPass(t *testing.T) {
	pool := poolWith(rl("cpu", "10", "memory", "10Gi"), rl("cpu", "10", "memory", "10Gi"))

	first := surge.Headroom(pool, rl("cpu", "1", "memory", "1Gi")).Resource
	for range 50 {
		if got := surge.Headroom(pool, rl("cpu", "1", "memory", "1Gi")).Resource; got != first {
			t.Fatalf("blocking resource is unstable: got %q, first %q", got, first)
		}
	}
	if first != corev1.ResourceCPU {
		t.Errorf("with cpu and memory both over budget the sorted-first resource is cpu, got %q", first)
	}
}

// A fitting request has no blocking resource to name.
func TestHeadroomFitsCarriesNoResource(t *testing.T) {
	pool := poolWith(rl("cpu", "100"), rl("cpu", "10"))

	got := surge.Headroom(pool, rl("cpu", "5"))

	if !got.Fits {
		t.Fatal("the request must fit")
	}
	if got.Resource != "" {
		t.Errorf("a fitting result must name no resource, got %q", got.Resource)
	}
}
