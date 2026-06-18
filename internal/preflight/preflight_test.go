package preflight_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/preflight"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

// fakeDisco stubs preflight.ResourceLister: it serves resources for exactly one
// group/version, mirroring discovery's "GroupVersion not found" error otherwise.
type fakeDisco struct {
	gv        string
	resources []string
	err       error
}

func (f fakeDisco) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	if f.err != nil {
		return nil, f.err
	}
	if gv != f.gv {
		return nil, errors.New("the server could not find the requested resource, GroupVersion " + gv + " not found")
	}
	rl := &metav1.APIResourceList{GroupVersion: gv}
	for _, name := range f.resources {
		rl.APIResources = append(rl.APIResources, metav1.APIResource{Name: name})
	}
	return rl, nil
}

// servingKarpenterV1 is a discovery stub serving the full required surface.
func servingKarpenterV1() fakeDisco {
	return fakeDisco{gv: preflight.GroupVersion, resources: []string{"nodeclaims", "nodepools", "nodeclaims/status"}}
}

func okReader() client.Reader {
	return fake.NewClientBuilder().WithScheme(scheme.New()).Build()
}

func TestCheckPasses(t *testing.T) {
	if err := preflight.Check(context.Background(), servingKarpenterV1(), okReader()); err != nil {
		t.Fatalf("Check on a compatible cluster: unexpected error %v", err)
	}
}

func TestCheckFailsWhenGroupVersionNotServed(t *testing.T) {
	// Karpenter absent, or serving only an older group/version.
	disco := fakeDisco{gv: "karpenter.sh/v1beta1", resources: []string{"nodeclaims", "nodepools"}}
	err := preflight.Check(context.Background(), disco, okReader())
	if err == nil {
		t.Fatal("expected an error when karpenter.sh/v1 is not served")
	}
	if !strings.Contains(err.Error(), preflight.GroupVersion) {
		t.Errorf("error should name the missing group/version %q: %v", preflight.GroupVersion, err)
	}
}

func TestCheckFailsWhenResourceMissing(t *testing.T) {
	// karpenter.sh/v1 served, but nodeclaims is missing from the surface.
	disco := fakeDisco{gv: preflight.GroupVersion, resources: []string{"nodepools"}}
	err := preflight.Check(context.Background(), disco, okReader())
	if err == nil || !strings.Contains(err.Error(), "nodeclaims") {
		t.Fatalf("expected an error naming the missing nodeclaims resource, got %v", err)
	}
}

func TestCheckFailsWhenNodePoolListForbidden(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(scheme.New()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*karpv1.NodePoolList); ok {
				return errors.New("nodepools.karpenter.sh is forbidden")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	err := preflight.Check(context.Background(), servingKarpenterV1(), reader)
	if err == nil || !strings.Contains(err.Error(), "NodePools") {
		t.Fatalf("expected a NodePool RBAC error, got %v", err)
	}
}

func TestCheckFailsWhenNodeClaimListForbidden(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(scheme.New()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*karpv1.NodeClaimList); ok {
				return errors.New("nodeclaims.karpenter.sh is forbidden")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	err := preflight.Check(context.Background(), servingKarpenterV1(), reader)
	if err == nil || !strings.Contains(err.Error(), "NodeClaims") {
		t.Fatalf("expected a NodeClaim RBAC error, got %v", err)
	}
}
