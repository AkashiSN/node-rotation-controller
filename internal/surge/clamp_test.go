package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

func TestDaemonSetRequestsSumsOnlyDaemonSetPods(t *testing.T) {
	pods := []corev1.Pod{
		pod("workload", reqs(rl("cpu", "500m", "memory", "1Gi"))),
		pod("ds-a", reqs(rl("cpu", "200m", "memory", "600Mi")), ownedBy("DaemonSet")),
		pod("ds-b", reqs(rl("cpu", "100m", "memory", "482Mi")), ownedBy("DaemonSet")),
		pod("ds-elsewhere", reqs(rl("cpu", "999")), ownedBy("DaemonSet"), onNode("other-node")),
		pod("mirror", reqs(rl("cpu", "9")), mirror()),
	}
	got := surge.DaemonSetRequests(pods, candidateNode)
	// Only the two DaemonSet Pods on the candidate node count; mirror/workload and
	// off-node DaemonSets are excluded — the same effective-request algorithm as
	// ReschedulableRequests.
	wantEqual(t, got, rl("cpu", "300m", "memory", "1082Mi"))
}

func TestDaemonSetRequestsExcludesCompletedDaemonSetPods(t *testing.T) {
	// A Succeeded/Failed DaemonSet Pod is still bound to the node but consumes no
	// allocatable, so kube-scheduler does not count it. Counting it here would
	// over-state the subtrahend, shrink the limit, and let the shortfall exceed the
	// per-AZ band that bounds it — the identity ReschedulableRequests <=
	// Node.allocatable - DaemonSetRequests only holds when both sides count the
	// same Pods.
	pods := []corev1.Pod{
		pod("ds-running", reqs(rl("cpu", "300m", "memory", "1082Mi")), ownedBy("DaemonSet")),
		pod("ds-succeeded", reqs(rl("cpu", "500m", "memory", "4Gi")), ownedBy("DaemonSet"), phase(corev1.PodSucceeded)),
		pod("ds-failed", reqs(rl("cpu", "500m", "memory", "4Gi")), ownedBy("DaemonSet"), phase(corev1.PodFailed)),
	}
	got := surge.DaemonSetRequests(pods, candidateNode)
	wantEqual(t, got, rl("cpu", "300m", "memory", "1082Mi"))
}

func TestBandIsTheAllocatableDiscrepancy(t *testing.T) {
	// The band is the per-AZ capacity discrepancy: what the real node reports minus
	// what Karpenter caches for its instance type. It bounds the clamp's shortfall.
	// A resource the claim reports larger than the node (an over-stating cache)
	// yields no band rather than a negative one.
	got := surge.Band(
		rl("cpu", "3770m", "memory", "15092676Ki", "pods", "110"),
		rl("cpu", "3770m", "memory", "14919360Ki"),
	)
	wantEqual(t, got, rl("cpu", "0", "memory", "173316Ki"))
}

func TestExceedsBandNamesTheOffendingResource(t *testing.T) {
	band := rl("cpu", "0", "memory", "169Mi")
	if name, ok := surge.ExceedsBand(rl("memory", "112Mi"), band); ok {
		t.Fatalf("a shortfall inside the band is the expected case, got %s", name)
	}
	name, ok := surge.ExceedsBand(rl("memory", "512Mi"), band)
	if !ok || name != corev1.ResourceMemory {
		t.Fatalf("want memory over band, got %s ok=%v", name, ok)
	}
	// A shortfall on a resource with no measurable band exceeds it by definition.
	if _, ok := surge.ExceedsBand(rl("cpu", "10m"), band); !ok {
		t.Error("a positive shortfall against a zero band must be reported")
	}
}

func TestClampFiresWhenRequestsExceedProvisionableLimit(t *testing.T) {
	// The issue's hb5x4 case: allocatable 14570Mi, DS 1082Mi → limit 13488Mi,
	// while the reschedulable drain is 13600Mi. Memory clamps by 112Mi; cpu fits.
	res := surge.Clamp(
		rl("cpu", "1200m", "memory", "13600Mi"),
		rl("cpu", "3770m", "memory", "14570Mi", "pods", "110"),
		rl("cpu", "300m", "memory", "1082Mi"),
	)
	if !res.Clamped {
		t.Fatalf("clamp must fire: %+v", res)
	}
	wantEqual(t, res.Requests, rl("cpu", "1200m", "memory", "13488Mi"))
	wantEqual(t, res.Limit, rl("memory", "13488Mi"))
	wantEqual(t, res.Shortfall, rl("memory", "112Mi"))
}

