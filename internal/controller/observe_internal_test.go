package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
)

// hasFinding reports whether findings carry one with the given stable Code.
func hasFinding(findings []schedule.Finding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// TestDerivedThresholdsPopulatesThroughputInputs covers issue #36: derivedThresholds
// must feed the layer-2 throughput inputs (WindowLen D, NodeCount N) into
// schedule.Derive, not leave them at zero. The real 23h59m window yields
// C = ceil(23h59m / (t_rot_est 15m + cooldown 10m)) = 58; a zero WindowLen (the
// pre-fix state) collapses C and silences every layer-2 check, proving the input
// is load-bearing. A/G are unaffected by these layer-2 inputs.
func TestDerivedThresholdsPopulatesThroughputInputs(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	r := newReconciler(t, testNow, nil, pool)
	sched := mustSchedule(t)
	res := r.resolve(pool, testPolicy(), sched)
	p, _ := sched.WorstCasePeriod()
	d, _ := sched.ShortestWindow()
	gap, ok := sched.ShortestIdleGap()
	if !ok {
		t.Fatal("the 23h59m daily window closes for a minute each midnight; want a defined idle gap")
	}

	got := r.derivedThresholds(pool, res, p, d, &gap, 3)
	if want := 287*time.Hour + 13*time.Minute; got.A != want {
		t.Errorf("A: got %v, want %v", got.A, want)
	}
	if got.G != 2 {
		t.Errorf("G: got %d, want 2", got.G)
	}
	if got.C != 58 {
		t.Errorf("C: got %d, want 58 (ceil(23h59m / 25m))", got.C)
	}

	// Regression guard: the pre-fix WindowLen=0 drives C to zero, which layer 2
	// treats as degenerate and skips wholesale — so the input genuinely drives the
	// throughput findings (issue #211 replaced the old ThroughputZero warning).
	zero := r.derivedThresholds(pool, res, p, 0, &gap, 3)
	if zero.C != 0 {
		t.Errorf("C with WindowLen=0: got %d, want 0", zero.C)
	}
	if hasFinding(zero.Findings, "RotationSpansNextWindow") {
		t.Errorf("layer 2 must be skipped entirely at a degenerate D; findings=%+v", zero.Findings)
	}
}

// TestDerivedThresholdsNoWindowsPopulatesForecast is the controller-level
// counterpart to schedule.TestDeriveNoWindowsStillPopulatesForecast (issue #218,
// Codex review): with an expiring template but no window occurrence (P <= 0),
// derivedThresholds still returns a non-zero deadline bound and service-time
// forecast while throughput C is zero — the exact PoolObservation shape observe()
// then copies verbatim (the copy itself is covered by TestObserveIdlePoolGauges).
// This pins the real pool's numbers through the controller path: t_rot = 15m + tGP
// 30m + buffer 2m = 47m; t_rot_est = min(15m,5m) + min(30m,10m) = 15m.
func TestDerivedThresholdsNoWindowsPopulatesForecast(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	r := newReconciler(t, testNow, nil, pool)
	sched := mustSchedule(t)
	res := r.resolve(pool, testPolicy(), sched)
	d, _ := sched.ShortestWindow()
	gap, _ := sched.ShortestIdleGap()

	got := r.derivedThresholds(pool, res, 0, d, &gap, 3) // P = 0: no window occurrence
	if !hasFinding(got.Findings, "NoWindows") {
		t.Errorf("want a NoWindows finding for P=0; findings=%+v", got.Findings)
	}
	if want := 47 * time.Minute; got.TRot != want {
		t.Errorf("TRot: got %v, want %v (populated before the P<=0 guard)", got.TRot, want)
	}
	if want := 15 * time.Minute; got.TRotEst != want {
		t.Errorf("TRotEst: got %v, want %v (populated before the P<=0 guard)", got.TRotEst, want)
	}
	if got.C != 0 {
		t.Errorf("C: got %d, want 0 (throughput undefined without a window)", got.C)
	}
}

// TestDerivedThresholdsPassesIdleGap covers the issue #211 wiring: the carry-over
// check fires only when the reconciler supplies the schedule's shortest idle gap.
// The 23h59m daily window closes for one minute, which the 25m forecast denominator
// (t_rot_est 15m + cooldown 10m) plainly spans; a nil gap (a continuously-open
// window, where nothing can carry into a "next" occurrence) must silence the check
// instead.
func TestDerivedThresholdsPassesIdleGap(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	r := newReconciler(t, testNow, nil, pool)
	sched := mustSchedule(t)
	res := r.resolve(pool, testPolicy(), sched)
	p, _ := sched.WorstCasePeriod()
	d, _ := sched.ShortestWindow()
	gap, _ := sched.ShortestIdleGap()

	got := r.derivedThresholds(pool, res, p, d, &gap, 3)
	if !hasFinding(got.Findings, "RotationSpansNextWindow") {
		t.Errorf("RotationSpansNextWindow expected: t_rot_est 15m + cooldown 10m > idle gap %v; findings=%+v", gap, got.Findings)
	}

	none := r.derivedThresholds(pool, res, p, d, nil, 3)
	if hasFinding(none.Findings, "RotationSpansNextWindow") {
		t.Errorf("a nil idle gap must skip the carry-over check; findings=%+v", none.Findings)
	}
}

// TestDerivedThresholdsPassesDrainEstimate: the policy field reaches schedule.Derive
// and moves ONLY the forecast side. An unset field must arrive as nil so Derive
// applies min(tGP, 10m) — here tGP = 30m, so the default resolves to 10m (issue #212).
func TestDerivedThresholdsPassesDrainEstimate(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	r := newReconciler(t, testNow, nil, pool)
	sched := mustSchedule(t)
	p, _ := sched.WorstCasePeriod()
	d, _ := sched.ShortestWindow()

	pol := testPolicy()
	res := r.resolve(pool, pol, sched)
	if res.drainEstimate != nil {
		t.Errorf("resolve() drainEstimate = %v, want nil for an unset policy field", res.drainEstimate)
	}
	unset := r.derivedThresholds(pool, res, p, d, nil, 3)
	if want := 10 * time.Minute; unset.DrainEstimate != want {
		t.Errorf("unset: DrainEstimate = %v, want %v (min(tGP 30m, default 10m))", unset.DrainEstimate, want)
	}
	if want := 15 * time.Minute; unset.TRotEst != want { // provisioning 5m + drain 10m
		t.Errorf("unset: TRotEst = %v, want %v", unset.TRotEst, want)
	}

	pol = testPolicy()
	pol.Surge.DrainEstimate = &metav1.Duration{Duration: 20 * time.Minute}
	res = r.resolve(pool, pol, sched)
	if res.drainEstimate == nil || *res.drainEstimate != 20*time.Minute {
		t.Fatalf("resolve() drainEstimate = %v, want 20m", res.drainEstimate)
	}
	explicit := r.derivedThresholds(pool, res, p, d, nil, 3)
	if want := 20 * time.Minute; explicit.DrainEstimate != want {
		t.Errorf("explicit: DrainEstimate = %v, want %v", explicit.DrainEstimate, want)
	}

	// The deadline side is untouched by either, and only C moves.
	if unset.TRot != explicit.TRot || unset.A != explicit.A || unset.G != explicit.G {
		t.Errorf("deadline side moved with drainEstimate: TRot %v/%v A %v/%v G %d/%d",
			unset.TRot, explicit.TRot, unset.A, explicit.A, unset.G, explicit.G)
	}
	if unset.C == explicit.C {
		t.Errorf("C = %d for both estimates; the field is not load-bearing", unset.C)
	}
}

// TestDerivedThresholdsPassesProvisioningEstimate mirrors the drainEstimate wiring
// (ADR-0003, issue #220): the policy field reaches schedule.Derive and moves ONLY the
// forecast side. An unset field must arrive as nil so Derive applies
// min(readyTimeout, 5m) — here readyTimeout = 15m, so the default resolves to 5m.
func TestDerivedThresholdsPassesProvisioningEstimate(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	r := newReconciler(t, testNow, nil, pool)
	sched := mustSchedule(t)
	p, _ := sched.WorstCasePeriod()
	d, _ := sched.ShortestWindow()

	pol := testPolicy()
	res := r.resolve(pool, pol, sched)
	if res.provisioningEstimate != nil {
		t.Errorf("resolve() provisioningEstimate = %v, want nil for an unset policy field", res.provisioningEstimate)
	}
	unset := r.derivedThresholds(pool, res, p, d, nil, 3)
	if want := 5 * time.Minute; unset.ProvisioningEstimate != want {
		t.Errorf("unset: ProvisioningEstimate = %v, want %v (min(readyTimeout 15m, default 5m))", unset.ProvisioningEstimate, want)
	}
	if want := 15 * time.Minute; unset.TRotEst != want { // provisioning 5m + drain 10m
		t.Errorf("unset: TRotEst = %v, want %v", unset.TRotEst, want)
	}

	pol = testPolicy()
	pol.Surge.ProvisioningEstimate = &metav1.Duration{Duration: 2 * time.Minute}
	res = r.resolve(pool, pol, sched)
	if res.provisioningEstimate == nil || *res.provisioningEstimate != 2*time.Minute {
		t.Fatalf("resolve() provisioningEstimate = %v, want 2m", res.provisioningEstimate)
	}
	explicit := r.derivedThresholds(pool, res, p, d, nil, 3)
	if want := 2 * time.Minute; explicit.ProvisioningEstimate != want {
		t.Errorf("explicit: ProvisioningEstimate = %v, want %v", explicit.ProvisioningEstimate, want)
	}
	if want := 12 * time.Minute; explicit.TRotEst != want { // provisioning 2m + drain 10m
		t.Errorf("explicit: TRotEst = %v, want %v", explicit.TRotEst, want)
	}

	// The deadline side is untouched by either, and only C moves.
	if unset.TRot != explicit.TRot || unset.A != explicit.A || unset.G != explicit.G {
		t.Errorf("deadline side moved with provisioningEstimate: TRot %v/%v A %v/%v G %d/%d",
			unset.TRot, explicit.TRot, unset.A, explicit.A, unset.G, explicit.G)
	}
	if unset.C == explicit.C {
		t.Errorf("C = %d for both estimates; the field is not load-bearing", unset.C)
	}
}

// templateE is the representative NodePool template expireAfter used across the
// observe tests; with leadTime 48h47m it yields A = 336h − 48h47m = 287h13m, G = 2.
const templateE = 14 * 24 * time.Hour

// withExpireAfter stamps the NodePool template's representative expireAfter so the
// derived ageThreshold/rotationChances gauges (§4.2) are deterministic.
func withExpireAfter(p *karpv1.NodePool) *karpv1.NodePool {
	d := templateE
	p.Spec.Template.Spec.ExpireAfter = karpv1.NillableDuration{Duration: &d}
	return p
}

// TestObserveIdlePoolGauges asserts the §4.2 reconcile-time gauges for a pool
// with no in-flight rotation. With withTGP, t_rot = readyTimeout 15m + tGP 30m +
// buffer 2m = 47m; the all-week window gives P = 24h; K = 2 ⇒ leadTime = 48h47m.
// Template E = 14d = 336h ⇒ A = 287h13m, G = 2.
// provisioningEstimate = min(readyTimeout 15m, 5m) = 5m; drainEstimate =
// min(tGP 30m, 10m) = 10m ⇒ t_rot_est = 15m. The 23h59m daily window and
// cooldown 10m give C = ceil(23h59m / 25m) = 58 (issue #218).
func TestObserveIdlePoolGauges(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	cand := testClaim("nc-cand", 20*24*time.Hour, ncNode(candNode)) // eligible
	young := testClaim("nc-young", 1*24*time.Hour)                  // not triggered
	failed := testClaim("nc-failed", 20*24*time.Hour, ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(testNow.Add(-1*time.Minute)), // within backoff → not eligible
		annotations.RetryCount, "3"))
	node := testK8sNode(candNode, true, nil, false)

	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand, young, failed, node)
	step(t, r, pool)

	o, ok := rec.obs[testPoolName]
	if !ok {
		t.Fatal("ObservePool not called for the pool")
	}
	if o.Candidates != 1 {
		t.Errorf("candidates: got %d, want 1", o.Candidates)
	}
	if o.InProgress != 0 {
		t.Errorf("in-progress: got %d, want 0", o.InProgress)
	}
	if o.DrainStuck {
		t.Error("drain-stuck: got true, want false")
	}
	if o.RetryCount != 3 {
		t.Errorf("retry-count: got %d, want 3", o.RetryCount)
	}
	if !o.FreezeUntil.IsZero() {
		t.Errorf("freeze-until: got %v, want zero", o.FreezeUntil)
	}
	if o.WindowPeriod != 24*time.Hour {
		t.Errorf("window-period: got %v, want 24h", o.WindowPeriod)
	}
	if want := 287*time.Hour + 13*time.Minute; o.AgeThreshold != want {
		t.Errorf("age-threshold: got %v, want %v", o.AgeThreshold, want)
	}
	if o.RotationChances != 2 {
		t.Errorf("rotation-chances: got %d, want 2", o.RotationChances)
	}
	if o.ShortLeadNodes != 0 {
		t.Errorf("short-lead: got %d, want 0", o.ShortLeadNodes)
	}
	if want := 47 * time.Minute; o.TRotBound != want {
		t.Errorf("t-rot-bound: got %v, want %v", o.TRotBound, want)
	}
	if want := 15 * time.Minute; o.TRotEstimate != want {
		t.Errorf("t-rot-estimate: got %v, want %v", o.TRotEstimate, want)
	}
	if o.ThroughputCapacity != 58 {
		t.Errorf("throughput-capacity: got %d, want 58 (ceil(23h59m / 25m))", o.ThroughputCapacity)
	}
	if len(rec.window) == 0 || !rec.window[len(rec.window)-1] {
		t.Errorf("window-active: got %v, want last true", rec.window)
	}
}

