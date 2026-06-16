package controller

import (
	"context"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// testNow is a Wednesday 03:00 UTC — inside the all-day test window.
var testNow = time.Date(2026, 6, 17, 3, 0, 0, 0, time.UTC)

// testNowOut is 23:59 UTC, outside the [00:00, 23:59) window.
var testNowOut = time.Date(2026, 6, 17, 23, 59, 0, 0, time.UTC)

const (
	testPoolName = "api"
	testNS       = "node-rotation-system"
	candNode     = "node-old"
	surgeNode    = "node-new"
)

func testPolicy() *policy.Policy {
	p := &policy.Policy{
		NodePoolSelectors: []policy.Selector{{MatchLabels: map[string]string{"workload": "api"}}},
		MaintenanceWindows: []policy.MaintenanceWindow{{
			Timezone: "UTC",
			Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
			Start:    "00:00",
			End:      "23:59",
		}},
	}
	p.ApplyDefaults()
	return p
}

type recDuration struct {
	pool, phase string
	d           time.Duration
}

type fakeRecorder struct {
	success, expired, failure int
	obs                       map[string]PoolObservation
	window                    []bool
	durations                 []recDuration
	forgotten                 []string
}

func (f *fakeRecorder) Success(string)         { f.success++ }
func (f *fakeRecorder) Expired(string, string) { f.expired++ }
func (f *fakeRecorder) Failure(string, string) { f.failure++ }
func (f *fakeRecorder) ObservePool(np string, o PoolObservation) {
	if f.obs == nil {
		f.obs = map[string]PoolObservation{}
	}
	f.obs[np] = o
}
func (f *fakeRecorder) ObserveWindow(active bool) { f.window = append(f.window, active) }
func (f *fakeRecorder) ObserveDuration(np, phase string, d time.Duration) {
	f.durations = append(f.durations, recDuration{np, phase, d})
}
func (f *fakeRecorder) ForgetPool(np string) { f.forgotten = append(f.forgotten, np) }

func newReconciler(t *testing.T, clock time.Time, rec *fakeRecorder, objs ...client.Object) *RotationReconciler {
	t.Helper()
	p := testPolicy()
	sched, err := window.New(p.MaintenanceWindows)
	if err != nil {
		t.Fatalf("build schedule: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(objs...).Build()
	if rec == nil {
		rec = &fakeRecorder{}
	}
	return &RotationReconciler{
		Client:            cl,
		Policy:            p,
		Schedule:          sched,
		Namespace:         testNS,
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
		Recorder:          rec,
		Clock:             func() time.Time { return clock },
	}
}

// --- object builders -------------------------------------------------------

func testNodePool(anns map[string]string) *karpv1.NodePool {
	return &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name:        testPoolName,
		Labels:      map[string]string{"workload": "api"},
		Annotations: anns,
	}}
}

// withTGP stamps a fixed terminationGracePeriod so the derived t_rot/drain bound
// are deterministic across the tests.
func withTGP(p *karpv1.NodePool) *karpv1.NodePool {
	p.Spec.Template.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 30 * time.Minute}
	return p
}

type ncOpt func(*karpv1.NodeClaim)

func ncReady() ncOpt {
	return func(c *karpv1.NodeClaim) {
		c.Status.Conditions = []status.Condition{{Type: status.ConditionReady, Status: metav1.ConditionTrue}}
	}
}
func ncNode(name string) ncOpt { return func(c *karpv1.NodeClaim) { c.Status.NodeName = name } }
func ncFinalizer() ncOpt {
	return func(c *karpv1.NodeClaim) { c.Finalizers = []string{"karpenter.sh/termination"} }
}
func ncCreated(at time.Time) ncOpt {
	return func(c *karpv1.NodeClaim) { c.CreationTimestamp = metav1.NewTime(at) }
}
func ncAnn(kv ...string) ncOpt {
	return func(c *karpv1.NodeClaim) {
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		for i := 0; i+1 < len(kv); i += 2 {
			c.Annotations[kv[i]] = kv[i+1]
		}
	}
}

