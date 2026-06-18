package surge

import (
	"slices"

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
	// to the rotation via the surge-for marker.
	Candidate *karpv1.NodeClaim
	// Node is the candidate node — the source of the replicated requirement values.
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

// replicatedRequirements copies each requested key's value from the candidate
// node into an In requirement, skipping keys absent on the node or whose value
// the NodePool no longer allows (spec §3.3 intersection).
func replicatedRequirements(in PlaceholderInputs, keys []string) []corev1.NodeSelectorRequirement {
	var out []corev1.NodeSelectorRequirement
	for _, key := range keys {
		value, ok := in.Node.Labels[key]
		if !ok {
			continue // key not present on the candidate node
		}
		if !poolAllows(in.Pool, key, value) {
			continue // narrowed out of the NodePool's allowed set
		}
		out = append(out, corev1.NodeSelectorRequirement{
			Key:      key,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{value},
		})
	}
	return out
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

// requirementPermits evaluates one NodeSelector operator against value. Gt/Lt
// are not used by the categorical node labels this replicates; an unrecognized
// operator is treated as not permitting, which drops the key — the
// schedulability-preserving default (spec §3.3).
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
	default:
		return false
	}
}
