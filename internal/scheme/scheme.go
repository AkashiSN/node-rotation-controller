// Package scheme assembles the runtime.Scheme shared by the manager and tests.
package scheme

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
)

// New returns a Scheme covering every type the controller works with: the
// core Kubernetes types (Nodes, Pods, Leases, Events, ...), karpenter.sh/v1
// (NodeClaim, NodePool), and noderotation.io/v1alpha1 (RotationPolicy). All
// client and cache construction must go through this single source so the type
// universe never diverges between binaries and tests.
func New() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(noderotationv1alpha1.AddToScheme(s))

	gv := schema.GroupVersion{Group: karpapis.Group, Version: "v1"}
	// Importing karpv1 triggers an init() that registers these types onto the
	// client-go *global* scheme singleton only. Because this constructor builds
	// its own Scheme (runtime.NewScheme) rather than reusing the global one,
	// the types must be registered here explicitly.
	s.AddKnownTypes(gv,
		&karpv1.NodeClaim{},
		&karpv1.NodeClaimList{},
		&karpv1.NodePool{},
		&karpv1.NodePoolList{},
	)
	metav1.AddToGroupVersion(s, gv)
	return s
}
