// Package resolve maps each NodePool to the single RotationPolicy that governs
// it (spec §5.4), and converts that policy's CRD spec into the validated
// internal/policy value object.
//
// Targeting and conflict resolution (issue #119 §3, decided):
//   - A RotationPolicy's spec.nodePoolSelector selects the NodePools it governs.
//   - When several policies match one NodePool, the MOST-SPECIFIC selector wins:
//     specificity is the number of label-key constraints (matchLabels entries +
//     matchExpressions entries).
//   - An exact-specificity tie among the top matches is a hard Conflict — a
//     node-deleting controller must never silently guess which policy applies.
//   - A NodePool matched by no policy is NoMatch: it is not rotated (the
//     expireAfter backstop still applies); there is no implicit cluster default.
package resolve

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	nrv1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
)

// Outcome classifies the result of resolving the governing policy for a NodePool.
type Outcome int

const (
	// Matched: exactly one most-specific policy governs the NodePool.
	Matched Outcome = iota
	// NoMatch: no policy selects the NodePool; it is not rotated.
	NoMatch
	// Conflict: two or more policies tie at the top specificity; rotation is
	// refused for the NodePool rather than guessed.
	Conflict
)

// Governing resolves the single RotationPolicy that governs pool. On Conflict it
// returns the sorted names of the tied policies (for the Warning event); winner
// is non-nil only when the outcome is Matched.
func Governing(pool *karpv1.NodePool, policies []nrv1.RotationPolicy) (winner *nrv1.RotationPolicy, outcome Outcome, tied []string) {
	poolLabels := labels.Set(pool.Labels)

	// Collect the matching policies and the top specificity in one pass. A policy
	// whose selector cannot be compiled (admission should reject these) governs
	// nothing, so it is skipped rather than treated as a catch-all.
	var top int
	var topMatches []*nrv1.RotationPolicy
	for i := range policies {
		p := &policies[i]
		sel, err := metav1.LabelSelectorAsSelector(p.Spec.NodePoolSelector)
		if err != nil {
			continue
		}
		if !sel.Matches(poolLabels) {
			continue
		}
		spec := specificity(p.Spec.NodePoolSelector)
		switch {
		case topMatches == nil || spec > top:
			top = spec
			topMatches = []*nrv1.RotationPolicy{p}
		case spec == top:
			topMatches = append(topMatches, p)
		}
	}

	switch len(topMatches) {
	case 0:
		return nil, NoMatch, nil
	case 1:
		return topMatches[0], Matched, nil
	default:
		names := make([]string, 0, len(topMatches))
		for _, p := range topMatches {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		return nil, Conflict, names
	}
}

// specificity scores a selector by the number of label-key constraints it
// carries: matchLabels entries plus matchExpressions entries. A more specific
// selector (more keys) wins over a broader one; an empty selector (catch-all)
// scores 0 and loses to any keyed selector (spec §5.4).
func specificity(sel *metav1.LabelSelector) int {
	if sel == nil {
		return 0
	}
	return len(sel.MatchLabels) + len(sel.MatchExpressions)
}

// ToPolicy converts a RotationPolicy spec into the validated internal value
// object, applying the §5.4 defaults and running structural validation. A
// non-nil error means the spec is structurally invalid at runtime (e.g. an
// overnight window the CRD HH:MM pattern cannot reject); the caller must refuse
// to rotate the governed NodePool rather than act on an unsafe policy.
func ToPolicy(spec nrv1.RotationPolicySpec) (*policy.Policy, error) {
	p := &policy.Policy{
		AgeThreshold:       spec.AgeThreshold,
		MaintenanceWindows: toWindows(spec.MaintenanceWindows),
		Surge:              toSurge(spec.Surge),
		PrePull:            policy.FeatureToggle{Enabled: spec.PrePull.Enabled},
	}
	if spec.MinRotationChances != nil {
		k := int(*spec.MinRotationChances)
		p.MinRotationChances = &k
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func toWindows(in []nrv1.MaintenanceWindow) []policy.MaintenanceWindow {
	if in == nil {
		return nil
	}
	out := make([]policy.MaintenanceWindow, len(in))
	for i, w := range in {
		out[i] = policy.MaintenanceWindow{
			Timezone: w.Timezone,
			Days:     append([]string(nil), w.Days...),
			Start:    w.Start,
			End:      w.End,
		}
	}
	return out
}

func toSurge(in nrv1.Surge) policy.Surge {
	s := policy.Surge{
		ReadyTimeout:  in.ReadyTimeout,
		CooldownAfter: in.CooldownAfter,
		RetryBackoff:  in.RetryBackoff,
		MatchNodeRequirements: policy.MatchNodeRequirements{
			Required:  append([]string(nil), in.MatchNodeRequirements.Required...),
			Preferred: append([]string(nil), in.MatchNodeRequirements.Preferred...),
		},
	}
	if in.MaxUnavailable != nil {
		mu := int(*in.MaxUnavailable)
		s.MaxUnavailable = &mu
	}
	return s
}
