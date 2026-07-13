package sim

import (
	"fmt"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// Resolved is the per-NodePool resolution the decisions read: exactly what the reconcile
// loop's resolve() + derivedThresholds() compute, from the same policy fields and the
// same §3.2 formulas, but sourced from a Fleet instead of a karpv1.NodePool.
//
// The two resolutions MUST agree. A simulator that resolves leadTime, the drain bound or
// the failurePause default differently from the controller is a second implementation of
// the very thing this design exists to keep single. It is exported for that reason:
// internal/controller's sim-parity test pins the two against each other, so a change to
// either side fails there rather than silently making the simulator lie.
type Resolved struct {
	LeadTime     selection.LeadTime
	Override     *time.Duration // explicit ageThreshold; nil in auto mode
	RetryBackoff time.Duration
	ReadyTimeout time.Duration
	Cooldown     time.Duration
	FailurePause time.Duration
	DrainBound   time.Duration
	// Derived is schedule.Derive's output: A, t_rot, t_rot_est, G, C and the findings.
	Derived schedule.Result
}

// Resolve mirrors RotationReconciler.resolve + derivedThresholds for a simulated fleet.
func Resolve(pol *policy.Policy, sched *window.Schedule, f Fleet) (Resolved, error) {
	s := pol.Surge
	p, _ := sched.WorstCasePeriod()
	d, _ := sched.ShortestWindow()
	var idleGap *time.Duration
	if g, ok := sched.ShortestIdleGap(); ok {
		idleGap = &g
	}

	override, isAuto, err := pol.AgeThresholdOverride()
	if err != nil {
		return Resolved{}, fmt.Errorf("sim: invalid ageThreshold: %w", err)
	}
	var ov *time.Duration
	if !isAuto {
		ov = &override
	}

	// tGP is the NodePool template's; unset substitutes the fixed DrainFallback bound
	// (self-managed Karpenter allows nil), exactly as poolTGP does.
	tgp, unset := schedule.DrainFallback, true
	if f.TGP != nil {
		tgp, unset = *f.TGP, false
	}
	drainBound := tgp + schedule.Buffer
	if unset {
		drainBound = schedule.DrainFallback
	}

	// nil is meaningful for both estimates: schedule.Derive resolves an unset one to
	// min(tGP, DrainEstimateDefault) / min(readyTimeout, ProvisioningEstimateDefault).
	// Never fold either into leadTime or drainBound — they are forecast-side only.
	var drainEst, provEst *time.Duration
	if s.DrainEstimate != nil {
		drainEst = new(s.DrainEstimate.Duration)
	}
	if s.ProvisioningEstimate != nil {
		provEst = new(s.ProvisioningEstimate.Duration)
	}

	// gate B: an unset failurePause defaults to max(FailurePauseFloor, cooldownAfter)
	// so lowering cooldownAfter for throughput never silently shortens it (ADR-0004).
	failurePause := max(policy.FailurePauseFloor, s.CooldownAfter.Duration)
	if s.FailurePause != nil {
		failurePause = s.FailurePause.Duration
	}

	return Resolved{
		// Base omits tGP; LeadTime.For adds each claim's own terminationGracePeriod.
		LeadTime: selection.LeadTime{
			Base:          time.Duration(pol.K())*p + s.ReadyTimeout.Duration + schedule.Buffer,
			DrainFallback: schedule.DrainFallback,
		},
		Override:     ov,
		RetryBackoff: s.RetryBackoff.Duration,
		ReadyTimeout: s.ReadyTimeout.Duration,
		Cooldown:     s.CooldownAfter.Duration,
		FailurePause: failurePause,
		DrainBound:   drainBound,
		Derived: schedule.Derive(schedule.Inputs{
			E:                    f.ExpireAfter,
			TGP:                  tgp,
			TGPWasUnset:          unset,
			P:                    p,
			WindowLen:            d,
			IdleGap:              idleGap,
			ReadyTimeout:         s.ReadyTimeout.Duration,
			Cooldown:             s.CooldownAfter.Duration,
			DrainEstimate:        drainEst,
			ProvisioningEstimate: provEst,
			RetryBackoff:         s.RetryBackoff.Duration,
			K:                    pol.K(),
			MaxUnavailable:       pol.SurgeMaxUnavailable(),
			NodeCount:            len(f.Nodes),
			Override:             ov,
		}),
	}, nil
}
