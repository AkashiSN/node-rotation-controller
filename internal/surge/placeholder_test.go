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

func TestBuildPlaceholderNodePoolExistsKeepsValue(t *testing.T) {
	// An Exists requirement on the key is satisfied by any present value, so the
	// candidate's value is kept (poolAllows → requirementPermits Exists branch).
	in := baseInputs()
	in.Pool = nodepool(req(corev1.LabelArchStable, corev1.NodeSelectorOpExists))
	e, ok := requiredExprs(t, surge.BuildPlaceholder(in))[corev1.LabelArchStable]
	if !ok || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "arm64" {
		t.Errorf("an Exists NodePool requirement must keep the candidate value: got %+v (present=%v)", e, ok)
	}
}

func TestBuildPlaceholderNodePoolDoesNotExistDropsValue(t *testing.T) {
	// A DoesNotExist requirement forbids the key entirely, so the candidate value
	// can never satisfy it and the key is dropped (requirementPermits DoesNotExist
	// branch → false).
	in := baseInputs()
	in.Pool = nodepool(req(corev1.LabelArchStable, corev1.NodeSelectorOpDoesNotExist))
	if _, ok := requiredExprs(t, surge.BuildPlaceholder(in))[corev1.LabelArchStable]; ok {
		t.Error("a DoesNotExist NodePool requirement forbids the key and must drop the value")
	}
}

func TestBuildPlaceholderUnknownOperatorDropsValue(t *testing.T) {
	// An unrecognized operator hits requirementPermits' default branch, which drops
	// the key — the schedulability-preserving default (spec §3.3). Defensive: real
	// NodePools never carry an unknown operator, but a future API addition must fail
	// safe rather than silently pin an unschedulable value.
	in := baseInputs()
	in.Pool = nodepool(req(corev1.LabelArchStable, corev1.NodeSelectorOperator("Frobnicate"), "arm64"))
	if _, ok := requiredExprs(t, surge.BuildPlaceholder(in))[corev1.LabelArchStable]; ok {
		t.Error("an unrecognized NodePool operator must drop the key (schedulability-safe default)")
	}
}

func TestBuildPlaceholderDropsEmptyBoundNumericRequirement(t *testing.T) {
	// Gt/Lt require exactly one integer bound; an empty Values (len == 0) is
	// malformed and the key is dropped (numericPermits len(values) != 1 branch). The
	// existing malformed test covers two bounds; this covers the zero-bound boundary.
	in := numericInputs("6", req(instanceGen, corev1.NodeSelectorOpGt))
	if _, ok := requiredExprs(t, surge.BuildPlaceholder(in))[instanceGen]; ok {
		t.Error("a numeric Gt requirement with no bound is malformed and must drop the key")
	}
}

// customKey is a parity key a workload's nodeAffinity depends on that Karpenter
// constrains on the NodeClaim but does not surface as a node label (issue #60).
const customKey = "example.com/rack"

