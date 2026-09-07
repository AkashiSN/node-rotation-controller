package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// Issue #326: the surge_headroom gate (spec §5.2 step 3) blocked a rotation with
// nothing an operator could alert on — an undeduplicated log line every
// longRequeue, no Event, and no row in the §4.2 table. A pool could stop rotating
// for as long as its limits stayed full and the only evidence was a line
// repeating in the log. These tests pin the Event, its content, and its dedup.

// tightPool builds a pool whose cpu limit leaves 1 cpu of headroom, against a
// candidate whose reschedulable drain needs 2 — the §7.2 shape.
func tightPool(anns map[string]string) *karpv1.NodePool {
	p := withTGP(testNodePool(anns))
	p.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse("1")}
	return p
}

func TestHeadroomBlockEmitsWarningEventNamingTheResource(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := tightPool(nil)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"))
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Fatal("the rotation must not start")
	}
	if !containsLine(lines, "insufficient limits headroom") {
		t.Errorf("the existing log line must stay; lines = %v", lines)
	}
	evs := drain(rec)
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 Event, got %d: %v", len(evs), evs)
	}
	// The operator has to know which resource, by how much, and against which
	// ceiling — the numbers that decide whether to raise spec.limits.
	for _, want := range []string{"Warning", reasonInsufficientHeadroom, "cpu", "nc-old"} {
		if !containsLine(evs, want) {
			t.Errorf("Event must contain %q: %v", want, evs)
		}
	}
}

func TestHeadroomBlockEventDedupsWhileTheConditionHolds(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := tightPool(nil)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"))
	r.Events = rec

	step(t, r, getPool(t, r))
	if evs := drain(rec); len(evs) != 1 {
		t.Fatalf("pass 1 must emit exactly 1 Event, got %d: %v", len(evs), evs)
	}

	step(t, r, getPool(t, r))
	if evs := drain(rec); len(evs) != 0 {
		t.Errorf("pass 2 must stay silent while the block is unchanged, got %v", evs)
	}
}

// The numbers in the message move on their own: status.resources tracks the
// pool's provisioned capacity, so any scale or consolidation elsewhere in the
// pool changes `remaining` while the block itself is unchanged. Deduplicating on
// the rendered message would re-fire the Event on every one of those, which on a
// busy pool means every longRequeue — the spam this dedup exists to prevent.
func TestHeadroomBlockEventDedupsWhileTheNumbersMove(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := tightPool(nil)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"))
	r.Events = rec

	step(t, r, getPool(t, r))
	if evs := drain(rec); len(evs) != 1 {
		t.Fatalf("pass 1 must announce the block, got %v", evs)
	}

	// Another node joins the pool: provisioned rises, remaining falls, the block
	// is the same block.
	moved := getPool(t, r)
	moved.Status.Resources = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}
	if err := r.Update(context.Background(), moved); err != nil {
		t.Fatalf("move status.resources: %v", err)
	}

	step(t, r, getPool(t, r))
	if evs := drain(rec); len(evs) != 0 {
		t.Errorf("a moved budget under an unchanged block must not re-fire, got %v", evs)
	}
}

// A block that clears and returns is a new occurrence and must be announced
// again, or an operator who raises the limit and later exhausts it hears nothing.
func TestHeadroomBlockEventRefiresAfterTheBlockClears(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := tightPool(nil)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"))
	r.Events = rec

	step(t, r, getPool(t, r))
	if evs := drain(rec); len(evs) != 1 {
		t.Fatalf("pass 1 must announce the block, got %v", evs)
	}

	// The operator raises the limit: the gate opens and the rotation starts.
	raised := getPool(t, r)
	raised.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse("100")}
	if err := r.Update(context.Background(), raised); err != nil {
		t.Fatalf("raise limits: %v", err)
	}
	step(t, r, getPool(t, r))
	if getPool(t, r).Annotations[annotations.ActiveRotation] != "nc-old" {
		t.Fatal("with headroom available the rotation must start")
	}
	drain(rec)

	// The rotation ends, leaving the claim a candidate again, and the limit is
	// exhausted once more → a new occurrence of the same block.
	back := getPool(t, r)
	delete(back.Annotations, annotations.ActiveRotation)
	back.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse("1")}
	if err := r.Update(context.Background(), back); err != nil {
		t.Fatalf("restore tight limits: %v", err)
	}
	reset := getClaimOrNil(t, r, "nc-old")
	reset.Annotations = nil
	if err := r.Update(context.Background(), reset); err != nil {
		t.Fatalf("reset the claim to candidate: %v", err)
	}
	step(t, r, getPool(t, r))
	if evs := drain(rec); len(evs) != 1 {
		t.Errorf("a block that returns must re-fire, got %d: %v", len(evs), evs)
	}
}

// The retry path is the silent one: a failed attempt whose backoff has elapsed
// and whose every other gate is open is held back by headroom alone, once per
// backoff, with no line and no Event at all before this change.
func TestHeadroomBlockOnRetryEmitsTheEvent(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StateFailed,
			annotations.FailedAt, rfc(testNow.Add(-2*time.Hour)),
			annotations.RetryCount, "1"))
	pool := tightPool(map[string]string{annotations.ActiveRotation: "nc-old"})
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"))
	r.Events = rec

	step(t, r, getPool(t, r))

	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatal("the retry must not start while headroom blocks it")
	}
	evs := drain(rec)
	if len(evs) != 1 {
		t.Fatalf("the held-back retry must announce why, got %d: %v", len(evs), evs)
	}
	if !containsLine(evs, reasonInsufficientHeadroom) {
		t.Errorf("Event must name the headroom block: %v", evs)
	}
}

