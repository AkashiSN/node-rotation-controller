package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

const candidateNode = "node-cand"

// podOpt mutates a Pod during construction.
type podOpt func(*corev1.Pod)

// rl is a terse corev1.ResourceList literal: rl("cpu", "500m", "memory", "1Gi").
func rl(kv ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return out
}

// reqs sets a single regular container's requests.
func reqs(r corev1.ResourceList) podOpt {
	return func(p *corev1.Pod) {
		p.Spec.Containers = []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: r}}}
	}
}

func onNode(name string) podOpt {
	return func(p *corev1.Pod) { p.Spec.NodeName = name }
}

func ownedBy(kind string) podOpt {
	return func(p *corev1.Pod) {
		ctrl := true
		p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{Kind: kind, Controller: &ctrl})
	}
}

// ownedByNonController adds an owner reference that is NOT the controlling one
// (Controller unset), mirroring a stray cross-reference rather than ownership.
func ownedByNonController(kind string) podOpt {
	return func(p *corev1.Pod) {
		p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{Kind: kind})
	}
}

func mirror() podOpt {
	return func(p *corev1.Pod) {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[corev1.MirrorPodAnnotationKey] = "abc123"
	}
}

func phase(ph corev1.PodPhase) podOpt {
	return func(p *corev1.Pod) { p.Status.Phase = ph }
}

// hostnamePinned adds a required nodeAffinity term pinning the Pod to one host.
func hostnamePinned() podOpt {
	return func(p *corev1.Pod) {
		p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      corev1.LabelHostname,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{candidateNode},
				}},
			}}},
		}}
	}
}

// hostnameSelectorPinned pins via a nodeSelector rather than affinity.
func hostnameSelectorPinned() podOpt {
	return func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{corev1.LabelHostname: candidateNode}
	}
}

// initContainer adds a plain (non-restartable) init container.
func initContainer(name string, r corev1.ResourceList) podOpt {
	return func(p *corev1.Pod) {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
			Name: name, Resources: corev1.ResourceRequirements{Requests: r},
		})
	}
}

// sidecar adds a restartable (RestartPolicy=Always) init container.
func sidecar(name string, r corev1.ResourceList) podOpt {
	always := corev1.ContainerRestartPolicyAlways
	return func(p *corev1.Pod) {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
			Name: name, RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: r},
		})
	}
}

func overhead(r corev1.ResourceList) podOpt {
	return func(p *corev1.Pod) { p.Spec.Overhead = r }
}

// pod builds a Running Pod scheduled on candidateNode with the given name.
// Defaults make it a plain reschedulable workload Pod; opts override.
func pod(name string, opts ...podOpt) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{NodeName: candidateNode},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// wantEqual asserts a ResourceList equals the expected quantities.
func wantEqual(t *testing.T, got, want corev1.ResourceList) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("resource count: got %v, want %v", got, want)
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Fatalf("missing resource %s in %v", k, got)
		}
		if g.Cmp(w) != 0 {
			t.Errorf("resource %s: got %s, want %s", k, g.String(), w.String())
		}
	}
}

func TestReschedulableRequestsSumsWorkloadPods(t *testing.T) {
	pods := []corev1.Pod{
		pod("a", reqs(rl("cpu", "500m", "memory", "1Gi"))),
		pod("b", reqs(rl("cpu", "250m", "memory", "512Mi"))),
	}
	got := surge.ReschedulableRequests(pods, candidateNode)
	wantEqual(t, got, rl("cpu", "750m", "memory", "1536Mi"))
}

func TestReschedulableRequestsExcludesOtherNodes(t *testing.T) {
	pods := []corev1.Pod{
		pod("here", reqs(rl("cpu", "500m"))),
		pod("elsewhere", reqs(rl("cpu", "999")), onNode("other-node")),
	}
	got := surge.ReschedulableRequests(pods, candidateNode)
	wantEqual(t, got, rl("cpu", "500m"))
}

