package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// phKey addresses the placeholder for the single rotation under test (nc-old).
func phKey() types.NamespacedName {
	return types.NamespacedName{Namespace: testNS, Name: surge.PlaceholderName("nc-old")}
}

// asDaemonSet marks an already-built Pod as DaemonSet-owned — the overhead the
// clamp subtracts from allocatable (spec §3.3). It mutates in place so its
// argument stays a genuine value, not a helper reused with constant arguments.
func asDaemonSet(p *corev1.Pod) *corev1.Pod {
	ctl := true
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds", Controller: &ctl}}
	return p
}

// ncAllocatable stamps the NodeClaim's estimated allocatable — Karpenter's own
// answer to "how big is a node I would provision for this pool", which the clamp
// reads as the ceiling (issue #224).
func ncAllocatable(kv ...string) ncOpt {
	return func(c *karpv1.NodeClaim) {
		rl := corev1.ResourceList{}
		for i := 0; i+1 < len(kv); i += 2 {
			rl[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
		}
		c.Status.Allocatable = rl
	}
}

// nodeAllocatable stamps the real Node's allocatable — what the kubelet reports
// and kube-scheduler packs against. Its gap from the NodeClaim's cached estimate
// is the per-AZ band that bounds the clamp's shortfall (issue #224).
func nodeAllocatable(n *corev1.Node, kv ...string) *corev1.Node {
	rl := corev1.ResourceList{}
	for i := 0; i+1 < len(kv); i += 2 {
		rl[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	n.Status.Allocatable = rl
	return n
}

// The clamp gives up more than the per-AZ band can explain, which means the
// controller's request accounting has diverged from the scheduler's. The
// rotation still proceeds — refusing would trade a bounded, in-window,
// PDB-respecting drain for Forceful Expiration, which honours neither — but the
// divergence is announced rather than silent (issue #224).
func TestPlaceholderClampWarnsWhenShortfallExceedsBand(t *testing.T) {
	// node 14600Mi − claim 14570Mi = band 30Mi, while the clamp gives up 112Mi.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "3770m", "memory", "14570Mi"))
	pool := withTGP(testNodePool(nil))
	workload := workloadPod("app", candNode, "1200m", "13600Mi")
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi"))
	rec := events.NewFakeRecorder(16)
	node := nodeAllocatable(testK8sNode(candNode, true, nil, false), "cpu", "3770m", "memory", "14600Mi")
	r := newReconciler(t, testNow, nil, pool, cand, node, workload, ds)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	// The rotation is NOT blocked: the placeholder exists at the clamped size.
	var ph corev1.Pod
	if err := r.Get(context.Background(), phKey(), &ph); err != nil {
		t.Fatalf("get placeholder: %v", err)
	}
	if got := ph.Spec.Containers[0].Resources.Requests.Memory(); got.Cmp(resource.MustParse("13488Mi")) != 0 {
		t.Errorf("placeholder memory: got %s, want the clamped 13488Mi", got.String())
	}
	if !containsLine(lines, "surge placeholder created", "bandExceeded", "memory") {
		t.Errorf("the line must name the resource whose shortfall exceeds the band; lines = %v", lines)
	}
	var warn string
	for _, e := range drain(rec) {
		if strings.Contains(e, reasonSurgeClampBandExceeded) {
			warn = e
		}
	}
	if warn == "" || !strings.Contains(warn, "Warning") {
		t.Fatalf("want a Warning SurgeClampBandExceeded Event, got %q", warn)
	}
}

// DaemonSet overhead at or above the NodeClaim's cached allocatable leaves that
// resource no positive ceiling, so every clamp value under it would reserve none
// of it. This fixture is exactly the case that makes the distinction matter: the
// cpu ceiling is positive and only memory is refused, so a clamped placeholder
// would still carry 1200m of cpu — not an empty Pod — while satisfying
// surge_ready with the 500Mi the evicted Pods need entirely unreserved. That is
// the break-before-make, on that one dimension. The clamp is refused instead:
// the placeholder keeps the full drain (issue #224). What this test
// pins is the sizing and the announcement; whether the placeholder then goes
// unschedulable is not decided by that ceiling at all, and
// TestClampRefusedEventDoesNotDecideSchedulability pins the Event saying so
// (issue #328).
func TestPlaceholderClampRefusedWhenDaemonSetExhaustsAllocatable(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "3770m", "memory", "1000Mi"))
	pool := withTGP(testNodePool(nil))
	workload := workloadPod("app", candNode, "1200m", "500Mi")
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1500Mi"))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false), workload, ds)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	// The placeholder carries the FULL drain — never a zero-sized reservation.
	var ph corev1.Pod
	if err := r.Get(context.Background(), phKey(), &ph); err != nil {
		t.Fatalf("get placeholder: %v", err)
	}
	if got := ph.Spec.Containers[0].Resources.Requests.Memory(); got.Cmp(resource.MustParse("500Mi")) != 0 {
		t.Errorf("refused clamp must keep the full drain: got %s, want 500Mi", got.String())
	}
	if !containsLine(lines, "surge placeholder created", "clampRefused", "memory") {
		t.Errorf("the line must state the refusal and its resource; lines = %v", lines)
	}
	var warn string
	for _, e := range drain(rec) {
		if strings.Contains(e, reasonSurgeClampRefused) {
			warn = e
		}
		if strings.Contains(e, reasonSurgeClamped) {
			t.Errorf("a refused clamp never reports SurgeClamped: %q", e)
		}
	}
	if warn == "" || !strings.Contains(warn, "Warning") {
		t.Fatalf("want a Warning SurgeClampRefused Event, got %q", warn)
	}
}

// The surge_headroom gate (spec §5.2 step 3) tests the CLAMPED footprint, not the
// raw drain: a nearly-full node whose clamped placeholder fits the remaining
// budget must start, even when its un-clamped reschedulable sum would not. Testing
// the raw sum would leave exactly the node #224 targets permanently unrotatable
// under a tight-but-sufficient limit.
func TestHeadroomGateUsesClampedFootprint(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("memory", "14570Mi"))
	pool := withTGP(testNodePool(nil))
	// Remaining budget 13500Mi: fits the clamped 13488Mi, not the raw 13600Mi.
	pool.Spec.Limits = karpv1.Limits{corev1.ResourceMemory: resource.MustParse("13500Mi")}
	workload := workloadPod("app", candNode, "1200m", "13600Mi")
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi"))
	node := nodeAllocatable(testK8sNode(candNode, true, nil, false), "memory", "14738Mi")
	r := newReconciler(t, testNow, nil, pool, cand, node, workload, ds)
	r.Events = events.NewFakeRecorder(16)

	step(t, r, pool)

	if getPool(t, r).Annotations[annotations.ActiveRotation] == "" {
		t.Error("a clamped placeholder that fits the budget must start the rotation")
	}
}

