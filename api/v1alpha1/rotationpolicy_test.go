package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestAddToSchemeRegistersRotationPolicy proves the package registers its types
// onto a Scheme so clients and the cache can encode/decode RotationPolicy under
// the noderotation.io/v1alpha1 GroupVersion.
func TestAddToSchemeRegistersRotationPolicy(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, obj := range []runtime.Object{&RotationPolicy{}, &RotationPolicyList{}} {
		gvks, _, err := s.ObjectKinds(obj)
		if err != nil {
			t.Fatalf("ObjectKinds(%T): %v", obj, err)
		}
		if len(gvks) == 0 {
			t.Fatalf("ObjectKinds(%T): no GVK registered", obj)
		}
		if got := gvks[0].GroupVersion(); got != GroupVersion {
			t.Errorf("ObjectKinds(%T) group/version = %s, want %s", obj, got, GroupVersion)
		}
	}
}

// TestRotationPolicyDeepCopyRoundTrip proves the generated deepcopy produces an
// independent, equal copy: mutating the copy must not affect the original.
func TestRotationPolicyDeepCopyRoundTrip(t *testing.T) {
	k := int32(2)
	mu := int32(1)
	orig := &RotationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "api"},
		Spec: RotationPolicySpec{
			NodePoolSelector:   &metav1.LabelSelector{MatchLabels: map[string]string{"workload": "api"}},
			AgeThreshold:       "auto",
			MinRotationChances: &k,
			MaintenanceWindows: []MaintenanceWindow{{
				Timezone: "Asia/Tokyo",
				Days:     []string{"Wed", "Sat"},
				Start:    "02:00",
				End:      "06:00",
			}},
			Surge: Surge{
				MaxUnavailable: &mu,
				ReadyTimeout:   &metav1.Duration{Duration: 0},
				MatchNodeRequirements: MatchNodeRequirements{
					Required:  []string{"topology.kubernetes.io/zone"},
					Preferred: []string{},
				},
			},
		},
	}

	cp := orig.DeepCopy()
	if cp == orig {
		t.Fatal("DeepCopy returned the same pointer")
	}

	// Mutate the copy's deep fields; the original must be untouched.
	cp.Spec.NodePoolSelector.MatchLabels["workload"] = "batch"
	cp.Spec.MaintenanceWindows[0].Days[0] = "Mon"
	cp.Spec.Surge.MatchNodeRequirements.Required[0] = "kubernetes.io/arch"

	if orig.Spec.NodePoolSelector.MatchLabels["workload"] != "api" {
		t.Error("mutating copy's selector leaked into the original")
	}
	if orig.Spec.MaintenanceWindows[0].Days[0] != "Wed" {
		t.Error("mutating copy's window days leaked into the original")
	}
	if orig.Spec.Surge.MatchNodeRequirements.Required[0] != "topology.kubernetes.io/zone" {
		t.Error("mutating copy's required requirements leaked into the original")
	}

	// DeepCopyObject must return a *RotationPolicy runtime.Object.
	if _, ok := orig.DeepCopyObject().(*RotationPolicy); !ok {
		t.Error("DeepCopyObject did not return *RotationPolicy")
	}
}

func TestRotationPolicyStatusDeepCopyPreservesRotatingNodePools(t *testing.T) {
	in := &RotationPolicy{
		Status: RotationPolicyStatus{
			ObservedGeneration: 7,
			MatchedNodePools:   3,
			RotatingNodePools:  2,
			Conditions: []metav1.Condition{{
				Type:   ConditionTypeReady,
				Status: metav1.ConditionTrue,
				Reason: ReasonAccepted,
			}},
		},
	}
	out := in.DeepCopy()
	if out.Status.RotatingNodePools != 2 {
		t.Errorf("RotatingNodePools = %d, want 2", out.Status.RotatingNodePools)
	}
	if out.Status.Conditions[0].Type != ConditionTypeReady || out.Status.Conditions[0].Reason != ReasonAccepted {
		t.Errorf("condition not preserved: %+v", out.Status.Conditions[0])
	}
}
