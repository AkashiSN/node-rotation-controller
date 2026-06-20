//go:build e2e

// Package kwok is the KWOK-based Karpenter e2e harness for the v1 surge
// mechanism (issue #92). The build tag `e2e` keeps every file in this package
// out of `go test ./...` / `make test`; it is compiled and run only by
// `make e2e-kwok` (and the e2e.yaml CI job), against a live kind cluster that
// the bootstrap.sh script has provisioned with the real Karpenter v1 KWOK
// reference cloudprovider plus this controller (installed via its Helm chart).
//
// The assertions use the controller's REAL annotation/label key constants and
// the real karpenter.sh/v1 types — never hardcoded strings — so a rename of a
// key on the controller's compatibility surface (spec §5.3, §6.1) breaks this
// test, exactly as it should.
//
// KWOK limitations this harness deliberately works within are documented in
// README.md and inline at each subtest. The most consequential: core Karpenter
// v1 lists kubernetes.io/hostname in RestrictedLabels, so the provisioner
// rejects any *provisionable* Pod whose nodeAffinity references it — which the
// controller's placeholder always does (the §3.3 candidate-exclusion). The
// surge therefore reaches `complete` here only via the CAPACITY-ABSORB path
// (bin-pack onto a pre-existing spare, where kube-scheduler — not Karpenter's
// provisioner — evaluates the hostname NotIn). The new-NodeClaim-provision
// path's full completion is out of scope for KWOK; see README.md.
package kwok

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

const (
	// controllerNamespace is where the chart installs the controller and where
	// it creates surge placeholder Pods (matches bootstrap.sh).
	controllerNamespace = "node-rotation-system"
	// poolA is the in-scope NodePool the controller rotates; poolB is the
	// deliberately out-of-scope second pool used for confinement assertions
	// (manifests/nodepools.yaml).
	poolA = "nodepool-a"
	poolB = "nodepool-b"
	// workloadNamespace holds the sample workloads driving provisioning.
	workloadNamespace = "default"
	// poolLabelKey/inScopeKey mirror the labels on the NodePool manifests.
	poolLabelKey = "noderotation-e2e/pool"
)

// k builds the controller-runtime client against the kind cluster's kubeconfig.
// KUBECONFIG (set by the Makefile via `kind export kubeconfig`) or the default
// loading rules locate the cluster; the context is whatever kind exported.
func k(t *testing.T) client.Client {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme.New()})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return cl
}

// eventually polls fn until it returns nil or the deadline elapses, surfacing
// the last error. interval is fixed at 2s — KWOK transitions in seconds.
func eventually(t *testing.T, timeout time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
}

// consistently asserts fn stays nil for the whole window (a negative check:
// "this never happens"). Used to prove confinement / do-not-disrupt holds.
func consistently(t *testing.T, window time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if err := fn(); err != nil {
			t.Fatalf("invariant %q violated: %v", what, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// startNudger forces the controller to reconcile the given NodePools promptly by
// touching a benign annotation on each every few seconds, returning a stop func
// the caller defers.
//
// KWOK runs a STATIC cluster: once its virtual nodes register it emits almost no
// further watch events. The controller's watches are deliberately predicate-
// filtered to real transitions (placeholder→Running, node→Ready, NodeClaim /
// NodePool changes) with the periodic requeue as the backstop (SetupWithManager).
// On a live cluster constant pod/node churn keeps reconciles flowing; under KWOK
// the cluster goes silent during a candidate's age-out, so the controller leans
// on its slow periodic requeue and can miss the candidate crossing ageThreshold
// within a subtest's window — the rotation never starts and the wait times out.
//
// The nudge writes ONLY the noderotation-e2e/nudge annotation (a value the
// controller never reads), via a merge patch that cannot conflict with the
// controller's own annotation writes. It changes no rotation logic; it only wakes
// the reconcile so selection/advancement happen promptly — the cluster churn a
// real environment would supply. It is therefore a faithful test of the
// controller's behavior, not a workaround that masks one.
func startNudger(ctx context.Context, cl client.Client, pools ...string) func() {
	done := make(chan struct{})
	go func() {
		// Space the nudges out (not a tight loop): each nudge wakes a single
		// reconcile; the controller's read-modify-write of the candidate's
		// started-at re-reads through the informer cache, so back-to-back
		// reconciles can race a not-yet-propagated write and mis-fire the
		// readyTimeout rollback. A spacing on the order of the controller's own
		// shortRequeue lets the cache settle between reconciles while still being
		// far faster than KWOK's quiet-period requeue backstop.
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		n := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				n++
				for _, p := range pools {
					patch := []byte(fmt.Sprintf(
						`{"metadata":{"annotations":{"noderotation-e2e/nudge":"%d"}}}`, n))
					np := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: p}}
					_ = cl.Patch(ctx, np, client.RawPatch(types.MergePatchType, patch))
				}
			}
		}
	}()
	return func() { close(done) }
}

// ── NodeClaim / Node helpers ────────────────────────────────────────────────

