package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/scheme"
)

// A transition line claims to fire once per transition. That holds only if it is
// emitted AFTER the durable write that makes the transition real: a reconcile
// whose patch fails is retried from the same phase, and an emission placed before
// the write fires again on every retry. The state machine's own writes are
// idempotent by design, so the retry itself is expected — the log must not
// multiply with it.
//
// These tests drive a fake client whose first Patch of the NodePool fails, which
// is exactly what a resourceVersion conflict or a transient API error looks like
// to the reconciler.

// patchPool/patchClaim write through Update (inside RetryOnConflict), so the
// seam is Update, not Patch. Both interceptors below target the write by its
// MEANING rather than by call order: advancePending issues several NodeClaim
// updates per pass (assert-pending, surge-claim) before the one that records
// draining, and only that last one makes the transition durable.

// failClearingPoolUpdate fails the NodePool update that releases the anchor.
func failClearingPoolUpdate(remaining *int) interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if p, ok := obj.(*karpv1.NodePool); ok && *remaining > 0 && p.Annotations[annotations.ActiveRotation] == "" {
				*remaining--
				return errors.New("simulated transient API error")
			}
			return c.Update(ctx, obj, opts...)
		},
	}
}

// failDrainingClaimUpdate fails the NodeClaim update that records draining.
func failDrainingClaimUpdate(remaining *int) interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if cl, ok := obj.(*karpv1.NodeClaim); ok && *remaining > 0 && cl.Annotations[annotations.State] == annotations.StateDraining {
				*remaining--
				return errors.New("simulated transient API error")
			}
			return c.Update(ctx, obj, opts...)
		},
	}
}

// newFlakyReconciler mirrors newReconciler but installs a client interceptor.
func newFlakyReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *RotationReconciler {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	return &RotationReconciler{
		Client:            cl,
		Namespace:         testNS,
		PlaceholderImage:  "registry.k8s.io/pause:3.10",
		PriorityClassName: "noderotation-placeholder",
		Recorder:          &fakeRecorder{},
		Clock:             func() time.Time { return testNow },
	}
}

// countLines returns how many captured lines contain every substring.
func countLines(lines []string, subs ...string) int {
	n := 0
	for _, l := range lines {
		all := true
		for _, s := range subs {
			if !strings.Contains(l, s) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// The claim's pending → draining write is what makes the transition durable. When
// it fails, the next pass re-enters advancePending with the placeholder still
// Running, so "surge node ready" must not have been logged yet.
func TestSurgeReadyLineIsNotRepeatedWhenTheClaimWriteFails(t *testing.T) {
	node, cand, pool := pendingRotation(testNow.Add(-3 * time.Minute))
	cand.Finalizers = []string{"karpenter.sh/termination"}
	remaining := 1
	r := newFlakyReconciler(t, failDrainingClaimUpdate(&remaining), pool, cand, node,
		testK8sNode(surgeNode, true, nil, false), placeholderPod(surgeNode, corev1.PodRunning))

	var all []string
	// Pass 1 fails on the claim patch; pass 2 (and 3) re-drive the same phase.
	for i := range 3 {
		var lines []string
		ctx := log.IntoContext(context.Background(), captureLogger(&lines))
		p := getPool(t, r)
		_, err := r.reconcileNodePool(ctx, p, testPolicy(), mustSchedule(t))
		if i == 0 && err == nil {
			t.Fatal("pass 1 must surface the simulated patch error")
		}
		all = append(all, lines...)
	}

	if got := countLines(all, "surge node ready"); got != 1 {
		t.Errorf(`"surge node ready" logged %d times across a failed-then-retried transition, want exactly 1`, got)
	}
	if got := countLines(all, "drain started"); got != 1 {
		t.Errorf(`"drain started" logged %d times, want exactly 1`, got)
	}
}

// completeOrAbort emits the completion line and Event; the pool patch that clears
// the anchor is what ends the rotation. A failed patch re-enters completeOrAbort
// with the anchor still present.
func TestRotationCompleteIsNotRepeatedWhenThePoolWriteFails(t *testing.T) {
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
		annotations.DrainingAt:          rfc(testNow.Add(-4 * time.Minute)),
	}))
	remaining := 1
	rec := events.NewFakeRecorder(16)
	r := newFlakyReconciler(t, failClearingPoolUpdate(&remaining), pool) // nc-old already finalized away
	r.Events = rec

	var all []string
	for i := range 3 {
		var lines []string
		ctx := log.IntoContext(context.Background(), captureLogger(&lines))
		p := getPool(t, r)
		_, err := r.reconcileNodePool(ctx, p, testPolicy(), mustSchedule(t))
		if i == 0 && err == nil {
			t.Fatal("pass 1 must surface the simulated patch error")
		}
		all = append(all, lines...)
	}

	if got := countLines(all, "rotation complete"); got != 1 {
		t.Errorf(`"rotation complete" logged %d times across a failed-then-retried completion, want exactly 1`, got)
	}
	if evs := drain(rec); len(evs) != 1 {
		t.Errorf("want exactly 1 RotationCompleted Event, got %d: %v", len(evs), evs)
	}
}

// createPlaceholder's create is idempotent, but the line must describe what this
// pass actually did: a cached read can report the placeholder absent moments after
// it was created, and an AlreadyExists create must not announce a creation.
func TestPlaceholderCreatedIsNotLoggedWhenItAlreadyExists(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow)))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	existing := placeholderPod("", corev1.PodPending) // already in the API server
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false), existing)

	// Call createPlaceholder directly: advancePending reaches it only via a ph == nil
	// read, which is precisely what a lagging cache reports for a placeholder that
	// already exists. The create then comes back AlreadyExists.
	var lines []string
	ctx := log.IntoContext(context.Background(), captureLogger(&lines))
	if err := r.createPlaceholder(ctx, pool, cand, r.resolve(pool, testPolicy(), mustSchedule(t))); err != nil {
		t.Fatalf("createPlaceholder must swallow AlreadyExists: %v", err)
	}
	if got := countLines(lines, "surge placeholder created"); got != 0 {
		t.Errorf("must not announce a creation it did not perform; lines = %v", lines)
	}
}

// The policy-conflict path reaps an in-flight rotation but deliberately keeps the
// pool's warn state (it dedups the conflict itself), so the reap must drop the
// reaped claim's unschedulable-placeholder entry explicitly. Otherwise repeated
// conflict/recover cycles with fresh claim names grow the map without bound.
func TestReapUngovernedRotationClearsThePlaceholderPendingDedup(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StatePending, annotations.StartedAt, rfc(testNow)))
	pool := withTGP(testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	ctx := context.Background()
	r.warn().EmitPlaceholderPending(ctx, pool.Name, cand, unschedulablePlaceholder("Unschedulable", "no capacity"))
	if _, ok := r.warn().state[pool.Name].phPending["nc-old"]; !ok {
		t.Fatal("precondition: the dedup entry must exist before the reap")
	}

	if err := r.reapUngovernedRotation(ctx, pool); err != nil {
		t.Fatalf("reapUngovernedRotation: %v", err)
	}

	if _, ok := r.warn().state[pool.Name].phPending["nc-old"]; ok {
		t.Error("the reaped claim's dedup entry must not survive the rollback")
	}
}
