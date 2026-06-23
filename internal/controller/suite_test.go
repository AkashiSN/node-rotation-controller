package controller_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

// smokeRotationPolicy is a minimal, valid RotationPolicy whose selector matches
// the smoke-test NodePool. The all-week window keeps the schedule well-formed;
// the smoke test only proves the manager boots, watches (NodePool and
// RotationPolicy), and reconciles.
func smokeRotationPolicy() *noderotationv1alpha1.RotationPolicy {
	return &noderotationv1alpha1.RotationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke"},
		Spec: noderotationv1alpha1.RotationPolicySpec{
			NodePoolSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"workload": "api"}},
			MaintenanceWindows: []noderotationv1alpha1.MaintenanceWindow{{
				Timezone: "UTC",
				Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
				Start:    "00:00",
				End:      "23:59",
			}},
		},
	}
}

// TestManagerReconcilesNodePool boots a real API server (envtest) with
// Karpenter's embedded CRDs, starts the manager, creates an in-scope NodePool,
// and asserts the rotation reconciler observes it — an end-to-end proof of the
// bootstrap (scheme, cache, watch, reconcile).
func TestManagerReconcilesNodePool(t *testing.T) {
	// envtest needs the etcd/kube-apiserver binaries that KUBEBUILDER_ASSETS
	// points at; without them it falls back to a default path that does not
	// exist. 'make test' sets the variable, so skip rather than fail when a
	// plain 'go test ./...' (or 'go test -short') runs without it.
	if testing.Short() || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest requires KUBEBUILDER_ASSETS; run via 'make test'")
	}

	testEnv := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			CRDs: karpapis.CRDs,
			// Install the noderotation.io RotationPolicy CRD alongside Karpenter's so
			// the controller can resolve a NodePool's governing policy (spec §5.4).
			Paths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		},
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

	inner := &controller.RotationReconciler{
		Client:            mgr.GetClient(),
		Namespace:         "node-rotation-system",
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
	}
	seen := make(chan string, 16)
	err = ctrl.NewControllerManagedBy(mgr).
		Named("rotation-smoke").
		For(&karpv1.NodePool{}).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			res, err := inner.Reconcile(ctx, req)
			seen <- req.Name
			return res, err
		}))
	if err != nil {
		t.Fatalf("failed to set up controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var mgrErr error
	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		mgrErr = mgr.Start(ctx)
	}()
	// Drain the manager before stopping the API server: cancel, wait for
	// Start to return, surface any non-cancellation error, then tear down.
	// Registered last so it runs first (Cleanup is LIFO).
	t.Cleanup(func() {
		cancel()
		<-mgrDone
		if mgrErr != nil && !errors.Is(mgrErr, context.Canceled) {
			t.Errorf("manager exited with error: %v", mgrErr)
		}
	})

	// The watch must be established before the Create, otherwise the create
	// event can fire before the informer is listening and never be observed.
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatal("cache did not sync within 30s")
	}

	// Create the governing RotationPolicy before the NodePool so the first
	// reconcile of np-smoke resolves a policy (spec §5.4) and exercises the read.
	if err := mgr.GetClient().Create(ctx, smokeRotationPolicy()); err != nil {
		t.Fatalf("failed to create RotationPolicy: %v", err)
	}

	np := &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "np-smoke",
			Labels: map[string]string{"workload": "api"},
		},
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: "eks.amazonaws.com",
						Kind:  "NodeClass",
						Name:  "default",
					},
					Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
				},
			},
		},
	}
	if err := mgr.GetClient().Create(ctx, np); err != nil {
		t.Fatalf("failed to create NodePool: %v", err)
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case name := <-seen:
			if name == "np-smoke" {
				return
			}
		case <-deadline:
			t.Fatal("reconcile for np-smoke not observed within 30s")
		}
	}
}
