package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// claimAge is a representative old-claim age used across the sweep tests; the
// exact value is irrelevant — the sweep keys off State, not age.
const claimAge = 20 * 24 * time.Hour

// sweep runs the startup sweep and fails the test on a hard error.
func sweep(t *testing.T, r *RotationReconciler) {
	t.Helper()
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
}

// frozenNode builds the controller-frozen surge node for the canonical rotation
// (claim nc-old, as throughout this package's tests): the do-not-disrupt +
// do-not-disrupt-owned + surge-for markers applyFreeze writes.
func frozenNode() *corev1.Node {
	return testK8sNode(surgeNode, true, map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true",
		annotations.DoNotDisruptOwned:    "true",
		annotations.SurgeFor:             "nc-old",
	}, false)
}

// --- placeholder Pods ------------------------------------------------------

func TestSweepDeletesOrphanedPlaceholder(t *testing.T) {
	// No NodePool carries an active-rotation anchor, so the placeholder for
	// "nc-old" is orphaned and must be deleted (spec §5.3 sweep).
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		placeholderPod(surgeNode, corev1.PodRunning),
	)
	sweep(t, r)
	if placeholderExists(t, r) {
		t.Fatal("orphaned placeholder should have been deleted")
	}
}

func TestSweepKeepsAnchoredPlaceholder(t *testing.T) {
	// The pool anchors a rotation on nc-old, so its placeholder is active and
	// the reconcile loop — not the sweep — owns it.
	r := newReconciler(t, testNow, nil,
		testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}),
		placeholderPod(surgeNode, corev1.PodRunning),
	)
	sweep(t, r)
	if !placeholderExists(t, r) {
		t.Fatal("anchored placeholder must be preserved")
	}
}

// --- node freeze markers ---------------------------------------------------

func TestSweepUnfreezesOrphanedNode(t *testing.T) {
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		frozenNode(),
	)
	sweep(t, r)
	n := getNodeObj(t, r, surgeNode)
	if _, ok := n.Annotations[annotations.SurgeFor]; ok {
		t.Error("orphaned surge-for marker should be removed")
	}
	if _, ok := n.Annotations[karpv1.DoNotDisruptAnnotationKey]; ok {
		t.Error("controller-owned do-not-disrupt should be removed")
	}
}

func TestSweepKeepsAnchoredNodeFrozen(t *testing.T) {
	r := newReconciler(t, testNow, nil,
		testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}),
		frozenNode(),
	)
	sweep(t, r)
	n := getNodeObj(t, r, surgeNode)
	if n.Annotations[annotations.SurgeFor] != "nc-old" {
		t.Error("anchored surge-for marker must be preserved")
	}
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Error("anchored do-not-disrupt must be preserved")
	}
}

func TestSweepPreservesOperatorDoNotDisrupt(t *testing.T) {
	// do-not-disrupt with no surge-for marker is operator-owned: never touched.
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		testK8sNode(surgeNode, true, map[string]string{karpv1.DoNotDisruptAnnotationKey: "true"}, false),
	)
	sweep(t, r)
	n := getNodeObj(t, r, surgeNode)
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Error("operator-owned do-not-disrupt must be preserved")
	}
}

func TestSweepUnfreezeKeepsOperatorDoNotDisruptOnSurgedNode(t *testing.T) {
	// An operator had already protected the node with do-not-disrupt before the
	// controller froze it for the rotation (surge-for, but no owned marker). With
	// no anchor the surge-for marker is orphaned and must be removed, but the
	// operator's do-not-disrupt — which the controller never owned — must survive
	// (spec §3.3, §5.3).
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		testK8sNode(surgeNode, true, map[string]string{
			karpv1.DoNotDisruptAnnotationKey: "true",
			annotations.SurgeFor:             "nc-old",
		}, false),
	)
	sweep(t, r)
	n := getNodeObj(t, r, surgeNode)
	if _, ok := n.Annotations[annotations.SurgeFor]; ok {
		t.Error("orphaned surge-for marker should be removed")
	}
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Error("operator do-not-disrupt (no owned marker) must be preserved")
	}
}