// TestReconcileForgetsDeletedPool: once the NodePool is gone, Reconcile drops its
// metric series (§4.2) so the recomputed gauges do not latch at their last value
// after its reconciles stop.
func TestReconcileForgetsDeletedPool(t *testing.T) {
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec) // no objects → Get returns NotFound

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: testPoolName},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(rec.forgotten) != 1 || rec.forgotten[0] != testPoolName {
		t.Errorf("ForgetPool: got %v, want [%s]", rec.forgotten, testPoolName)
	}
}

// TestObserveShortLeadNodes counts claims whose own expireAfter can no longer
// guarantee K chances (per-node A ≤ 0; §3.2 layer 3). leadTime = 48h47m here.
func TestObserveShortLeadNodes(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	short := testClaim("nc-short", 1*24*time.Hour) // expireAfter set below
	short.Spec.ExpireAfter = karpv1.NillableDuration{Duration: new(40 * time.Hour)}
	ample := testClaim("nc-ample", 1*24*time.Hour)

	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, short, ample)
	step(t, r, pool)

	if o := rec.obs[testPoolName]; o.ShortLeadNodes != 1 {
		t.Errorf("short-lead: got %d, want 1", o.ShortLeadNodes)
	}
}

// TestObserveDrainStuckAndInProgress: an anchored, draining rotation whose old
// NodeClaim has been deleting past the drain bound (tGP 30m + buffer 2m = 32m).
func TestObserveDrainStuckAndInProgress(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
	})))
	old := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StateDraining))
	dt := metav1.NewTime(testNow.Add(-2 * time.Hour)) // > 32m bound
	old.DeletionTimestamp = &dt
	node := testK8sNode(candNode, true, nil, false)

	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, old, node)
	step(t, r, pool)

	o := rec.obs[testPoolName]
	if o.InProgress != 1 {
		t.Errorf("in-progress: got %d, want 1", o.InProgress)
	}
	if !o.DrainStuck {
		t.Error("drain-stuck: got false, want true")
	}
}

