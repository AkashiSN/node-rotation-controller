// Package preflight validates, at startup, that the cluster serves the public
// Karpenter v1 API surface the controller depends on (issue #58). EKS Auto Mode
// is the primary target, but its managed Karpenter minor is not visible to
// users, so the contract is karpenter.sh/v1 compatibility — not a specific
// Karpenter version. A missing or unreadable API surface is turned into an
// immediate, actionable startup error instead of a deferred reconcile failure.
package preflight

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// GroupVersion is the Karpenter public API surface the controller is built
// against. The compatibility contract is this group/version, deliberately
// independent of the Karpenter controller's minor (e.g. v1.13.x).
const GroupVersion = "karpenter.sh/v1"

// requiredResources are the Karpenter v1 resources the controller reads and
// writes (NodeClaims it deletes; NodePools it reads and annotates, spec §3.3).
var requiredResources = []string{"nodeclaims", "nodepools"}

// ResourceLister is the slice of discovery.DiscoveryInterface that Check needs.
// A *discovery.DiscoveryClient satisfies it; tests pass a stub.
type ResourceLister interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// Check fails fast when the required Karpenter v1 API surface is unavailable or
// unreadable (issue #58). It verifies two things before the manager begins
// reconciling:
//
//  1. The cluster serves karpenter.sh/v1 and that version includes the
//     nodeclaims and nodepools resources — an incompatible Karpenter (absent, or
//     serving only an older group/version) is reported as such.
//  2. The controller can list NodePools and NodeClaims with its configured RBAC.
//     The typed List doubles as a schema-compatibility probe: a successful decode
//     of the karpenter.sh/v1 types the controller is built against confirms the
//     served schema is wire-compatible with the fields it relies on.
//
// CRD field-level introspection (walking each CRD's OpenAPI schema) is
// intentionally not attempted: it is brittle and the typed karpenter.sh/v1
// contract already covers the required fields (spec §1.1 compatibility note).
func Check(ctx context.Context, disco ResourceLister, reader client.Reader) error {
	if err := checkAPISurface(disco); err != nil {
		return err
	}
	return checkAccess(ctx, reader)
}

// checkAPISurface confirms karpenter.sh/v1 is served with the required resources.
func checkAPISurface(disco ResourceLister) error {
	rl, err := disco.ServerResourcesForGroupVersion(GroupVersion)
	if err != nil {
		return fmt.Errorf("the Karpenter API %q is not served; the controller requires Karpenter v1+ (EKS Auto Mode or self-managed Karpenter): %w", GroupVersion, err)
	}
	served := make(map[string]bool, len(rl.APIResources))
	for _, r := range rl.APIResources {
		served[r.Name] = true
	}
	for _, name := range requiredResources {
		if !served[name] {
			return fmt.Errorf("the Karpenter resource %q is not served under %q; the cluster's Karpenter API surface is incompatible", name, GroupVersion)
		}
	}
	return nil
}

// checkAccess confirms the controller can list both kinds with its RBAC. Limit(1)
// keeps the probe cheap — it only needs the request to be authorized and the
// response to decode, not the full set.
func checkAccess(ctx context.Context, reader client.Reader) error {
	var nps karpv1.NodePoolList
	if err := reader.List(ctx, &nps, client.Limit(1)); err != nil {
		return fmt.Errorf("cannot list NodePools (verify the controller's RBAC grants list/watch on nodepools.karpenter.sh): %w", err)
	}
	var ncs karpv1.NodeClaimList
	if err := reader.List(ctx, &ncs, client.Limit(1)); err != nil {
		return fmt.Errorf("cannot list NodeClaims (verify the controller's RBAC grants list/watch on nodeclaims.karpenter.sh): %w", err)
	}
	return nil
}
