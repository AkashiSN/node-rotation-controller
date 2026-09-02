package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// anchoredPool builds the governed-then-orphaned NodePool: name=api, the given
// labels, with an in-flight rotation anchored on nc-old — the precondition for
// the §5.4 governance-loss reap (issue #141).
func anchoredPool(labels map[string]string) *karpv1.NodePool {
	return &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name:        testPoolName,
		Labels:      labels,
		Annotations: map[string]string{annotations.ActiveRotation: "nc-old"},
	}}
}

// assertReaped checks the controller drove the in-flight rotation to a clean
// terminal state when it ceased to govern the pool: placeholder Pod deleted,
// candidate node unfrozen (surge-for + controller-owned do-not-disrupt removed),
// and the active-rotation anchor cleared — nothing orphaned (issue #141).
func assertReaped(t *testing.T, r *RotationReconciler) {
	t.Helper()
	if placeholderExists(t, r) {
		t.Error("orphaned placeholder Pod should have been deleted on governance loss")
	}
	n := getNodeObj(t, r, surgeNode)
	if _, ok := n.Annotations[annotations.SurgeFor]; ok {
		t.Error("surge-for marker should have been removed on governance loss")
	}
	if _, ok := n.Annotations[karpv1.DoNotDisruptAnnotationKey]; ok {
		t.Error("controller-owned do-not-disrupt should have been removed on governance loss")
	}
	if p := getPool(t, r); p.Annotations[annotations.ActiveRotation] != "" {
		t.Errorf("active-rotation anchor should have been cleared, got %q", p.Annotations[annotations.ActiveRotation])
	}
}

// TestReconcileNoMatchReapsAnchoredRotation: a NodePool with an in-flight rotation
// loses governance (no RotationPolicy selects it any longer). Because no future
// reconcile will touch the now-ungoverned pool, the controller must reap the
// artifacts it owns — placeholder, freeze markers, anchor — before ceding the
// pool, rather than orphan them (issue #141).
func TestReconcileNoMatchReapsAnchoredRotation(t *testing.T) {
	pool := anchoredPool(map[string]string{"workload": "api"})
	other := rotPolicy("batch", map[string]string{"workload": "batch"}) // does not match the pool
	r := newReconciler(t, testNow, nil,
		pool, other,
		placeholderPod(surgeNode, corev1.PodRunning),
		frozenNode(),
	)

	reconcilePool(t, r, testPoolName)

	assertReaped(t, r)
}

// TestReconcileConflictReapsAnchoredRotation: a NodePool with an in-flight rotation
// becomes contested by an equal-specificity tie. The controller refuses to keep
// rotating it — still surfacing the conflict — and must additionally reap the
// in-flight rotation's artifacts rather than leave a do-not-disrupt marker and
// placeholder dangling on a pool it no longer advances (issue #141).
func TestReconcileConflictReapsAnchoredRotation(t *testing.T) {
	pool := anchoredPool(map[string]string{"workload": "api", "tier": "web"})
	a := rotPolicy("a", map[string]string{"workload": "api"})
	b := rotPolicy("b", map[string]string{"tier": "web"})
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec,
		pool, a, b,
		placeholderPod(surgeNode, corev1.PodRunning),
		frozenNode(),
	)

	reconcilePool(t, r, testPoolName)

	assertReaped(t, r)
	// Reaping the orphan must not paper over the misconfiguration: the pool is
	// still flagged as conflicted so an operator sees why it stopped rotating.
	if blocked, ok := rec.conflicts[testPoolName]; !ok || !blocked {
		t.Errorf("policy_conflict gauge = %v (present=%v), want blocked=true", blocked, ok)
	}
}

// --- the announcement must describe work that has been done (issue #315) ----

// reapEvents returns the GovernanceLost Events buffered in rec.
func reapEvents(rec *events.FakeRecorder) []string {
	var out []string
	for _, e := range drain(rec) {
		if strings.Contains(e, reasonGovernanceLost) {
			out = append(out, e)
		}
	}
	return out
}

// The reap is entered from the anchor the caller was handed, which is an
// informer-cache read: a pass that arrives after an earlier one already reaped
// still sees the anchor set and re-runs the whole rollback. The rollback is
// idempotent, so re-running it is harmless — but the Warning Event is the
// alerting surface, and one reaped rotation must raise exactly one of them.
// Only the write that clears the anchor can decide who announces (issue #315).
func TestGovernanceLostAnnouncesOncePerReapedRotation(t *testing.T) {
	pool := anchoredPool(map[string]string{"workload": "api"})
	rec := events.NewFakeRecorder(16)
	r := newReconciler(t, testNow, nil,
		pool.DeepCopy(),
		placeholderPod(surgeNode, corev1.PodRunning),
		frozenNode(),
	)
	r.Events = rec

	var lines []string
	ctx := log.IntoContext(context.Background(), captureLogger(&lines))

	// Two passes entered from the same cached anchor — what the informer serves
	// while the first pass's write propagates.
	if err := r.reapUngovernedRotation(ctx, pool.DeepCopy()); err != nil {
		t.Fatalf("first reap: %v", err)
	}
	if err := r.reapUngovernedRotation(ctx, pool.DeepCopy()); err != nil {
		t.Fatalf("second reap: %v", err)
	}

	assertReaped(t, r)
	if got := reapEvents(rec); len(got) != 1 {
		t.Errorf("want exactly 1 GovernanceLost Event for one reaped rotation, got %d: %v", len(got), got)
	}
	if got := countLines(lines, "reaped orphaned rotation artifacts"); got != 1 {
		t.Errorf("want exactly 1 reap log line, got %d; lines = %v", got, lines)
	}
}

