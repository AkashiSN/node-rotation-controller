package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Issue #221: the rotation state machine used to emit no logs and no Events as it
// moved through its phases — a 90-minute window that rotated 7 nodes and failed on
// an 8th produced zero lines after startup. These tests pin one Info line per
// transition, and pin the two LEVEL-TRIGGERED lines (idle reason, unschedulable
// placeholder) to fire on transition only: advancePending re-enters every
// shortRequeue, so an undeduplicated line would print ~30 times across a single
// 15-minute readyTimeout stall.

// unschedulablePlaceholder builds a Pending placeholder carrying the
// PodScheduled=False condition the scheduler writes when nothing fits — the
// signal the controller must surface rather than stall silently on.
func unschedulablePlaceholder(reason, msg string) *corev1.Pod {
	p := placeholderPod("", corev1.PodPending)
	p.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  reason,
		Message: msg,
	}}
	return p
}

// pendingRotation seeds a pool anchored on a pending nc-old stamped started-at
// at startedAt, plus its Ready candidate Node.
func pendingRotation(startedAt time.Time) (*corev1.Node, *karpv1.NodeClaim, *karpv1.NodePool) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(startedAt)))
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation: "nc-old",
	}))
	return testK8sNode(candNode, true, nil, false), cand, pool
}

// workloadPod is a plain reschedulable Pod on nodeName — sized into the placeholder.
func workloadPod(name, nodeName, cpu, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{
			Name: "c",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// daemonSetPod is excluded from the placeholder's sizing — Karpenter adds
// DaemonSet overhead to every node it provisions (spec §3.3).
func daemonSetPod(name, nodeName string) *corev1.Pod {
	ctl := true
	p := workloadPod(name, nodeName, "300m", "1082Mi")
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds", Controller: &ctl}}
	return p
}

// infoOnlyLogger records lines at verbosity 0 — V(1) debug lines are dropped, as
// they are under the controller's default -zap-log-level.
func infoOnlyLogger(lines *[]string) logr.Logger {
	return funcr.New(func(_, args string) { *lines = append(*lines, args) }, funcr.Options{})
}

func TestStartGatesReportsTheBlockingGate(t *testing.T) {
	r := newReconciler(t, testNow, nil)
	res := r.resolve(withTGP(testNodePool(nil)), testPolicy(), mustSchedule(t))

	t.Run("open", func(t *testing.T) {
		pool := testNodePool(nil)
		ok, reason := r.startGates(pool, res, testNow)
		if !ok || reason != "" {
			t.Errorf("open gates: got (%v, %q), want (true, \"\")", ok, reason)
		}
	})

	t.Run("out of window", func(t *testing.T) {
		pool := testNodePool(nil)
		ok, reason := r.startGates(pool, res, testNowOut)
		if ok || reason != gateOutOfWindow {
			t.Errorf("got (%v, %q), want (false, %q)", ok, reason, gateOutOfWindow)
		}
	})

	t.Run("frozen", func(t *testing.T) {
		pool := testNodePool(map[string]string{annotations.Freeze: rfc(testNow.Add(time.Hour))})
		ok, reason := r.startGates(pool, res, testNow)
		if ok || reason != gateFrozen {
			t.Errorf("got (%v, %q), want (false, %q)", ok, reason, gateFrozen)
		}
	})

	t.Run("post-success cooldown", func(t *testing.T) {
		pool := testNodePool(map[string]string{annotations.LastRotationAt: rfc(testNow.Add(-time.Minute))})
		ok, reason := r.startGates(pool, res, testNow)
		if ok || reason != gateCooldownAfterSuccess {
			t.Errorf("got (%v, %q), want (false, %q)", ok, reason, gateCooldownAfterSuccess)
		}
	})

	t.Run("post-failure pause", func(t *testing.T) {
		pool := testNodePool(map[string]string{annotations.LastFailureAt: rfc(testNow.Add(-time.Minute))})
		ok, reason := r.startGates(pool, res, testNow)
		if ok || reason != gateCooldownAfterFailure {
			t.Errorf("got (%v, %q), want (false, %q)", ok, reason, gateCooldownAfterFailure)
		}
	})
}

// A blocked start gate names the gate, once. The reconcile self-requeues every
// longRequeue, so re-logging each pass would print this line every minute for as
// long as the pool idles.
func TestNoCandidateLogsTheBlockingGateOnceUntilItChanges(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(map[string]string{
		annotations.LastRotationAt: rfc(testNow.Add(-time.Minute)), // inside the 10m cooldown
	}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	var pass1 []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&pass1)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if !containsLine(pass1, "no rotation candidate", gateCooldownAfterSuccess) {
		t.Errorf("pass 1 must name the blocking gate; lines = %v", pass1)
	}

	var pass2 []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&pass2)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if containsLine(pass2, "no rotation candidate") {
		t.Errorf("pass 2 must stay silent while the reason is unchanged; lines = %v", pass2)
	}
}

// With the gates open and no claim past its trigger, the line reports the census
// so "nothing is old enough" is distinguishable from "everything is in backoff".
func TestNoCandidateLogsTheCensusWhenNothingIsTriggered(t *testing.T) {
	young := testClaim("nc-young", time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, nil, pool, young, testK8sNode(candNode, true, nil, false))

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "no rotation candidate", `"notTriggered"=1`, `"claims"=1`, `"inBackoff"=0`) {
		t.Errorf("must report the census; lines = %v", lines)
	}
}

