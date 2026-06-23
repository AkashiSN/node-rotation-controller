// Package v1alpha1 holds the noderotation.io/v1alpha1 API types. v1alpha1 is the
// cluster-scoped RotationPolicy CRD that replaces the single policy.yaml ConfigMap
// (issue #119): one object per NodePool policy, so distinct NodePools can carry
// divergent rotation policy. The version is pre-1.0 and NOT frozen — it stabilizes
// to v1 at the 1.0 milestone (spec §6.1).
//
// +kubebuilder:object:generate=true
// +groupName=noderotation.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group/version for every type in this package. The group
// matches the noderotation.io/ annotation and label prefix used elsewhere.
var GroupVersion = schema.GroupVersion{Group: "noderotation.io", Version: "v1alpha1"}

// SchemeBuilder registers this package's types onto a runtime.Scheme. It uses the
// apimachinery builder (not controller-runtime's) to keep the api package's
// dependencies minimal so it stays cheap to import.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the types in this package to a Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &RotationPolicy{}, &RotationPolicyList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
