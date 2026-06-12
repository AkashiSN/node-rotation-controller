package controller_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

func TestReconcileExistingNodeClaim(t *testing.T) {
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-test"}}
	c := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(nc).Build()
	r := &controller.NodeClaimReconciler{Client: c}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nc-test"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsZero() {
		t.Errorf("expected zero result, got %+v", res)
	}
}

func TestReconcileMissingNodeClaimIsNotAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.New()).Build()
	r := &controller.NodeClaimReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "absent"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
