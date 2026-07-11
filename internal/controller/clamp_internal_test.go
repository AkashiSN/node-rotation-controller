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

// DaemonSet overhead at or above the NodeClaim's allocatable leaves no room for
// any placeholder, so no clamp value can induce a node. Clamping to zero would
// bind a zero-request Pod anywhere and satisfy surge_ready with nothing
// reserved — a silent break-before-make. The clamp is refused: the placeholder
// keeps the full drain, stays unschedulable, and the rotation rolls back
// (issue #224).
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