// A candidate node filled past Karpenter's cached per-AZ estimate: reschedulable
// drain 13600Mi, allocatable 14570Mi, DaemonSet 1082Mi → limit 13488Mi. The
// placeholder is clamped by 112Mi, the line announces it, and a Normal
// SurgeClamped Event lands on the NodeClaim (issue #224). The real node reports
// 14738Mi, so the 112Mi shortfall sits inside the 168Mi band and no warning fires.
func TestPlaceholderClampedWhenNodeExceedsProvisionableCapacity(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "3770m", "memory", "14570Mi", "pods", "110"))
	pool := withTGP(testNodePool(nil))
	workload := workloadPod("app", candNode, "1200m", "13600Mi")
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi"))
	rec := events.NewFakeRecorder(16)
	// Real node 14738Mi vs claim estimate 14570Mi → band 168Mi, so the 112Mi
	// shortfall sits inside it: clamp fires, no band-exceeded warning.
	node := nodeAllocatable(testK8sNode(candNode, true, nil, false), "cpu", "3770m", "memory", "14738Mi", "pods", "110")
	r := newReconciler(t, testNow, nil, pool, cand, node, workload, ds)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	// The line states the clamped requests plus the clamp breakdown.
	if !containsLine(lines, "surge placeholder created", "clamped", "13488Mi") {
		t.Errorf("clamped line must state clamped=true and the clamped requests; lines = %v", lines)
	}
	if !containsLine(lines, "surge placeholder created", "unclamped", "13600Mi", "shortfall", "112Mi") {
		t.Errorf("clamped line must state the unclamped drain and the shortfall; lines = %v", lines)
	}

	// Two Normal Events: RotationStarted (on the pool) and SurgeClamped (on the claim).
	evs := drain(rec)
	var clamped string
	for _, e := range evs {
		if strings.Contains(e, reasonSurgeClamped) {
			clamped = e
		}
	}
	if clamped == "" {
		t.Fatalf("want a SurgeClamped Event, got %v", evs)
	}
	if !strings.Contains(clamped, "Normal") {
		t.Errorf("SurgeClamped must be a Normal Event, got %q", clamped)
	}
	// The limit it reports is the SAME candidate-derived estimate the refusal is
	// computed from — cached allocatable minus the DaemonSet overhead observed
	// here — so it must not be announced as capacity Karpenter definitely has.
	// Karpenter's own estimate of the overhead for a fresh node can be larger, in
	// which case even this clamped placeholder fails resource fit (issue #328).
	if !containsLine([]string{clamped}, "candidate-derived provisionable estimate") {
		t.Errorf("SurgeClamped must name the limit as an estimate: %q", clamped)
	}
	if containsLine([]string{clamped}, "Karpenter's provisionable capacity") {
		t.Errorf("SurgeClamped must not report the limit as capacity Karpenter has: %q", clamped)
	}
	// The shortfall is inside the band, so no divergence warning.
	for _, e := range evs {
		if strings.Contains(e, reasonSurgeClampBandExceeded) {
			t.Errorf("shortfall within band must not warn: %q", e)
		}
	}
	for _, l := range lines {
		if strings.Contains(l, "surge placeholder created") && strings.Contains(l, "bandExceeded") {
			t.Errorf("in-band clamp line must not mention bandExceeded: %q", l)
		}
	}

	// The placeholder was actually sized to the clamped value.
	var ph corev1.Pod
	if err := r.Get(context.Background(), phKey(), &ph); err != nil {
		t.Fatalf("get placeholder: %v", err)
	}
	if got := ph.Spec.Containers[0].Resources.Requests.Memory(); got.Cmp(resource.MustParse("13488Mi")) != 0 {
		t.Errorf("placeholder memory: got %s, want 13488Mi", got.String())
	}
}

