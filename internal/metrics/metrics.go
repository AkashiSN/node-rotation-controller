// Package metrics is the Prometheus-backed implementation of the §4.2
// observability surface. It implements controller.Recorder so the rotation state
// machine stays free of any metrics-library dependency (controller defines the
// interface; this package supplies the wiring). Collectors register on the
// controller-runtime metrics registry, so the manager's already-bound /metrics
// endpoint serves them with no extra server.
//
// Label semantics follow the §4.2 label note: window_active is intentionally
// label-free (cluster-wide in v1); window_period_seconds keeps a forward-looking
// nodepool label that is identical across pools in v1.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AkashiSN/node-rotation-controller/internal/controller"
)

// Recorder is the Prometheus-backed controller.Recorder.
type Recorder struct {
	completed       *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	candidates      *prometheus.GaugeVec
	inProgress      *prometheus.GaugeVec
	freezeUntil     *prometheus.GaugeVec
	ageThreshold    *prometheus.GaugeVec
	rotationChances *prometheus.GaugeVec
	windowPeriod    *prometheus.GaugeVec
	shortLead       *prometheus.GaugeVec
	drainStuck      *prometheus.GaugeVec
	retryCount      *prometheus.GaugeVec
	windowActive    prometheus.Gauge
}

var _ controller.Recorder = (*Recorder)(nil)

// New builds the recorder and registers every collector on reg. In production reg
// is the controller-runtime metrics.Registry (served on /metrics); tests pass a
// private registry. MustRegister panics on a duplicate, which surfaces a
// double-registration bug loudly at startup.
func New(reg prometheus.Registerer) *Recorder {
	poolLabel := []string{"nodepool"}
	r := &Recorder{
		completed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "noderotation_completed_total",
			Help: "Cumulative rotation completions by outcome (success, failure, expired).",
		}, []string{"nodepool", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "noderotation_duration_seconds",
			Help:    "Per-phase rotation duration in seconds (phase: surge_wait, drain).",
			Buckets: prometheus.ExponentialBuckets(30, 2, 10), // 30s .. ~4h
		}, []string{"nodepool", "phase"}),
		candidates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_candidates",
			Help: "Eligible NodeClaim count.",
		}, poolLabel),
		inProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_in_progress",
			Help: "Active rotation count.",
		}, poolLabel),
		freezeUntil: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_freeze_until_timestamp",
			Help: "Unix timestamp of the active freeze (0 = no freeze).",
		}, poolLabel),
		ageThreshold: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_age_threshold_seconds",
			Help: "Derived ageThreshold in seconds.",
		}, poolLabel),
		rotationChances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_rotation_chances",
			Help: "Guaranteed rotation chances G for the derived threshold.",
		}, poolLabel),
		windowPeriod: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_window_period_seconds",
			Help: "Worst-case window period P of the schedule union, in seconds.",
		}, poolLabel),
		shortLead: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_short_lead_nodes",
			Help: "NodeClaims whose own expireAfter can no longer guarantee K chances.",
		}, poolLabel),
		drainStuck: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_drain_stuck",
			Help: "1 when the in-flight drain has exceeded tGP + buffer, else 0.",
		}, poolLabel),
		retryCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "noderotation_retry_count",
			Help: "Highest retry-count across the NodePool's NodeClaims (0 when none).",
		}, poolLabel),
		windowActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "noderotation_window_active",
			Help: "0/1 indicator of maintenance-window membership (cluster-wide).",
		}),
	}
	reg.MustRegister(
		r.completed, r.duration, r.candidates, r.inProgress, r.freezeUntil,
		r.ageThreshold, r.rotationChances, r.windowPeriod, r.shortLead,
		r.drainStuck, r.retryCount, r.windowActive,
	)
	return r
}

func (r *Recorder) Success(nodePool string) { r.completed.WithLabelValues(nodePool, "success").Inc() }
func (r *Recorder) Expired(nodePool, _ string) {
	r.completed.WithLabelValues(nodePool, "expired").Inc()
}
func (r *Recorder) Failure(nodePool, _ string) {
	r.completed.WithLabelValues(nodePool, "failure").Inc()
}

func (r *Recorder) ObserveWindow(active bool) { r.windowActive.Set(b2f(active)) }

func (r *Recorder) ObserveDuration(nodePool, phase string, d time.Duration) {
	r.duration.WithLabelValues(nodePool, phase).Observe(d.Seconds())
}

func (r *Recorder) ObservePool(nodePool string, o controller.PoolObservation) {
	r.candidates.WithLabelValues(nodePool).Set(float64(o.Candidates))
	r.inProgress.WithLabelValues(nodePool).Set(float64(o.InProgress))
	r.shortLead.WithLabelValues(nodePool).Set(float64(o.ShortLeadNodes))
	r.retryCount.WithLabelValues(nodePool).Set(float64(o.RetryCount))
	r.drainStuck.WithLabelValues(nodePool).Set(b2f(o.DrainStuck))
	r.ageThreshold.WithLabelValues(nodePool).Set(o.AgeThreshold.Seconds())
	r.rotationChances.WithLabelValues(nodePool).Set(float64(o.RotationChances))
	r.windowPeriod.WithLabelValues(nodePool).Set(o.WindowPeriod.Seconds())

	freeze := 0.0
	if !o.FreezeUntil.IsZero() {
		freeze = float64(o.FreezeUntil.Unix())
	}
	r.freezeUntil.WithLabelValues(nodePool).Set(freeze)
}

// ForgetPool deletes every per-NodePool series for nodePool, called when the
// NodePool is deleted (§4.2). The gauges are recomputed each reconcile, so once a
// pool's reconciles stop they would otherwise latch their last value forever — a
// since-deleted drain_stuck=1 would alert indefinitely. The cluster-wide
// window_active gauge has no nodepool label and is untouched.
func (r *Recorder) ForgetPool(nodePool string) {
	for _, g := range []*prometheus.GaugeVec{
		r.candidates, r.inProgress, r.freezeUntil, r.ageThreshold, r.rotationChances,
		r.windowPeriod, r.shortLead, r.drainStuck, r.retryCount,
	} {
		g.DeleteLabelValues(nodePool)
	}
	// completed_total{nodepool,outcome} and duration_seconds{nodepool,phase} carry
	// an extra label, so drop every series sharing this nodepool.
	r.completed.DeletePartialMatch(prometheus.Labels{"nodepool": nodePool})
	r.duration.DeletePartialMatch(prometheus.Labels{"nodepool": nodePool})
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
