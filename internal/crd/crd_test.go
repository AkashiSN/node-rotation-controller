package crd

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nrv1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
)

func matchLabels(kv ...string) *metav1.LabelSelector {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return &metav1.LabelSelector{MatchLabels: m}
}

func TestToPolicyMapsAndDefaults(t *testing.T) {
	k := int32(3)
	mu := int32(1)
	spec := nrv1.RotationPolicySpec{
		NodePoolSelector:   matchLabels("workload", "api"),
		AgeThreshold:       "120h",
		MinRotationChances: &k,
		MaintenanceWindows: []nrv1.MaintenanceWindow{{
			Timezone: "Asia/Tokyo",
			Days:     []string{"Wed", "Sat"},
			Start:    "02:00",
			End:      "06:00",
		}},
		Surge: nrv1.Surge{
			MaxUnavailable: &mu,
			ReadyTimeout:   &metav1.Duration{Duration: 12 * time.Minute},
		},
	}
	p, err := ToPolicy(spec)
	if err != nil {
		t.Fatalf("ToPolicy err = %v", err)
	}
	if p.AgeThreshold != "120h" {
		t.Errorf("ageThreshold = %q, want 120h", p.AgeThreshold)
	}
	if p.K() != 3 {
		t.Errorf("K = %d, want 3", p.K())
	}
	if got := len(p.MaintenanceWindows); got != 1 || p.MaintenanceWindows[0].Timezone != "Asia/Tokyo" {
		t.Errorf("windows not mapped: %+v", p.MaintenanceWindows)
	}
	if p.Surge.ReadyTimeout.Duration != 12*time.Minute {
		t.Errorf("readyTimeout = %v, want 12m", p.Surge.ReadyTimeout.Duration)
	}
	// Unset durations must be defaulted.
	if p.Surge.CooldownAfter == nil || p.Surge.CooldownAfter.Duration != 10*time.Minute {
		t.Errorf("cooldownAfter not defaulted: %+v", p.Surge.CooldownAfter)
	}
	// Unset MatchNodeRequirements.Required must default to the §5.4 set.
	if got := len(p.Surge.MatchNodeRequirements.Required); got != 3 {
		t.Errorf("required default len = %d, want 3", got)
	}
}

func TestToPolicyCarriesDrainEstimate(t *testing.T) {
	// A RotationPolicy with an explicit surge.drainEstimate must survive the
	// CRD-to-policy conversion; if toSurge drops it the controller silently takes
	// the min(tGP, 10m) default path and the configured value never takes effect
	// in a cluster (#212).
	spec := nrv1.RotationPolicySpec{
		NodePoolSelector: matchLabels("workload", "api"),
		MaintenanceWindows: []nrv1.MaintenanceWindow{{
			Timezone: "UTC", Days: []string{"Mon"}, Start: "02:00", End: "06:00",
		}},
		Surge: nrv1.Surge{DrainEstimate: &metav1.Duration{Duration: 20 * time.Minute}},
	}
	p, err := ToPolicy(spec)
	if err != nil {
		t.Fatalf("ToPolicy err = %v", err)
	}
	if p.Surge.DrainEstimate == nil {
		t.Fatal("drainEstimate dropped: policy.Surge.DrainEstimate == nil (toSurge must copy it)")
	}
	if p.Surge.DrainEstimate.Duration != 20*time.Minute {
		t.Errorf("drainEstimate = %v, want 20m", p.Surge.DrainEstimate.Duration)
	}
}

func TestToPolicyLeavesUnsetDrainEstimateNil(t *testing.T) {
	// An unset drainEstimate must stay nil after ToPolicy (which runs ApplyDefaults):
	// the nil is load-bearing — it is what makes schedule.Derive apply the
	// min(tGP, 10m) fallback, which admission and ApplyDefaults cannot compute
	// because tGP lives on the NodePool template.
	spec := nrv1.RotationPolicySpec{
		NodePoolSelector: matchLabels("workload", "api"),
		MaintenanceWindows: []nrv1.MaintenanceWindow{{
			Timezone: "UTC", Days: []string{"Mon"}, Start: "02:00", End: "06:00",
		}},
	}
	p, err := ToPolicy(spec)
	if err != nil {
		t.Fatalf("ToPolicy err = %v", err)
	}
	if p.Surge.DrainEstimate != nil {
		t.Errorf("drainEstimate = %v, want nil (unset; resolved in schedule.Derive)", p.Surge.DrainEstimate)
	}
}

