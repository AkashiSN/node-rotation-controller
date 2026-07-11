package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// These end-to-end tests chain the full rotation state machine across several
// reconciles (spec §5.2/§5.3), driving it with the same fake client and helpers
// the per-step tests use. Where the per-step tests pin one transition with a
// hand-built starting state, these drive the chain start → complete (and
// failure → retry) and inject the external signals — a placeholder reaching
// Running, its host Node going Ready, the old NodeClaim finalizing away — that
// Karpenter/kubelet/the scheduler would supply in a live cluster, asserting the
// durable state after each step (issue #74).

// lifecycleClock is a mutable clock the lifecycle tests advance between phases,
// so timeouts/backoffs elapse the way real wall-clock would across reconciles.
type lifecycleClock struct{ t time.Time }

func (c *lifecycleClock) now() time.Time          { return c.t }
func (c *lifecycleClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// stepFresh re-reads the NodePool from the store and runs one reconcile on that
// fresh copy — the way the real loop reads current state each pass — so the
// chain never reuses a stale in-memory pool across steps.
func stepFresh(t *testing.T, r *RotationReconciler) {
	t.Helper()
	step(t, r, getPool(t, r))
}

// markPlaceholderRunning sets the named placeholder Running on host and records
// the bind target, simulating the scheduler binding it to a surge node.
func markPlaceholderRunning(t *testing.T, r *RotationReconciler, claimName, host string) {
	t.Helper()
	var ph corev1.Pod
	key := types.NamespacedName{Namespace: testNS, Name: surge.PlaceholderName(claimName)}
	if err := r.Get(context.Background(), key, &ph); err != nil {
		t.Fatalf("get placeholder: %v", err)
	}
	ph.Spec.NodeName = host
	if err := r.Update(context.Background(), &ph); err != nil {
		t.Fatalf("update placeholder spec: %v", err)
	}
	// Phase lives on the status subresource — write it through the status writer,
	// or the fake client silently drops it (a plain Update never persists status).
	ph.Status.Phase = corev1.PodRunning
	if err := r.Status().Update(context.Background(), &ph); err != nil {
		t.Fatalf("update placeholder status: %v", err)
	}
}

// finalizeAway removes the karpenter finalizer from a claim already carrying a
// deletion timestamp, letting the fake store reap it — what Karpenter's
// termination controller does once the node has drained.
func finalizeAway(t *testing.T, r *RotationReconciler, name string) {
	t.Helper()
	c := getClaimOrNil(t, r, name)
	if c == nil {
		return
	}
	c.Finalizers = nil
	if err := r.Update(context.Background(), c); err != nil {
		t.Fatalf("clear finalizers on %s: %v", name, err)
	}
	if getClaimOrNil(t, r, name) != nil {
		t.Fatalf("claim %s should be finalized away", name)
	}
}

// TestLifecycleStartToComplete drives the happy-path rotation across every
// reconcile from start to success, injecting the external signals between steps
// (issue #74):
//
//  1. start          → anchor written, placeholder created, old node frozen + cordoned
//  2. surge ready    → pending → draining (mirror + draining-at written, old claim deleted)
//  3. drain finalize → completion: placeholder deleted, surge target unfrozen,
//     last-rotation-at written, success + drain duration emitted, anchor cleared.
func TestLifecycleStartToComplete(t *testing.T) {
	clk := &lifecycleClock{t: testNow}
	rec := &fakeRecorder{}

	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer())
	oldNode := testK8sNode(candNode, true, nil, false)
	surgeHost := testK8sNode(surgeNode, true, nil, false)
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, clk.t, rec, pool, cand, oldNode, surgeHost)
	r.Clock = clk.now // mutable clock for the chained timeline

	// ── Step 1: start ─────────────────────────────────────────────────────────
	stepFresh(t, r)

	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Fatalf("step 1 anchor: got %q, want nc-old", got)
	}
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Fatalf("step 1 claim state: got %+v", c)
	}
	startedAt := c.Annotations[annotations.StartedAt]
	if startedAt == "" {
		t.Fatal("step 1: started-at must be stamped")
	}
	if n := getNodeObj(t, r, candNode); n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" ||
		n.Annotations[annotations.SurgeFor] != "nc-old" || !n.Spec.Unschedulable ||
		n.Annotations[annotations.Cordoned] != "true" {
		t.Fatalf("step 1: old node must be frozen + cordoned: %+v / unschedulable=%v", n.Annotations, n.Spec.Unschedulable)
	}
	if !placeholderExists(t, r) {
		t.Fatal("step 1: placeholder must be created")
	}

	// ── Step 2: surge becomes ready → draining ─────────────────────────────────
	clk.advance(2 * time.Minute)
	markPlaceholderRunning(t, r, "nc-old", surgeNode)
	stepFresh(t, r)

	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotationState] != annotations.StateDraining {
		t.Fatalf("step 2 mirror: got %q, want draining", p.Annotations[annotations.ActiveRotationState])
	}
	if p.Annotations[annotations.DrainingAt] == "" {
		t.Fatal("step 2: draining-at must be stamped at pending → draining")
	}
	// surge-wait is stamped write-once in the same update, carrying the surge phase
	// (2m of chained wall time) forward to the completion line (#228).
	if got := p.Annotations[annotations.SurgeWait]; got != "2m0s" {
		t.Fatalf("step 2: surge-wait must be stamped with the surge-phase duration at pending → draining: got %q, want 2m0s", got)
	}
	if n := getNodeObj(t, r, surgeNode); n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Fatalf("step 2: surge target must be frozen with the rotation marker: %+v", n.Annotations)
	}
	c = getClaimOrNil(t, r, "nc-old")
	if c == nil || c.DeletionTimestamp == nil {
		t.Fatal("step 2: the old NodeClaim must be deleted (delete issued)")
	}
	// surge_wait duration was observed at the transition (started-at → surge_ready).
	if !hasPhaseDuration(rec, PhaseSurgeWait) {
		t.Errorf("step 2: surge_wait duration must be observed; durations=%+v", rec.durations)
	}

	// ── Step 3: old NodeClaim finalizes away → completion ──────────────────────
	clk.advance(30 * time.Minute) // wall time for the drain phase
	finalizeAway(t, r, "nc-old")
	stepFresh(t, r)

	p = getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" || p.Annotations[annotations.ActiveRotationState] != "" {
		t.Errorf("step 3: anchor and mirror must be cleared on completion: %+v", p.Annotations)
	}
	if p.Annotations[annotations.DrainingAt] != "" {
		t.Error("step 3: draining-at must be cleared on completion")
	}
	if p.Annotations[annotations.SurgeWait] != "" {
		t.Error("step 3: surge-wait must be cleared with the anchor on completion")
	}
	if p.Annotations[annotations.LastRotationAt] == "" {
		t.Error("step 3: last-rotation-at must be stamped on success")
	}
	if n := getNodeObj(t, r, surgeNode); n.Annotations[annotations.SurgeFor] != "" ||
		n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "" {
		t.Errorf("step 3: surge target must be unfrozen by the marker: %+v", n.Annotations)
	}
	if placeholderExists(t, r) {
		t.Error("step 3: placeholder must be deleted on completion")
	}
	if rec.success != 1 {
		t.Errorf("step 3: success metric: got %d, want 1", rec.success)
	}
	if rec.failure != 0 || rec.expired != 0 {
		t.Errorf("step 3: a clean lifecycle must emit no failure/expired: %+v", rec)
	}
	if d, ok := phaseDuration(rec, PhaseDrain); !ok {
		t.Errorf("step 3: drain duration must be observed; durations=%+v", rec.durations)
	} else if d != 30*time.Minute {
		t.Errorf("step 3: drain duration: got %v, want 30m", d)
	}
}