func testClaim(name string, age time.Duration, opts ...ncOpt) *karpv1.NodeClaim {
	d := 14 * 24 * time.Hour
	c := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Labels:            map[string]string{karpv1.NodePoolLabelKey: testPoolName},
			CreationTimestamp: metav1.NewTime(testNow.Add(-age)),
		},
	}
	c.Spec.ExpireAfter = karpv1.NillableDuration{Duration: &d}
	ncReady()(c)
	for _, o := range opts {
		o(c)
	}
	return c
}

func testK8sNode(name string, ready bool, anns map[string]string, unschedulable bool) *corev1.Node {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				karpv1.NodePoolLabelKey:      testPoolName,
				corev1.LabelHostname:         name,
				corev1.LabelTopologyZone:     "us-east-1a",
				corev1.LabelArchStable:       "arm64",
				"karpenter.sh/capacity-type": "spot",
			},
			Annotations: anns,
		},
		Spec:   corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: cond}}},
	}
}

// placeholderPod builds the surge placeholder for the candidate "nc-old" — the
// single rotation under test throughout this file.
func placeholderPod(nodeName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      surge.PlaceholderName("nc-old"),
			Namespace: testNS,
			Labels:    map[string]string{annotations.SurgeFor: "nc-old"},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func rfc(at time.Time) string { return at.Format(time.RFC3339) }

// --- accessors -------------------------------------------------------------

func getPool(t *testing.T, r *RotationReconciler) *karpv1.NodePool {
	t.Helper()
	var p karpv1.NodePool
	if err := r.Get(context.Background(), types.NamespacedName{Name: testPoolName}, &p); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	return &p
}

func getClaimOrNil(t *testing.T, r *RotationReconciler, name string) *karpv1.NodeClaim {
	t.Helper()
	var c karpv1.NodeClaim
	err := r.Get(context.Background(), types.NamespacedName{Name: name}, &c)
	if err != nil {
		return nil
	}
	return &c
}

func getNodeObj(t *testing.T, r *RotationReconciler, name string) *corev1.Node {
	t.Helper()
	var n corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatalf("get node %s: %v", name, err)
	}
	return &n
}

func placeholderExists(t *testing.T, r *RotationReconciler) bool {
	t.Helper()
	var p corev1.Pod
	err := r.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: surge.PlaceholderName("nc-old")}, &p)
	return err == nil
}

func step(t *testing.T, r *RotationReconciler, pool *karpv1.NodePool) {
	t.Helper()
	if _, err := r.reconcileNodePool(context.Background(), pool); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
}

// --- start path ------------------------------------------------------------

func TestStartRotationAnchorsAndCreatesPlaceholder(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	node := testK8sNode(candNode, true, nil, false)
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, nil, pool, cand, node)

	step(t, r, pool)

	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Fatalf("anchor: got %q, want nc-old", got)
	}
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Fatalf("claim state: got %+v", c)
	}
	if c.Annotations[annotations.StartedAt] == "" {
		t.Error("started-at must be stamped")
	}
	n := getNodeObj(t, r, candNode)
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" || n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Errorf("candidate node not frozen: %+v", n.Annotations)
	}
	if !n.Spec.Unschedulable || n.Annotations[annotations.Cordoned] != "true" {
		t.Errorf("candidate node not cordoned: %+v", n)
	}
	if !placeholderExists(t, r) {
		t.Error("placeholder must be created")
	}
}

func TestNoStartOutOfWindow(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNowOut, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Error("no rotation may start outside the maintenance window")
	}
	if placeholderExists(t, r) {
		t.Error("no placeholder outside the window")
	}
}

func TestNoStartWhenFrozen(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(map[string]string{annotations.Freeze: rfc(testNow.Add(time.Hour))}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Error("a frozen NodePool must not start a rotation")
	}
}

func TestNoStartWithinCooldown(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	// cooldownAfter defaults to 10m; last rotation 5m ago → still cooling down.
	pool := withTGP(testNodePool(map[string]string{annotations.LastRotationAt: rfc(testNow.Add(-5 * time.Minute))}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Error("must not start within cooldownAfter of the last rotation")
	}
}

func TestNoStartWhenHeadroomInsufficient(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(nil))
	pool.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse("1")}
	bigPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: candNode,
			Containers: []corev1.Container{{
				Name:      "c",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
			}},
		},
	}
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false), bigPod)

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Error("must not start when the surge would exceed NodePool limits")
	}
}