func TestSweepUncordonsOrphanedCordon(t *testing.T) {
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		testK8sNode(candNode, true, map[string]string{annotations.Cordoned: "true"}, true),
	)
	sweep(t, r)
	n := getNodeObj(t, r, candNode)
	if n.Spec.Unschedulable {
		t.Error("orphaned controller cordon should be lifted")
	}
	if _, ok := n.Annotations[annotations.Cordoned]; ok {
		t.Error("orphaned cordoned marker should be removed")
	}
}

func TestSweepCordonOnlyKeepsOperatorDoNotDisrupt(t *testing.T) {
	// A node the controller cordoned (cordoned marker) but never froze (no
	// surge-for) may also carry an operator-applied do-not-disrupt. With no
	// anchor the stale cordon must be lifted, but the operator's do-not-disrupt
	// — not the controller's — must survive: the sweep strips do-not-disrupt
	// only from nodes carrying the controller's do-not-disrupt-owned marker (spec §5.3).
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		testK8sNode(candNode, true, map[string]string{
			karpv1.DoNotDisruptAnnotationKey: "true",
			annotations.Cordoned:             "true",
		}, true),
	)
	sweep(t, r)
	n := getNodeObj(t, r, candNode)
	if n.Spec.Unschedulable {
		t.Error("stale controller cordon should be lifted")
	}
	if _, ok := n.Annotations[annotations.Cordoned]; ok {
		t.Error("cordoned marker should be removed")
	}
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		t.Error("operator-applied do-not-disrupt (no surge-for) must be preserved")
	}
}

func TestSweepPreservesOperatorCordon(t *testing.T) {
	// Unschedulable with no cordoned marker is an operator cordon: never touched.
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		testK8sNode(candNode, true, nil, true),
	)
	sweep(t, r)
	n := getNodeObj(t, r, candNode)
	if !n.Spec.Unschedulable {
		t.Error("operator cordon must be preserved")
	}
}

// --- impossible-state claims ----------------------------------------------

func TestSweepFailsImpossiblePendingClaim(t *testing.T) {
	rec := &fakeRecorder{}
	// A pending claim with no anchor cannot result from any crash point; set it
	// failed and alert (spec §5.3).
	r := newReconciler(t, testNow, rec,
		testNodePool(nil),
		testClaim("nc-old", claimAge, ncAnn(annotations.State, annotations.StatePending)),
	)
	sweep(t, r)
	c := getClaimOrNil(t, r, "nc-old")
	if c == nil {
		t.Fatal("claim should still exist")
	}
	if c.Annotations[annotations.State] != annotations.StateFailed {
		t.Errorf("state: got %q, want failed", c.Annotations[annotations.State])
	}
	if c.Annotations[annotations.FailedAt] == "" {
		t.Error("failed-at backoff anchor should be stamped")
	}
	if rec.failure != 1 {
		t.Errorf("failure alert: got %d, want 1", rec.failure)
	}
}

func TestSweepKeepsAnchoredPendingClaim(t *testing.T) {
	rec := &fakeRecorder{}
	r := newReconciler(t, testNow, rec,
		testNodePool(map[string]string{annotations.ActiveRotation: "nc-old"}),
		testClaim("nc-old", claimAge, ncAnn(annotations.State, annotations.StatePending)),
	)
	sweep(t, r)
	c := getClaimOrNil(t, r, "nc-old")
	if c.Annotations[annotations.State] != annotations.StatePending {
		t.Errorf("anchored pending claim must be left for the reconcile loop, got %q",
			c.Annotations[annotations.State])
	}
	if rec.failure != 0 {
		t.Errorf("no failure alert expected for an anchored rotation, got %d", rec.failure)
	}
}

func TestSweepKeepsFailedAndExpiredClaims(t *testing.T) {
	for _, st := range []string{annotations.StateFailed, annotations.StateExpired} {
		r := newReconciler(t, testNow, nil,
			testNodePool(nil),
			testClaim("nc-old", claimAge, ncAnn(annotations.State, st)),
		)
		sweep(t, r)
		c := getClaimOrNil(t, r, "nc-old")
		if c.Annotations[annotations.State] != st {
			t.Errorf("%s claim must be preserved, got %q", st, c.Annotations[annotations.State])
		}
	}
}

// --- torn pool state -------------------------------------------------------

