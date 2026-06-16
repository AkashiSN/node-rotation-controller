package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// templateE is the representative NodePool template expireAfter used across the
// observe tests; with leadTime 49h it yields A = 336h − 49h = 287h, G = 2.
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
// buffer 15m = 1h; the all-week window gives P = 24h; K = 2 ⇒ leadTime = 49h.
// Template E = 14d = 336h ⇒ A = 287h, G = 2.
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
	if o.AgeThreshold != 287*time.Hour {
		t.Errorf("age-threshold: got %v, want 287h", o.AgeThreshold)
	}
	if o.RotationChances != 2 {
		t.Errorf("rotation-chances: got %d, want 2", o.RotationChances)
	}
	if o.ShortLeadNodes != 0 {
		t.Errorf("short-lead: got %d, want 0", o.ShortLeadNodes)
	}
	if len(rec.window) == 0 || !rec.window[len(rec.window)-1] {
		t.Errorf("window-active: got %v, want last true", rec.window)
	}
}

// TestObserveShortLeadNodes counts claims whose own expireAfter can no longer
// guarantee K chances (per-node A ≤ 0; §3.2 layer 3). leadTime = 49h here.
func TestObserveShortLeadNodes(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(nil)))
	short := testClaim("nc-short", 1*24*time.Hour) // expireAfter set below
	short.Spec.ExpireAfter = karpv1.NillableDuration{Duration: durPtr(40 * time.Hour)}
	ample := testClaim("nc-ample", 1*24*time.Hour)

	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, short, ample)
	step(t, r, pool)

	if o := rec.obs[testPoolName]; o.ShortLeadNodes != 1 {
		t.Errorf("short-lead: got %d, want 1", o.ShortLeadNodes)
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

// TestObserveDrainStuckAndInProgress: an anchored, draining rotation whose old
// NodeClaim has been deleting past the drain bound (tGP 30m + buffer 15m = 45m).
func TestObserveDrainStuckAndInProgress(t *testing.T) {
	pool := withExpireAfter(withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
	})))
	old := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StateDraining))
	dt := metav1.NewTime(testNow.Add(-2 * time.Hour)) // > 45m bound
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
