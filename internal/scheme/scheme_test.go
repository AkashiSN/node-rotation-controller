package scheme_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

func TestNewRegistersKarpenterV1(t *testing.T) {
	s := scheme.New()
	for _, kind := range []string{"NodeClaim", "NodeClaimList", "NodePool", "NodePoolList"} {
		gvk := schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: kind}
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}

func TestNewRegistersCoreTypes(t *testing.T) {
	s := scheme.New()
	for _, gvk := range []schema.GroupVersionKind{
		{Version: "v1", Kind: "Node"},
		{Version: "v1", Kind: "Pod"},
		{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"},
		{Version: "v1", Kind: "Event"},
	} {
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}