// A node with headroom below the estimate: the clamp does not fire, the line
// stays exactly the #223 line, and only RotationStarted is emitted.
func TestPlaceholderNotClampedWhenDrainFits(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "3770m", "memory", "14570Mi"))
	pool := withTGP(testNodePool(nil))
	workload := workloadPod("app", candNode, "1200m", "12600Mi")
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi"))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false), workload, ds)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	if !containsLine(lines, "surge placeholder created", "12600Mi") {
		t.Errorf("missing placeholder line; lines = %v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "surge placeholder created") && strings.Contains(l, "clamped") {
			t.Errorf("common path must not mention the clamp: %q", l)
		}
	}
	for _, e := range drain(rec) {
		if strings.Contains(e, reasonSurgeClamped) {
			t.Errorf("no SurgeClamped Event on the common path, got %q", e)
		}
	}
}

// An unregistered candidate (empty status.allocatable) must never clamp toward
// zero — it falls back to the full reschedulable drain (issue #224).
func TestPlaceholderNotClampedWhenAllocatableEmpty(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode)) // no ncAllocatable
	pool := withTGP(testNodePool(nil))
	workload := workloadPod("app", candNode, "1200m", "13600Mi")
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi"))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false), workload, ds)
	r.Events = events.NewFakeRecorder(16)

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	for _, l := range lines {
		if strings.Contains(l, "surge placeholder created") && strings.Contains(l, "clamped") {
			t.Errorf("empty allocatable must not clamp: %q", l)
		}
	}
	var ph corev1.Pod
	if err := r.Get(context.Background(), phKey(), &ph); err != nil {
		t.Fatalf("get placeholder: %v", err)
	}
	if got := ph.Spec.Containers[0].Resources.Requests.Memory(); got.Cmp(resource.MustParse("13600Mi")) != 0 {
		t.Errorf("placeholder memory must be the full drain: got %s, want 13600Mi", got.String())
	}
}