// A footprint the clamp would REFUSE — the candidate's own instance class has no
// provisionable capacity left once its DaemonSet overhead is counted — can also
// be short of the pool's budget, and then the headroom gate blocks first and the
// refusal is never reached. The operator has to hear both conditions.
//
// The caveat is deliberately CONDITIONAL, and an earlier version of this test
// pinned the opposite. Refused is scoped to the candidate's class: the
// placeholder does not pin the instance type, so Karpenter may still satisfy the
// reservation on a larger type the NodePool allows, in which case raising the
// budget really is the whole fix. Asserting "raising limits will not help" would
// be a verdict this controller cannot reach.
func TestHeadroomBlockNamesAnUnprovisionableReservation(t *testing.T) {
	// allocatable == DaemonSet overhead → no capacity to reserve on this type.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "300m", "memory", "1Gi"))
	pool := tightPool(nil)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"),
		asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1Gi")))
	r.Events = rec

	if _, err := r.reconcileNodePool(context.Background(), pool, wholeNodePolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Fatal("the rotation must not start")
	}
	evs := drain(rec)
	if len(evs) != 1 {
		t.Fatalf("want 1 Event, got %d: %v", len(evs), evs)
	}
	// The budget really is blocking, so the ordinary advice stays.
	if !containsLine(evs, "Raise spec.limits") {
		t.Errorf("the Event must still name the budget that is blocking: %v", evs)
	}
	// And the second condition is stated as a caveat, scoped to the candidate's
	// own instance type rather than asserted as unprovisionable everywhere.
	if !containsLine(evs, "may not be sufficient on its own", "DaemonSet", "larger instance type") {
		t.Errorf("the Event must add the refusal caveat with its scope: %v", evs)
	}
	if containsLine(evs, "whatever the budget") || containsLine(evs, "will NOT let this rotation proceed") {
		t.Errorf("the caveat must not claim the reservation is unprovisionable on every type: %v", evs)
	}
}

// The refusal names a resource of its own, and it can move independently of the
// resource headroom blocks on: a DaemonSet change can shift the exhausted
// dimension from cpu to memory while the budget still runs out on cpu first.
// That is a different diagnosis with a different remedy, so it has to
// re-announce — which it only does if the refused resource is part of the
// identity, not just the fact of refusal.
func TestHeadroomBlockRefiresWhenTheRefusedResourceChanges(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "300m", "memory", "1Gi"))
	pool := withTGP(testNodePool(nil))
	// 100m: the 300m whole-node cpu request never fits, so headroom always blocks
	// on cpu whichever dimension the clamp refuses.
	pool.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse("100m")}
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "0"))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"), ds)
	r.Events = rec

	if _, err := r.reconcileNodePool(context.Background(), pool, wholeNodePolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	first := drain(rec)
	if len(first) != 1 {
		t.Fatalf("pass 1 must announce the block, got %v", first)
	}
	if !containsLine(first, "cpu") {
		t.Fatalf("pass 1 must refuse on cpu: %v", first)
	}

	// The DaemonSet's footprint moves from cpu to memory: cpu becomes
	// provisionable and memory is the exhausted dimension instead.
	var live corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "kube-proxy"}, &live); err != nil {
		t.Fatalf("get daemonset pod: %v", err)
	}
	live.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("0"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	if err := r.Update(context.Background(), &live); err != nil {
		t.Fatalf("move the DaemonSet footprint: %v", err)
	}

	if _, err := r.reconcileNodePool(context.Background(), getPool(t, r), wholeNodePolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if evs := drain(rec); len(evs) != 1 {
		t.Errorf("a different refused resource is a different block and must re-fire, got %d: %v", len(evs), evs)
	}
}

// The Event claims headroom is the SOLE remaining blocker, and its message tells
// the operator that raising spec.limits lets the rotation proceed. On a pool
// whose schedule is fatally infeasible that advice is wrong: the fresh-start
// fatal gate would refuse the rotation anyway. The retry leg sits above that gate
// — the same shape #302 closed for static pools — so it has to close on fatal
// feasibility too, or the Event misdirects and the retry starts an attempt a
// fresh start would have refused.
func TestHeadroomBlockOnRetryStaysSilentWhenFeasibilityIsFatal(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StateFailed,
			annotations.FailedAt, rfc(testNow.Add(-2*time.Hour)),
			annotations.RetryCount, "1"))
	// A short template expireAfter drives a fatal ANonPositive finding (§3.2).
	pool := withTemplateE(tightPool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateFailed,
	}), 40*time.Hour)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false),
		workloadPod("app", candNode, "2", "1Gi"))
	r.Events = rec

	step(t, r, getPool(t, r))

	if containsLine(drain(rec), reasonInsufficientHeadroom) {
		t.Error("headroom must not be announced as the blocker while a fatal finding also blocks")
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Error("the retry must not start on a fatally infeasible schedule")
	}
	// Same treatment as the static gate: the repair branch releases the anchor so
	// the pool falls through to the fresh-start fatal gate on the next pass.
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("the anchor must be released, got %q", got)
	}
}