func TestSweepRemovesOrphanedRotationState(t *testing.T) {
	r := newReconciler(t, testNow, nil,
		testNodePool(map[string]string{annotations.ActiveRotationState: annotations.StateDraining}),
	)
	sweep(t, r)
	p := getPool(t, r)
	if _, ok := p.Annotations[annotations.ActiveRotationState]; ok {
		t.Error("active-rotation-state with no anchor should be removed")
	}
}

func TestSweepKeepsRotationStateWithAnchor(t *testing.T) {
	r := newReconciler(t, testNow, nil,
		testNodePool(map[string]string{
			annotations.ActiveRotation:      "nc-old",
			annotations.ActiveRotationState: annotations.StateDraining,
		}),
	)
	sweep(t, r)
	p := getPool(t, r)
	if p.Annotations[annotations.ActiveRotationState] != annotations.StateDraining {
		t.Error("active-rotation-state with an anchor must be preserved")
	}
}

// --- reconcile gates the sweep (issue #25 / PR #33 review) ------------------

// reconcile drives one Reconcile of the named NodePool and fails on a hard
// error — exercising the public entry point that gates the startup sweep.
func runReconcile(t *testing.T, r *RotationReconciler, poolName string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: poolName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// TestReconcileRunsSweepBeforeRotation proves the sweep is ordered before any
// reconcile work: a crash-orphaned placeholder (no anchor) must be cleaned by
// the first Reconcile itself, not only by the dedicated Sweep entry point. The
// pool is out of window so the reconcile body starts nothing — the deletion can
// only come from the gated sweep.
func TestReconcileRunsSweepBeforeRotation(t *testing.T) {
	r := newReconciler(t, testNowOut, nil,
		testNodePool(nil),
		placeholderPod(surgeNode, corev1.PodRunning),
	)

	runReconcile(t, r, testPoolName)

	if placeholderExists(t, r) {
		t.Fatal("first Reconcile must run the startup sweep and delete the orphaned placeholder")
	}
}

// TestReconcileSweepsOnlyOnce proves the sweep is a one-time startup operation,
// not a per-reconcile pass: once the first Reconcile has swept, a placeholder
// created afterward (standing in for a live rotation's artifact) must survive a
// second Reconcile. Re-sweeping on every reconcile would race new rotations and
// wrongly reap their artifacts.
func TestReconcileSweepsOnlyOnce(t *testing.T) {
	r := newReconciler(t, testNowOut, nil, testNodePool(nil))

	// First reconcile: sweep fires with nothing to clean.
	runReconcile(t, r, testPoolName)

	// A placeholder appears after the sweep window — e.g. a rotation that just
	// started. A second sweep would treat it as orphaned (no anchor) and delete it.
	if err := r.Create(context.Background(), placeholderPod(surgeNode, corev1.PodRunning)); err != nil {
		t.Fatalf("create placeholder: %v", err)
	}

	runReconcile(t, r, testPoolName)

	if !placeholderExists(t, r) {
		t.Fatal("the sweep must run once at startup, not on every reconcile")
	}
}

// --- the sweep's write, and what it may announce ---------------------------

// The sweep decides from a List — a cache read — and writes some time later. Two
// things can happen in that window, neither of them under this controller's
// control: Karpenter's termination controller can finalize the claim away, and
// the claim's durable state can move past what the List saw. In both cases the
// sweep repairs nothing, so it must announce nothing (issue #311).

// vanishOnFirstClaimGet finalizes the claim away on the first NodeClaim Get —
// the one the conditional write makes — and answers it NotFound, which is what
// the sweep sees when the termination controller wins that window.
func vanishOnFirstClaimGet() interceptor.Funcs {
	first := true
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*karpv1.NodeClaim); !ok || !first {
				return c.Get(ctx, key, obj, opts...)
			}
			first = false
			var live karpv1.NodeClaim
			if err := c.Get(ctx, key, &live); err != nil {
				return err
			}
			live.Finalizers = nil
			if err := c.Update(ctx, &live); err != nil {
				return err
			}
			if err := client.IgnoreNotFound(c.Delete(ctx, &live)); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// staleClaimList serves every listed NodeClaim as state, whatever it durably
// holds — the lagging List the sweep selects from.
func staleClaimList(state string) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			l, ok := list.(*karpv1.NodeClaimList)
			if !ok {
				return nil
			}
			for i := range l.Items {
				l.Items[i].Annotations[annotations.State] = state
			}
			return nil
		},
	}
}