// --- pending → draining ----------------------------------------------------

func TestSurgeReadyTransitionsToDraining(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-2*time.Minute))),
		ncFinalizer())
	surgeClaim := testClaim("nc-new", time.Hour, ncNode(surgeNode))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old"}, true)
	newNode := testK8sNode(surgeNode, true, nil, false)
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	r := newReconciler(t, testNow, nil, pool, cand, surgeClaim, oldNode, newNode, ph)

	step(t, r, pool)

	if got := getPool(t, r).Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Errorf("active-rotation-state: got %q, want draining", got)
	}
	if getPool(t, r).Annotations[annotations.DrainingAt] == "" {
		t.Error("draining-at drain-start anchor must be stamped at the pending → draining transition")
	}
	if n := getNodeObj(t, r, surgeNode); n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Errorf("surge target not frozen: %+v", n.Annotations)
	}
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.DeletionTimestamp == nil {
		t.Error("old NodeClaim must be deleted (delete issued)")
	}
}

func TestSurgeNotReadyHostStaysPending(t *testing.T) {
	// Placeholder is Running but its host is NotReady: surge_ready must not hold,
	// so the rotation stays in pending and the old NodeClaim is not deleted.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-2*time.Minute))))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old"}, true)
	notReadyHost := testK8sNode(surgeNode, false, nil, false)
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	r := newReconciler(t, testNow, nil, pool, cand, oldNode, notReadyHost, ph)

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotationState] == annotations.StateDraining {
		t.Error("a NotReady surge host must not trigger the draining transition")
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.DeletionTimestamp != nil {
		t.Error("the old NodeClaim must not be deleted while the surge host is NotReady")
	}
}

// --- pending timeout → failed ----------------------------------------------

func TestReadyTimeoutFailsAndReaps(t *testing.T) {
	rec := &fakeRecorder{}
	// readyTimeout defaults to 15m; started 20m ago → timed out.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-20*time.Minute)),
		annotations.SurgeClaim, "nc-new",
	))
	// surge claim created after started-at, never registered a node → reapable.
	surgeClaim := testClaim("nc-new", 0, ncCreated(testNow.Add(-10*time.Minute)))
	surgeClaim.Status.NodeName = ""
	// draining-at is not normally present on a pending timeout; seed it to lock the
	// invariant that failPending clears it alongside the anchor on every end path.
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation: "nc-old",
		annotations.DrainingAt:     rfc(testNow.Add(-5 * time.Minute)),
	}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
	ph := placeholderPod("", corev1.PodPending)
	r := newReconciler(t, testNow, rec, pool, cand, surgeClaim, oldNode, ph)

	step(t, r, pool)

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("claim must be failed: %+v", c)
	}
	if c.Annotations[annotations.RetryCount] != "1" {
		t.Errorf("retry-count: got %q, want 1", c.Annotations[annotations.RetryCount])
	}
	if c.Annotations[annotations.StartedAt] != "" || c.Annotations[annotations.SurgeClaim] != "" {
		t.Errorf("started-at and surge-claim must be cleared: %+v", c.Annotations)
	}
	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" {
		t.Error("anchor must be released on failure")
	}
	if p.Annotations[annotations.DrainingAt] != "" {
		t.Error("draining-at must be cleared on the failPending path")
	}
	if p.Annotations[annotations.LastFailureAt] == "" {
		t.Error("last-failure-at must be stamped")
	}
	if getClaimOrNil(t, r, "nc-new") != nil {
		t.Error("the induced surge claim must be reaped")
	}
	if placeholderExists(t, r) {
		t.Error("placeholder must be deleted on failure")
	}
	if n := getNodeObj(t, r, candNode); n.Spec.Unschedulable || n.Annotations[annotations.SurgeFor] != "" {
		t.Error("candidate node must be unfrozen and uncordoned on failure")
	}
	if rec.failure != 1 {
		t.Errorf("failure metric: got %d", rec.failure)
	}
}

// --- pending force-expiry → expired ----------------------------------------

