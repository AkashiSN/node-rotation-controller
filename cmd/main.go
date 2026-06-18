// The node-rotation-controller manager entrypoint (spec §5.1): a
// controller-runtime manager with leader election, /metrics, and
// health/readiness probes, driving the rotation state machine (spec §5.2).
package main

import (
	"flag"
	"os"

	// tzdata embeds the IANA timezone database so time.LoadLocation resolves
	// names like "Asia/Tokyo" on a distroless image (spec §3.1, internal/window).
	_ "time/tzdata"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	appmetrics "github.com/AkashiSN/node-rotation-controller/internal/metrics"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var configPath string
	var namespace string
	var placeholderImage string
	var priorityClassName string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health/readiness probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election. Required when running replicas=2 (spec §5.1); "+
			"disable only for local development.")
	flag.StringVar(&configPath, "config-path", "/etc/node-rotation/policy.yaml",
		"Path to the policy.yaml document (mounted from the node-rotation-config ConfigMap, spec §5.4).")
	flag.StringVar(&namespace, "namespace", "node-rotation-system",
		"Namespace the surge placeholder Pods are created in.")
	flag.StringVar(&placeholderImage, "placeholder-image", "registry.k8s.io/pause:3.10",
		"The pause image the surge placeholder Pod runs (spec §3.3).")
	flag.StringVar(&priorityClassName, "priority-class", "noderotation-placeholder",
		"The dedicated negative-priority class for the surge placeholder Pod (spec §3.3).")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")

	pol, sched, err := loadPolicy(configPath)
	if err != nil {
		setupLog.Error(err, "unable to load policy", "path", configPath)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme.New(),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "node-rotation-controller.noderotation.io",
		// Release the Lease on SIGTERM so the standby replica takes over
		// immediately rather than waiting out LeaseDuration. Safe here: all
		// rotation state is annotation-backed (spec §5.3), so there is no
		// in-memory state a newly elected leader could corrupt.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register the §4.2 metrics on the controller-runtime registry the manager
	// already serves on /metrics — no extra server.
	recorder := appmetrics.New(ctrlmetrics.Registry)

	reconciler := &controller.RotationReconciler{
		Client:            mgr.GetClient(),
		Policy:            pol,
		Schedule:          sched,
		Namespace:         namespace,
		PlaceholderImage:  placeholderImage,
		PriorityClassName: priorityClassName,
		Recorder:          recorder,
		Events:            mgr.GetEventRecorder("node-rotation-controller"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "rotation")
		os.Exit(1)
	}

	// The spec §5.3 startup sweep is gated into the reconciler's first Reconcile
	// (see RotationReconciler.sweepOnce), so it completes before any NodePool can
	// start or resume a rotation. A separate manager Runnable would not be ordered
	// against the reconcile loop — controller-runtime starts leader runnables
	// concurrently — so the sweep could read a stale anchor snapshot and reap a
	// live rotation's artifacts (PR #33 review).

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// loadPolicy reads and validates the policy document and builds the maintenance
// schedule from it (spec §5.4, §3.1).
func loadPolicy(path string) (*policy.Policy, *window.Schedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	pol, err := policy.Load(data)
	if err != nil {
		return nil, nil, err
	}
	sched, err := window.New(pol.MaintenanceWindows)
	if err != nil {
		return nil, nil, err
	}
	return pol, sched, nil
}