func TestSweepAnnouncesNoFailureForAClaimThatVanished(t *testing.T) {
	rec := &fakeRecorder{}
	r := newFlakyReconciler(t, rec, vanishOnFirstClaimGet(),
		testNodePool(nil),
		testClaim("nc-old", claimAge, ncFinalizer(), ncAnn(annotations.State, annotations.StatePending)),
	)

	sweep(t, r)

	if getClaimOrNil(t, r, "nc-old") != nil {
		t.Fatal("test did not exercise the window: the claim must be gone by the write")
	}
	if rec.failure != 0 {
		t.Errorf("the sweep announced %d failures for a claim it never wrote, want 0", rec.failure)
	}
}

func TestSweepDoesNotOverwriteAClaimThatIsAlreadyFailed(t *testing.T) {
	rec := &fakeRecorder{}
	failedAt := rfc(testNow.Add(-40 * time.Minute))
	r := newFlakyReconciler(t, rec, staleClaimList(annotations.StatePending),
		testNodePool(nil),
		testClaim("nc-old", claimAge, ncAnn(
			annotations.State, annotations.StateFailed,
			annotations.FailedAt, failedAt,
			annotations.RetryCount, "1",
		)),
	)

	sweep(t, r)

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil {
		t.Fatal("claim should still exist")
	}
	if c.Annotations[annotations.State] != annotations.StateFailed {
		t.Errorf("state: got %q, want it left at failed", c.Annotations[annotations.State])
	}
	if got := c.Annotations[annotations.FailedAt]; got != failedAt {
		t.Errorf("failed-at = %q, want %q: rewriting it moves the escalated-backoff anchor", got, failedAt)
	}
	if rec.failure != 0 {
		t.Errorf("the sweep announced %d failures for a rollback it did not perform, want 0", rec.failure)
	}
}

// vanishWithNotFoundOnFirstClaimUpdate lets the conditional write's Get succeed
// and finalizes the claim away on its Update — the second half of the
// List-to-write window, which the Get-side interceptor above cannot reach.
func vanishWithNotFoundOnFirstClaimUpdate() interceptor.Funcs {
	first := true
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			nc, ok := obj.(*karpv1.NodeClaim)
			if !ok || !first {
				return c.Update(ctx, obj, opts...)
			}
			first = false
			var live karpv1.NodeClaim
			if err := c.Get(ctx, client.ObjectKeyFromObject(nc), &live); err != nil {
				return err
			}
			live.Finalizers = nil
			if err := c.Update(ctx, &live); err != nil {
				return err
			}
			if err := client.IgnoreNotFound(c.Delete(ctx, &live)); err != nil {
				return err
			}
			return apierrors.NewNotFound(
				schema.GroupResource{Group: karpapis.Group, Resource: "nodeclaims"}, nc.Name)
		},
	}
}

// A claim that vanishes needs no repair, so it is not an error either. The sweep
// runs once and is never retried, so an error here would only report a startup
// problem that is not one.
func TestSweepReportsNoErrorForAClaimFinalizedDuringTheWrite(t *testing.T) {
	rec := &fakeRecorder{}
	r := newFlakyReconciler(t, rec, vanishWithNotFoundOnFirstClaimUpdate(),
		testNodePool(nil),
		testClaim("nc-old", claimAge, ncFinalizer(), ncAnn(annotations.State, annotations.StatePending)),
	)

	if err := r.Sweep(context.Background()); err != nil {
		t.Errorf("a claim that vanished mid-write is not a sweep error: %v", err)
	}
	if getClaimOrNil(t, r, "nc-old") != nil {
		t.Fatal("test did not exercise the window: the claim must be gone by the write")
	}
	if rec.failure != 0 {
		t.Errorf("the sweep announced %d failures for a claim it never wrote, want 0", rec.failure)
	}
}

