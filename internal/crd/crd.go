// Package crd converts the RotationPolicy CRD spec into the validated
// internal/policy value object, applying the §5.4 defaults and running the
// structural validation the CRD's OpenAPI schema cannot express.
//
// It is deliberately its own package rather than part of internal/resolve.
// resolve maps NodePools to policies and therefore imports the Karpenter API,
// which drags 6 MB of gzipped scheme/reflect metadata into a GOOS=js binary.
// crd is pure — the CRD types plus internal/policy — so the wasm simulator can
// run the CONTROLLER'S OWN defaulting and validation on the YAML an operator
// pastes into the browser, and report the controller's own error messages,
// without linking Karpenter. The CI size guard (make wasm-guard) pins that.
package crd

import (
	nrv1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
)

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
		ReadyTimeout:         in.ReadyTimeout,
		CooldownAfter:        in.CooldownAfter,
		RetryBackoff:         in.RetryBackoff,
		DrainEstimate:        in.DrainEstimate,
		ProvisioningEstimate: in.ProvisioningEstimate,
		FailurePause:         in.FailurePause,
		MatchNodeRequirements: policy.MatchNodeRequirements{
			Required:  append([]string(nil), in.MatchNodeRequirements.Required...),
			Preferred: append([]string(nil), in.MatchNodeRequirements.Preferred...),
		},
		ForcefulFallback: policy.FeatureToggle{Enabled: in.ForcefulFallback.Enabled},
	}
	if in.MaxUnavailable != nil {
		mu := int(*in.MaxUnavailable)
		s.MaxUnavailable = &mu
	}
	return s
}