// TestToSurgeCopiesEveryField guards the hand-written field-by-field copy in
// toSurge against the bug class that lost drainEstimate (#212): the next field
// added to nrv1.Surge / policy.Surge can be silently dropped the same way. It
// builds an nrv1.Surge with EVERY field set to a distinctive non-zero value,
// converts it, and reflects over the resulting policy.Surge, failing on any
// field left at its zero value.
//
// This is not automatic: a field added to both structs also needs a non-zero
// value added to the fixture below, or the fixture itself leaves the field at
// zero and the test passes vacuously. The guard only guarantees that adding a
// field forces someone to touch this test — a failure does not say whether
// toSurge forgot to copy the field or the fixture forgot to set it, so the
// author must check both.
//
// The two nested-struct fields (MatchNodeRequirements, FeatureToggle) need no
// special handling: reflect.Value.IsZero recurses into a struct and reports it
// zero only when all of its own fields are zero, so populating every sub-field
// of the nrv1 inputs below makes the whole-struct check meaningful.
func TestToSurgeCopiesEveryField(t *testing.T) {
	mu := int32(1)
	in := nrv1.Surge{
		MaxUnavailable: &mu,
		ReadyTimeout:   &metav1.Duration{Duration: 15 * time.Minute},
		CooldownAfter:  &metav1.Duration{Duration: 10 * time.Minute},
		RetryBackoff:   &metav1.Duration{Duration: 30 * time.Minute},
		MatchNodeRequirements: nrv1.MatchNodeRequirements{
			Required:  []string{"topology.kubernetes.io/zone"},
			Preferred: []string{"kubernetes.io/arch"},
		},
		ForcefulFallback:     nrv1.FeatureToggle{Enabled: true},
		DrainEstimate:        &metav1.Duration{Duration: 20 * time.Minute},
		ProvisioningEstimate: &metav1.Duration{Duration: 3 * time.Minute},
		FailurePause:         &metav1.Duration{Duration: 25 * time.Minute},
	}

	got := reflect.ValueOf(toSurge(in))
	typ := got.Type()
	for i := range got.NumField() {
		if got.Field(i).IsZero() {
			t.Errorf("toSurge left policy.Surge.%[1]s at its zero value; add %[1]s to toSurge in internal/crd/crd.go", typ.Field(i).Name)
		}
	}
}

func TestToPolicyRejectsRuntimeInvalid(t *testing.T) {
	// end before start passes the CRD HH:MM pattern but fails runtime validation.
	spec := nrv1.RotationPolicySpec{
		NodePoolSelector: matchLabels("workload", "api"),
		MaintenanceWindows: []nrv1.MaintenanceWindow{{
			Timezone: "UTC",
			Days:     []string{"Mon"},
			Start:    "06:00",
			End:      "02:00",
		}},
	}
	if _, err := ToPolicy(spec); err == nil {
		t.Fatal("ToPolicy accepted overnight wrap, want error")
	}
}

func TestToPolicyRejectsPrePullEnabled(t *testing.T) {
	spec := nrv1.RotationPolicySpec{
		NodePoolSelector: matchLabels("workload", "api"),
		MaintenanceWindows: []nrv1.MaintenanceWindow{{
			Timezone: "UTC", Days: []string{"Mon"}, Start: "02:00", End: "06:00",
		}},
		PrePull: nrv1.FeatureToggle{Enabled: true},
	}
	if _, err := ToPolicy(spec); err == nil {
		t.Fatal("ToPolicy accepted prePull.enabled, want error")
	}
}

func TestToPolicyAcceptsForcefulFallbackEnabled(t *testing.T) {
	spec := nrv1.RotationPolicySpec{
		NodePoolSelector: matchLabels("workload", "api"),
		MaintenanceWindows: []nrv1.MaintenanceWindow{{
			Timezone: "UTC", Days: []string{"Mon"}, Start: "02:00", End: "06:00",
		}},
		Surge: nrv1.Surge{ForcefulFallback: nrv1.FeatureToggle{Enabled: true}},
	}
	pol, err := ToPolicy(spec)
	if err != nil {
		t.Fatalf("ToPolicy rejected surge.forcefulFallback.enabled, want accept: %v", err)
	}
	if !pol.Surge.ForcefulFallback.Enabled {
		t.Fatal("ToPolicy dropped surge.forcefulFallback.enabled=true")
	}
}
