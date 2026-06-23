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
// pod overhead) via k8s.io/component-helpers; v1 applies no extra padding beyond
// that computed request (spec §3.3).
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
	if IsInfraOrCompleted(p) || isNodePinned(p, nodeName) {
		return false
	}
	return true
}

// IsInfraOrCompleted reports whether a Pod is infrastructure the surge need not
// re-fit — a DaemonSet Pod (Karpenter already adds DaemonSet overhead to every
// node it provisions) or a mirror/static Pod — or has already completed
// (Succeeded/Failed). Unlike full reschedulability it deliberately does NOT
// consider node-pinning: a node-pinned Pod is still real workload occupying a
// node, which the rollback's absorb-host guard must count (spec §3.3). Exported
// so that guard can share this filter instead of re-implementing it.
func IsInfraOrCompleted(p *corev1.Pod) bool {
	return isDaemonSet(p) || isMirror(p) || isCompleted(p)
}

func isDaemonSet(p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "DaemonSet" {
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

// isNodePinned reports whether the Pod is confined to the candidate node it is
// already running on, so it cannot re-land elsewhere and must not be reserved
// for on the surge node (spec §3.3). It must distinguish a real pin from a
// hostname constraint that still leaves the Pod reschedulable — over-excluding
// reschedulable Pods under-sizes the placeholder (issue #29).
//
//   - A kubernetes.io/hostname nodeSelector pins the Pod to its single value;
//     that is a pin only when the value is the candidate node.
//   - For required nodeAffinity, the Pod is pinned only when its hostname
//     constraints permit exactly one host across all ORed NodeSelectorTerms AND
//     that host is the candidate: every term must positively bound the hostname
//     with an In expression, and the union of those permitted hosts must be the
//     single value nodeName. A term with no hostname In (only a NotIn, an Exists,
//     or no hostname key at all) leaves the Pod free to schedule elsewhere via
//     that term, so it is not pinned; an In spanning more than one host, or a
//     single host other than the candidate, is likewise not a pin.
//
// nodeName is the candidate's identity, matched against the kubernetes.io/hostname
// values (which default to the node name). A genuine pin to a host whose label
// differs from the node name is therefore treated as not-pinned — that errs
// toward over-provisioning (safe), never under-provisioning. When uncertain the
// predicate always errs toward "not pinned".
func isNodePinned(p *corev1.Pod, nodeName string) bool {
	if v, ok := p.Spec.NodeSelector[corev1.LabelHostname]; ok {
		return v == nodeName
	}
	aff := p.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) == 0 {
		return false
	}
	hosts := map[string]struct{}{}
	for _, term := range terms {
		bounded := false
		for _, expr := range term.MatchExpressions {
			if expr.Key != corev1.LabelHostname {
				continue
			}
			if expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
				bounded = true
				for _, v := range expr.Values {
					hosts[v] = struct{}{}
				}
			}
		}
		if !bounded {
			return false // this ORed term lets the Pod schedule on another host
		}
	}
	_, onlyCandidate := hosts[nodeName]
	return len(hosts) == 1 && onlyCandidate
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
