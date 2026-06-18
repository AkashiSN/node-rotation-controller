package surge

import (
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
)

// placeholderContainerName is the name of the single pause container.
const placeholderContainerName = "pause"

// PlaceholderInputs are the resolved inputs for one placeholder Pod (spec §3.3).
// They are plain values; the caller fetches the candidate Node, the NodePool,
// and the reschedulable request sum and passes them in.
type PlaceholderInputs struct {
	// Candidate is the old NodeClaim being rotated; its name pairs the placeholder
	// to the rotation via the surge-for marker, and its spec.requirements are a
	// source of replicated requirement values for keys not surfaced as node labels
	// (spec §3.3).
	Candidate *karpv1.NodeClaim
	// Node is the candidate node; its labels are the authoritative source of the
	// replicated requirement values (the node's actual placement), winning over the
	// candidate NodeClaim's requirements on conflict (spec §3.3).
	Node *corev1.Node
	// Pool is the candidate's NodePool; its allowed requirements bound the
	// replicated values so the placeholder stays schedulable (spec §3.3).
	Pool *karpv1.NodePool
	// Requests sizes the placeholder; the ReschedulableRequests sum (spec §3.3).
	Requests corev1.ResourceList
	// Match selects which candidate-node requirements to replicate (spec §5.4).
	Match policy.MatchNodeRequirements
	// ExcludedHostnames is the kubernetes.io/hostname NotIn set: the candidate node
	// plus every near-deadline host, computed by the caller (spec §3.3).
	ExcludedHostnames []string
	// PriorityClassName is the dedicated negative-priority class (spec §3.3).
	PriorityClassName string
	// Image is the pause image the placeholder runs.
	Image string
	// Namespace is where the placeholder is created.
	Namespace string
}

// BuildPlaceholder constructs the surge placeholder Pod (spec §3.3): a single
// low-priority pause Pod, sized to the reschedulable request sum, constrained to
// the candidate's NodePool and (configurably) its node requirements, and kept
// off the candidate and near-deadline nodes. It performs no I/O — the caller
// creates the returned Pod.
func BuildPlaceholder(in PlaceholderInputs) *corev1.Pod {
	preempt := corev1.PreemptNever
	// The placeholder is a pause Pod that only reserves capacity; it never calls
	// the Kubernetes API, so it must not be handed a service account token. Set
	// this explicitly rather than relying on namespace/ServiceAccount defaults
	// (issue #35, least privilege).
	noAutomount := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PlaceholderName(in.Candidate.Name),
			Namespace: in.Namespace,
			// surge-for pairs the placeholder to its rotation; the karpenter.sh/nodepool
			// label lets the controller's Pod watch map the placeholder back to its
			// owning NodePool without a client lookup (spec §3.3, issue #14).
			Labels: map[string]string{
				annotations.SurgeFor:    in.Candidate.Name,
				karpv1.NodePoolLabelKey: in.Pool.Name,
			},
			// Pod-level do-not-disrupt: blocks voluntary disruption of whatever node
			// the placeholder runs on, covering the surge target in the bind →
			// surge_ready gap before the node-level freeze lands (spec §3.3, §5.3).
			Annotations: map[string]string{karpv1.DoNotDisruptAnnotationKey: "true"},
		},
		Spec: corev1.PodSpec{
			PriorityClassName:            in.PriorityClassName,
			PreemptionPolicy:             &preempt,
			AutomountServiceAccountToken: &noAutomount,
			Tolerations:                  poolTolerations(in.Pool),
			Containers: []corev1.Container{{
				Name:      placeholderContainerName,
				Image:     in.Image,
				Resources: corev1.ResourceRequirements{Requests: in.Requests},
			}},
			Affinity: &corev1.Affinity{NodeAffinity: nodeAffinity(in)},
		},
	}
}

// poolTolerations builds the placeholder's tolerations from the candidate
// NodePool's template taints so it can schedule onto the same tainted capacity
// the displaced workload uses (spec §3.3, issue #34). Without them, a placeholder
// in a NodePool that uses permanent taints stays unschedulable while the real
// Pods (which carry matching tolerations) could land — every rotation attempt
// would then wait out readyTimeout and roll back.
//
// Only spec.template.spec.taints are copied: startupTaints are removed once a
// node is Ready and are ignored for provisioning. Each taint maps to an
// exact-match Equal toleration, so the placeholder tolerates exactly that taint
// and no more — it never gains access to capacity the workload could not use.
func poolTolerations(pool *karpv1.NodePool) []corev1.Toleration {
	taints := pool.Spec.Template.Spec.Taints
	if len(taints) == 0 {
		return nil
	}
	tols := make([]corev1.Toleration, 0, len(taints))
	for _, t := range taints {
		tols = append(tols, corev1.Toleration{
			Key:      t.Key,
			Operator: corev1.TolerationOpEqual,
			Value:    t.Value,
			Effect:   t.Effect,
		})
	}
	return tols
}

