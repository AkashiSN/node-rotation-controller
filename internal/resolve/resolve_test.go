package resolve

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	nrv1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
)

func pool(labels map[string]string) *karpv1.NodePool {
	return &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: labels}}
}

// policyWith builds a minimal structurally-valid RotationPolicy with the given
// name and selector. The window is well-formed so ToPolicy succeeds.
func policyWith(name string, sel *metav1.LabelSelector) nrv1.RotationPolicy {
	return nrv1.RotationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: nrv1.RotationPolicySpec{
			NodePoolSelector: sel,
			MaintenanceWindows: []nrv1.MaintenanceWindow{{
				Timezone: "UTC",
				Days:     []string{"Mon"},
				Start:    "02:00",
				End:      "06:00",
			}},
		},
	}
}

func matchLabels(kv ...string) *metav1.LabelSelector {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return &metav1.LabelSelector{MatchLabels: m}
}

func TestGoverningSingleMatch(t *testing.T) {
	p := policyWith("api", matchLabels("workload", "api"))
	winner, outcome, tied := Governing(pool(map[string]string{"workload": "api"}), []nrv1.RotationPolicy{p})
	if outcome != Matched {
		t.Fatalf("outcome = %v, want Matched", outcome)
	}
	if winner == nil || winner.Name != "api" {
		t.Fatalf("winner = %+v, want api", winner)
	}
	if len(tied) != 0 {
		t.Errorf("tied = %v, want none", tied)
	}
}

func TestGoverningNoMatch(t *testing.T) {
	p := policyWith("api", matchLabels("workload", "api"))
	_, outcome, _ := Governing(pool(map[string]string{"workload": "batch"}), []nrv1.RotationPolicy{p})
	if outcome != NoMatch {
		t.Fatalf("outcome = %v, want NoMatch", outcome)
	}
}

func TestGoverningMostSpecificWins(t *testing.T) {
	broad := policyWith("broad", matchLabels("workload", "api"))
	narrow := policyWith("narrow", matchLabels("workload", "api", "team", "core"))
	labels := map[string]string{"workload": "api", "team": "core"}
	winner, outcome, _ := Governing(pool(labels), []nrv1.RotationPolicy{broad, narrow})
	if outcome != Matched {
		t.Fatalf("outcome = %v, want Matched", outcome)
	}
	if winner.Name != "narrow" {
		t.Errorf("winner = %s, want narrow (more label keys)", winner.Name)
	}
}

func TestGoverningEqualSpecificityIsConflict(t *testing.T) {
	a := policyWith("a", matchLabels("workload", "api"))
	b := policyWith("b", matchLabels("tier", "frontend"))
	labels := map[string]string{"workload": "api", "tier": "frontend"}
	_, outcome, tied := Governing(pool(labels), []nrv1.RotationPolicy{a, b})
	if outcome != Conflict {
		t.Fatalf("outcome = %v, want Conflict", outcome)
	}
	if len(tied) != 2 || tied[0] != "a" || tied[1] != "b" {
		t.Errorf("tied = %v, want sorted [a b]", tied)
	}
}

func TestGoverningTopWinnerOverridesLowerTie(t *testing.T) {
	// Two policies tie at specificity 1, but a third matches at specificity 2.
	// The single most-specific winner governs — the lower-level tie must NOT
	// surface as a Conflict. A regression here would falsely block a legitimately
	// governed pool from rotating, which on a node-deleting controller stalls the
	// make-before-break path that keeps it under expireAfter.
	tieA := policyWith("tie-a", matchLabels("workload", "api"))
	tieB := policyWith("tie-b", matchLabels("tier", "web"))
	winner := policyWith("winner", matchLabels("workload", "api", "tier", "web"))
	labels := map[string]string{"workload": "api", "tier": "web"}
	got, outcome, tied := Governing(pool(labels), []nrv1.RotationPolicy{tieA, tieB, winner})
	if outcome != Matched {
		t.Fatalf("outcome = %v, want Matched (top winner overrides lower tie)", outcome)
	}
	if got.Name != "winner" {
		t.Errorf("winner = %s, want winner", got.Name)
	}
	if len(tied) != 0 {
		t.Errorf("tied = %v, want none (lower tie is overridden, not reported)", tied)
	}
}

func TestGoverningCatchAllLosesToSpecific(t *testing.T) {
	catchAll := policyWith("catch-all", &metav1.LabelSelector{})
	specific := policyWith("specific", matchLabels("workload", "api"))
	labels := map[string]string{"workload": "api"}
	winner, outcome, _ := Governing(pool(labels), []nrv1.RotationPolicy{catchAll, specific})
	if outcome != Matched || winner.Name != "specific" {
		t.Fatalf("got (%v, %v), want Matched specific", outcome, winnerName(winner))
	}
}

func TestGoverningMatchExpressionsCountTowardSpecificity(t *testing.T) {
	// labels-only specificity 1 vs labels+expression specificity 2.
	broad := policyWith("broad", matchLabels("workload", "api"))
	narrow := policyWith("narrow", &metav1.LabelSelector{
		MatchLabels: map[string]string{"workload": "api"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      "team",
			Operator: metav1.LabelSelectorOpExists,
		}},
	})
	labels := map[string]string{"workload": "api", "team": "core"}
	winner, outcome, _ := Governing(pool(labels), []nrv1.RotationPolicy{broad, narrow})
	if outcome != Matched || winner.Name != "narrow" {
		t.Fatalf("got (%v, %v), want Matched narrow", outcome, winnerName(winner))
	}
}

func winnerName(p *nrv1.RotationPolicy) string {
	if p == nil {
		return "<nil>"
	}
	return p.Name
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
			t.Errorf("toSurge left policy.Surge.%[1]s at its zero value; add %[1]s to toSurge in internal/resolve/resolve.go", typ.Field(i).Name)
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