// TestLifecycleReadyTimeoutThenRetry chains the failure path: a pending rotation
// whose surge never becomes ready times out → failed (escalated backoff stamped,
// anchor released), the backoff window elapses, then the claim re-enters pending
// on a fresh attempt with a re-stamped started-at (spec §5.2/§5.3, issue #74).
func TestLifecycleReadyTimeoutThenRetry(t *testing.T) {
	clk := &lifecycleClock{t: testNow}
	rec := &fakeRecorder{}

	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer())
	oldNode := testK8sNode(candNode, true, nil, false)
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, clk.t, rec, pool, cand, oldNode)
	r.Clock = clk.now

	// ── Step 1: start → pending, placeholder created ───────────────────────────
	stepFresh(t, r)
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Fatalf("step 1: claim must be pending: %+v", c)
	}
	firstStartedAt := c.Annotations[annotations.StartedAt]
	if firstStartedAt == "" {
		t.Fatal("step 1: started-at must be stamped")
	}
	if !placeholderExists(t, r) {
		t.Fatal("step 1: placeholder must be created")
	}

	// ── Step 2: readyTimeout elapses with no surge → failed ────────────────────
	// readyTimeout defaults to 15m; advance past it. The placeholder stays Pending
	// (never bound) so surge readiness never holds.
	clk.advance(20 * time.Minute)
	stepFresh(t, r)

	c = getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("step 2: claim must be failed after readyTimeout: %+v", c)
	}
	if c.Annotations[annotations.RetryCount] != "1" {
		t.Errorf("step 2: retry-count: got %q, want 1", c.Annotations[annotations.RetryCount])
	}
	if c.Annotations[annotations.StartedAt] != "" {
		t.Errorf("step 2: started-at must be cleared on failure: %q", c.Annotations[annotations.StartedAt])
	}
	failedAt := c.Annotations[annotations.FailedAt]
	if failedAt == "" {
		t.Fatal("step 2: failed-at must be stamped")
	}
	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" {
		t.Error("step 2: anchor must be released on failure")
	}
	if p.Annotations[annotations.LastFailureAt] == "" {
		t.Error("step 2: last-failure-at pause anchor must be stamped")
	}
	if placeholderExists(t, r) {
		t.Error("step 2: placeholder must be deleted on failure")
	}
	if n := getNodeObj(t, r, candNode); n.Spec.Unschedulable ||
		n.Annotations[annotations.SurgeFor] != "" || n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "" {
		t.Errorf("step 2: candidate node must be unfrozen + uncordoned on failure: %+v", n.Annotations)
	}
	if rec.failure != 1 {
		t.Errorf("step 2: failure metric: got %d, want 1", rec.failure)
	}

	// ── Step 3: still within the escalated backoff → stays failed ──────────────
	// retryCount 1 → escalated backoff is the base retryBackoff (30m default). Only
	// 5m has elapsed since failed-at, so a re-entry must NOT yet fire.
	clk.advance(5 * time.Minute)
	stepFresh(t, r)
	c = getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("step 3: claim must stay failed within the backoff: %+v", c)
	}
	if placeholderExists(t, r) {
		t.Error("step 3: no placeholder may be recreated within the backoff")
	}

	// ── Step 4: backoff + cooldown elapsed → failed re-enters pending ──────────
	// cooldownAfter (10m) and the 30m backoff must both elapse since the failure.
	clk.advance(30 * time.Minute)
	stepFresh(t, r)

	c = getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StatePending {
		t.Fatalf("step 4: failed claim must re-enter pending: %+v", c)
	}
	reStartedAt := c.Annotations[annotations.StartedAt]
	if reStartedAt == "" {
		t.Fatal("step 4: started-at must be re-stamped on re-entry")
	}
	if reStartedAt == firstStartedAt {
		t.Errorf("step 4: started-at must be re-stamped to the new attempt time, not the first (%q)", firstStartedAt)
	}
	if c.Annotations[annotations.RetryCount] != "1" {
		t.Errorf("step 4: retry-count must be preserved across the re-entry: got %q", c.Annotations[annotations.RetryCount])
	}
	if !placeholderExists(t, r) {
		t.Error("step 4: a fresh placeholder must be (re)created on re-entry")
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("step 4: the rotation must be re-anchored on re-entry: got %q", got)
	}
}