// The highest-value line in the issue: a placeholder that cannot be scheduled
// stalls for the full readyTimeout. Surface the scheduler's own reason/message at
// once, and only re-log when the message changes.
func TestPlaceholderPendingLogsSchedulingReasonAndDedups(t *testing.T) {
	node, cand, pool := pendingRotation(testNow)
	const msg = `no instance type has enough resources, requirements= instance-size In [xlarge]`
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, node, unschedulablePlaceholder("Unschedulable", msg))
	r.Events = rec

	var pass1 []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&pass1)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if !containsLine(pass1, "surge placeholder is not schedulable", "Unschedulable", "no instance type has enough resources") {
		t.Errorf("pass 1 must surface the scheduler's reason and message; lines = %v", pass1)
	}
	if evs := drain(rec); len(evs) != 1 {
		t.Errorf("want exactly 1 Warning Event, got %d: %v", len(evs), evs)
	}

	var pass2 []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&pass2)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if containsLine(pass2, "surge placeholder is not schedulable") {
		t.Errorf("pass 2 must stay silent on an unchanged message; lines = %v", pass2)
	}
	if evs := drain(rec); len(evs) != 0 {
		t.Errorf("want 0 Events on unchanged repeat, got %d: %v", len(evs), evs)
	}
}

// Selecting a candidate and creating its placeholder are both one-shot: the
// anchor and the "placeholder absent" guard each admit exactly one pass.
// createPlaceholder's line is what would have prevented the issue's misdiagnosis:
// it states the computed requests AND the Pods excluded from them.
func TestCandidateSelectedAndPlaceholderCreatedAreLogged(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(nil))
	// One workload Pod (sized in) and one DaemonSet Pod (excluded) on the candidate.
	workload := workloadPod("app", candNode, "1200m", "13600Mi")
	ds := daemonSetPod("kube-proxy", candNode)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false), workload, ds)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	if !containsLine(lines, "rotation candidate selected", "nc-old") {
		t.Errorf("missing candidate-selected line; lines = %v", lines)
	}
	if !containsLine(lines, "surge placeholder created", surge.PlaceholderName("nc-old"), "13600Mi", "reschedulablePods", "daemonSetPods") {
		t.Errorf("placeholder line must state the computed requests and the excluded Pod counts; lines = %v", lines)
	}
	if evs := drain(rec); len(evs) != 1 {
		t.Errorf("want 1 Normal RotationStarted Event, got %d: %v", len(evs), evs)
	}
}

// surge ready → the old NodeClaim is deleted. Both the readiness and the drain
// start are one-shot: the next pass sees state=draining and takes another branch.
func TestSurgeReadyAndDrainStartAreLogged(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-3 * time.Minute))
	cand.Finalizers = []string{"karpenter.sh/termination"}
	r := newReconciler(t, testNow, nil, pool, cand, node,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "surge node ready", surgeNode, "surgeWait") {
		t.Errorf("missing surge-ready line with the surge node and surge_wait; lines = %v", lines)
	}
	if !containsLine(lines, "drain started", "nc-old") {
		t.Errorf("missing drain-start line; lines = %v", lines)
	}
}

