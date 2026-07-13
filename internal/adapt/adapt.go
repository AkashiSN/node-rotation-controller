// Package adapt is the only bridge between Karpenter's CRD types and the pure
// decision layer (internal/selection, internal/decide). Those packages compile to
// wasm for the policy simulator and must not link sigs.k8s.io/karpenter, whose
// scheme/reflect metadata costs ~6 MB gzipped that nothing in them uses.
//
// A Karpenter type change therefore surfaces here as a compile error, and
// adapt_test.go pins the field mapping.
package adapt

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// Claim projects a NodeClaim onto the view selection reads.
func Claim(c *karpv1.NodeClaim) selection.Claim {
	v := selection.Claim{
		Name:        c.Name,
		CreatedAt:   c.CreationTimestamp.Time,
		Deleting:    c.DeletionTimestamp != nil,
		ExpireAfter: c.Spec.ExpireAfter.Duration,
		Ready:       ready(c),
		Annotations: c.Annotations,
	}
	if d := c.Spec.TerminationGracePeriod; d != nil {
		v.TGP = &d.Duration
	}
	return v
}

// Claims projects a List result. The returned map aliases the CALLER's slice
// (&claims[i]) so the reconcile loop can patch the picked claim without a re-Get —
// re-getting by name would widen the list→patch window and add NotFound and
// resourceVersion-skew cases that do not exist today.
func Claims(claims []karpv1.NodeClaim) ([]selection.Claim, map[string]*karpv1.NodeClaim) {
	views := make([]selection.Claim, len(claims))
	byName := make(map[string]*karpv1.NodeClaim, len(claims))
	for i := range claims {
		views[i] = Claim(&claims[i])
		byName[claims[i].Name] = &claims[i]
	}
	return views, byName
}

func ready(c *karpv1.NodeClaim) bool {
	for _, cond := range c.Status.Conditions {
		if cond.Type == status.ConditionReady {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}
