package controller_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

func TestStatusReconcilerEnvtest(t *testing.T) {
	if testing.Short() || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest requires KUBEBUILDER_ASSETS; run via 'make test'")
	}
	testEnv := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			CRDs:  karpapis.CRDs,
			Paths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.New(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		// Controller names must be globally unique within a process, so a second
		// in-process iteration would fail at setup — which would make this test
		// impossible to run under -count=N. N repeats are how the #244 flake is
		// gated, so opt out of the name check; nothing here reads the metrics.
		Controller: ctrlconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := (&controller.RotationPolicyStatusReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var mgrErr error
	mgrDone := make(chan struct{})
	go func() { defer close(mgrDone); mgrErr = mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-mgrDone
		if mgrErr != nil && !errors.Is(mgrErr, context.Canceled) {
			t.Errorf("manager exited: %v", mgrErr)
		}
	})

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatal("cache did not sync")
	}

	cl := mgr.GetClient()
	pol := &noderotationv1alpha1.RotationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "api"},
		Spec: noderotationv1alpha1.RotationPolicySpec{
			NodePoolSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"workload": "api"}},
			MaintenanceWindows: []noderotationv1alpha1.MaintenanceWindow{{
				Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "00:00", End: "23:59",
			}},
		},
	}
	if err := cl.Create(ctx, pol); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	np := &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "np-api",
			Labels:      map[string]string{"workload": "api"},
			Annotations: map[string]string{annotations.ActiveRotation: "claim-x"},
		},
		Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: "eks.amazonaws.com", Kind: "NodeClass", Name: "default"},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
		}}},
	}
	if err := cl.Create(ctx, np); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		var got noderotationv1alpha1.RotationPolicy
		if err := cl.Get(ctx, types.NamespacedName{Name: "api"}, &got); err == nil {
			if got.Status.MatchedNodePools == 1 && got.Status.RotatingNodePools == 1 {
				return // success
			}
		}
		select {
		case <-deadline:
			var got noderotationv1alpha1.RotationPolicy
			_ = cl.Get(ctx, types.NamespacedName{Name: "api"}, &got)
			t.Fatalf("status not populated within 30s: %+v", got.Status)
		case <-tick.C:
		}
	}
}
