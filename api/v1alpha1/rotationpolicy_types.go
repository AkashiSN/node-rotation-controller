package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RotationPolicy status condition types and reasons (#124). The controller and
// its tests share these so the observed-state vocabulary has one source of truth.
const (
	// ConditionTypeReady summarizes whether the policy can govern rotations.
	ConditionTypeReady = "Ready"
	// ReasonAccepted: the policy spec is valid and not contested by a tie.
	ReasonAccepted = "Accepted"
	// ReasonInvalid: the spec fails reconcile-time validation (e.g. an overnight
	// window the OpenAPI schema cannot reject). Takes precedence over Conflict.
	ReasonInvalid = "Invalid"
	// ReasonConflict: the policy ties with one or more equally-specific policies
	// for at least one NodePool, so neither governs it (spec §5.4, #119 §3).
	ReasonConflict = "Conflict"
)

// RotationPolicySpec defines the rotation policy for the NodePools its selector
// matches. The field shapes mirror the former policy.yaml ConfigMap one-to-one
// (spec §5.4) — this is a carrier change (one ConfigMap → N CRD objects), not a
// redefinition of the policy fields. The OpenAPI schema generated from the markers
// below enforces the structural rules at admission time, closing the ConfigMap
// weakness where a typo failed only at runtime.
type RotationPolicySpec struct {
	// nodePoolSelector selects the NodePools this policy governs. A NodePool matched
	// by no policy is not rotated (the expireAfter backstop still applies). When
	// multiple policies match one NodePool, conflict resolution applies (spec §5.4).
	// +kubebuilder:validation:Required
	NodePoolSelector *metav1.LabelSelector `json:"nodePoolSelector"`

	// ageThreshold is "auto" (derived per NodePool, spec §3.2) or an explicit Go
	// duration override (e.g. "120h"); an override is still validated (recomputed
	// G < 1 is fatal, G < K warns).
	// +kubebuilder:validation:Pattern=`^(auto|([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+)$`
	// +kubebuilder:default="auto"
	// +optional
	AgeThreshold string `json:"ageThreshold,omitempty"`

	// minRotationChances is K: the number of full maintenance windows a candidate
	// should be eligible for before its expireAfter backstop fires (spec §3.2).
	// Floor 1; values < 2 only warn.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	// +optional
	MinRotationChances *int32 `json:"minRotationChances,omitempty"`

	// maintenanceWindows is the per-policy list of recurrence entries; the effective
	// window is the UNION of all entries (spec §3.1).
	// +kubebuilder:validation:MinItems=1
	MaintenanceWindows []MaintenanceWindow `json:"maintenanceWindows"`

	// surge holds the v1 surge-orchestration knobs (spec §3.3, §5.4).
	// +optional
	Surge Surge `json:"surge,omitempty"`

	// prePull is the reserved v2 expansion point; only enabled:false is accepted in v1.
	// The CEL rule enforces the v1 reservation at admission time, mirroring the
	// ConfigMap validator (internal/policy.Validate), so enabling it is rejected
	// rather than silently honored by a controller that deletes nodes.
	// +kubebuilder:validation:XValidation:rule="!self.enabled",message="prePull is reserved for v2 and must be disabled (enabled: false) in v1"
	// +optional
	PrePull FeatureToggle `json:"prePull,omitempty"`
}

// MaintenanceWindow is one recurrence entry; the effective window is the union of
// all entries in a policy (spec §3.1).
type MaintenanceWindow struct {
	// timezone is an IANA tz database name (e.g. "Asia/Tokyo").
	// +kubebuilder:validation:MinLength=1
	Timezone string `json:"timezone"`

	// days is the set of ISO weekdays the window recurs on.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
	Days []string `json:"days"`

	// start is the window open time, "HH:MM" 24-hour.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start"`

	// end is the window close time, "HH:MM" 24-hour; must be after start (no
	// overnight wrap — split into two entries). Structural ordering is validated at
	// runtime (spec §5.4).
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	End string `json:"end"`
}

// Surge holds the v1 surge-orchestration knobs (spec §3.3, §5.4).
type Surge struct {
	// maxUnavailable is fixed at 1 in v1 (serial per NodePool); > 1 is reserved.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:default=1
	// +optional
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`

	// readyTimeout is how long a surge node has to reach Ready before the attempt
	// fails; must be positive (validated at runtime).
	// +kubebuilder:default="15m"
	// +optional
	ReadyTimeout *metav1.Duration `json:"readyTimeout,omitempty"`

	// cooldownAfter is the settle pause between consecutive rotations in a window,
	// also reused as the pool-level inter-attempt pause after a failed attempt
	// (spec §5.2); must be positive (validated at runtime).
	// +kubebuilder:default="10m"
	// +optional
	CooldownAfter *metav1.Duration `json:"cooldownAfter,omitempty"`

	// retryBackoff is the base wait before re-selecting a failed NodeClaim; doubles
	// per consecutive failure, capped at 8x (spec §5.3); must be positive.
	// +kubebuilder:default="30m"
	// +optional
	RetryBackoff *metav1.Duration `json:"retryBackoff,omitempty"`

	// matchNodeRequirements selects which candidate-node requirements the
	// placeholder Pod replicates (spec §3.3).
	// +optional
	MatchNodeRequirements MatchNodeRequirements `json:"matchNodeRequirements,omitempty"`

	// forcefulFallback is the opt-in window-bounded surge-less forceful fallback
	// (spec §3.3). RESERVED until its controller implementation lands (#156): the
	// CEL rule rejects enabled:true at admission, mirroring internal/policy.Validate,
	// so the flag cannot be set on a controller that does not yet honor it.
	// +kubebuilder:validation:XValidation:rule="!self.enabled",message="surge.forcefulFallback is not yet implemented and must be disabled (enabled: false) until #156 lands"
	// +optional
	ForcefulFallback FeatureToggle `json:"forcefulFallback,omitempty"`
}

// MatchNodeRequirements selects which candidate-node requirements the placeholder
// Pod replicates (spec §3.3). The required karpenter.sh/nodepool selector is NOT
// listed here — it is always applied unconditionally.
type MatchNodeRequirements struct {
	// required is the hard nodeAffinity copied from the candidate node. Defaulted to
	// the §5.4 set (zone, arch, capacity-type) when empty; defaulting is applied at
	// runtime so an explicit empty list and an unset list remain distinguishable.
	// +optional
	Required []string `json:"required,omitempty"`

	// preferred is the soft nodeAffinity, relaxed under capacity pressure.
	// +optional
	Preferred []string `json:"preferred,omitempty"`
}

// FeatureToggle gates a reserved expansion point (spec §5.4).
type FeatureToggle struct {
	// enabled turns the feature on. Reserved features must stay false in v1.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// RotationPolicyStatus is an observational, derived view of the policy — never the
// authoritative runtime state. Per the architectural invariant, durable rotation
// state lives on NodeClaim/NodePool annotations and transient markers on Nodes and
// the placeholder Pod (spec §5.3); status is populated by a follow-up (#124).
type RotationPolicyStatus struct {
	// observedGeneration is the .metadata.generation the status was last computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// matchedNodePools is the number of NodePools this policy currently governs.
	// +optional
	MatchedNodePools int32 `json:"matchedNodePools,omitempty"`

	// rotatingNodePools is the number of governed NodePools with an in-flight
	// rotation (the noderotation.io/active-rotation anchor set, spec §5.2/§5.3).
	// Observational only — derived from the NodePool anchors each pass, never the
	// source of truth.
	// +optional
	RotatingNodePools int32 `json:"rotatingNodePools,omitempty"`

	// conditions reports the policy's observed state (e.g. a conflict that blocks rotation).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=rotpol
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age-Threshold",type=string,JSONPath=`.spec.ageThreshold`
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.matchedNodePools`
// +kubebuilder:printcolumn:name="Rotating",type=integer,JSONPath=`.status.rotatingNodePools`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RotationPolicy is a cluster-scoped rotation policy governing the NodePools its
// selector matches (issue #119).
type RotationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is required: it carries the mandatory nodePoolSelector and
	// maintenanceWindows whose admission-time guarantees would be bypassed by an
	// empty object if spec itself were optional.
	// +kubebuilder:validation:Required
	Spec RotationPolicySpec `json:"spec"`
	// +optional
	Status RotationPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RotationPolicyList is a list of RotationPolicy objects.
type RotationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RotationPolicy `json:"items"`
}
