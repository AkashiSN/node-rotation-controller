package surge_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func req(key string, op corev1.NodeSelectorOperator, values ...string) karpv1.NodeSelectorRequirementWithMinValues {
	return karpv1.NodeSelectorRequirementWithMinValues{Key: key, Operator: op, Values: values}
}

// poolName is the fixed NodePool name used across the placeholder tests.
const poolName = "api"

func nodepool(reqs ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodePool {
	p := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: poolName}}
	p.Spec.Template.Spec.Requirements = reqs
	return p
}

func claimNamed(name string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// baseInputs is a fully-populated, schedulable set of placeholder inputs.
func baseInputs() surge.PlaceholderInputs {
	return surge.PlaceholderInputs{
		Candidate: claimNamed("nc-old"),
		Node: node(candidateNode, map[string]string{
			corev1.LabelTopologyZone:     "us-east-1a",
			corev1.LabelArchStable:       "arm64",
			"karpenter.sh/capacity-type": "spot",
		}),
		Pool:              nodepool(),
		Requests:          rl("cpu", "1", "memory", "2Gi"),
		Match:             policy.MatchNodeRequirements{Required: []string{corev1.LabelTopologyZone, corev1.LabelArchStable}},
		ExcludedHostnames: []string{candidateNode, "node-near-deadline"},
		PriorityClassName: "noderotation-placeholder",
		Image:             "registry.k8s.io/pause:3.10",
		Namespace:         "node-rotation-system",
	}
}

// requiredExprs returns the single required NodeSelectorTerm's expressions keyed
// by label key; it fails if the affinity shape is not the expected one term.
func requiredExprs(t *testing.T, p *corev1.Pod) map[string]corev1.NodeSelectorRequirement {
	t.Helper()
	na := p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if na == nil || len(na.NodeSelectorTerms) != 1 {
		t.Fatalf("want exactly one required nodeSelectorTerm, got %+v", na)
	}
	out := map[string]corev1.NodeSelectorRequirement{}
	for _, e := range na.NodeSelectorTerms[0].MatchExpressions {
		out[e.Key] = e
	}
	return out
}

func TestBuildPlaceholderMetadata(t *testing.T) {
	p := surge.BuildPlaceholder(baseInputs())

	if p.Namespace != "node-rotation-system" {
		t.Errorf("namespace: got %q", p.Namespace)
	}
	if p.Labels[annotations.SurgeFor] != "nc-old" {
		t.Errorf("surge-for label: got %q", p.Labels[annotations.SurgeFor])
	}
	if p.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Errorf("do-not-disrupt annotation: got %q", p.Annotations[karpv1.DoNotDisruptAnnotationKey])
	}
	if p.Name == "" && p.GenerateName == "" {
		t.Error("placeholder must have a name or generateName")
	}
}

func TestBuildPlaceholderPodShape(t *testing.T) {
	p := surge.BuildPlaceholder(baseInputs())

	if p.Spec.PriorityClassName != "noderotation-placeholder" {
		t.Errorf("priorityClassName: got %q", p.Spec.PriorityClassName)
	}
	if p.Spec.PreemptionPolicy == nil || *p.Spec.PreemptionPolicy != corev1.PreemptNever {
		t.Errorf("preemptionPolicy: want Never, got %v", p.Spec.PreemptionPolicy)
	}
	// The pause Pod never calls the Kubernetes API, so it must not receive a
	// service account token (issue #35, least privilege).
	if p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken: want explicit false, got %v", p.Spec.AutomountServiceAccountToken)
	}
	if len(p.Spec.Containers) != 1 {
		t.Fatalf("want exactly one container, got %d", len(p.Spec.Containers))
	}
	c := p.Spec.Containers[0]
	if c.Image != "registry.k8s.io/pause:3.10" {
		t.Errorf("image: got %q", c.Image)
	}
	wantEqual(t, c.Resources.Requests, rl("cpu", "1", "memory", "2Gi"))
}

func TestBuildPlaceholderToleratesNodePoolTaints(t *testing.T) {
	// The placeholder must tolerate the candidate NodePool's template taints so it
	// can schedule onto the same tainted capacity the displaced workload uses
	// (issue #34). Each taint maps to an exact-match Equal toleration.
	in := baseInputs()
	pool := nodepool()
	pool.Spec.Template.Spec.Taints = []corev1.Taint{
		{Key: "workload", Value: "api", Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Effect: corev1.TaintEffectNoExecute}, // empty value
	}
	in.Pool = pool
	p := surge.BuildPlaceholder(in)

	want := map[string]corev1.Toleration{
		"workload":  {Key: "workload", Operator: corev1.TolerationOpEqual, Value: "api", Effect: corev1.TaintEffectNoSchedule},
		"dedicated": {Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "", Effect: corev1.TaintEffectNoExecute},
	}
	if len(p.Spec.Tolerations) != len(want) {
		t.Fatalf("toleration count: got %d (%+v), want %d", len(p.Spec.Tolerations), p.Spec.Tolerations, len(want))
	}
	for _, tol := range p.Spec.Tolerations {
		w, ok := want[tol.Key]
		if !ok || tol != w {
			t.Errorf("toleration %q: got %+v, want %+v", tol.Key, tol, w)
		}
	}
}