func TestClampSilentWhenDrainFitsWithinLimit(t *testing.T) {
	req := rl("cpu", "1200m", "memory", "12600Mi")
	res := surge.Clamp(
		req,
		rl("cpu", "3770m", "memory", "14570Mi"),
		rl("cpu", "300m", "memory", "1082Mi"),
	)
	if res.Clamped {
		t.Fatalf("clamp must not fire when the drain fits: %+v", res)
	}
	wantEqual(t, res.Requests, req)
	if res.Limit != nil || res.Shortfall != nil {
		t.Errorf("silent path must carry no limit/shortfall: %+v", res)
	}
}

func TestClampIsPerResourceIndependent(t *testing.T) {
	// cpu clamps while memory keeps its headroom — the clamp is per resource.
	res := surge.Clamp(
		rl("cpu", "4000m", "memory", "10000Mi"),
		rl("cpu", "3770m", "memory", "14570Mi"),
		rl("cpu", "300m", "memory", "1082Mi"),
	)
	if !res.Clamped {
		t.Fatalf("clamp must fire on cpu: %+v", res)
	}
	wantEqual(t, res.Requests, rl("cpu", "3470m", "memory", "10000Mi"))
	wantEqual(t, res.Limit, rl("cpu", "3470m"))
	wantEqual(t, res.Shortfall, rl("cpu", "530m"))
}

func TestClampNoOpWhenAllocatableEmpty(t *testing.T) {
	req := rl("cpu", "1200m", "memory", "13600Mi")
	for _, tc := range []struct {
		name        string
		allocatable corev1.ResourceList
	}{
		{"nil", nil},
		{"empty", corev1.ResourceList{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := surge.Clamp(req, tc.allocatable, rl("memory", "1082Mi"))
			if res.Clamped {
				t.Fatalf("empty allocatable must be a no-op, not a clamp: %+v", res)
			}
			// The full drain is preserved — never clamped toward zero.
			wantEqual(t, res.Requests, req)
		})
	}
}

func TestClampRefusedWhenLimitNonPositiveWithPositiveDemand(t *testing.T) {
	// DaemonSet overhead at or above allocatable leaves no room for any placeholder,
	// so no clamp value can induce a node. Clamping to zero would bind a
	// zero-request Pod to any existing node and satisfy surge_ready with nothing
	// reserved — a silent break-before-make. Refuse instead: the full drain is
	// preserved, the placeholder stays unschedulable, and the rotation rolls back.
	// Operators who want surge-less rotation opt into surge.forcefulFallback.
	for _, tc := range []struct {
		name      string
		daemonSet corev1.ResourceList
	}{
		{"overhead exceeds allocatable", rl("memory", "1500Mi")},
		{"overhead equals allocatable", rl("memory", "1000Mi")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := rl("memory", "500Mi")
			res := surge.Clamp(req, rl("memory", "1000Mi"), tc.daemonSet)
			if !res.Refused {
				t.Fatalf("clamp must be refused, not applied: %+v", res)
			}
			if res.Clamped {
				t.Errorf("a refused clamp never reports Clamped: %+v", res)
			}
			wantEqual(t, res.Requests, req)
		})
	}
}

func TestClampNotRefusedWhenNonPositiveLimitHasNoDemand(t *testing.T) {
	// memory has no room left, but nothing requests memory: there is nothing to
	// refuse. cpu still clamps normally.
	res := surge.Clamp(
		rl("cpu", "4000m"),
		rl("cpu", "3770m", "memory", "1000Mi"),
		rl("cpu", "300m", "memory", "1500Mi"),
	)
	if res.Refused {
		t.Fatalf("no positive demand on the exhausted resource: %+v", res)
	}
	wantEqual(t, res.Requests, rl("cpu", "3470m"))
}

func TestClampLeavesResourcesAbsentFromAllocatableUntouched(t *testing.T) {
	// An extended resource the NodeClaim does not report allocatable for has no
	// known ceiling, so it must pass through unclamped.
	res := surge.Clamp(
		rl("cpu", "1200m", "example.com/gpu", "2"),
		rl("cpu", "3770m", "memory", "14570Mi"),
		rl("cpu", "300m"),
	)
	if res.Clamped {
		t.Fatalf("no resource exceeds its limit: %+v", res)
	}
	wantEqual(t, res.Requests, rl("cpu", "1200m", "example.com/gpu", "2"))
}
