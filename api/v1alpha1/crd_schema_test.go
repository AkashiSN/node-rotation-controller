package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// crdPath is the generated CRD manifest, relative to this package directory.
const crdPath = "../../config/crd/bases/noderotation.io_rotationpolicies.yaml"

// loadCRDSchema returns the v1alpha1 openAPIV3Schema from the generated CRD as a
// generic map so the tests can assert structural guarantees without depending on
// the apiextensions types.
func loadCRDSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(crdPath))
	if err != nil {
		t.Fatalf("read CRD manifest: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}
	versions, _ := crd["spec"].(map[string]any)["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("expected exactly one served version, got %d", len(versions))
	}
	v := versions[0].(map[string]any)
	if v["name"] != "v1alpha1" {
		t.Fatalf("version name = %v, want v1alpha1", v["name"])
	}
	return v["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
}

// TestCRDRequiresSpec guards the admission guarantee that spec (and the
// nodePoolSelector/maintenanceWindows it carries) cannot be bypassed by an empty
// RotationPolicy — the top-level schema must list spec as required.
func TestCRDRequiresSpec(t *testing.T) {
	schema := loadCRDSchema(t)
	required, _ := schema["required"].([]any)
	found := false
	for _, r := range required {
		if r == "spec" {
			found = true
		}
	}
	if !found {
		t.Errorf("CRD openAPIV3Schema.required = %v, must include \"spec\"", required)
	}
}

// TestCRDRejectsPrePullEnabled guards that the v1 reservation of pre-pull is
// enforced at admission: prePull must carry a CEL rule forbidding enabled:true,
// mirroring the ConfigMap validator (internal/policy.Validate).
func TestCRDRejectsPrePullEnabled(t *testing.T) {
	schema := loadCRDSchema(t)
	prePull := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["prePull"].(map[string]any)
	rules, ok := prePull["x-kubernetes-validations"].([]any)
	if !ok || len(rules) == 0 {
		t.Fatalf("prePull has no x-kubernetes-validations; enabled:true would be accepted")
	}
	for _, r := range rules {
		if rule, _ := r.(map[string]any)["rule"].(string); rule == "!self.enabled" {
			return
		}
	}
	t.Errorf("prePull validations %v do not forbid enabled:true", rules)
}
