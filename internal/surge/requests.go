// Package surge builds the make-before-break surge inputs of the reconcile loop
// (spec §3.3): the placeholder Pod that induces NodePool-owned replacement
// capacity, the summed requests that size it, and the candidate-dependent
// headroom gate (spec §5.2 step 3). Like internal/selection it is a pure layer —
// no Kubernetes client calls — so the caller fetches the candidate Node, its
// Pods, and the NodePool and passes plain values in; the actual create/delete is
// the state machine's job.
package surge

import (
	corev1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"
)

// ReschedulableRequests returns the summed effective requests of the
// reschedulable Pods scheduled on nodeName — the workload that must re-land
// after the drain, which sizes the placeholder (spec §3.3). It excludes the Pods
// Karpenter does not need to re-fit onto fresh capacity:
//
//   - DaemonSet Pods — Karpenter already adds DaemonSet overhead to every node it
//     provisions, so counting them here would double-count and over-provision;
//   - mirror/static Pods (the kubelet's config.mirror annotation);
//   - completed Pods (Succeeded/Failed);
//   - Pods node-pinned to this specific node by a required kubernetes.io/hostname
//     selector or nodeAffinity — they cannot re-land elsewhere.
//
// Each remaining Pod is sized by the standard Kubernetes effective-request
// algorithm (regular + restartable-init/sidecar containers, init-container peak,
// pod overhead) via k8s.io/component-helpers. The precise padding and exclusion
// filter are finalized in the PoC (spec §3.3).
func ReschedulableRequests(pods []corev1.Pod, nodeName string) corev1.ResourceList {
	total := corev1.ResourceList{}
	for i := range pods {
		p := &pods[i]
		if !reschedulable(p, nodeName) {
			continue
		}
		add(total, resourcehelper.PodRequests(p, resourcehelper.PodResourcesOptions{}))
	}
	return total
}

// reschedulable reports whether a Pod's requests count toward the surge sizing.
func reschedulable(p *corev1.Pod, nodeName string) bool {
	if p.Spec.NodeName != nodeName {
		return false // not on the candidate node
	}
	if isDaemonSet(p) || isMirror(p) || isCompleted(p) || isNodePinned(p) {
		return false
	}
	return true
}

func isDaemonSet(p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isMirror(p *corev1.Pod) bool {
	_, ok := p.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

func isCompleted(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// isNodePinned reports whether the Pod is constrained to one specific host by a
// required kubernetes.io/hostname nodeSelector or nodeAffinity term. Such a Pod
// (already running on the candidate node) cannot re-land elsewhere, so it must
// not be reserved for on the surge node (spec §3.3).
func isNodePinned(p *corev1.Pod) bool {
	if _, ok := p.Spec.NodeSelector[corev1.LabelHostname]; ok {
		return true
	}
	aff := p.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == corev1.LabelHostname {
				return true
			}
		}
	}
	return false
}

// add accumulates src into dst (dst is modified in place).
func add(dst, src corev1.ResourceList) {
	for name, q := range src {
		if cur, ok := dst[name]; ok {
			cur.Add(q)
			dst[name] = cur
		} else {
			dst[name] = q.DeepCopy()
		}
	}
}
