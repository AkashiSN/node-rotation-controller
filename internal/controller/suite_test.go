package controller_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

// TestManagerReconcilesNodeClaim boots a real API server (envtest) with
// Karpenter's embedded CRDs, starts the manager, creates a NodeClaim, and
// asserts the stub reconciler observes it — an end-to-end proof of the
// v0.2 bootstrap (scheme, cache, watch, reconcile).
func TestManagerReconcilesNodeClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest requires KUBEBUILDER_ASSETS; run via 'make test'")
	}

	testEnv := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{CRDs: karpapis.CRDs},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.New(),
		Metrics:                metricsserver.Options{BindAddress: "0"}, // disabled in tests
		HealthProbeBindAddress: "0",                                     // disabled in tests
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	inner := &controller.NodeClaimReconciler{Client: mgr.GetClient()}
	seen := make(chan string, 16)
	err = ctrl.NewControllerManagedBy(mgr).
		Named("nodeclaim-smoke").
		For(&karpv1.NodeClaim{}).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			res, err := inner.Reconcile(ctx, req)
			seen <- req.Name
			return res, err
		}))
	if err != nil {
		t.Fatalf("failed to set up controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "nc-smoke"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: "eks.amazonaws.com",
				Kind:  "NodeClass",
				Name:  "default",
			},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
		},
	}
	if err := mgr.GetClient().Create(ctx, nc); err != nil {
		t.Fatalf("failed to create NodeClaim: %v", err)
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case name := <-seen:
			if name == "nc-smoke" {
				return
			}
		case <-deadline:
			t.Fatal("reconcile for nc-smoke not observed within 30s")
		}
	}
}