// The Event's text is past tense and enumerates the rollback. Emitting it before
// the rollback lets a transient API error leave an operator told the placeholder
// is gone and the markers lifted while both are still in place. The pass that
// fails must announce nothing and leave the anchor for the retry (issue #315).
func TestGovernanceLostDoesNotAnnounceWhenCleanupFails(t *testing.T) {
	pool := anchoredPool(map[string]string{"workload": "api"})
	rec := events.NewFakeRecorder(16)
	r := newFlakyReconciler(t, nil, failFirstPodDelete(),
		pool.DeepCopy(),
		placeholderPod(surgeNode, corev1.PodRunning),
		frozenNode(),
	)
	r.Events = rec

	var lines []string
	ctx := log.IntoContext(context.Background(), captureLogger(&lines))

	if err := r.reapUngovernedRotation(ctx, pool.DeepCopy()); err == nil {
		t.Fatal("test did not exercise the cleanup failure: the pass must return the error")
	}
	if got := reapEvents(rec); len(got) != 0 {
		t.Errorf("a pass whose rollback failed must announce nothing, got %v", got)
	}
	if got := countLines(lines, "reaped orphaned rotation artifacts"); got != 0 {
		t.Errorf("a pass whose rollback failed must log no reap line; lines = %v", lines)
	}
	if p := getPool(t, r); p.Annotations[annotations.ActiveRotation] != "nc-old" {
		t.Fatalf("the anchor must survive a failed rollback so a retry re-enters it, got %q",
			p.Annotations[annotations.ActiveRotation])
	}

	// The retry completes the rollback and is the pass that announces it.
	if err := r.reapUngovernedRotation(ctx, pool.DeepCopy()); err != nil {
		t.Fatalf("retry reap: %v", err)
	}
	assertReaped(t, r)
	if got := reapEvents(rec); len(got) != 1 {
		t.Errorf("want exactly 1 GovernanceLost Event from the pass that reaped, got %d: %v", len(got), got)
	}
}

// The veto is not the whole invariant: clearAnchorIf's outcome has to be reset at
// the top of every RetryOnConflict attempt, because the attempt that loses the
// race still runs the mutator and still sets it. Here the interceptor plays the
// other pass — it clears the same anchor underneath the first Update and answers
// with the Conflict the API server would return for the now-stale resourceVersion
// — so the retry's fresh read vetoes. A `cleared` left true by the losing attempt
// would announce a reap this pass did not perform (issue #315, the #304 shape).
func TestGovernanceLostEmitsNothingWhenTheAnchorIsClearedUnderneathIt(t *testing.T) {
	pool := anchoredPool(map[string]string{"workload": "api"})
	evs := events.NewFakeRecorder(16)

	first := true
	funcs := interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			p, ok := obj.(*karpv1.NodePool)
			if !ok || !first || p.Annotations[annotations.ActiveRotation] != "" {
				return c.Update(ctx, obj, opts...)
			}
			first = false
			var won karpv1.NodePool
			if err := c.Get(ctx, client.ObjectKeyFromObject(p), &won); err != nil {
				return err
			}
			clearRotationAnchorFields(won.Annotations)
			if err := c.Update(ctx, &won); err != nil {
				return err
			}
			return apierrors.NewConflict(
				schema.GroupResource{Group: karpapis.Group, Resource: "nodepools"}, p.Name,
				errors.New("simulated stale resourceVersion"))
		},
	}
	r := newFlakyReconciler(t, nil, funcs,
		pool.DeepCopy(),
		placeholderPod(surgeNode, corev1.PodRunning),
		frozenNode(),
	)
	r.Events = evs

	var lines []string
	ctx := log.IntoContext(context.Background(), captureLogger(&lines))

	if err := r.reapUngovernedRotation(ctx, pool.DeepCopy()); err != nil {
		t.Fatalf("the losing pass must not surface the resolved conflict as an error: %v", err)
	}
	if first {
		t.Fatal("test did not exercise the retry: no anchor-clearing Update reached the interceptor")
	}
	if got := reapEvents(evs); len(got) != 0 {
		t.Errorf("the pass that lost the anchor must announce nothing, got %v", got)
	}
	if got := countLines(lines, "reaped orphaned rotation artifacts"); got != 0 {
		t.Errorf("the pass that lost the anchor must log no reap line; lines = %v", lines)
	}
	// The winner's clear stands, and the rollback this pass performed is intact.
	assertReaped(t, r)
}