// The predicate accepts either in-flight state, so the state the write repaired
// is not necessarily the one the List reported. The line must name the state the
// write actually found, or it describes a rollback that did not happen.
func TestSweepLogsTheStateItActuallyRepaired(t *testing.T) {
	rec := &fakeRecorder{}
	r := newFlakyReconciler(t, rec, staleClaimList(annotations.StatePending),
		testNodePool(nil),
		testClaim("nc-old", claimAge, ncAnn(annotations.State, annotations.StateDraining)),
	)

	var lines []string
	if err := r.Sweep(log.IntoContext(context.Background(), captureLogger(&lines))); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	c := getClaimOrNil(t, r, "nc-old")
	if c == nil || c.Annotations[annotations.State] != annotations.StateFailed {
		t.Fatalf("an un-anchored draining claim must still be repaired: %+v", c)
	}
	if rec.failure != 1 {
		t.Errorf("failure alert: got %d, want 1", rec.failure)
	}
	if !containsLine(lines, "failed un-anchored in-flight claim", `"state"="draining"`) {
		t.Errorf(`the line must name the state the write found, not the stale List value; lines = %v`, lines)
	}
}

// --- announcement follows the write ----------------------------------------
//
// The claim leg above announces only the repair its write performed (issue
// #311). Its two neighbours reach the same window from the other side: each
// calls something that is a no-op when the object vanished or already held the
// desired state, and a line placed after such a call names work this sweep did
// not do (issue #313).

// deletedUnderneathFirstPodDelete deletes the placeholder on the sweep's own
// Delete and answers it NotFound — the window between the Pod List and the
// Delete, which a completeOrAbort on another instance's watch, or an operator,
// can close first.
func deletedUnderneathFirstPodDelete() interceptor.Funcs {
	first := true
	return interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			p, ok := obj.(*corev1.Pod)
			if !ok || !first {
				return c.Delete(ctx, obj, opts...)
			}
			first = false
			if err := client.IgnoreNotFound(c.Delete(ctx, p)); err != nil {
				return err
			}
			return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, p.Name)
		},
	}
}

func TestSweepAnnouncesNoDeleteForAPlaceholderThatVanished(t *testing.T) {
	r := newFlakyReconciler(t, nil, deletedUnderneathFirstPodDelete(),
		testNodePool(nil),
		placeholderPod(surgeNode, corev1.PodRunning),
	)

	var lines []string
	if err := r.Sweep(log.IntoContext(context.Background(), captureLogger(&lines))); err != nil {
		t.Fatalf("a placeholder someone else deleted is not a sweep error: %v", err)
	}

	if placeholderExists(t, r) {
		t.Fatal("test did not exercise the window: the Pod must be gone by the Delete")
	}
	if containsLine(lines, "deleted orphaned placeholder") {
		t.Errorf("the sweep announced a delete it did not perform; lines = %v", lines)
	}
}

// The gate is only worth having if the line still fires for a placeholder this
// sweep really deleted.
func TestSweepLogsThePlaceholderItDeleted(t *testing.T) {
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		placeholderPod(surgeNode, corev1.PodRunning),
	)

	var lines []string
	if err := r.Sweep(log.IntoContext(context.Background(), captureLogger(&lines))); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if placeholderExists(t, r) {
		t.Fatal("orphaned placeholder should have been deleted")
	}
	if !containsLine(lines, "deleted orphaned placeholder", `"claim"="nc-old"`) {
		t.Errorf("a delete the sweep performed must be announced; lines = %v", lines)
	}
}

// vanishOnFirstNodeGet deletes the node on the first Node Get — the one
// patchNode makes — and answers it NotFound, which is what the sweep sees when
// the node is gone by the time it writes.
func vanishOnFirstNodeGet() interceptor.Funcs {
	first := true
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Node); !ok || !first {
				return c.Get(ctx, key, obj, opts...)
			}
			first = false
			var live corev1.Node
			if err := c.Get(ctx, key, &live); err != nil {
				return err
			}
			if err := client.IgnoreNotFound(c.Delete(ctx, &live)); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

func TestSweepAnnouncesNoUnfreezeForANodeThatVanished(t *testing.T) {
	r := newFlakyReconciler(t, nil, vanishOnFirstNodeGet(),
		testNodePool(nil),
		frozenNode(),
	)

	var lines []string
	if err := r.Sweep(log.IntoContext(context.Background(), captureLogger(&lines))); err != nil {
		t.Fatalf("a node that vanished is not a sweep error: %v", err)
	}

	var gone corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: surgeNode}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("test did not exercise the window: the node must be gone by the write, got %v", err)
	}
	if containsLine(lines, "unfroze orphaned node") {
		t.Errorf("the sweep announced an unfreeze of a node that no longer exists; lines = %v", lines)
	}
}

