//go:build e2e

package kwok

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// metricsService is the ClusterIP Service the chart creates for /metrics
// (charts/.../templates/service.yaml). The release name in bootstrap.sh is
// "node-rotation", so the fullname-prefixed Service is this.
const metricsService = "node-rotation-node-rotation-controller-metrics"

// scrapeMetrics fetches the controller's /metrics through the kube-apiserver
// Service proxy — no Prometheus, no port-forward goroutine. This is the issue
// #92 requirement: success + drain-duration observed by SCRAPING the endpoint,
// not by reading controller internal state.
func scrapeMetrics(ctx context.Context, t *testing.T) string {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s:8080/proxy/metrics",
		controllerNamespace, metricsService)
	raw, err := cs.RESTClient().Get().AbsPath(path).DoRaw(ctx)
	if err != nil {
		t.Fatalf("scrape /metrics via service proxy: %v", err)
	}
	return string(raw)
}

// assertSuccessAndDrainMetrics polls the scraped /metrics until both the success
// counter for the pool is >= 1 AND a drain-phase duration histogram has a
// non-zero count. Both are §4.2 observables produced only on a genuine
// (draining→complete) rotation, so their presence corroborates the absorb
// rotation actually completed through the drain path.
func assertSuccessAndDrainMetrics(ctx context.Context, t *testing.T, pool string) {
	t.Helper()
	eventually(t, 60*time.Second, "success + drain-duration metrics on /metrics", func() error {
		body := scrapeMetrics(ctx, t)
		if !hasSuccess(body, pool) {
			return fmt.Errorf("noderotation_completed_total{nodepool=%q,outcome=\"success\"} not >=1 yet", pool)
		}
		if !hasDrainDuration(body, pool) {
			return fmt.Errorf("noderotation_duration_seconds drain histogram for %q has zero count", pool)
		}
		return nil
	})
}

// hasSuccess reports whether the success counter for the pool parses to >= 1.
func hasSuccess(metrics, pool string) bool {
	want := fmt.Sprintf(`noderotation_completed_total{nodepool="%s",outcome="success"}`, pool)
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, want) {
			return metricValueAtLeastOne(line)
		}
	}
	return false
}

// hasDrainDuration reports whether the drain-phase duration histogram for the
// pool has recorded at least one observation (its _count series is >= 1).
func hasDrainDuration(metrics, pool string) bool {
	want := fmt.Sprintf(`noderotation_duration_seconds_count{nodepool="%s",phase="drain"}`, pool)
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, want) {
			return metricValueAtLeastOne(line)
		}
	}
	return false
}

// metricValueAtLeastOne parses the trailing value of a Prometheus text-format
// line and reports whether it is >= 1.
func metricValueAtLeastOne(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	v := fields[len(fields)-1]
	// Counters/histogram counts are integers in practice; a "0" means not yet.
	return v != "0" && v != "0.0" && !strings.HasPrefix(v, "0e")
}
