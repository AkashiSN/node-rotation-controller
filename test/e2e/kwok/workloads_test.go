//go:build e2e

package kwok

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pauseImage is the same image the chart hands the placeholder; reused for
// sample workloads so KWOK needs no extra image and pulls nothing.
const pauseImage = "registry.k8s.io/pause:3.10"

// appsv1Deployment wraps a built Deployment so harness helpers can pass it
// around without leaking the apps/v1 import everywhere.
type appsv1Deployment struct {
	obj *appsv1.Deployment
}

func newDeploymentShell(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workloadNamespace},
	}
}

// newPauseDeployment builds a 1-replica pause Deployment pinned to a NodePool by
// the pool label, requesting cpuMilli milli-CPU. When antiAffinityApp is
// non-empty it adds required hostname anti-affinity against Pods carrying
// app=<antiAffinityApp>, so KWOK provisions the workload onto a node distinct
// from that app's node (used to stage a separate spare node in the same pool).
func newPauseDeployment(name, pool string, cpuMilli int, antiAffinityApp string) *appsv1Deployment {
	labels := map[string]string{"app": name}
	one := int32(1)
	tolerations := []corev1.Toleration{{
		// KWOK / Karpenter taint critical addons; tolerate so the workload can
		// land on the kwok virtual nodes Karpenter manages.
		Key:      "CriticalAddonsOnly",
		Operator: corev1.TolerationOpExists,
	}}
	spec := corev1.PodSpec{
		NodeSelector: map[string]string{poolLabelKey: poolSuffix(pool)},
		Tolerations:  tolerations,
		Containers: []corev1.Container{{
			Name:  "app",
			Image: pauseImage,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(cpuMilli)},
			},
		}},
	}
	if antiAffinityApp != "" {
		spec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": antiAffinityApp}},
					TopologyKey:   corev1.LabelHostname,
				}},
			},
		}
	}
	return &appsv1Deployment{obj: &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workloadNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
		},
	}}
}

// withReplicas overrides the replica count (used to raise headroom so a blocked
// drain can proceed in the PDB subtest).
func (d *appsv1Deployment) withReplicas(n int32) *appsv1Deployment {
	d.obj.Spec.Replicas = &n
	return d
}

// withPriority pins a PriorityClass (used by the preemption subtest's preemptor).
func (d *appsv1Deployment) withPriority(className string) *appsv1Deployment {
	d.obj.Spec.Template.Spec.PriorityClassName = className
	return d
}

// preemptorPod builds a bare pause Pod pinned to a specific host (by hostname
// nodeSelector, so kube-scheduler — not nodeName — places it and can preempt to
// make room) requesting cpuMilli milli-CPU. It carries the DEFAULT priority (0),
// which is strictly above the placeholder's negative PriorityClass, so when the
// host has room only for it after evicting the placeholder, the scheduler
// preempts the placeholder — the §3.3 "rescheduled workload preempts the bare
// placeholder" path. It tolerates the KWOK CriticalAddonsOnly taint like the
// other sample workloads.
func preemptorPod(name, hostname string, cpuMilli int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workloadNamespace, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{corev1.LabelHostname: hostname},
			Tolerations:  []corev1.Toleration{{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:  "app",
				Image: pauseImage,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(cpuMilli)},
				},
			}},
		},
	}
}

// poolSuffix maps a NodePool metadata.name (nodepool-a) to its pool label value
// (a), matching the manifests' noderotation-e2e/pool labels.
func poolSuffix(pool string) string {
	switch pool {
	case poolA:
		return "a"
	case poolB:
		return "b"
	default:
		return pool
	}
}
