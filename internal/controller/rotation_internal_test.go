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

// withTemplateE stamps the NodePool template's representative expireAfter so the
// §3.2 feasibility derivation is deterministic. A short E (≤ K·P + t_rot) drives a
// fatal A ≤ 0.
func withTemplateE(p *karpv1.NodePool, d time.Duration) *karpv1.NodePool {
	p.Spec.Template.Spec.ExpireAfter = karpv1.NillableDuration{Duration: &d}
	return p
}

// TestNoStartWhenFeasibilityFatal covers issue #27: a NodePool whose schedule
// derivation is fatal must not start a new rotation, even with an otherwise
// eligible candidate. Here leadTime = K·P + t_rot = 2·24h + 1h = 49h, so a
// template E of 40h gives A = 40h − 49h = −9h ≤ 0 (fatal ANonPositive, §3.2).
func TestNoStartWhenFeasibilityFatal(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode)) // eligible
	pool := withTemplateE(withTGP(testNodePool(nil)), 40*time.Hour)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("no rotation may start under a fatal schedule; anchor = %q", got)
	}
	if c := getClaimOrNil(t, r, "nc-old"); c != nil && c.Annotations[annotations.State] != "" {
		t.Errorf("candidate must not be anchored into pending; state = %q", c.Annotations[annotations.State])
	}
	if placeholderExists(t, r) {
		t.Error("no placeholder may be created under a fatal schedule")
	}
}

// TestStartWhenFeasibilityHealthy is the gate's negative control: a feasible
// template E (14d ⇒ A = 287h > 0) still starts normally, so the fatal gate does
// not over-block.
func TestStartWhenFeasibilityHealthy(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withTemplateE(withTGP(testNodePool(nil)), 14*24*time.Hour)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("a feasible NodePool must start; anchor = %q, want nc-old", got)
	}
	if !placeholderExists(t, r) {
		t.Error("placeholder must be created for a feasible NodePool")
	}
}

// TestFatalDoesNotBlockInFlightRotation: the fatal gate blocks only NEW starts.
// An already-anchored rotation must keep advancing even while the schedule is
// fatal — step 1 (drive in-flight) runs before the gate.
func TestFatalDoesNotBlockInFlightRotation(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-1*time.Minute))),
		ncFinalizer())
	pool := withTemplateE(withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"})), 40*time.Hour)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	step(t, r, pool)

	if !placeholderExists(t, r) {
		t.Error("an in-flight rotation must keep advancing despite a fatal schedule")
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("in-flight anchor must be preserved; got %q", got)
	}
}

// --- pending → draining ----------------------------------------------------

func TestSurgeReadyTransitionsToDraining(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-2*time.Minute))),
		ncFinalizer())
	surgeClaim := testClaim("nc-new", time.Hour, ncNode(surgeNode))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old"}, true)
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
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old"}, true)
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

func TestSurgeTerminatingPlaceholderStaysPending(t *testing.T) {
	// Placeholder is Running on a Ready host in the same NodePool, but it is already
	// terminating (deletionTimestamp set, e.g. preempted by a higher-priority Pod):
	// its reservation capacity is being removed, so surge_ready must not hold and the
	// old NodeClaim must not be deleted (issue #28).
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-2*time.Minute))))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old"}, true)
	readyHost := testK8sNode(surgeNode, true, nil, false)
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	dt := metav1.NewTime(testNow.Add(-time.Second))
	ph.DeletionTimestamp = &dt
	ph.Finalizers = []string{"noderotation.io/test"} // keep the terminating Pod in the fake store
	r := newReconciler(t, testNow, nil, pool, cand, oldNode, readyHost, ph)

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotationState] == annotations.StateDraining {
		t.Error("a terminating placeholder must not trigger the draining transition")
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.DeletionTimestamp != nil {
		t.Error("the old NodeClaim must not be deleted while the placeholder is terminating")
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
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
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
	if n := getNodeObj(t, r, candNode); n.Spec.Unschedulable || n.Annotations[annotations.SurgeFor] != "" ||
		n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "" {
		t.Error("candidate node must be unfrozen (surge-for + controller-owned do-not-disrupt removed) and uncordoned on failure")
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
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old"}, false)
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

// TestDrainingReassertsMirrorWhenAbsent covers the crash-recovery branch in
// advanceDraining where the candidate already reached state=draining but the
// pool-side active-rotation-state mirror is missing (a crash between the mirror
// write and the claim's state write, spec §5.2). The handler must re-assert the
// mirror so the serial gate and completion outcome are well-defined.
func TestDrainingReassertsMirrorWhenAbsent(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StateDraining))
	// Anchor present but the active-rotation-state mirror is ABSENT.
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, true))

	step(t, r, pool)

	if got := getPool(t, r).Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Errorf("advanceDraining must re-assert the absent mirror; active-rotation-state = %q, want draining", got)
	}
	// The candidate is already being deleted, so no re-delete is needed; the anchor
	// stays held.
	if getPool(t, r).Annotations[annotations.ActiveRotation] != "nc-old" {
		t.Error("the anchor must stay held while draining")
	}
}

