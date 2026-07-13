package adapt_test

import (
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/adapt"
)

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestClaimMapsEveryFieldSelectionReads pins the seven-field mapping. If Karpenter
// renames or retypes one of these, this test stops compiling — which is the point:
// the adapter is where a CRD change must be noticed.
func TestClaimMapsEveryFieldSelectionReads(t *testing.T) {
	e := 240 * time.Hour
	g := 30 * time.Minute
	del := metav1.NewTime(now)
	in := karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nc-1",
			CreationTimestamp: metav1.NewTime(now.Add(-100 * time.Hour)),
			DeletionTimestamp: &del,
			Annotations:       map[string]string{"noderotation.io/state": "failed"},
		},
		Spec: karpv1.NodeClaimSpec{
			ExpireAfter:            karpv1.NillableDuration{Duration: &e},
			TerminationGracePeriod: &metav1.Duration{Duration: g},
		},
		Status: karpv1.NodeClaimStatus{
			Conditions: []status.Condition{{Type: status.ConditionReady, Status: metav1.ConditionTrue}},
		},
	}

	got := adapt.Claim(&in)

	if got.Name != "nc-1" {
		t.Errorf("Name = %q, want nc-1", got.Name)
	}
	if !got.CreatedAt.Equal(now.Add(-100 * time.Hour)) {
		t.Errorf("CreatedAt = %v", got.CreatedAt)
	}
	if !got.Deleting {
		t.Error("Deleting = false, want true (DeletionTimestamp is set)")
	}
	if got.ExpireAfter == nil || *got.ExpireAfter != e {
		t.Errorf("ExpireAfter = %v, want %v", got.ExpireAfter, e)
	}
	if got.TGP == nil || *got.TGP != g {
		t.Errorf("TGP = %v, want %v", got.TGP, g)
	}
	if !got.Ready {
		t.Error("Ready = false, want true")
	}
	if got.Annotations["noderotation.io/state"] != "failed" {
		t.Errorf("Annotations not carried: %v", got.Annotations)
	}
}

// TestClaimNilsAreCarried: expireAfter Never and an unset TGP must stay nil — the
// predicates branch on exactly that (Never = no deadline; unset TGP = DrainFallback).
func TestClaimNilsAreCarried(t *testing.T) {
	got := adapt.Claim(&karpv1.NodeClaim{
		Spec: karpv1.NodeClaimSpec{ExpireAfter: karpv1.NillableDuration{Duration: nil}},
	})
	if got.ExpireAfter != nil {
		t.Errorf("ExpireAfter = %v, want nil (Never)", got.ExpireAfter)
	}
	if got.TGP != nil {
		t.Errorf("TGP = %v, want nil (unset)", got.TGP)
	}
	if got.Deleting {
		t.Error("Deleting = true, want false (no DeletionTimestamp)")
	}
	if got.Ready {
		t.Error("Ready = true, want false (no Ready condition)")
	}
}

// TestClaimsIndexAliasesTheSameSlice is the pointer-identity rule: the map must
// point into the caller's slice so a pick can be patched without a re-Get.
func TestClaimsIndexAliasesTheSameSlice(t *testing.T) {
	claims := []karpv1.NodeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
	}
	views, byName := adapt.Claims(claims)

	if len(views) != 2 {
		t.Fatalf("len(views) = %d, want 2", len(views))
	}
	if byName["b"] != &claims[1] {
		t.Fatal("byName[\"b\"] must alias &claims[1], not a copy")
	}
}
