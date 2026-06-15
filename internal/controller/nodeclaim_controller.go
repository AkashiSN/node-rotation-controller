// Package controller hosts the controller-runtime reconcilers.
package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// NodeClaimReconciler is the v0.2 skeleton reconciler: it only proves the
// NodeClaim watch plumbing. The rotation state machine (spec §5.2) replaces
// this in v0.3.
type NodeClaimReconciler struct {
	client.Client
}

// Reconcile observes one NodeClaim and does nothing else yet.
func (r *NodeClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nodeClaim := &karpv1.NodeClaim{}
	if err := r.Get(ctx, req.NamespacedName, nodeClaim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// nodepool is empty when the karpenter.sh/nodepool label is absent (e.g. a
	// manually created NodeClaim); name keeps the line identifiable regardless.
	log.FromContext(ctx).V(1).Info("observed NodeClaim",
		"name", nodeClaim.Name,
		"nodepool", nodeClaim.Labels[karpv1.NodePoolLabelKey])
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *NodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeclaim").
		For(&karpv1.NodeClaim{}).
		Complete(r)
}