func TestBuildPlaceholderNoTolerationsWhenUntainted(t *testing.T) {
	p := surge.BuildPlaceholder(baseInputs()) // baseInputs' NodePool has no taints
	if len(p.Spec.Tolerations) != 0 {
		t.Errorf("untainted NodePool: want no tolerations, got %+v", p.Spec.Tolerations)
	}
}

func TestBuildPlaceholderUnconditionalNodePoolSelector(t *testing.T) {
	in := baseInputs()
	in.Match = policy.MatchNodeRequirements{} // even with no configured keys
	p := surge.BuildPlaceholder(in)

	e, ok := requiredExprs(t, p)[karpv1.NodePoolLabelKey]
	if !ok {
		t.Fatalf("nodepool selector missing; exprs=%v", requiredExprs(t, p))
	}
	if e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "api" {
		t.Errorf("nodepool selector: got %+v, want In [api]", e)
	}
}

func TestBuildPlaceholderHostnameExclusion(t *testing.T) {
	p := surge.BuildPlaceholder(baseInputs())
	e, ok := requiredExprs(t, p)[corev1.LabelHostname]
	if !ok {
		t.Fatal("hostname exclusion missing")
	}
	if e.Operator != corev1.NodeSelectorOpNotIn {
		t.Errorf("hostname operator: want NotIn, got %v", e.Operator)
	}
	if len(e.Values) != 2 || e.Values[0] != candidateNode || e.Values[1] != "node-near-deadline" {
		t.Errorf("hostname exclusion values: got %v", e.Values)
	}
}

func TestBuildPlaceholderNoHostnameExprWhenNoExclusions(t *testing.T) {
	in := baseInputs()
	in.ExcludedHostnames = nil
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[corev1.LabelHostname]; ok {
		t.Error("no hostname expression should be emitted when there are no exclusions")
	}
}

func TestBuildPlaceholderReplicatesRequiredFromNode(t *testing.T) {
	p := surge.BuildPlaceholder(baseInputs())
	exprs := requiredExprs(t, p)

	zone, ok := exprs[corev1.LabelTopologyZone]
	if !ok || zone.Operator != corev1.NodeSelectorOpIn || len(zone.Values) != 1 || zone.Values[0] != "us-east-1a" {
		t.Errorf("zone requirement: got %+v, want In [us-east-1a]", zone)
	}
	arch, ok := exprs[corev1.LabelArchStable]
	if !ok || arch.Values[0] != "arm64" {
		t.Errorf("arch requirement: got %+v, want In [arm64]", arch)
	}
}

func TestBuildPlaceholderSkipsRequiredKeyAbsentOnNode(t *testing.T) {
	in := baseInputs()
	in.Match = policy.MatchNodeRequirements{Required: []string{"node.kubernetes.io/instance-type"}}
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)["node.kubernetes.io/instance-type"]; ok {
		t.Error("a required key absent from the candidate node must be skipped")
	}
}

func TestBuildPlaceholderIntersectsWithNodePoolAllowed(t *testing.T) {
	// The NodePool has narrowed zone to {us-east-1b,1c}; the candidate node's
	// us-east-1a is no longer allowed, so pinning it would leave the placeholder
	// unschedulable forever — the key is dropped instead (spec §3.3).
	in := baseInputs()
	in.Pool = nodepool(req(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-east-1b", "us-east-1c"))
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[corev1.LabelTopologyZone]; ok {
		t.Error("a candidate value disallowed by the NodePool must be dropped, not pinned")
	}
}

func TestBuildPlaceholderKeepsValueAllowedByNodePool(t *testing.T) {
	in := baseInputs()
	in.Pool = nodepool(req(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-east-1a", "us-east-1b"))
	p := surge.BuildPlaceholder(in)
	e, ok := requiredExprs(t, p)[corev1.LabelTopologyZone]
	if !ok || e.Values[0] != "us-east-1a" {
		t.Errorf("an allowed candidate value must be kept: got %+v", e)
	}
}

func TestBuildPlaceholderNodePoolNotInExclusion(t *testing.T) {
	// A NotIn requirement that excludes the candidate's value drops the key.
	in := baseInputs()
	in.Pool = nodepool(req(corev1.LabelArchStable, corev1.NodeSelectorOpNotIn, "arm64"))
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[corev1.LabelArchStable]; ok {
		t.Error("a value excluded by a NodePool NotIn requirement must be dropped")
	}
}

func TestBuildPlaceholderPreferred(t *testing.T) {
	in := baseInputs()
	in.Match.Preferred = []string{"node.kubernetes.io/instance-type"}
	in.Node.Labels["node.kubernetes.io/instance-type"] = "m6g.large"
	p := surge.BuildPlaceholder(in)

	pref := p.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(pref) != 1 {
		t.Fatalf("want one preferred term, got %d", len(pref))
	}
	e := pref[0].Preference.MatchExpressions[0]
	if e.Key != "node.kubernetes.io/instance-type" || e.Values[0] != "m6g.large" {
		t.Errorf("preferred expr: got %+v", e)
	}
}
