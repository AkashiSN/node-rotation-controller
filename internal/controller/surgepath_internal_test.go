package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

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

// failGetOfClaim fails every Get of the named NodeClaim — the second of the two
// reads the path lookup makes, and the only one in this pass that belongs to it
// alone (the candidate's surge-claim is seeded, so nothing else resolves a claim
// by that name).
func failGetOfClaim(name string) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*karpv1.NodeClaim); ok && key.Name == name {
				return errors.New("simulated transient API error")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// The path is an observation, so failing to make it must cost the field and
// nothing else. The lookup sits after surge_ready and before the durable
// draining write, where returning an error aborts the transition — and since
// readyTimeout is evaluated ahead of surge_ready on the next pass, one transient
// error near the deadline would roll back a surge that was already Ready.
func TestSurgeReadyTransitionSurvivesAFailingPathLookup(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-3 * time.Minute))
	cand.Finalizers = []string{"karpenter.sh/termination"}
	cand.Annotations[annotations.SurgeClaim] = "nc-surge"
	surgeClaim := testClaim("nc-surge", time.Minute, ncNode(surgeNode))
	r := newFlakyReconciler(t, nil, failGetOfClaim("nc-surge"), pool, cand, node, surgeClaim,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("a failed path lookup must not fail the reconcile: %v", err)
	}
	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotationState] != annotations.StateDraining {
		t.Errorf("the transition must still reach draining: got %q", p.Annotations[annotations.ActiveRotationState])
	}
	if p.Annotations[annotations.SurgePath] != "" {
		t.Errorf("an unresolved path must stamp nothing: got %q", p.Annotations[annotations.SurgePath])
	}
	if c := getClaimOrNil(t, r, "nc-old"); c == nil || c.DeletionTimestamp == nil {
		t.Error("the old NodeClaim must still be deleted")
	}
	if !containsLine(lines, "surge node ready") {
		t.Errorf("the transition must still be logged; lines = %v", lines)
	}
}

// The value on the two lines must be the same value, and a retried transition is
// where a locally-recomputed one diverges. The anchor field is write-once at the
// first transition, so a path resolved only on a later pass is never persisted —
// and reporting that later resolution would announce on "surge node ready" a
// path that "rotation complete" and its Event then omit, breaking the contract
// this feature exists to provide. What is reported is what was persisted.
func TestSurgeReadyReportsThePersistedPathNotALaterResolution(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-3 * time.Minute))
	cand.Finalizers = []string{"karpenter.sh/termination"}
	remaining := 1
	// Pass 1: the surge host has no NodeClaim yet, so the path is unresolved and
	// the write-once block stamps draining-at and surge-wait without it. The
	// claim's state=draining update then fails, so the pass returns an error
	// before the line is emitted and the next pass re-enters pending.
	r := newFlakyReconciler(t, nil, failDrainingClaimUpdate(&remaining), pool, cand, node,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))

	var pass1 []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&pass1)), pool, testPolicy(), mustSchedule(t)); err == nil {
		t.Fatal("pass 1 must fail on the claim update, leaving the transition to be retried")
	}
	p := getPool(t, r)
	if p.Annotations[annotations.DrainingAt] == "" || p.Annotations[annotations.SurgePath] != "" {
		t.Fatalf("pass 1 must persist the transition without a path: %+v", p.Annotations)
	}

	// Between the passes the surge host's NodeClaim becomes visible, so pass 2
	// resolves a path the write-once block will not stamp.
	if err := r.Create(context.Background(), testClaim("nc-surge", time.Minute, ncNode(surgeNode))); err != nil {
		t.Fatalf("create surge claim: %v", err)
	}

	var pass2 []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&pass2)), getPool(t, r), testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if !containsLine(pass2, "surge node ready") {
		t.Fatalf("pass 2 must complete the transition; lines = %v", pass2)
	}
	if containsLine(pass2, "surge node ready", "surgePath") {
		t.Errorf("the line must report the persisted path, not a resolution the anchor never took; lines = %v", pass2)
	}
	if got := getPool(t, r).Annotations[annotations.SurgePath]; got != "" {
		t.Errorf("the write-once anchor must stay unstamped: got %q", got)
	}
}
