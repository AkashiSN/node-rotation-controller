package resolve

import (
	"testing"

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
