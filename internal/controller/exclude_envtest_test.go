package controller_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/controller"
)

// bindNode creates a Node in the pool (so excludedClaims' label-scoped List sees
// it) with the given annotations, and binds the candidate claim to it via
// status.nodeName. ffSeed leaves the claim unscheduled; a real opt-out lives on
// the Node, so the candidate must be scheduled onto one.
func bindNode(t *testing.T, cl client.Client, poolName, claimName string, anns map[string]string) {
	t.Helper()
	ctx := context.Background()
	nodeName := "node-" + claimName
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        nodeName,
		Labels:      map[string]string{karpv1.NodePoolLabelKey: poolName},
		Annotations: anns,
	}}
	if err := cl.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	nc := ffGetClaim(t, cl, claimName)
	nc.Status.NodeName = nodeName
	if err := cl.Status().Update(ctx, nc); err != nil {
		t.Fatalf("bind claim to node: %v", err)
	}
}

// provisionSurgePrereqs creates the controller Namespace and placeholder
// PriorityClass that a real anchor+advance() needs once a candidate is
// scheduled onto a Node (unlike ffSeed's unscheduled baseline, which never
// reaches placeholder creation): envtest's real apiserver enforces both
// (namespace existence, PriorityClass existence) on Pod admission, and the
// controller does not manage either resource's lifecycle itself.
func provisionSurgePrereqs(t *testing.T, cl client.Client, r *controller.RotationReconciler) {
	t.Helper()
	ctx := context.Background()
	if err := cl.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: r.Namespace},
	}); err != nil {
		t.Fatalf("create controller namespace: %v", err)
	}
	preempt := corev1.PreemptNever
	if err := cl.Create(ctx, &schedulingv1.PriorityClass{
		ObjectMeta:       metav1.ObjectMeta{Name: r.PriorityClassName},
		Value:            -1,
		PreemptionPolicy: &preempt,
	}); err != nil {
		t.Fatalf("create placeholder priority class: %v", err)
	}
}

// TestExcludeDoNotDisruptNodeIsNotRotated: a triggered candidate whose Node
// carries an operator-set do-not-disrupt is never selected — no anchor, no
// placeholder, no deletion. Fallback is disabled (ffSeed .. false), so absent the
// opt-out this candidate would take the normal surge path (see the control
// below).
func TestExcludeDoNotDisruptNodeIsNotRotated(t *testing.T) {
	cl := ffStartEnv(t)
	r, poolName, claimName := ffSeed(t, cl, "excl", false)
	bindNode(t, cl, poolName, claimName, map[string]string{
		karpv1.DoNotDisruptAnnotationKey: "true", // operator-set (no owned marker)
	})
	provisionSurgePrereqs(t, cl, r)

	ffReconcile(t, r, poolName)

	pool := ffGetPool(t, cl, poolName)
	if got := pool.Annotations[annotations.ActiveRotation]; got != "" {
		t.Errorf("excluded node must NOT be anchored; active-rotation=%q", got)
	}
	if ffPlaceholderExists(t, cl, claimName) {
		t.Error("excluded node must not get a placeholder")
	}
	if c := ffGetClaim(t, cl, claimName); c.DeletionTimestamp != nil {
		t.Error("excluded node must not be deleted")
	}
}

// TestExcludeControlPlainNodeIsRotated is the positive control: the same fixture
// with a plain Node (no do-not-disrupt) DOES start a rotation, proving the opt-out
// — not some unrelated gate — is what prevents anchoring above.
func TestExcludeControlPlainNodeIsRotated(t *testing.T) {
	cl := ffStartEnv(t)
	r, poolName, claimName := ffSeed(t, cl, "ctl", false)
	bindNode(t, cl, poolName, claimName, nil)
	provisionSurgePrereqs(t, cl, r)

	ffReconcile(t, r, poolName)

	pool := ffGetPool(t, cl, poolName)
	if pool.Annotations[annotations.ActiveRotation] != claimName {
		t.Errorf("plain node must be anchored; active-rotation=%q want %q",
			pool.Annotations[annotations.ActiveRotation], claimName)
	}
}
