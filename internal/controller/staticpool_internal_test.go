package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// withReplicas turns a NodePool into a Karpenter static capacity pool: it keeps a
// fixed node count instead of provisioning for pending pods (issue #302). The
// count itself is immaterial to the gate — any non-nil spec.replicas is static.
func withReplicas(p *karpv1.NodePool) *karpv1.NodePool {
	p.Spec.Replicas = new(int64(2))
	if p.UID == "" {
		p.UID = "pool-uid-1"
	}
	return p
}

// countEvents returns how many of the drained events name reason.
func countEvents(evs []string, reason string) int {
	n := 0
	for _, e := range evs {
		if strings.Contains(e, reason) {
			n++
		}
	}
	return n
}

// A static NodePool can never complete a surge: the placeholder's node affinity
// pins karpenter.sh/nodepool to the candidate's own pool, and Karpenter's
// provisioner does not consider static pools for pending pods — so the
// placeholder is unschedulable and every attempt burns a readyTimeout. The
// controller must refuse to start instead of rediscovering that once per window,
// and must say so where an operator sees it (issue #302).
func TestStaticNodePoolBlocksRotationStart(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	node := testK8sNode(candNode, true, nil, false)
	pool := withReplicas(withTGP(testNodePool(nil)))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, node)
	r.Events = rec

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}

	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("a static NodePool must not anchor a rotation, got anchor %q", got)
	}
	if placeholderExists(t, r) {
		t.Error("a static NodePool must not create a surge placeholder")
	}
	n := getNodeObj(t, r, candNode)
	if n.Spec.Unschedulable || n.Annotations[annotations.SurgeFor] != "" {
		t.Errorf("the candidate node must be left untouched, got %+v", n.Annotations)
	}
	if !containsLine(lines, "static NodePool", "nodepool") {
		t.Errorf("the block must be logged; lines = %v", lines)
	}
	evs := drain(rec)
	if countEvents(evs, reasonStaticNodePool) != 1 {
		t.Fatalf("want exactly one %s Event, got %v", reasonStaticNodePool, evs)
	}
	if !strings.Contains(evs[0], "Warning") {
		t.Errorf("the Event must be a Warning, got %q", evs[0])
	}
}

// The reconcile re-evaluates every longRequeue, so an undeduplicated Event would
// repeat for as long as the pool stays static. It warns on the transition into
// the state and stays silent after — the same discipline as the other emitters.
func TestStaticNodePoolWarnsOncePerTransition(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withReplicas(withTGP(testNodePool(nil)))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))
	r.Events = rec

	step(t, r, pool)
	step(t, r, pool)

	if got := countEvents(drain(rec), reasonStaticNodePool); got != 1 {
		t.Errorf("two passes over the same static pool emitted %d Events, want 1", got)
	}
}

// Karpenter rejects a transition between static and dynamic on an existing
// NodePool (a CEL rule on spec.replicas), so the only way out of the condition is
// to delete the pool and create a dynamic one. A pool deleted and recreated under
// the same name is a different object that must get its own warning — and the
// recreate may never surface a NotFound reconcile for Forget to act on, so the
// dedup cannot be keyed on the name alone.
func TestStaticNodePoolWarnsAgainForARecreatedPool(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withReplicas(withTGP(testNodePool(nil)))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))
	r.Events = rec

	step(t, r, pool)
	drain(rec)

	// Deleted and recreated under the same name, with no reconcile in between to
	// observe the NotFound.
	if err := r.Delete(context.Background(), getPool(t, r)); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	recreated := withReplicas(withTGP(testNodePool(nil)))
	recreated.UID = types.UID("pool-uid-2")
	if err := r.Create(context.Background(), recreated); err != nil {
		t.Fatalf("recreate pool: %v", err)
	}
	step(t, r, recreated)

	if got := countEvents(drain(rec), reasonStaticNodePool); got != 1 {
		t.Errorf("a recreated static NodePool emitted %d Events, want 1", got)
	}
}

// The gate blocks only the START of a rotation. An anchor written before this
// gate existed — by an earlier controller version, since Karpenter does not allow
// a pool to become static while it is running — must still be driven to a
// terminal outcome with its markers cleaned up, not stranded with a cordoned node
// and an orphaned placeholder.
func TestStaticNodePoolDrivesAnInFlightRotationToCompletion(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncFinalizer(),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-2*time.Minute))))
	surgeClaim := testClaim("nc-new", time.Hour, ncNode(surgeNode))
	pool := withReplicas(withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"})))
	oldNode := testK8sNode(candNode, true, map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true", annotations.DoNotDisruptOwned: "true", annotations.SurgeFor: "nc-old",
	}, true)
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec, pool, cand, surgeClaim, oldNode,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))
	r.Events = events.NewFakeRecorder(16)

	// The surge is already Ready: this pass drains.
	step(t, r, pool)

	if got := getPool(t, r).Annotations[annotations.ActiveRotationState]; got != annotations.StateDraining {
		t.Fatalf("active-rotation-state: got %q, want draining", got)
	}
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.DeletionTimestamp == nil {
		t.Fatal("the old NodeClaim must be deleted for an already-static pool carrying a pre-gate anchor")
	}

	// Karpenter finalizes the drained claim; the next pass completes the rotation.
	c.Finalizers = nil
	if err := r.Update(context.Background(), c); err != nil {
		t.Fatalf("finalize claim: %v", err)
	}
	step(t, r, getPool(t, r))

	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotation] != "" || p.Annotations[annotations.ActiveRotationState] != "" {
		t.Errorf("the anchor must be released at completion, got %v", p.Annotations)
	}
	if p.Annotations[annotations.LastRotationAt] == "" {
		t.Error("last-rotation-at must be stamped on success")
	}
	if rec.success != 1 {
		t.Errorf("success counted %d times, want 1", rec.success)
	}
	if placeholderExists(t, r) {
		t.Error("the placeholder must be cleaned up at completion")
	}
	if n := getNodeObj(t, r, surgeNode); n.Annotations[annotations.SurgeFor] != "" {
		t.Errorf("the surge node must be unfrozen, got %v", n.Annotations)
	}
}

// advanceFailed treats a re-entry as a NEW attempt, and it runs above the step-1a
// gate because the anchor sends the reconcile into advance() first. On a static
// pool it must not hand the claim back to pending: that is the doomed placeholder
// attempt this gate exists to prevent, repeated once per escalated backoff.
func TestStaticNodePoolBlocksTheFailedRetry(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode), ncAnn(
		annotations.State, annotations.StateFailed,
		annotations.FailedAt, rfc(testNow.Add(-2*time.Hour)),
		annotations.RetryCount, "1"))
	pool := withReplicas(withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateFailed,
	})))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))
	r.Events = events.NewFakeRecorder(16)

	step(t, r, pool)

	got := getClaimOrNil(t, r, "nc-old")
	if got == nil {
		t.Fatal("the failed claim must still exist")
	}
	if got.Annotations[annotations.State] != annotations.StateFailed {
		t.Errorf("a failed claim on a static pool must not re-enter pending, got %v", got.Annotations)
	}
	if placeholderExists(t, r) {
		t.Error("no placeholder may be created for the retry")
	}
	// The repair branch releases the anchor, so the pool falls through to the
	// step-1a gate on the next pass instead of retrying forever.
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("the anchor must be released, got %q", got)
	}
	if got := getPool(t, r).Annotations[annotations.LastFailureAt]; got == "" {
		t.Error("the failure pause anchor must be preserved by the repair branch")
	}
}
