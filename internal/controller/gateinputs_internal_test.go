package controller

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gateInputs is the seam between the controller's internal `resolved` struct and
// the pure decide.Inputs view. Nothing else pins this mapping: internal/decide's
// own tests use literal Cooldown/FailurePause values, and the reconcile-level
// fixtures happen to set cooldownAfter == failurePause == 10m (testPolicy's
// ApplyDefaults), so a mapping that swapped the two fields would still pass every
// other test in the repo. Cooldown and FailurePause below are deliberately
// distinct so a swap fails this test.
func TestGateInputsMapsResolvedFieldsOntoDecideInputs(t *testing.T) {
	const cooldown = 7 * time.Minute
	const failurePause = 42 * time.Minute

	pool := withTGP(testNodePool(map[string]string{"custom-annotation-key": "custom-value"}))
	pol := testPolicy()
	pol.Surge.CooldownAfter = &metav1.Duration{Duration: cooldown}
	pol.Surge.FailurePause = &metav1.Duration{Duration: failurePause}
	pol.Surge.ForcefulFallback.Enabled = true
	sched := mustSchedule(t)

	r := newReconciler(t, testNow, nil, pool)
	res := r.resolve(pool, pol, sched)

	gi := r.gateInputs(pool, res, testNow)

	if !gi.Now.Equal(testNow) {
		t.Errorf("Now = %v, want %v", gi.Now, testNow)
	}
	if want := sched.InWindow(testNow); gi.InWindow != want {
		t.Errorf("InWindow = %v, want sched.InWindow(now) = %v", gi.InWindow, want)
	}
	if !reflect.DeepEqual(gi.Annotations, pool.Annotations) {
		t.Errorf("Annotations = %v, want pool.Annotations = %v", gi.Annotations, pool.Annotations)
	}
	if gi.Cooldown != cooldown {
		t.Errorf("Cooldown = %v, want surge.cooldownAfter = %v", gi.Cooldown, cooldown)
	}
	if gi.FailurePause != failurePause {
		t.Errorf("FailurePause = %v, want surge.failurePause = %v", gi.FailurePause, failurePause)
	}
	if !gi.FallbackEnabled {
		t.Error("FallbackEnabled = false, want true (surge.forcefulFallback.enabled)")
	}
	if gi.ReadyTimeout != res.readyTimeout {
		t.Errorf("ReadyTimeout = %v, want resolved readyTimeout = %v", gi.ReadyTimeout, res.readyTimeout)
	}
	if gi.DrainBound != res.drainBound {
		t.Errorf("DrainBound = %v, want resolved drainBound (tGP + buffer) = %v", gi.DrainBound, res.drainBound)
	}
}