func TestPendingForceExpiryMarksExpired(t *testing.T) {
	rec := &fakeRecorder{}
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-2*time.Minute)),
	))
	cand.DeletionTimestamp = &dt
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old"}, false)
	ph := placeholderPod("", corev1.PodPending)
	r := newReconciler(t, testNow, rec, pool, cand, oldNode, ph)

	step(t, r, pool)

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateExpired {
		t.Fatalf("claim must be marked expired: %+v", c)
	}
	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Error("anchor must be released")
	}
	if getPool(t, r).Annotations[annotations.LastRotationAt] != "" {
		t.Error("a force-expiry rotates nothing — no cooldown anchor")
	}
	if placeholderExists(t, r) {
		t.Error("placeholder must be deleted")
	}
	if rec.expired != 1 || rec.success != 0 {
		t.Errorf("must emit expired, not success: %+v", rec)
	}
}

// --- draining recovery -----------------------------------------------------

func TestDrainingReissuesDelete(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StateDraining))
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
	}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, true))

	step(t, r, pool)

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.DeletionTimestamp == nil {
		t.Error("draining recovery must re-issue the delete")
	}
	// A rotation that reached draining before the anchor existed must NOT be
	// back-anchored: an uncounted drain beats a mis-anchored one (spec §4.2).
	if getPool(t, r).Annotations[annotations.DrainingAt] != "" {
		t.Error("draining recovery must not backfill a missing draining-at anchor")
	}
}

// --- completion ------------------------------------------------------------

func TestCompletionSuccess(t *testing.T) {
	rec := &fakeRecorder{}
	// old NodeClaim already gone; mirror says draining → success. draining-at set
	// 30m ago anchors the §4.2 drain-phase duration.
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-30 * time.Minute)),
	}))
	surgeFrozen := testK8sNode(surgeNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old"}, false)
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	r := newReconciler(t, testNow, rec, pool, surgeFrozen, ph)

	step(t, r, pool)

	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" || p.Annotations[annotations.ActiveRotationState] != "" {
		t.Error("anchor and mirror must be cleared on completion")
	}
	if p.Annotations[annotations.DrainingAt] != "" {
		t.Error("draining-at must be cleared on completion")
	}
	if p.Annotations[annotations.LastRotationAt] == "" {
		t.Error("last-rotation-at must be stamped on success")
	}
	if n := getNodeObj(t, r, surgeNode); n.Annotations[annotations.SurgeFor] != "" {
		t.Error("surge target must be unfrozen")
	}
	if placeholderExists(t, r) {
		t.Error("placeholder must be deleted")
	}
	if rec.success != 1 {
		t.Errorf("success metric: got %d", rec.success)
	}

	var drain time.Duration
	found := false
	for _, d := range rec.durations {
		if d.phase == PhaseDrain {
			drain, found = d.d, true
		}
	}
	if !found {
		t.Fatalf("drain duration not observed; durations=%+v", rec.durations)
	}
	if drain != 30*time.Minute {
		t.Errorf("drain duration: got %v, want 30m", drain)
	}
}

// TestCompletionWithoutDrainAnchorSkipsDuration: a rotation that reached draining
// before the draining-at anchor existed (or had it cleared) still completes as
// success, but emits no drain duration rather than a mis-anchored one.
func TestCompletionWithoutDrainAnchorSkipsDuration(t *testing.T) {
	rec := &fakeRecorder{}
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
	}))
	surgeFrozen := testK8sNode(surgeNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old"}, false)
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	r := newReconciler(t, testNow, rec, pool, surgeFrozen, ph)

	step(t, r, pool)

	if rec.success != 1 {
		t.Errorf("success metric: got %d, want 1", rec.success)
	}
	for _, d := range rec.durations {
		if d.phase == PhaseDrain {
			t.Errorf("no drain duration must be observed without a draining-at anchor; got %v", d.d)
		}
	}
}

func TestVanishedPendingAborts(t *testing.T) {
	rec := &fakeRecorder{}
	// old NodeClaim gone with NO draining mirror → expired abort, no cooldown.
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	ph := placeholderPod("", corev1.PodPending)
	r := newReconciler(t, testNow, rec, pool, ph)

	step(t, r, pool)

	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" {
		t.Error("anchor must be released")
	}
	if p.Annotations[annotations.LastRotationAt] != "" {
		t.Error("an aborted pending rotates nothing — no cooldown")
	}
	if rec.expired != 1 || rec.success != 0 {
		t.Errorf("must emit expired, not success: %+v", rec)
	}
}