// nodeAffinity assembles the placeholder's node affinity (spec §3.3): one
// required term ANDing the unconditional NodePool selector, the replicated
// required requirements, and the hostname exclusion; plus the soft preferred
// requirements.
func nodeAffinity(in PlaceholderInputs) *corev1.NodeAffinity {
	required := []corev1.NodeSelectorRequirement{{
		// Same-NodePool is a structural invariant, applied unconditionally and
		// independent of Match (spec §3.3).
		Key:      karpv1.NodePoolLabelKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{in.Pool.Name},
	}}
	required = append(required, replicatedRequirements(in, in.Match.Required)...)
	if len(in.ExcludedHostnames) > 0 {
		required = append(required, corev1.NodeSelectorRequirement{
			Key:      corev1.LabelHostname,
			Operator: corev1.NodeSelectorOpNotIn,
			Values:   in.ExcludedHostnames,
		})
	}

	na := &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: required}},
		},
	}
	for _, e := range replicatedRequirements(in, in.Match.Preferred) {
		na.PreferredDuringSchedulingIgnoredDuringExecution = append(
			na.PreferredDuringSchedulingIgnoredDuringExecution,
			corev1.PreferredSchedulingTerm{
				Weight:     1,
				Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{e}},
			},
		)
	}
	return na
}

// replicatedRequirements copies each requested key's candidate value(s) into an
// In requirement, intersected with the NodePool's allowed set (spec §3.3). Values
// come from the candidate node's label (its actual placement — authoritative) or,
// for a key not surfaced as a node label, the candidate NodeClaim's own
// spec.requirements; the node label wins on conflict. A key absent from both
// sources, or whose every candidate value the NodePool no longer allows, is
// skipped — the schedulability-preserving default.
func replicatedRequirements(in PlaceholderInputs, keys []string) []corev1.NodeSelectorRequirement {
	var out []corev1.NodeSelectorRequirement
	for _, key := range keys {
		var allowed []string
		for _, value := range candidateValues(in, key) {
			if poolAllows(in.Pool, key, value) {
				allowed = append(allowed, value)
			}
		}
		if len(allowed) == 0 {
			continue // absent from both sources, or narrowed out of the allowed set
		}
		out = append(out, corev1.NodeSelectorRequirement{
			Key:      key,
			Operator: corev1.NodeSelectorOpIn,
			Values:   allowed,
		})
	}
	return out
}

// candidateValues resolves the parity value(s) for one key. The candidate node's
// label — its actual placement — is authoritative and wins on conflict, so when
// present it is the sole value. Otherwise the candidate NodeClaim's own
// spec.requirements supply the values (covering custom parity keys that never
// surface as node labels, spec §3.3); only In requirements carry concrete values
// to pin — NotIn/Exists/DoesNotExist/Gt/Lt express no positive value and yield
// nothing.
func candidateValues(in PlaceholderInputs, key string) []string {
	if value, ok := in.Node.Labels[key]; ok {
		return []string{value}
	}
	var vals []string
	for _, r := range in.Candidate.Spec.Requirements {
		if r.Key == key && r.Operator == corev1.NodeSelectorOpIn {
			vals = append(vals, r.Values...)
		}
	}
	return vals
}

// poolAllows reports whether value for key satisfies every NodePool requirement
// on that key (they AND). A key the NodePool does not constrain is allowed.
func poolAllows(pool *karpv1.NodePool, key, value string) bool {
	for _, r := range pool.Spec.Template.Spec.Requirements {
		if r.Key != key {
			continue
		}
		if !requirementPermits(r.Operator, r.Values, value) {
			return false
		}
	}
	return true
}

// requirementPermits evaluates one NodeSelector operator against value, matching
// Kubernetes' and Karpenter's NodeSelector semantics. The numeric operators
// compare integers — a NodePool that constrains a numeric Karpenter requirement
// (e.g. karpenter.k8s.aws/instance-generation) with these operators still has its
// candidate-node value replicated rather than silently dropped: Kubernetes' Gt/Lt
// (issue #49) and Karpenter's own Gte/Lte (issue #55), the latter present in the
// bundled sigs.k8s.io/karpenter API. When a numeric comparison cannot be
// evaluated (malformed bound or non-integer node value) the key is dropped — the
// schedulability-preserving default (spec §3.3), shared with any unrecognized
// operator.
func requirementPermits(op corev1.NodeSelectorOperator, values []string, value string) bool {
	switch op {
	case corev1.NodeSelectorOpIn:
		return slices.Contains(values, value)
	case corev1.NodeSelectorOpNotIn:
		return !slices.Contains(values, value)
	case corev1.NodeSelectorOpExists:
		return true // the key is present (value is non-empty by construction)
	case corev1.NodeSelectorOpDoesNotExist:
		return false // the NodePool forbids the key
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt,
		karpv1.NodeSelectorOpGte, karpv1.NodeSelectorOpLte:
		return numericPermits(op, values, value)
	default:
		return false
	}
}

// numericPermits evaluates a Gt, Lt, Gte, or Lte requirement. Per the Kubernetes
// and Karpenter NodeSelector validation the requirement carries exactly one
// integer bound, and the candidate node value must parse as an integer; any
// deviation drops the key (returns false), the schedulability-preserving default.
func numericPermits(op corev1.NodeSelectorOperator, values []string, value string) bool {
	if len(values) != 1 {
		return false
	}
	bound, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil {
		return false
	}
	got, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return false
	}
	switch op {
	case corev1.NodeSelectorOpGt:
		return got > bound
	case karpv1.NodeSelectorOpGte:
		return got >= bound
	case corev1.NodeSelectorOpLt:
		return got < bound
	case karpv1.NodeSelectorOpLte:
		return got <= bound
	default:
		return false
	}
}
