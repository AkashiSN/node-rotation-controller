package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
)

// Issue #326: with surge.wholeNodeReservation on, the placeholder demands a
// whole node's worth of what the drain needs, so a host already running other
// Pods cannot absorb it — the class of host on which the aggregate reservation
// stops being fungible with the individual Pods' placement.

// wholeNodePolicy is testPolicy with the opt-in reservation mode enabled.
func wholeNodePolicy() *policy.Policy {
	p := testPolicy()
	p.Surge.WholeNodeReservation.Enabled = true
	return p
}

// smallDrainFixture is a candidate whose reschedulable drain (800m/2Gi) is a
// fraction of the node it sits on (allocatable 3770m/14570Mi, DaemonSet
// 300m/1082Mi → limit 3470m/13488Mi).
func smallDrainFixture(t *testing.T, pol *policy.Policy) (*RotationReconciler, []string) {
	t.Helper()
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "3770m", "memory", "14570Mi", "pods", "110"))
	pool := withTGP(testNodePool(nil))
	node := nodeAllocatable(testK8sNode(candNode, true, nil, false), "cpu", "3770m", "memory", "14738Mi")
	r := newReconciler(t, testNow, nil, pool, cand, node,
		workloadPod("app", candNode, "800m", "2Gi"),
		asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi")))

	var lines []string
	if _, err := r.reconcileNodePool(log.IntoContext(context.Background(), captureLogger(&lines)), pool, pol, mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	return r, lines
}

// placeholderRequests reads back what the created placeholder actually asks for.
func placeholderRequests(t *testing.T, r *RotationReconciler) corev1.ResourceList {
	t.Helper()
	var ph corev1.Pod
	key := types.NamespacedName{Namespace: testNS, Name: surge.PlaceholderName("nc-old")}
	if err := r.Get(context.Background(), key, &ph); err != nil {
		t.Fatalf("no placeholder was created: %v", err)
	}
	return ph.Spec.Containers[0].Resources.Requests
}

func TestWholeNodeReservationSizesThePlaceholderToAWholeNode(t *testing.T) {
	r, lines := smallDrainFixture(t, wholeNodePolicy())

	got := placeholderRequests(t, r)
	if got.Cpu().String() != "3470m" {
		t.Errorf("cpu = %v, want 3470m (allocatable − DaemonSet)", got.Cpu())
	}
	if got.Memory().String() != "13488Mi" {
		t.Errorf("memory = %v, want 13488Mi", got.Memory())
	}
	// The line must still say what the drain actually is, or the reservation looks
	// like a node that really needs 3470m and the mode becomes invisible.
	if !containsLine(lines, "surge placeholder created", "whole-node", "800m") {
		t.Errorf("the line must name the mode and the real drain; lines = %v", lines)
	}
}

// The negative control the mode needs: with the toggle off the same fixture
// keeps the drain sizing, so the assertions above are about the mode and not
// about the fixture.
func TestWholeNodeReservationOffKeepsTheDrainSizing(t *testing.T) {
	r, lines := smallDrainFixture(t, testPolicy())

	got := placeholderRequests(t, r)
	if got.Cpu().String() != "800m" {
		t.Errorf("cpu = %v, want the drain 800m with the mode off", got.Cpu())
	}
	if got.Memory().String() != "2Gi" {
		t.Errorf("memory = %v, want the drain 2Gi with the mode off", got.Memory())
	}
	if containsLine(lines, "whole-node") {
		t.Errorf("the mode must not be named when it is off; lines = %v", lines)
	}
}

// The gate must pre-check the footprint the placeholder will actually have. A
// budget that fits the drain but not a whole node has to block BEFORE the anchor
// is written, not leave the placeholder unschedulable until readyTimeout.
func TestWholeNodeReservationHeadroomGateTestsTheRaisedFootprint(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAllocatable("cpu", "3770m", "memory", "14570Mi"))
	pool := withTGP(testNodePool(nil))
	// 2 cpu: room for the 800m drain, not for the 3470m whole-node reservation.
	pool.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse("2")}
	node := nodeAllocatable(testK8sNode(candNode, true, nil, false), "cpu", "3770m", "memory", "14738Mi")
	r := newReconciler(t, testNow, nil, pool, cand, node,
		workloadPod("app", candNode, "800m", "2Gi"),
		asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi")))

	if _, err := r.reconcileNodePool(context.Background(), pool, wholeNodePolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if getPool(t, r).Annotations[annotations.ActiveRotation] != "" {
		t.Error("the gate must test the raised footprint and block the start")
	}

	// The same budget with the mode off admits the rotation — the drain fits.
	r2 := newReconciler(t, testNow, nil, withTGP(testNodePool(nil)), cand.DeepCopy(), node.DeepCopy(),
		workloadPod("app", candNode, "800m", "2Gi"),
		asDaemonSet(workloadPod("kube-proxy", candNode, "300m", "1082Mi")))
	pool2 := getPool(t, r2)
	pool2.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse("2")}
	if err := r2.Update(context.Background(), pool2); err != nil {
		t.Fatalf("set limits: %v", err)
	}
	if _, err := r2.reconcileNodePool(context.Background(), getPool(t, r2), testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool (mode off): %v", err)
	}
	if getPool(t, r2).Annotations[annotations.ActiveRotation] == "" {
		t.Error("with the mode off the same budget must still admit the rotation")
	}
}