// --- failed re-entry -------------------------------------------------------

func TestFailedReentryReStampsAndRecreates(t *testing.T) {
	// failed past its backoff, all gates pass → re-enter pending with a fresh
	// started-at and a new placeholder.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, rfc(testNow.Add(-40*time.Minute)), // backoff 30m → elapsed
	))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Fatalf("failed claim must re-enter pending: %+v", c)
	}
	if c.Annotations[annotations.StartedAt] == "" {
		t.Error("started-at must be re-stamped on re-entry")
	}
	if !placeholderExists(t, r) {
		t.Error("placeholder must be (re)created")
	}
}

func TestFailedBackstopReachesDeletedClaim(t *testing.T) {
	rec := &fakeRecorder{}
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	cand := testClaim("nc-old", 20*24*time.Hour, ncFinalizer(), ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, rfc(testNow.Add(-time.Minute)),
	))
	cand.DeletionTimestamp = &dt
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	r := newReconciler(t, testNow, rec, pool, cand)

	step(t, r, pool)

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateExpired {
		t.Fatalf("a deleting failed claim must become expired: %+v", c)
	}
	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Error("anchor must be released")
	}
	if rec.expired != 1 {
		t.Errorf("expired metric: got %d", rec.expired)
	}
}

// --- expired terminal cleanup ----------------------------------------------

func TestExpiredTerminalCleanup(t *testing.T) {
	rec := &fakeRecorder{}
	cand := testClaim("nc-old", 20*24*time.Hour, ncFinalizer(), ncAnn(annotations.State, annotations.StateExpired))
	dt := metav1.NewTime(testNow.Add(-time.Minute))
	cand.DeletionTimestamp = &dt
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-30 * time.Minute)), // reached draining, then force-expired
	}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
	ph := placeholderPod("", corev1.PodPending)
	r := newReconciler(t, testNow, rec, pool, cand, oldNode, ph)

	step(t, r, pool)

	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" || p.Annotations[annotations.ActiveRotationState] != "" {
		t.Error("anchor and mirror must be cleared")
	}
	if p.Annotations[annotations.DrainingAt] != "" {
		t.Error("draining-at must be cleared on the expired-terminal clearAnchor path")
	}
	if placeholderExists(t, r) {
		t.Error("placeholder must be deleted")
	}
	if n := getNodeObj(t, r, candNode); n.Spec.Unschedulable || n.Annotations[annotations.SurgeFor] != "" {
		t.Error("node must be unfrozen and uncordoned")
	}
	if rec.expired != 0 || rec.success != 0 || rec.failure != 0 {
		t.Errorf("expired-terminal cleanup must not re-emit metrics: %+v", rec)
	}
}

// --- step 1 precedence -----------------------------------------------------

func TestAnchorTakesPrecedenceOverNewSelection(t *testing.T) {
	// An anchored rotation must be driven (step 1) before any new candidate is
	// considered — even a fresher, older candidate is ignored while anchored.
	anchored := testClaim("nc-anchored", 10*24*time.Hour, ncFinalizer(),
		ncAnn(annotations.State, annotations.StateDraining))
	older := testClaim("nc-older", 30*24*time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-anchored",
		annotations.ActiveRotationState: annotations.StateDraining,
	}))
	r := newReconciler(t, testNow, nil, pool, anchored, older, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	// The anchor is unchanged and the older claim was never selected/anchored.
	if getPool(t, r).Annotations[annotations.ActiveRotation] != "nc-anchored" {
		t.Error("the in-flight anchor must be preserved over a new selection")
	}
	if c := getClaimOrNil(t, r, "nc-older"); c.Annotations[annotations.State] != "" {
		t.Error("a fresh candidate must not be touched while a rotation is in flight")
	}
}

func TestAnchorWriteIsOptimisticallyLocked(t *testing.T) {
	// Two reconciles can race on one NodePool under informer-cache skew. The
	// anchor write carries the reader's resourceVersion, so the loser's write
	// fails with Conflict and it does nothing but requeue (spec §5.2).
	pool := testNodePool(nil)
	r := newReconciler(t, testNow, nil, pool)
	ctx := context.Background()

	stale := getPool(t, r) // reader A's snapshot

	winner := getPool(t, r) // reader B writes first, advancing the resourceVersion
	if err := r.anchorRotation(ctx, winner, "nc-winner"); err != nil {
		t.Fatalf("first anchor write should succeed: %v", err)
	}

	err := r.anchorRotation(ctx, stale, "nc-loser")
	if !apierrors.IsConflict(err) {
		t.Fatalf("the racing write must fail with Conflict, got %v", err)
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-winner" {
		t.Errorf("the winner's anchor must stand: got %q", got)
	}
}