func TestReschedulableRequestsExclusions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  podOpt
	}{
		{"daemonset", ownedBy("DaemonSet")},
		{"mirror", mirror()},
		{"succeeded", phase(corev1.PodSucceeded)},
		{"failed", phase(corev1.PodFailed)},
		{"hostname-affinity-pinned", hostnamePinned()},
		{"hostname-selector-pinned", hostnameSelectorPinned()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := []corev1.Pod{
				pod("keep", reqs(rl("cpu", "200m"))),
				pod("drop", reqs(rl("cpu", "1")), tc.opt),
			}
			got := surge.ReschedulableRequests(pods, candidateNode)
			wantEqual(t, got, rl("cpu", "200m"))
		})
	}
}

func TestReschedulableRequestsKeepsReplicaSetAndStatefulSet(t *testing.T) {
	pods := []corev1.Pod{
		pod("rs", reqs(rl("cpu", "100m")), ownedBy("ReplicaSet")),
		pod("ss", reqs(rl("cpu", "100m")), ownedBy("StatefulSet")),
	}
	got := surge.ReschedulableRequests(pods, candidateNode)
	wantEqual(t, got, rl("cpu", "200m"))
}

func TestReschedulableRequestsKeepsNonControllerDaemonSetRef(t *testing.T) {
	// A DaemonSet owner reference that is not the controlling one is a stray
	// cross-reference, not ownership — the Pod is a normal reschedulable workload
	// and must still be counted.
	pods := []corev1.Pod{
		pod("a", reqs(rl("cpu", "100m")), ownedByNonController("DaemonSet")),
	}
	got := surge.ReschedulableRequests(pods, candidateNode)
	wantEqual(t, got, rl("cpu", "100m"))
}

func TestReschedulableRequestsCountsSidecarsAndOverhead(t *testing.T) {
	// Standard effective-request algorithm: the running total is the regular
	// containers plus the restartable (sidecar) init containers; the init peak is
	// that restartable sum plus the current init container; the pod request is the
	// max of the two, plus pod overhead.
	//   running = regular(500m) + sidecar(100m) = 600m
	//   init peak (plain "setup") = sidecar(100m) + 200m = 300m  → below running
	//   request = max(600m, 300m) + overhead(50m) = 650m
	p := pod("a",
		reqs(rl("cpu", "500m")),
		sidecar("log", rl("cpu", "100m")),
		initContainer("setup", rl("cpu", "200m")),
		overhead(rl("cpu", "50m")),
	)
	got := surge.ReschedulableRequests([]corev1.Pod{p}, candidateNode)
	wantEqual(t, got, rl("cpu", "650m"))
}

func TestReschedulableRequestsInitPeakDominates(t *testing.T) {
	// A large plain init container raises the pod request above the running sum:
	//   running = regular(500m) = 500m; init peak = 2 = 2000m → dominates.
	p := pod("a", reqs(rl("cpu", "500m")), initContainer("migrate", rl("cpu", "2")))
	got := surge.ReschedulableRequests([]corev1.Pod{p}, candidateNode)
	wantEqual(t, got, rl("cpu", "2"))
}

func TestReschedulableRequestsEmptyWhenNothingReschedulable(t *testing.T) {
	pods := []corev1.Pod{
		pod("ds", reqs(rl("cpu", "1")), ownedBy("DaemonSet")),
		pod("done", reqs(rl("cpu", "1")), phase(corev1.PodSucceeded)),
	}
	got := surge.ReschedulableRequests(pods, candidateNode)
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}

func TestIsInfraOrCompleted(t *testing.T) {
	cases := []struct {
		name string
		p    corev1.Pod
		want bool
	}{
		{"plain workload", pod("app"), false},
		{"daemonset", pod("ds", ownedBy("DaemonSet")), true},
		{"non-controller daemonset ref", pod("ds-ref", ownedByNonController("DaemonSet")), false},
		{"mirror/static", pod("static", mirror()), true},
		{"succeeded", pod("done", phase(corev1.PodSucceeded)), true},
		{"failed", pod("crashed", phase(corev1.PodFailed)), true},
		// node-pinning is NOT infra: a pinned Pod is still real workload occupying
		// a node, which the rollback's absorb-host guard must count (spec §3.3).
		{"node-pinned workload", pod("pinned", hostnamePinned()), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := surge.IsInfraOrCompleted(&tc.p); got != tc.want {
				t.Errorf("IsInfraOrCompleted = %v, want %v", got, tc.want)
			}
		})
	}
}
