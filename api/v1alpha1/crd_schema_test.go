package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// crdPath is the generated CRD manifest, relative to this package directory.
const crdPath = "../../config/crd/bases/noderotation.io_rotationpolicies.yaml"

// loadCRD returns the whole generated CRD manifest as a generic map so the tests
// can assert structural guarantees without depending on the apiextensions types.
func loadCRD(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(crdPath))
	if err != nil {
		t.Fatalf("read CRD manifest: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}
	return crd
}

// loadCRDVersion returns the single served v1alpha1 version entry.
func loadCRDVersion(t *testing.T) map[string]any {
	t.Helper()
	crd := loadCRD(t)
	versions, _ := crd["spec"].(map[string]any)["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("expected exactly one served version, got %d", len(versions))
	}
	v := versions[0].(map[string]any)
	if v["name"] != "v1alpha1" {
		t.Fatalf("version name = %v, want v1alpha1", v["name"])
	}
	return v
}

// loadCRDSchema returns the v1alpha1 openAPIV3Schema from the generated CRD.
func loadCRDSchema(t *testing.T) map[string]any {
	t.Helper()
	v := loadCRDVersion(t)
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

// TestCRDIsClusterScoped guards the design decision (spec §5.4) that
// RotationPolicy is cluster-scoped — NodePools are cluster-scoped, so a
// namespaced policy would be an impedance mismatch.
func TestCRDIsClusterScoped(t *testing.T) {
	crd := loadCRD(t)
	if scope := crd["spec"].(map[string]any)["scope"]; scope != "Cluster" {
		t.Errorf("CRD scope = %v, want Cluster", scope)
	}
}

// TestCRDFixesMaxUnavailableAtOne guards the v1 invariant that surge is serial
// per NodePool: maxUnavailable is pinned to 1, so an explicit 0 and any value >1
// are both rejected at admission rather than only at runtime.
func TestCRDFixesMaxUnavailableAtOne(t *testing.T) {
	schema := loadCRDSchema(t)
	surge := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["surge"].(map[string]any)
	mu := surge["properties"].(map[string]any)["maxUnavailable"].(map[string]any)
	if mu["minimum"] != float64(1) || mu["maximum"] != float64(1) {
		t.Errorf("maxUnavailable bounds = [%v, %v], want [1, 1]", mu["minimum"], mu["maximum"])
	}
}

// TestCRDRejectsForcefulFallbackEnabled guards that surge.forcefulFallback is
// reserved-disabled at admission until its controller implementation lands
// (#156): it must carry a CEL rule forbidding enabled:true, mirroring
// internal/policy.Validate and the prePull reservation.
func TestCRDRejectsForcefulFallbackEnabled(t *testing.T) {
	schema := loadCRDSchema(t)
	surge := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["surge"].(map[string]any)
	ff := surge["properties"].(map[string]any)["forcefulFallback"].(map[string]any)
	rules, ok := ff["x-kubernetes-validations"].([]any)
	if !ok || len(rules) == 0 {
		t.Fatalf("surge.forcefulFallback has no x-kubernetes-validations; enabled:true would be accepted")
	}
	for _, r := range rules {
		if rule, _ := r.(map[string]any)["rule"].(string); rule == "!self.enabled" {
			return
		}
	}
	t.Errorf("surge.forcefulFallback validations %v do not forbid enabled:true", rules)
}

// TestCRDHasStatusSubresource guards that the observational status subresource
// (spec §5.4 / §6) is served, so a status write never mutates spec and the
// RotationPolicyStatusReconciler's Status().Update path exists at the API level.
func TestCRDHasStatusSubresource(t *testing.T) {
	v := loadCRDVersion(t)
	subs, ok := v["subresources"].(map[string]any)
	if !ok {
		t.Fatal("version has no subresources; status subresource missing")
	}
	if _, ok := subs["status"]; !ok {
		t.Errorf("subresources = %v, must include status", subs)
	}
}