// TestLifecycleConcurrentNodePools asserts the cross-NodePool concurrency
// invariant (spec §2/§5.2, issue #74): distinct NodePools rotate independently —
// one reaching draining never gates another from starting — while each remains
// serial within itself. Two pools are driven through start → draining; the
// reconciler is keyed on the pool, so each carries its own anchor.
func TestLifecycleConcurrentNodePools(t *testing.T) {
	clk := &lifecycleClock{t: testNow}
	rec := &fakeRecorder{}

	const poolB = "web"
	// Pool A reuses the shared "api" fixtures. Pool B is a second in-scope pool.
	candA := testClaim("nc-a-old", 20*24*time.Hour, ncNode("node-a-old"), ncFinalizer())
	nodeA := testK8sNode("node-a-old", true, nil, false)
	hostA := testK8sNode("node-a-new", true, nil, false)

	candB := testClaim("nc-b-old", 20*24*time.Hour, ncNode("node-b-old"), ncFinalizer())
	candB.Labels[karpv1.NodePoolLabelKey] = poolB
	nodeB := testK8sNode("node-b-old", true, nil, false)
	nodeB.Labels[karpv1.NodePoolLabelKey] = poolB
	hostB := testK8sNode("node-b-new", true, nil, false)
	hostB.Labels[karpv1.NodePoolLabelKey] = poolB

	poolAObj := withTGP(testNodePool(nil))
	poolBObj := withTGP(&karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name:   poolB,
		Labels: map[string]string{"workload": "api"}, // matches the test selector
	}})

	r := newReconciler(t, clk.t, rec, poolAObj, poolBObj, candA, nodeA, hostA, candB, nodeB, hostB)
	r.Clock = clk.now

	getPoolBy := func(name string) *karpv1.NodePool {
		t.Helper()
		var p karpv1.NodePool
		if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &p); err != nil {
			t.Fatalf("get pool %s: %v", name, err)
		}
		return &p
	}

	// ── Start both pools; each anchors its own candidate independently. ─────────
	step(t, r, getPoolBy(testPoolName))
	step(t, r, getPoolBy(poolB))

	if got := getPoolBy(testPoolName).Annotations[annotations.ActiveRotation]; got != "nc-a-old" {
		t.Fatalf("pool A anchor: got %q, want nc-a-old", got)
	}
	if got := getPoolBy(poolB).Annotations[annotations.ActiveRotation]; got != "nc-b-old" {
		t.Fatalf("pool B anchor: got %q, want nc-b-old — a busy pool A must not gate pool B", got)
	}

	// ── Drive both to draining concurrently; bind each placeholder to its host. ─
	clk.advance(2 * time.Minute)
	markPlaceholderRunning(t, r, "nc-a-old", "node-a-new")
	markPlaceholderRunning(t, r, "nc-b-old", "node-b-new")
	step(t, r, getPoolBy(testPoolName))
	step(t, r, getPoolBy(poolB))

	if got := getPoolBy(testPoolName).Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Errorf("pool A must reach draining: got %q", got)
	}
	if got := getPoolBy(poolB).Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Errorf("pool B must reach draining independently of pool A: got %q", got)
	}
	if ca := getClaimOrNil(t, r, "nc-a-old"); ca == nil || ca.DeletionTimestamp == nil {
		t.Error("pool A old NodeClaim must be deleted")
	}
	if cb := getClaimOrNil(t, r, "nc-b-old"); cb == nil || cb.DeletionTimestamp == nil {
		t.Error("pool B old NodeClaim must be deleted")
	}

	// ── Complete pool A; pool B must remain mid-rotation and unaffected. ────────
	clk.advance(10 * time.Minute)
	finalizeAway(t, r, "nc-a-old")
	step(t, r, getPoolBy(testPoolName))

	if got := getPoolBy(testPoolName).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("pool A must complete and clear its anchor: got %q", got)
	}
	if got := getPoolBy(poolB).Annotations[annotations.ActiveRotation]; got != "nc-b-old" {
		t.Errorf("pool A completing must not disturb pool B's in-flight rotation: got %q", got)
	}
	if got := getPoolBy(poolB).Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Errorf("pool B must still be draining after pool A completes: got %q", got)
	}
}

// phaseDuration returns the most recently observed duration for phase.
func phaseDuration(rec *fakeRecorder, phase string) (time.Duration, bool) {
	var d time.Duration
	found := false
	for _, rd := range rec.durations {
		if rd.phase == phase {
			d, found = rd.d, true
		}
	}
	return d, found
}

func hasPhaseDuration(rec *fakeRecorder, phase string) bool {
	_, ok := phaseDuration(rec, phase)
	return ok
}
