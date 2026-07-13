package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/sim"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// TestSimResolutionMatchesController is the drift guard between the controller and the
// policy simulator.
//
// internal/sim calls the REAL decide/selection functions, so the decisions cannot drift —
// but the INPUTS those functions read (leadTime = K·P + readyTimeout + Buffer, the drain
// bound, the failurePause default, the schedule.Derive inputs) are resolved twice: here in
// resolve()/derivedThresholds() from a karpv1.NodePool, and in sim.Resolve() from a
// sim.Fleet. Two resolutions of the same §3.2 formulas is exactly the second
// implementation the design exists to prevent, so they are pinned against each other:
// change one side and this fails, instead of the simulator quietly lying to operators.
//
// (The duplication itself is deliberate: the pure layer cannot import karpv1 — it costs
// ~6 MB gzipped in wasm — so it cannot share a resolver that takes a NodePool.)
func TestSimResolutionMatchesController(t *testing.T) {
	t.Parallel()

	e := 14 * 24 * time.Hour
	tgp := 90 * time.Minute

	windows := []policy.MaintenanceWindow{{
		Timezone: "Asia/Tokyo",
		Days:     []string{"Tue", "Sat"},
		Start:    "01:00",
		End:      "05:00",
	}}

	tests := map[string]struct {
		mutate func(*policy.Policy)
		tgp    *time.Duration // NodePool template terminationGracePeriod; nil = unset
	}{
		"auto threshold, tGP set": {
			mutate: func(*policy.Policy) {},
			tgp:    &tgp,
		},
		"auto threshold, tGP unset (DrainFallback substituted)": {
			mutate: func(*policy.Policy) {},
			tgp:    nil,
		},
		"explicit ageThreshold override": {
			mutate: func(p *policy.Policy) { p.AgeThreshold = "200h" },
			tgp:    &tgp,
		},
		"explicit failurePause (does not track cooldownAfter)": {
			mutate: func(p *policy.Policy) {
				p.Surge.CooldownAfter = &metav1.Duration{Duration: time.Minute}
				p.Surge.FailurePause = &metav1.Duration{Duration: 45 * time.Minute}
			},
			tgp: &tgp,
		},
		"unset failurePause with a cooldownAfter above the floor": {
			mutate: func(p *policy.Policy) {
				p.Surge.CooldownAfter = &metav1.Duration{Duration: 25 * time.Minute}
			},
			tgp: &tgp,
		},
		"zero cooldownAfter (ADR-0004)": {
			mutate: func(p *policy.Policy) {
				p.Surge.CooldownAfter = &metav1.Duration{Duration: 0}
			},
			tgp: &tgp,
		},
		"explicit forecast estimates": {
			mutate: func(p *policy.Policy) {
				p.Surge.DrainEstimate = &metav1.Duration{Duration: 8 * time.Minute}
				p.Surge.ProvisioningEstimate = &metav1.Duration{Duration: 3 * time.Minute}
			},
			tgp: &tgp,
		},
		"K = 3": {
			mutate: func(p *policy.Policy) { p.MinRotationChances = new(3) },
			tgp:    &tgp,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pol := &policy.Policy{AgeThreshold: "auto", MaintenanceWindows: windows}
			pol.ApplyDefaults()
			tc.mutate(pol)
			pol.ApplyDefaults()
			if err := pol.Validate(); err != nil {
				t.Fatalf("policy: %v", err)
			}

			sched, err := window.New(pol.MaintenanceWindows)
			if err != nil {
				t.Fatalf("window.New: %v", err)
			}

			// Controller side: a NodePool carrying the same template values.
			pool := &karpv1.NodePool{}
			pool.Name = "pool"
			pool.Spec.Template.Spec.ExpireAfter = karpv1.NillableDuration{Duration: &e}
			if tc.tgp != nil {
				pool.Spec.Template.Spec.TerminationGracePeriod = &metav1.Duration{Duration: *tc.tgp}
			}

			const nodeCount = 5
			r := &RotationReconciler{}
			res := r.resolve(pool, pol, sched)
			p, _ := sched.WorstCasePeriod()
			d, _ := sched.ShortestWindow()
			var idleGap *time.Duration
			if g, ok := sched.ShortestIdleGap(); ok {
				idleGap = &g
			}
			derived := r.derivedThresholds(pool, res, p, d, idleGap, nodeCount)

			// Simulator side: the same NodePool as a Fleet.
			fleet := sim.Fleet{ExpireAfter: e, TGP: tc.tgp}
			for i := range nodeCount {
				fleet.Nodes = append(fleet.Nodes, sim.Node{Name: string(rune('a' + i))})
			}
			got, err := sim.Resolve(pol, sched, fleet)
			if err != nil {
				t.Fatalf("sim.Resolve: %v", err)
			}

			if got.LeadTime != res.leadTime {
				t.Errorf("leadTime = %+v, controller resolves %+v", got.LeadTime, res.leadTime)
			}
			if !eqDurPtr(got.Override, res.override) {
				t.Errorf("override = %v, controller resolves %v", fmtDurPtr(got.Override), fmtDurPtr(res.override))
			}
			if got.RetryBackoff != res.retryBackoff {
				t.Errorf("retryBackoff = %v, controller resolves %v", got.RetryBackoff, res.retryBackoff)
			}
			if got.ReadyTimeout != res.readyTimeout {
				t.Errorf("readyTimeout = %v, controller resolves %v", got.ReadyTimeout, res.readyTimeout)
			}
			if got.Cooldown != res.cooldown {
				t.Errorf("cooldown = %v, controller resolves %v", got.Cooldown, res.cooldown)
			}
			if got.FailurePause != res.failurePause {
				t.Errorf("failurePause = %v, controller resolves %v", got.FailurePause, res.failurePause)
			}
			if got.DrainBound != res.drainBound {
				t.Errorf("drainBound = %v, controller resolves %v", got.DrainBound, res.drainBound)
			}

			// The derivation itself: A, t_rot, t_rot_est, G, C and every finding.
			if got.Derived.A != derived.A || got.Derived.TRot != derived.TRot ||
				got.Derived.TRotEst != derived.TRotEst || got.Derived.G != derived.G ||
				got.Derived.C != derived.C ||
				got.Derived.DrainEstimate != derived.DrainEstimate ||
				got.Derived.ProvisioningEstimate != derived.ProvisioningEstimate {
				t.Errorf("derived = %+v, controller derives %+v", got.Derived, derived)
			}
			if !eqFindings(got.Derived.Findings, derived.Findings) {
				t.Errorf("findings = %+v, controller derives %+v", got.Derived.Findings, derived.Findings)
			}
		})
	}
}

func eqDurPtr(a, b *time.Duration) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func fmtDurPtr(d *time.Duration) string {
	if d == nil {
		return "<nil>"
	}
	return d.String()
}

func eqFindings(a, b []schedule.Finding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