func TestObserveWindowInactiveOutOfWindow(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNowOut, rec, pool)
	step(t, r, pool)

	if len(rec.window) == 0 || rec.window[len(rec.window)-1] {
		t.Errorf("window-active: got %v, want last false", rec.window)
	}
}

func TestObserveFreezeUntil(t *testing.T) {
	until := testNow.Add(2 * time.Hour)
	pool := withExpireAfter(withTGP(testNodePool(map[string]string{
		annotations.Freeze: rfc(until),
	})))
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool)
	step(t, r, pool)

	if o := rec.obs[testPoolName]; o.FreezeUntil.Unix() != until.Unix() {
		t.Errorf("freeze-until: got %v, want %v", o.FreezeUntil, until)
	}
}

// TestObserveSurgeWaitDuration: the surge_wait phase histogram is observed at the
// pending → draining transition as now − started-at (here, 2m).
func TestObserveSurgeWaitDuration(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-2*time.Minute))),
		ncFinalizer())
	surgeClaim := testClaim("nc-new", time.Hour, ncNode(surgeNode))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old"}, true)
	newNode := testK8sNode(surgeNode, true, nil, false)
	ph := placeholderPod(surgeNode, corev1.PodRunning)

	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand, surgeClaim, oldNode, newNode, ph)
	step(t, r, pool)

	var got time.Duration
	found := false
	for _, d := range rec.durations {
		if d.phase == PhaseSurgeWait {
			got, found = d.d, true
		}
	}
	if !found {
		t.Fatalf("surge_wait duration not observed; durations=%+v", rec.durations)
	}
	if got != 2*time.Minute {
		t.Errorf("surge_wait duration: got %v, want 2m", got)
	}
}
