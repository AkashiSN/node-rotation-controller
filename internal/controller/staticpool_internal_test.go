package controller

import (
	"context"
	"strings"
	"testing"
	"time"

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

// Dropping spec.replicas makes the pool dynamic again: the rotation starts, and a
// pool that becomes static once more warns again rather than staying silent on
// the stale dedup entry.
func TestStaticNodePoolClearsWhenReplicasRemoved(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withReplicas(withTGP(testNodePool(nil)))
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))
	r.Events = rec

	step(t, r, pool)
	drain(rec)

	dynamic := getPool(t, r)
	dynamic.Spec.Replicas = nil
	dynamic.Spec.Template.Spec.TerminationGracePeriod = pool.Spec.Template.Spec.TerminationGracePeriod
	if err := r.Update(context.Background(), dynamic); err != nil {
		t.Fatalf("update pool: %v", err)
	}
	step(t, r, dynamic)

	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Fatalf("a pool that is no longer static must rotate, anchor = %q", got)
	}

	// Static again, with the rotation it started meanwhile cleared away: the
	// warning is a new occurrence and must re-fire.
	again := getPool(t, r)
	again.Annotations = nil
	again.Spec.Replicas = pool.Spec.Replicas
	if err := r.Update(context.Background(), again); err != nil {
		t.Fatalf("update pool: %v", err)
	}
	step(t, r, again)

	if got := countEvents(drain(rec), reasonStaticNodePool); got != 1 {
		t.Errorf("a pool that becomes static again emitted %d Events, want 1", got)
	}
}

// The gate blocks only the START of a rotation. A pool that becomes static while
// one is in flight must still drive it to completion — the alternative is a
// cordoned node and a placeholder left behind with no path forward.
func TestStaticNodePoolDoesNotBlockInFlightRotation(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow.Add(-time.Minute))))
	pool := withReplicas(withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StatePending,
	})))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))
	r.Events = events.NewFakeRecorder(16)

	step(t, r, pool)

	if !placeholderExists(t, r) {
		t.Error("an in-flight rotation must keep advancing on a pool that turned static")
	}
	if got := getPool(t, r).Annotations[annotations.ActiveRotation]; got != "nc-old" {
		t.Errorf("the in-flight anchor must survive, got %q", got)
	}
}
