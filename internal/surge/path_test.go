package surge_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

func claimCreated(at time.Time) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:              "nc-surge",
		CreationTimestamp: metav1.NewTime(at),
	}}
}

// The two surge paths (spec §3.3) are told apart by one question: did the host's
// NodeClaim come into existence during this attempt?
func TestHostPathNamesTheTwoProvisioningPaths(t *testing.T) {
	startedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	if got := surge.HostPath(claimCreated(startedAt.Add(30*time.Second)), startedAt); got != surge.PathProvisioned {
		t.Errorf("a claim created after started-at is this attempt's new node: got %q, want %q", got, surge.PathProvisioned)
	}
	if got := surge.HostPath(claimCreated(startedAt.Add(-time.Hour)), startedAt); got != surge.PathAbsorbed {
		t.Errorf("a claim that predates started-at is pre-existing capacity: got %q, want %q", got, surge.PathAbsorbed)
	}
}

// The boundary is pinned deliberately, and in the same direction the rollback's
// reap guard already reads it: CreationTimestamp is stored to the second and
// started-at is an RFC3339 string, so a node launched inside the attempt's first
// second is indistinguishable from one that predates it. Both guards must call
// that host pre-existing, or the rollback could reap a node it also refuses to
// call its own.
func TestHostPathTreatsTheSameSecondAsPreExisting(t *testing.T) {
	startedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	if got := surge.HostPath(claimCreated(startedAt), startedAt); got != surge.PathAbsorbed {
		t.Errorf("a claim created in the same second as started-at must read as pre-existing: got %q, want %q", got, surge.PathAbsorbed)
	}
	if surge.CreatedByAttempt(claimCreated(startedAt), startedAt) {
		t.Error("CreatedByAttempt must be strict: the same second is not after started-at")
	}
}

// An unresolvable host yields no answer rather than a guessed one — the callers
// omit the field instead of printing a value the predicate did not establish.
func TestHostPathIsEmptyWithoutAClaim(t *testing.T) {
	if got := surge.HostPath(nil, time.Now()); got != "" {
		t.Errorf("a nil claim must yield no path: got %q", got)
	}
}
