package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Issue #305: a surge that was absorbed by pre-existing capacity reserves
// aggregate capacity on a host already running other Pods, so its short
// surge_wait does not bound the time until the evicted Pods are running — a
// 4-second wait can hide a node launch that has simply moved behind the drain.
// The controller knows which path it took and never said so, leaving the two
// indistinguishable in the log. These tests pin that it now says so on both
// lines that report the surge: at the transition, and at completion.

// The transition line carries the qualifier next to the number it qualifies: a
// surgeWait is only interpretable alongside the path that produced it.
func TestSurgeReadyNamesTheProvisionedPath(t *testing.T) {
	startedAt := testNow.Add(-3 * time.Minute)
	node, cand, pool := pendingRotation(startedAt)
	cand.Finalizers = []string{"karpenter.sh/termination"}
	// The surge host's NodeClaim was created inside this attempt → Karpenter
	// launched it for the placeholder.
	surgeClaim := testClaim("nc-surge", time.Minute, ncNode(surgeNode))
	r := newReconciler(t, testNow, nil, pool, cand, node, surgeClaim,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "surge node ready", `"surgePath"="`+surge.PathProvisioned+`"`) {
		t.Errorf("the surge-ready line must name the provisioning path; lines = %v", lines)
	}
	if got := getPool(t, r).Annotations[annotations.SurgePath]; got != surge.PathProvisioned {
		t.Errorf("surge-path must be carried on the anchor for the completion line: got %q, want %q", got, surge.PathProvisioned)
	}
}

// The path this issue is about: the placeholder bin-packed onto capacity that
// already existed, so the reservation is aggregate and the wait is short.
func TestSurgeReadyNamesTheAbsorbedPath(t *testing.T) {
	startedAt := testNow.Add(-3 * time.Minute)
	node, cand, pool := pendingRotation(startedAt)
	cand.Finalizers = []string{"karpenter.sh/termination"}
	// The host's NodeClaim predates the attempt → pre-existing spare capacity.
	surgeClaim := testClaim("nc-surge", 10*time.Minute, ncNode(surgeNode))
	r := newReconciler(t, testNow, nil, pool, cand, node, surgeClaim,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "surge node ready", `"surgePath"="`+surge.PathAbsorbed+`"`) {
		t.Errorf("the surge-ready line must name the absorb path; lines = %v", lines)
	}
	if got := getPool(t, r).Annotations[annotations.SurgePath]; got != surge.PathAbsorbed {
		t.Errorf("surge-path must be carried on the anchor: got %q, want %q", got, surge.PathAbsorbed)
	}
}

// A host whose NodeClaim cannot be resolved yields no answer rather than a
// guessed one. Omission is the same choice the completion line already makes for
// an ambiguous surge node, and it keeps the field's presence meaningful.
func TestSurgeReadyOmitsThePathWhenTheHostClaimIsUnresolvable(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-3 * time.Minute))
	cand.Finalizers = []string{"karpenter.sh/termination"}
	// No NodeClaim for the surge host in the store.
	r := newReconciler(t, testNow, nil, pool, cand, node,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "surge node ready") {
		t.Fatalf("the transition must still be logged; lines = %v", lines)
	}
	if containsLine(lines, "surge node ready", "surgePath") {
		t.Errorf("an unresolvable host must omit surgePath rather than guess; lines = %v", lines)
	}
	if got := getPool(t, r).Annotations[annotations.SurgePath]; got != "" {
		t.Errorf("no surge-path may be stamped when the path is unknown: got %q", got)
	}
}

// The old NodeClaim — and with it started-at — is deleted at the transition, so
// completion cannot re-derive the path and reads it from the anchor, the way it
// already reads surge-wait (#228).
func TestRotationCompleteReportsTheCarriedSurgePath(t *testing.T) {
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-4 * time.Minute)),
		annotations.SurgeWait:           (90 * time.Second).String(),
		annotations.SurgePath:           surge.PathAbsorbed,
	}))
	surgeHost := testK8sNode(surgeNode, true, map[string]string{annotations.SurgeFor: "nc-old"}, false)
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, surgeHost) // nc-old absent → finalized away
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "rotation complete", `"surgePath"="`+surge.PathAbsorbed+`"`) {
		t.Errorf("the completion line must report the carried surge path; lines = %v", lines)
	}
	evs := drain(rec)
	if len(evs) != 1 {
		t.Fatalf("want 1 RotationCompleted Event, got %d: %v", len(evs), evs)
	}
	if !containsLine(evs, surge.PathAbsorbed) {
		t.Errorf("the RotationCompleted Event must name the surge path — an operator without log access sees only this: %v", evs)
	}
	if got := getPool(t, r).Annotations[annotations.SurgePath]; got != "" {
		t.Errorf("surge-path must be cleared with the anchor on completion: got %q", got)
	}
}

// The surge-less forceful fallback has no surge phase and therefore no path;
// completion must not invent one, exactly as it omits surge-wait there.
func TestRotationCompleteOmitsAnAbsentSurgePath(t *testing.T) {
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-4 * time.Minute)),
	}))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "rotation complete") {
		t.Fatalf("completion must still be logged; lines = %v", lines)
	}
	if containsLine(lines, "rotation complete", "surgePath") {
		t.Errorf("completion must omit surgePath when none was carried; lines = %v", lines)
	}
	if evs := drain(rec); len(evs) != 1 || containsLine(evs, "surge path") {
		t.Errorf("the Event must not name a path that was never observed: %v", evs)
	}
}