// TestStuckDrainHoldsGateAndSetsGauge: a drain that has run past tGP + buffer
// (the §5.2 drain bound) is STUCK, but the controller deliberately keeps the
// serial gate held — the state stays draining, the anchor is not cleared — and
// surfaces the condition as the recomputed drain_stuck gauge (true), not by
// forcing a rollback. The gauge is a 0/1 recompute every pass, so a separate
// not-stuck pass must clear it (TestObserveIdlePoolGauges covers the idle clear).
func TestStuckDrainHoldsGateAndSetsGauge(t *testing.T) {
	rec := &fakeRecorder{}
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StateDraining))
	// Deleting for 2h, well past the 45m bound (tGP 30m + buffer 15m).
	dt := metav1.NewTime(testNow.Add(-2 * time.Hour))
	cand.DeletionTimestamp = &dt
	pool := withExpireAfter(withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
	})))
	r := newReconciler(t, testNow, rec, pool, cand, testK8sNode(candNode, true, nil, true))

	step(t, r, pool)

	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "nc-old" {
		t.Error("a stuck drain must NOT clear the anchor — the serial gate stays held")
	}
	if p.Annotations[annotations.ActiveRotationState] != annotations.StateDraining {
		t.Error("a stuck drain must stay in the draining state (no forced rollback)")
	}
	if !rec.obs[testPoolName].DrainStuck {
		t.Error("the drain_stuck gauge must be set while the drain is past the bound")
	}
	if rec.success != 0 || rec.failure != 0 {
		t.Errorf("a stuck (but not finished) drain must not emit success/failure: %+v", rec)
	}
}

// TestDrainWithinBoundClearsStuckGauge is the recompute-clears negative control
// for the drain_stuck gauge: a rotation still draining but within the bound must
// report drain_stuck=false (the 0/1 gauge resets every pass), while the gate
// stays held.
func TestDrainWithinBoundClearsStuckGauge(t *testing.T) {
	rec := &fakeRecorder{}
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StateDraining))
	dt := metav1.NewTime(testNow.Add(-5 * time.Minute)) // within the 45m bound
	cand.DeletionTimestamp = &dt
	pool := withExpireAfter(withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
	})))
	r := newReconciler(t, testNow, rec, pool, cand, testK8sNode(candNode, true, nil, true))

	step(t, r, pool)

	if rec.obs[testPoolName].DrainStuck {
		t.Error("a drain within the bound must recompute drain_stuck=false")
	}
	if getPool(t, r).Annotations[annotations.ActiveRotation] != "nc-old" {
		t.Error("the anchor must stay held while draining within the bound")
	}
}