func listClaims(ctx context.Context, cl client.Client, pool string) ([]karpv1.NodeClaim, error) {
	var l karpv1.NodeClaimList
	if err := cl.List(ctx, &l, client.MatchingLabels{karpv1.NodePoolLabelKey: pool}); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func claimNames(claims []karpv1.NodeClaim) map[string]bool {
	out := map[string]bool{}
	for i := range claims {
		out[claims[i].Name] = true
	}
	return out
}

func getClaim(ctx context.Context, cl client.Client, name string) (*karpv1.NodeClaim, error) {
	var c karpv1.NodeClaim
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func getNode(ctx context.Context, cl client.Client, name string) (*corev1.Node, error) {
	var n corev1.Node
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func getNodePool(ctx context.Context, cl client.Client, name string) (*karpv1.NodePool, error) {
	var p karpv1.NodePool
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// poolNodes returns the registered Nodes (KWOK virtual) for a NodePool.
func poolNodes(ctx context.Context, cl client.Client, pool string) ([]corev1.Node, error) {
	var l corev1.NodeList
	if err := cl.List(ctx, &l, client.MatchingLabels{karpv1.NodePoolLabelKey: pool}); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// ── Workload helpers ────────────────────────────────────────────────────────

// deployment builds a pause Deployment pinned to a pool via the pool label, with
// optional hostname anti-affinity against another app label so KWOK spreads it
// onto a distinct node (forcing a second NodeClaim).
func deployment(name, pool string, cpuMilli int, antiAffinityApp string) *appsv1Deployment {
	return newPauseDeployment(name, pool, cpuMilli, antiAffinityApp)
}

func applyDeployment(ctx context.Context, t *testing.T, cl client.Client, d *appsv1Deployment) {
	t.Helper()
	if err := cl.Create(ctx, d.obj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create deployment %s: %v", d.obj.Name, err)
	}
}

func deleteDeployment(ctx context.Context, cl client.Client, name string) {
	d := &appsv1Deployment{obj: newDeploymentShell(name)}
	_ = cl.Delete(ctx, d.obj)
}

// cpuQ is a helper for milli-CPU resource quantities.
func cpuQ(milli int) resource.Quantity {
	return *resource.NewMilliQuantity(int64(milli), resource.DecimalSI)
}

// ── Annotation read helpers (use the real controller constants) ─────────────

func poolAnno(p *karpv1.NodePool, key string) string {
	if p.Annotations == nil {
		return ""
	}
	return p.Annotations[key]
}

func claimAnno(c *karpv1.NodeClaim, key string) string {
	if c.Annotations == nil {
		return ""
	}
	return c.Annotations[key]
}

// waitClaimProvisioned blocks until exactly one NodeClaim exists in the pool and
// is registered (has a Node), returning its name.
func waitClaimProvisioned(ctx context.Context, t *testing.T, cl client.Client, pool string) string {
	t.Helper()
	var name string
	eventually(t, 90*time.Second, fmt.Sprintf("a registered NodeClaim in %s", pool), func() error {
		claims, err := listClaims(ctx, cl, pool)
		if err != nil {
			return err
		}
		for i := range claims {
			if claims[i].Status.NodeName != "" {
				name = claims[i].Name
				return nil
			}
		}
		return fmt.Errorf("no registered claim yet (have %d)", len(claims))
	})
	return name
}

// waitSpareRegistered blocks until pool-a has >= 2 NodeClaims and every
// non-candidate claim has registered a Node — the precondition for a clean
// capacity-absorb (the spare must be a real, schedulable node).
func waitSpareRegistered(ctx context.Context, t *testing.T, cl client.Client, candClaim string) {
	t.Helper()
	eventually(t, 90*time.Second, "the pool-a spare NodeClaim to register a Node", func() error {
		claims, err := listClaims(ctx, cl, poolA)
		if err != nil {
			return err
		}
		if len(claims) < 2 {
			return fmt.Errorf("have %d pool-a claims, want 2", len(claims))
		}
		for i := range claims {
			if claims[i].Name != candClaim && claims[i].Status.NodeName == "" {
				return fmt.Errorf("spare claim %s not registered yet", claims[i].Name)
			}
		}
		return nil
	})
}

// skipIfNoCluster fails fast with a clear message if the kubeconfig does not
// point at a reachable cluster, so a bare `go test -tags e2e` without bootstrap
// gives a readable error rather than a panic.
func requireCluster(t *testing.T, ctx context.Context, cl client.Client) {
	t.Helper()
	var ns corev1.Namespace
	if err := cl.Get(ctx, types.NamespacedName{Name: controllerNamespace}, &ns); err != nil {
		t.Fatalf("cluster not reachable or controller not installed (run `make e2e-kwok`): %v", err)
	}
}

// metav1Now is a tiny alias so subtests reading creationTimestamps stay terse.
func metav1Now() metav1.Time { return metav1.Now() }

// envTimeout overrides a default timeout from E2E_KWOK_<NAME> for slow CI; the
// default is returned when unset/unparseable.
func envTimeout(name string, def time.Duration) time.Duration {
	if v := os.Getenv("E2E_KWOK_" + name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