// staleNodeList stamps claim's surge-for marker onto every listed Node whatever
// the node durably holds — the lagging List the sweep selects from.
func staleNodeList(claim string) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			l, ok := list.(*corev1.NodeList)
			if !ok {
				return nil
			}
			for i := range l.Items {
				if l.Items[i].Annotations == nil {
					l.Items[i].Annotations = map[string]string{}
				}
				l.Items[i].Annotations[annotations.SurgeFor] = claim
			}
			return nil
		},
	}
}

func TestSweepAnnouncesNoUnfreezeForANodeAlreadyClear(t *testing.T) {
	// The List reports a surge-for marker the node no longer carries — an
	// earlier instance's rollback, or this controller's own previous run, got
	// there first — so the mutator finds nothing to reverse and skips the Update.
	r := newFlakyReconciler(t, nil, staleNodeList("nc-old"),
		testNodePool(nil),
		testK8sNode(surgeNode, true, nil, false),
	)

	var lines []string
	if err := r.Sweep(log.IntoContext(context.Background(), captureLogger(&lines))); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if containsLine(lines, "unfroze orphaned node") {
		t.Errorf("the sweep announced an unfreeze of a node whose markers were already clear; lines = %v", lines)
	}
}

// A sweep line names the reversal it applied. A cordon-only node was never
// frozen — it has no surge-for marker and belongs to no claim — so reporting it
// as an unfreeze, with an empty claim, describes work of a kind the sweep did
// not do on it.
func TestSweepNamesTheReversalItApplied(t *testing.T) {
	r := newReconciler(t, testNow, nil,
		testNodePool(nil),
		frozenNode(),
		testK8sNode(candNode, true, map[string]string{annotations.Cordoned: "true"}, true),
	)

	var lines []string
	if err := r.Sweep(log.IntoContext(context.Background(), captureLogger(&lines))); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if !containsLine(lines, "unfroze orphaned node", `"node"="`+surgeNode+`"`, `"claim"="nc-old"`) {
		t.Errorf("the surge-frozen node's unfreeze must be announced with its claim; lines = %v", lines)
	}
	if !containsLine(lines, "uncordoned orphaned node", `"node"="`+candNode+`"`) {
		t.Errorf("the cordon-only node's uncordon must be announced; lines = %v", lines)
	}
	if containsLine(lines, "unfroze orphaned node", `"node"="`+candNode+`"`) {
		t.Errorf("a cordon-only node was never frozen, so it cannot be unfrozen; lines = %v", lines)
	}
}

// vanishWithConflictOnFirstNodeUpdate deletes the node on its first Update and
// answers with the Conflict a stale resourceVersion would draw, so RetryOnConflict
// runs a second attempt whose Get finds the node gone and whose mutator therefore
// never runs.
func vanishWithConflictOnFirstNodeUpdate() interceptor.Funcs {
	first := true
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			n, ok := obj.(*corev1.Node)
			if !ok || !first {
				return c.Update(ctx, obj, opts...)
			}
			first = false
			if err := client.IgnoreNotFound(c.Delete(ctx, n)); err != nil {
				return err
			}
			return apierrors.NewConflict(
				schema.GroupResource{Resource: "nodes"}, n.Name,
				errors.New("simulated stale resourceVersion"))
		},
	}
}

// The same shape that made a conditional claim write announce a claim it had not
// written (issue #307): the report must be produced by the Update, not by the
// mutator's verdict on an attempt that then lost. A mutator that reported "yes,
// there is something to reverse" on the losing attempt says nothing about the
// node the retry found gone.
func TestPatchNodeReportsNoWriteWhenTheNodeVanishesDuringTheRetry(t *testing.T) {
	r := newFlakyReconciler(t, nil, vanishWithConflictOnFirstNodeUpdate(), frozenNode())

	wrote, err := r.patchNode(context.Background(), surgeNode, applyUnfreeze)

	if err != nil {
		t.Fatalf("a node that vanished mid-retry is a no-op, not an error: %v", err)
	}
	var gone corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: surgeNode}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("test did not exercise the window: the node must be gone by the retry, got %v", err)
	}
	if wrote {
		t.Error("patchNode reported a write for a node that no longer exists")
	}
}