// --- rollback: absorb-host guard -------------------------------------------

func TestReadyTimeoutDoesNotReapAbsorbHost(t *testing.T) {
	// readyTimeout (15m) elapsed, so the attempt fails — but the induced surge
	// claim has registered a node onto which an unrelated Pod has since landed: an
	// absorb host. The rollback must NOT reap it; the surge node is repurposed as
	// normal capacity, not surge debris (spec §3.3 Rollback, second guard).
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-20*time.Minute)),
		annotations.SurgeClaim, "nc-new",
	))
	// surge claim created after started-at AND registered a node → guarded by occupancy.
	surgeClaim := testClaim("nc-new", 0, ncCreated(testNow.Add(-10*time.Minute)), ncNode(surgeNode))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
	surgeHost := testK8sNode(surgeNode, true, nil, false)
	// an unrelated, reschedulable Pod bin-packed onto the surge node.
	realPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-app", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: surgeNode},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	r := newReconciler(t, testNow, nil, pool, cand, surgeClaim, oldNode, surgeHost, realPod, ph)

	step(t, r, pool)

	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("the attempt must still fail: %+v", c)
	}
	if getClaimOrNil(t, r, "nc-new") == nil {
		t.Error("an absorb host's surge claim must NOT be reaped (spec §3.3)")
	}
}

// --- failed: torn-write crash recovery -------------------------------------

func TestFailedTornWriteRepairReleasesAnchorAndPreservesPause(t *testing.T) {
	// A failed claim is still anchored but its escalated backoff has not elapsed,
	// so it cannot re-enter pending. This is the torn-write crash-recovery branch
	// (a crash between the failed write and the pool update): it must release the
	// anchor and re-stamp last-failure-at = max(existing, failed-at) so the §4.4
	// inter-attempt pause is never voided (spec §5.2 case failed).
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.RetryCount, "1",
		annotations.FailedAt, rfc(testNow.Add(-5*time.Minute)), // backoff 30m → NOT elapsed
	))
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation: "nc-old",
		annotations.LastFailureAt:  rfc(testNow.Add(-1 * time.Hour)),   // stale, older than failed-at
		annotations.DrainingAt:     rfc(testNow.Add(-5 * time.Minute)), // defensive-invariant seed
	}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("the claim must stay failed before its backoff elapses: %+v", c)
	}
	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" {
		t.Error("the stale anchor must be released")
	}
	if p.Annotations[annotations.DrainingAt] != "" {
		t.Error("draining-at must be cleared on the advanceFailed torn-repair path")
	}
	if got, want := p.Annotations[annotations.LastFailureAt], rfc(testNow.Add(-5*time.Minute)); got != want {
		t.Errorf("last-failure-at must advance to max(existing, failed-at): got %q, want %q", got, want)
	}
}

// --- freeze hold on an in-flight pending rotation --------------------------

func TestFrozenHoldsInFlightPending(t *testing.T) {
	// A NodePool frozen mid-rotation HOLDS an in-flight pending rotation: the
	// protective markers are still (re)asserted, but no placeholder is created and
	// the rotation does not advance to draining (spec §3.1 freeze hold).
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-2*time.Minute)),
	))
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation: "nc-old",
		annotations.Freeze:         rfc(testNow.Add(time.Hour)),
	}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	n := getNodeObj(t, r, candNode)
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" || n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Errorf("freeze markers must still be asserted while frozen: %+v", n.Annotations)
	}
	if placeholderExists(t, r) {
		t.Error("a frozen pending rotation must not create the placeholder")
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Errorf("a frozen rotation must stay pending: %+v", c)
	}
	if getPool(t, r).Annotations[annotations.ActiveRotationState] == annotations.StateDraining {
		t.Error("a frozen rotation must not advance to draining")
	}
}
