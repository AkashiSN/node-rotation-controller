package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