// A readyTimeout rollback must say why, how many attempts have been made, and
// when the next attempt becomes possible — the issue's "failure" row.
//
// The reported values are pinned, not merely present: `backoffUntil` is the
// instant the claim becomes re-selectable, so it must equal
// failed-at + EscalatedBackoff(stored retry-count). Both `selection.failedPastBackoff`
// and `advanceFailed` read the retry count AFTER failPending's increment, so the
// line must report the incremented value — reporting the pre-increment count
// would name an instant half a backoff too early.
func TestFailureLogsRetryCountAndBackoffExpiry(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-20 * time.Minute)) // past the 15m readyTimeout
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, node)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	// First failure ⇒ stored retry-count 1 ⇒ EscalatedBackoff(1, 30m) = 30m << 0.
	wantUntil := rfc3339(testNow.Add(30 * time.Minute))
	if !containsLine(lines, "rotation attempt failed", `"reason"="readyTimeout"`, `"retryCount"=1`, `"backoffUntil"="`+wantUntil+`"`) {
		t.Errorf("failure line must carry reason, the INCREMENTED retry count and the exact backoff expiry %s; lines = %v", wantUntil, lines)
	}
	// The value the line reports must be the one the re-selection gate actually uses.
	got := getClaimOrNil(t, r, "nc-old")
	if got == nil {
		t.Fatal("candidate must survive the rollback")
	}
	stored := parseInt(got.Annotations[annotations.RetryCount])
	failedAt, ok := parseTime(got.Annotations[annotations.FailedAt])
	if !ok {
		t.Fatal("failed-at must be stamped")
	}
	if reopensAt := failedAt.Add(selection.EscalatedBackoff(stored, testPolicy().Surge.RetryBackoff.Duration)); rfc3339(reopensAt) != wantUntil {
		t.Errorf("logged backoffUntil %s != the instant the gate reopens %s", wantUntil, rfc3339(reopensAt))
	}
	if evs := drain(rec); len(evs) != 1 {
		t.Errorf("want 1 Warning RotationFailed Event, got %d: %v", len(evs), evs)
	}
}

// On the FIRST failure EscalatedBackoff(0) and EscalatedBackoff(1) coincide — the
// shift is max(retryCount−1, 0) — so `backoffUntil` alone cannot catch an
// off-by-one there. A second failure separates them: retry-count 2 doubles the
// base, and reporting the pre-increment 1 would name an instant 30m too early.
func TestFailureLogsEscalatedBackoffOnARepeatFailure(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-20 * time.Minute))
	ncAnn(annotations.RetryCount, "1")(cand) // one prior failure already recorded
	r := newReconciler(t, testNow, nil, pool, cand, node)

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	// retry-count 2 ⇒ EscalatedBackoff(2, 30m) = 30m << 1 = 1h.
	wantUntil := rfc3339(testNow.Add(time.Hour))
	if !containsLine(lines, "rotation attempt failed", `"retryCount"=2`, `"backoffUntil"="`+wantUntil+`"`) {
		t.Errorf("repeat failure must report the escalated backoff expiry %s; lines = %v", wantUntil, lines)
	}
}

// The old NodeClaim finalized away out of draining → success. The line closes the
// rotation with the drain duration the §4.2 histogram also records.
func TestRotationCompleteIsLogged(t *testing.T) {
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-4 * time.Minute)),
	}))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool) // nc-old absent → finalized away
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "rotation complete", "nc-old", "drain") {
		t.Errorf("missing rotation-complete line with the drain duration; lines = %v", lines)
	}
	if evs := drain(rec); len(evs) != 1 {
		t.Errorf("want 1 Normal RotationCompleted Event, got %d: %v", len(evs), evs)
	}
}

// The transition lines are Info, not V(1): a handful per rotation. Assert they
// survive a verbosity-0 logger, unlike the #100 heartbeat.
func TestTransitionLinesAreInfoNotDebug(t *testing.T) {
	young := testClaim("nc-young", time.Hour, ncNode(candNode))
	pool := withTGP(testNodePool(nil))
	r := newReconciler(t, testNow, nil, pool, young, testK8sNode(candNode, true, nil, false))

	var lines []string
	ctx := log.IntoContext(context.Background(), infoOnlyLogger(&lines))
	if _, err := r.reconcileNodePool(ctx, pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "no rotation candidate") {
		t.Errorf("transition line must be Info; lines = %v", lines)
	}
	if containsLine(lines, `"reconcile"`) {
		t.Errorf("the V(1) heartbeat must NOT appear at verbosity 0; lines = %v", lines)
	}
}