// The refusal is arithmetic about ONE ceiling on ONE resource: the candidate's
// own CACHED NodeClaim.status.allocatable minus the DaemonSet overhead observed
// running on it, for the resource Clamp refused on. Under that ceiling no clamp
// value reserves any positive share of THAT resource — that much is exact, and
// no more: the drain's other resources may have positive ceilings and be
// reservable, since Clamp returns on the first refusal it finds. Everything past
// it is not exact either: whether the
// full-drain placeholder finds a host is decided by kube-scheduler against real
// nodes, whose Node.status.allocatable can EXCEED the cached per-type estimate.
// That gap is the band the clamp itself is built on, so a node of the same type
// carrying the same DaemonSets can still have room — a third way out that is
// neither "a larger instance type" nor "less applicable overhead".
//
// So the Event must not enumerate the ways out and then treat the rollback as
// the complement of that set. It names them as examples, says the measurement
// does not decide schedulability, and reaches the rollback only as an outcome
// (issue #328). Asserting otherwise is the same overclaim in a smaller box.
func TestClampRefusedEventDoesNotDecideSchedulability(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "3770m", "memory", "1000Mi"))
	pool := withTGP(testNodePool(nil))
	workload := workloadPod("app", candNode, "1200m", "500Mi")
	ds := asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1500Mi"))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false), workload, ds)
	r.Events = rec

	if _, err := r.reconcileNodePool(context.Background(), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	var evs []string
	for _, e := range drain(rec) {
		if strings.Contains(e, reasonSurgeClampRefused) {
			evs = append(evs, e)
		}
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 SurgeClampRefused Event, got %d: %v", len(evs), evs)
	}
	// What was measured, and on what — not a verdict about the NodePool.
	if !containsLine(evs, "this candidate's own values", "observed on it", "memory") {
		t.Errorf("the Event must scope the refusal to the candidate's observed values: %v", evs)
	}
	// The one thing the ceiling really settles — and it is PER RESOURCE. Clamp
	// returns Refused on the first resource whose ceiling is non-positive while
	// the drain demands it; the other resources may have positive ceilings and be
	// reservable. Saying "any of the drain" widens a per-resource fact into a
	// whole-drain one.
	if !containsLine(evs, "no clamp value reserves any positive amount of memory") {
		t.Errorf("the Event must state what the ceiling settles, scoped to the resource: %v", evs)
	}
	// And the thing it does not.
	if !containsLine(evs, "does not decide whether", "schedulable") {
		t.Errorf("the Event must disclaim deciding schedulability: %v", evs)
	}
	// The band escape — a node of the SAME type with the SAME DaemonSets, whose
	// real allocatable exceeds the cached estimate. Its absence is what made the
	// earlier two-item list read as exhaustive.
	if !containsLine(evs, "more allocatable than", "cached estimate") {
		t.Errorf("the Event must name the band escape, not only type and overhead: %v", evs)
	}
	// Named as examples, with the controller disclaiming which apply.
	if !containsLine(evs, "larger instance type", "less applicable DaemonSet overhead",
		"not something this controller can determine", "examples rather than the full set") {
		t.Errorf("the ways out must be examples, not an enumerated set: %v", evs)
	}
	// The rollback is an outcome, never the complement of the list above.
	if !containsLine(evs, "If nothing can take it", "rolls back") {
		t.Errorf("the rollback must be stated as an outcome: %v", evs)
	}
	for _, absolute := range []string{
		"the surge placeholder cannot be clamped and the rotation will roll back",
		"the rotation will roll back",
		"If neither is",
		"cannot be induced on a node like this one",
		"reserves any of the drain",
	} {
		if containsLine(evs, absolute) {
			t.Errorf("the Event must not assert %q — the measurement does not reach it: %v", absolute, evs)
		}
	}
}