// TestSurgeNotReadyOnNodePoolLabelMismatch covers the surgeReady NodePool-label
// guard: the placeholder is Running on a Ready host distinct from the candidate,
// but the host carries a karpenter.sh/nodepool label for a DIFFERENT pool, so it
// is not a valid surge target and surge_ready must not hold (spec §5.2). The
// rotation stays pending and the old NodeClaim is not deleted.
func TestSurgeNotReadyOnNodePoolLabelMismatch(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-2*time.Minute))))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old"}, true)
	// Ready host, but in a different NodePool than the candidate's.
	otherPoolHost := testK8sNode(surgeNode, true, nil, false)
	otherPoolHost.Labels[karpv1.NodePoolLabelKey] = "other-pool"
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	r := newReconciler(t, testNow, nil, pool, cand, oldNode, otherPoolHost, ph)

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotationState] == annotations.StateDraining {
		t.Error("a surge host in a different NodePool must not trigger the draining transition")
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.DeletionTimestamp != nil {
		t.Error("the old NodeClaim must not be deleted when the surge host's NodePool label mismatches")
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
	surgeFrozen := testK8sNode(surgeNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old"}, false)
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
	if n := getNodeObj(t, r, surgeNode); n.Annotations[annotations.SurgeFor] != "" ||
		n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "" {
		t.Error("surge target must be unfrozen (surge-for + controller-owned do-not-disrupt removed)")
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
	surgeFrozen := testK8sNode(surgeNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old"}, false)
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
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
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
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
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

func TestReadyTimeoutDoesNotReapSameNamedPodInOtherNamespace(t *testing.T) {
	// readyTimeout elapsed → the attempt fails and the induced surge claim has
	// registered a node. A real workload Pod in ANOTHER namespace happens to share
	// the placeholder's name. Pod names are unique only within a namespace, so the
	// rollback guard must not mistake it for the placeholder and reap an occupied
	// host (issue #37).
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(testNow.Add(-20*time.Minute)),
		annotations.SurgeClaim, "nc-new",
	))
	surgeClaim := testClaim("nc-new", 0, ncCreated(testNow.Add(-10*time.Minute)), ncNode(surgeNode))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
	surgeHost := testK8sNode(surgeNode, true, nil, false)
	// a real workload Pod whose name collides with the placeholder but lives in a
	// different namespace than the controller's.
	collidingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: surge.PlaceholderName("nc-old"), Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: surgeNode},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	ph := placeholderPod(surgeNode, corev1.PodRunning)
	r := newReconciler(t, testNow, nil, pool, cand, surgeClaim, oldNode, surgeHost, collidingPod, ph)

	step(t, r, pool)

	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("the attempt must still fail: %+v", c)
	}
	if getClaimOrNil(t, r, "nc-new") == nil {
		t.Error("a host carrying a same-named Pod in another namespace must NOT be reaped (issue #37)")
	}
}

// TestReadyTimeoutInducedClaimFallbackReaps covers the never-bound fallback —
// the DOMINANT readyTimeout cause (spec §3.3 Rollback, §5.2). The induced
// instance never registers / never reaches Ready, so no bind is ever observable:
// the placeholder carries no spec.nodeName and the pending handler never persisted
// surge-claim. failPending must then fall back to inducedClaim(), which resolves
// the surge claim as the pool's NodeClaim created after started-at with NO
// registered Node, reap it under the §5.2 guards, and fail the rotation cleanly.
//
// The fixture also seeds the two negative cases the guards must reject:
//   - an absorb host created BEFORE started-at (the after-start guard); and
//   - a claim created after started-at but already carrying real Pods on its node
//     (the hosting-nothing-but-the-placeholder guard).
//
// Both must survive; only the never-registered claim is reaped.
func TestReadyTimeoutInducedClaimFallbackReaps(t *testing.T) {
	rec := &fakeRecorder{}
	startedAt := testNow.Add(-20 * time.Minute) // readyTimeout (15m) elapsed
	// Candidate WITHOUT surge-claim: the never-bound path never persisted it.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StatePending,
		annotations.StartedAt, rfc(startedAt),
	))
	// The induced surge claim: created after started-at, never registered a Node →
	// the only claim inducedClaim's fallback may resolve, reapable trivially.
	induced := testClaim("nc-induced", 0, ncCreated(startedAt.Add(5*time.Minute)))
	induced.Status.NodeName = ""
	// Negative case A — a pre-existing capacity-absorb host created BEFORE
	// started-at. The after-start guard must spare it (healthy production capacity,
	// not surge debris), and inducedClaim must never resolve it.
	absorbNode := "node-absorb"
	absorb := testClaim("nc-absorb", time.Hour, ncCreated(startedAt.Add(-30*time.Minute)), ncNode(absorbNode))
	// Negative case B — a claim born after started-at (an unrelated concurrent
	// scale-up) that already registered a Node carrying a real Pod. inducedClaim
	// skips it (has a Node), and even if named the reap guard would spare it.
	scaleupNode := "node-scaleup"
	scaleup := testClaim("nc-scaleup", 0, ncCreated(startedAt.Add(2*time.Minute)), ncNode(scaleupNode))
	realPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-app", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: scaleupNode},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	oldNode := testK8sNode(candNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old", annotations.Cordoned: "true"}, true)
	// Placeholder never bound (no spec.nodeName) — the no-bind precondition.
	ph := placeholderPod("", corev1.PodPending)
	r := newReconciler(t, testNow, rec, pool, cand, induced, absorb, scaleup,
		oldNode, testK8sNode(absorbNode, true, nil, false), testK8sNode(scaleupNode, true, nil, false), realPod, ph)

	// Direct unit check: the fallback resolves the never-registered claim, not the
	// candidate, the absorb host, or the already-occupied scale-up claim.
	if got, err := r.inducedClaim(context.Background(), pool, cand); err != nil || got != "nc-induced" {
		t.Fatalf("inducedClaim fallback: got %q (err %v), want nc-induced", got, err)
	}

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
	if getPool(t, r).Annotations[annotations.LastFailureAt] == "" {
		t.Error("last-failure-at must be stamped on the NodePool")
	}
	if getClaimOrNil(t, r, "nc-induced") != nil {
		t.Error("the never-registered induced claim must be reaped via the fallback")
	}
	if getClaimOrNil(t, r, "nc-absorb") == nil {
		t.Error("a pre-existing absorb host (created before started-at) must NOT be reaped (spec §3.3)")
	}
	if getClaimOrNil(t, r, "nc-scaleup") == nil {
		t.Error("a claim carrying real Pods must NOT be reaped (spec §3.3)")
	}
	if placeholderExists(t, r) {
		t.Error("placeholder must be deleted on failure")
	}
	if rec.failure != 1 {
		t.Errorf("failure metric: got %d", rec.failure)
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