// claimWithReqs builds the candidate NodeClaim (matching baseInputs' name)
// carrying spec.requirements.
func claimWithReqs(reqs ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodeClaim {
	c := claimNamed("nc-old")
	c.Spec.Requirements = reqs
	return c
}

func TestBuildPlaceholderReplicatesKeyOnlyOnNodeClaimRequirements(t *testing.T) {
	// The key lives on the candidate NodeClaim's requirements but is NOT a node
	// label, so the node-labels-only path silently dropped it (issue #60).
	in := baseInputs()
	in.Candidate = claimWithReqs(req(customKey, corev1.NodeSelectorOpIn, "r7"))
	in.Match = policy.MatchNodeRequirements{Required: []string{customKey}}
	e, ok := requiredExprs(t, surge.BuildPlaceholder(in))[customKey]
	if !ok || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "r7" {
		t.Errorf("key present only on NodeClaim requirements must be replicated: got %+v, want In [r7]", e)
	}
}

func TestBuildPlaceholderNodeLabelWinsOverNodeClaimRequirement(t *testing.T) {
	// The candidate node's label is its actual placement and must win over the
	// NodeClaim's (broader/stale) requirement on conflict (issue #60).
	in := baseInputs()
	in.Candidate = claimWithReqs(req(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-east-1b"))
	e := requiredExprs(t, surge.BuildPlaceholder(in))[corev1.LabelTopologyZone]
	if len(e.Values) != 1 || e.Values[0] != "us-east-1a" {
		t.Errorf("node label must win on conflict: got %+v, want In [us-east-1a]", e)
	}
}

func TestBuildPlaceholderReplicatesMultipleNodeClaimRequirementValues(t *testing.T) {
	in := baseInputs()
	in.Candidate = claimWithReqs(req(customKey, corev1.NodeSelectorOpIn, "r7", "r8"))
	in.Match = policy.MatchNodeRequirements{Required: []string{customKey}}
	e := requiredExprs(t, surge.BuildPlaceholder(in))[customKey]
	if len(e.Values) != 2 || e.Values[0] != "r7" || e.Values[1] != "r8" {
		t.Errorf("multi-value NodeClaim requirement must replicate all allowed values: got %+v, want In [r7 r8]", e)
	}
}

func TestBuildPlaceholderIntersectsNodeClaimRequirementWithNodePoolAllowed(t *testing.T) {
	// A NodeClaim-sourced value the NodePool no longer allows is dropped, same as a
	// node-label value (spec §3.3 intersection).
	in := baseInputs()
	in.Candidate = claimWithReqs(req(customKey, corev1.NodeSelectorOpIn, "r7", "r9"))
	in.Pool = nodepool(req(customKey, corev1.NodeSelectorOpIn, "r7", "r8"))
	in.Match = policy.MatchNodeRequirements{Required: []string{customKey}}
	e, ok := requiredExprs(t, surge.BuildPlaceholder(in))[customKey]
	if !ok || len(e.Values) != 1 || e.Values[0] != "r7" {
		t.Errorf("only NodePool-allowed NodeClaim values must survive: got %+v, want In [r7]", e)
	}
}

func TestBuildPlaceholderSkipsNonInNodeClaimRequirement(t *testing.T) {
	// A key the NodeClaim constrains only with a non-In operator carries no concrete
	// value to pin, so it is skipped (not surfaced as a node label either).
	in := baseInputs()
	in.Candidate = claimWithReqs(req(customKey, corev1.NodeSelectorOpExists))
	in.Match = policy.MatchNodeRequirements{Required: []string{customKey}}
	if _, ok := requiredExprs(t, surge.BuildPlaceholder(in))[customKey]; ok {
		t.Error("a non-In NodeClaim requirement carries no value to pin and must be skipped")
	}
}

// instanceGen is a numeric Karpenter requirement key whose NodePool constraints
// use the Gt/Lt operators (issue #49).
const instanceGen = "karpenter.k8s.aws/instance-generation"

// numericInputs replicates a single numeric required key whose value on the
// candidate node is nodeValue.
func numericInputs(nodeValue string, reqs ...karpv1.NodeSelectorRequirementWithMinValues) surge.PlaceholderInputs {
	in := baseInputs()
	in.Node.Labels[instanceGen] = nodeValue
	in.Match = policy.MatchNodeRequirements{Required: []string{instanceGen}}
	in.Pool = nodepool(reqs...)
	return in
}

func TestBuildPlaceholderKeepsValueAllowedByNumericGt(t *testing.T) {
	// NodePool allows generations > 5; the candidate's 6 satisfies it, so the key
	// must be replicated, not silently dropped (issue #49).
	in := numericInputs("6", req(instanceGen, corev1.NodeSelectorOpGt, "5"))
	p := surge.BuildPlaceholder(in)
	e, ok := requiredExprs(t, p)[instanceGen]
	if !ok || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "6" {
		t.Errorf("numeric Gt-allowed value must be kept as In [6]: got %+v (present=%v)", e, ok)
	}
}

func TestBuildPlaceholderDropsValueExcludedByNumericGt(t *testing.T) {
	// 5 is not > 5, so the NodePool no longer allows it; the key is dropped to keep
	// the placeholder schedulable (issue #49).
	in := numericInputs("5", req(instanceGen, corev1.NodeSelectorOpGt, "5"))
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[instanceGen]; ok {
		t.Error("a value not satisfying a numeric Gt requirement must be dropped")
	}
}

func TestBuildPlaceholderKeepsValueAllowedByNumericLt(t *testing.T) {
	in := numericInputs("6", req(instanceGen, corev1.NodeSelectorOpLt, "8"))
	p := surge.BuildPlaceholder(in)
	e, ok := requiredExprs(t, p)[instanceGen]
	if !ok || e.Values[0] != "6" {
		t.Errorf("numeric Lt-allowed value must be kept: got %+v (present=%v)", e, ok)
	}
}

func TestBuildPlaceholderKeepsValueAllowedByNumericGte(t *testing.T) {
	// NodePool allows generations >= 5; the candidate's 5 satisfies the inclusive
	// bound (where Gt would have dropped it), so the key must be replicated (#55).
	in := numericInputs("5", req(instanceGen, karpv1.NodeSelectorOpGte, "5"))
	p := surge.BuildPlaceholder(in)
	e, ok := requiredExprs(t, p)[instanceGen]
	if !ok || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "5" {
		t.Errorf("numeric Gte-allowed value must be kept as In [5]: got %+v (present=%v)", e, ok)
	}
}

func TestBuildPlaceholderDropsValueExcludedByNumericGte(t *testing.T) {
	// 4 is not >= 5, so the NodePool no longer allows it; drop to stay schedulable.
	in := numericInputs("4", req(instanceGen, karpv1.NodeSelectorOpGte, "5"))
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[instanceGen]; ok {
		t.Error("a value below a numeric Gte requirement must be dropped")
	}
}

func TestBuildPlaceholderKeepsValueAllowedByNumericLte(t *testing.T) {
	// NodePool allows generations <= 8; the candidate's 8 satisfies the inclusive
	// bound (where Lt would have dropped it), so the key must be replicated (#55).
	in := numericInputs("8", req(instanceGen, karpv1.NodeSelectorOpLte, "8"))
	p := surge.BuildPlaceholder(in)
	e, ok := requiredExprs(t, p)[instanceGen]
	if !ok || e.Values[0] != "8" {
		t.Errorf("numeric Lte-allowed value must be kept: got %+v (present=%v)", e, ok)
	}
}

func TestBuildPlaceholderDropsValueExcludedByNumericLte(t *testing.T) {
	// 9 is not <= 8, so the NodePool no longer allows it; drop to stay schedulable.
	in := numericInputs("9", req(instanceGen, karpv1.NodeSelectorOpLte, "8"))
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[instanceGen]; ok {
		t.Error("a value above a numeric Lte requirement must be dropped")
	}
}

func TestBuildPlaceholderNumericRequirementBracketsValue(t *testing.T) {
	// Both Gt and Lt on the same key AND together; 6 satisfies 5 < x < 8.
	in := numericInputs("6",
		req(instanceGen, corev1.NodeSelectorOpGt, "5"),
		req(instanceGen, corev1.NodeSelectorOpLt, "8"),
	)
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[instanceGen]; !ok {
		t.Error("a value inside the Gt/Lt bracket must be kept")
	}

	// 9 violates the Lt bound, so the bracketed key is dropped.
	in = numericInputs("9",
		req(instanceGen, corev1.NodeSelectorOpGt, "5"),
		req(instanceGen, corev1.NodeSelectorOpLt, "8"),
	)
	p = surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[instanceGen]; ok {
		t.Error("a value outside the Gt/Lt bracket must be dropped")
	}
}

func TestBuildPlaceholderDropsNonIntegerNumericComparison(t *testing.T) {
	// A non-integer candidate value cannot satisfy a numeric comparison; drop it
	// rather than emit an unschedulable pin (issue #49, schedulability-safe default).
	in := numericInputs("not-a-number", req(instanceGen, corev1.NodeSelectorOpGt, "5"))
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[instanceGen]; ok {
		t.Error("a non-integer value against a numeric Gt requirement must be dropped")
	}
}

func TestBuildPlaceholderDropsMalformedNumericRequirement(t *testing.T) {
	// Gt/Lt require exactly one integer bound; anything else is malformed and the
	// key is dropped (matches Kubernetes NodeSelector semantics).
	in := numericInputs("6", req(instanceGen, corev1.NodeSelectorOpGt, "5", "7"))
	p := surge.BuildPlaceholder(in)
	if _, ok := requiredExprs(t, p)[instanceGen]; ok {
		t.Error("a Gt requirement with multiple bounds is malformed and must drop the key")
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

// preferredExprs returns the preferred NodeSelectorRequirements keyed by label
// key (each soft term carries exactly one expression, per nodeAffinity).
func preferredExprs(t *testing.T, p *corev1.Pod) map[string]corev1.NodeSelectorRequirement {
	t.Helper()
	out := map[string]corev1.NodeSelectorRequirement{}
	na := p.Spec.Affinity.NodeAffinity
	if na == nil {
		return out
	}
	for _, term := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, e := range term.Preference.MatchExpressions {
			out[e.Key] = e
		}
	}
	return out
}

func TestBuildPlaceholderPreferredSkipsKeyAbsentFromBothSources(t *testing.T) {
	// A preferred key present on neither the candidate node's labels nor the
	// NodeClaim's requirements carries no value to replicate, so it is skipped — the
	// preferred path shares replicatedRequirements' skip-when-empty default.
	in := baseInputs()
	in.Match.Preferred = []string{"node.kubernetes.io/instance-type"} // not on node, not on claim
	if _, ok := preferredExprs(t, surge.BuildPlaceholder(in))["node.kubernetes.io/instance-type"]; ok {
		t.Error("a preferred key absent from both sources must be skipped")
	}
}

func TestBuildPlaceholderPreferredIntersectsWithNodePoolAllowed(t *testing.T) {
	// A preferred value the NodePool no longer allows is dropped, exactly like the
	// required path (replicatedRequirements intersects with poolAllows for both).
	in := baseInputs()
	in.Match.Preferred = []string{corev1.LabelTopologyZone} // candidate node label = us-east-1a
	in.Pool = nodepool(req(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-east-1b", "us-east-1c"))
	if _, ok := preferredExprs(t, surge.BuildPlaceholder(in))[corev1.LabelTopologyZone]; ok {
		t.Error("a preferred value disallowed by the NodePool must be dropped, not pinned")
	}

	// Negative control: when the NodePool still allows the value, the preferred
	// requirement survives the intersection.
	in.Pool = nodepool(req(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-east-1a", "us-east-1b"))
	e, ok := preferredExprs(t, surge.BuildPlaceholder(in))[corev1.LabelTopologyZone]
	if !ok || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "us-east-1a" {
		t.Errorf("a NodePool-allowed preferred value must survive the intersection: got %+v (present=%v)", e, ok)
	}
}
