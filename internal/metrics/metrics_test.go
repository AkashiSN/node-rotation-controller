package metrics_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/AkashiSN/node-rotation-controller/internal/controller"
	"github.com/AkashiSN/node-rotation-controller/internal/metrics"
)

// metricValue gathers reg and returns the value of the named metric whose label
// set matches labels (a counter or gauge). It fails the test when not found.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	switch {
	case m.Gauge != nil:
		return m.GetGauge().GetValue()
	case m.Counter != nil:
		return m.GetCounter().GetValue()
	default:
		t.Fatalf("metric %s is neither gauge nor counter", name)
		return 0
	}
}

func findMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m, labels) {
				return m
			}
		}
	}
	t.Fatalf("metric %s%v not found", name, labels)
	return nil
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestObservePoolSetsAllGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := metrics.New(reg)

	rec.ObservePool("api", controller.PoolObservation{
		Candidates:      3,
		InProgress:      1,
		ShortLeadNodes:  2,
		RetryCount:      4,
		DrainStuck:      true,
		AgeThreshold:    287 * time.Hour,
		RotationChances: 2,
		WindowPeriod:    24 * time.Hour,
		FreezeUntil:     time.Unix(1700000000, 0),
	})

	pool := map[string]string{"nodepool": "api"}
	for _, tc := range []struct {
		name string
		want float64
	}{
		{"noderotation_candidates", 3},
		{"noderotation_in_progress", 1},
		{"noderotation_short_lead_nodes", 2},
		{"noderotation_retry_count", 4},
		{"noderotation_drain_stuck", 1},
		{"noderotation_age_threshold_seconds", (287 * time.Hour).Seconds()},
		{"noderotation_rotation_chances", 2},
		{"noderotation_window_period_seconds", (24 * time.Hour).Seconds()},
		{"noderotation_freeze_until_timestamp", 1700000000},
	} {
		if got := metricValue(t, reg, tc.name, pool); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A cleared drain (DrainStuck false, no freeze) must reset the 0/1 gauges so the
// alert resolves — the reason the gauges are recomputed each reconcile.
func TestObservePoolResetsGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := metrics.New(reg)
	rec.ObservePool("api", controller.PoolObservation{DrainStuck: true, RetryCount: 3})
	rec.ObservePool("api", controller.PoolObservation{DrainStuck: false, RetryCount: 0})

	pool := map[string]string{"nodepool": "api"}
	if got := metricValue(t, reg, "noderotation_drain_stuck", pool); got != 0 {
		t.Errorf("drain_stuck not reset: got %v", got)
	}
	if got := metricValue(t, reg, "noderotation_retry_count", pool); got != 0 {
		t.Errorf("retry_count not reset: got %v", got)
	}
	if got := metricValue(t, reg, "noderotation_freeze_until_timestamp", pool); got != 0 {
		t.Errorf("freeze_until not reset to 0 for an absent freeze: got %v", got)
	}
}

func TestCompletedCountersByOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := metrics.New(reg)
	rec.Success("api")
	rec.Success("api")
	rec.Failure("api", "nc-1")
	rec.Expired("api", "nc-2")

	for outcome, want := range map[string]float64{"success": 2, "failure": 1, "expired": 1} {
		labels := map[string]string{"nodepool": "api", "outcome": outcome}
		if got := metricValue(t, reg, "noderotation_completed_total", labels); got != want {
			t.Errorf("completed_total{outcome=%s} = %v, want %v", outcome, got, want)
		}
	}
}

func TestWindowActiveGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := metrics.New(reg)

	rec.ObserveWindow(true)
	if got := metricValue(t, reg, "noderotation_window_active", map[string]string{}); got != 1 {
		t.Errorf("window_active = %v, want 1", got)
	}
	rec.ObserveWindow(false)
	if got := metricValue(t, reg, "noderotation_window_active", map[string]string{}); got != 0 {
		t.Errorf("window_active = %v, want 0", got)
	}
}

func TestDurationHistogramRecordsObservation(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := metrics.New(reg)
	rec.ObserveDuration("api", controller.PhaseSurgeWait, 2*time.Minute)

	m := findMetric(t, reg, "noderotation_duration_seconds", map[string]string{
		"nodepool": "api", "phase": controller.PhaseSurgeWait,
	})
	if m.Histogram == nil {
		t.Fatal("duration_seconds is not a histogram")
	}
	if c := m.GetHistogram().GetSampleCount(); c != 1 {
		t.Errorf("sample count = %d, want 1", c)
	}
	if s := m.GetHistogram().GetSampleSum(); s != (2 * time.Minute).Seconds() {
		t.Errorf("sample sum = %v, want %v", s, (2 * time.Minute).Seconds())
	}
}

// New must register on the controller-runtime registry the manager serves on
// /metrics; a second New on the same registry would panic on duplicate
// registration, so the test uses a private registry — this just guards the
// production path compiles against the shared registry type.
func TestNewRegistersOnRegisterer(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = metrics.New(reg)
	if n := countSeries(t, reg); n == 0 {
		t.Fatal("New registered no collectors")
	}
}

func countSeries(t *testing.T, reg *prometheus.Registry) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return len(mfs)
}
