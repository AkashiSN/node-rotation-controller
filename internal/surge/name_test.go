package surge_test

import (
	"strings"
	"testing"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// dns1123SubdomainMax is the Kubernetes Pod-name length bound; PlaceholderName
// must never exceed it, or the create fails with Invalid permanently (#10 review).
const dns1123SubdomainMax = 253

func TestPlaceholderNameShortIsReadable(t *testing.T) {
	// Karpenter NodeClaim names are short generated names: the common path keeps
	// the prefix+name form verbatim so the placeholder stays human-identifiable.
	got := surge.PlaceholderName("default-abc12")
	if got != "noderotation-surge-default-abc12" {
		t.Errorf("PlaceholderName(short): got %q", got)
	}
}

func TestPlaceholderNameDeterministic(t *testing.T) {
	long := strings.Repeat("a", 300)
	first := surge.PlaceholderName(long)
	second := surge.PlaceholderName(long)
	if first != second {
		t.Errorf("PlaceholderName must be deterministic for idempotent create: %q != %q", first, second)
	}
}

func TestPlaceholderNameWithinBound(t *testing.T) {
	for _, n := range []int{0, 1, 234, 235, 300, 1000} {
		name := surge.PlaceholderName(strings.Repeat("x", n))
		if len(name) > dns1123SubdomainMax {
			t.Errorf("PlaceholderName(len=%d) = %d chars, exceeds %d", n, len(name), dns1123SubdomainMax)
		}
	}
}

func TestPlaceholderNameDistinctForOverflowingNames(t *testing.T) {
	// Two long names sharing their truncation prefix but differing only in the
	// tail must still map to distinct placeholder names — the hash is over the
	// full name, so determinism (idempotency) never costs collision-freedom.
	base := strings.Repeat("a", 300)
	a := base + "-one"
	b := base + "-two"
	if surge.PlaceholderName(a) == surge.PlaceholderName(b) {
		t.Error("overflowing names with a shared prefix must not collide")
	}
}

func TestBuildPlaceholderUsesPlaceholderName(t *testing.T) {
	in := baseInputs()
	in.Candidate = claimNamed(strings.Repeat("a", 300))
	p := surge.BuildPlaceholder(in)
	if p.Name != surge.PlaceholderName(in.Candidate.Name) {
		t.Errorf("BuildPlaceholder name %q != PlaceholderName %q", p.Name, surge.PlaceholderName(in.Candidate.Name))
	}
	if len(p.Name) > dns1123SubdomainMax {
		t.Errorf("placeholder Pod name is %d chars, exceeds %d", len(p.Name), dns1123SubdomainMax)
	}
}
