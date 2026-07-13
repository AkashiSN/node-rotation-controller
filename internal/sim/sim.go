// Package sim is the rotation-policy simulator core (spec §3.2, §5.2): it advances
// a virtual clock over a fleet and reports which node rotates in which maintenance
// window, and whether every node makes it before its expireAfter backstop fires.
//
// It is NOT a second implementation of the controller's decisions. Every decision —
// may a rotation start now, which claim, is it a surge-less forceful fallback — is
// made by calling internal/decide and internal/selection, the same functions the
// reconcile loop calls. sim owns exactly two things the controller gets from the
// cluster instead: the clock, and Env (how long provisioning and draining actually
// take in the virtual world).
//
// The package is pure — stdlib plus the pure decision layer, no Karpenter or
// Kubernetes types — so it links into the GOOS=js wasm policy simulator and, later,
// an `nrc simulate` CLI.
//
// # What is modelled
//
// Rotation start, surge provisioning, drain and completion, including the opt-in
// window-bounded surge-less forceful fallback (ADR-0001) — a deterministic
// deadline-race branch, not a failure branch. Refusing to model it would leave those
// nodes un-rotated and paint them as expire-after-breach, i.e. report a false broken
// policy.
//
// # What is not modelled
//
// Failures. Nothing in sim fails, so readyTimeout expiry, retryBackoff and the failed
// state never occur. Note the precise consequence: surge.failurePause is not
// "ignored" — no last-failure-at annotation is ever written, so its gate is never
// reached. Cluster-side limits (the §5.2 headroom gate) are likewise absent: sim has
// no NodePool spec.limits. Deterministic failure injection is a follow-up.
package sim

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/decide"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// tick is the virtual clock's decision cadence: the grid on which sim asks "may a
// rotation start now?". It stands in for the controller's requeue cadence. Maintenance
// window bounds are HH:MM, so a one-minute grid lands exactly on every window edge;
// completions of provisioning and drain are processed at their exact instants, off the
// grid, so a sub-minute Env is not rounded away.
const tick = time.Minute

// maxSteps bounds the loop so a pathological input (a multi-year horizon) cannot spin
// forever in a browser tab. Reaching it truncates the timeline and sets Partial.
const maxSteps = 2_000_000

// Env is the virtual world's ACTUAL durations — the assumptions the operator enters.
// It is deliberately not the policy's forecast estimates: surge.provisioningEstimate
// and surge.drainEstimate are inputs to the layer-2 throughput forecast (they produce
// t_rot_est and C — ADR-0002, ADR-0003), whereas Env decides when node-ready and
// rotation-done actually fire. A zero field defaults to the corresponding resolved
// policy estimate, so an untouched simulation is self-consistent; moving them apart is
// the interesting case — a policy whose C is optimistic because its estimates are too
// low still derives the same header values while the timeline tells the truth.
type Env struct {
	// Provisioning is the time from the replacement NodeClaim's creation to the new
	// node being Ready. Above surge.readyTimeout the real surge would be abandoned —
	// the failure path, which sim does not model.
	Provisioning time.Duration
	// Drain is the time to drain the old node once its NodeClaim is deleted. It is
	// clamped per node to that node's terminationGracePeriod, the deadline Karpenter
	// force-completes a drain at (DrainFallback when unset), so a longer value is
	// unreachable in reality.
	Drain time.Duration
}

// Node is one node of the simulated fleet. ExpireAfter and TGP override the Fleet
// template for this node (a heterogeneous fleet, issue #157); nil inherits it.
type Node struct {
	Name        string
	CreatedAt   time.Time
	ExpireAfter *time.Duration
	TGP         *time.Duration
}

// Fleet is the simulated NodePool: the template values every claim inherits, plus the
// nodes themselves. ExpireAfter and TGP stand in for the NodePool's
// spec.template.spec fields — schedule.Derive needs a representative E and tGP (the
// controller reads them off the template), and a replacement node is provisioned from
// the template, so it inherits these and not the old node's per-node overrides.
type Fleet struct {
	ExpireAfter time.Duration  // NodePool template expireAfter; must be positive
	TGP         *time.Duration // NodePool template terminationGracePeriod; nil = unset
	Nodes       []Node
}

// Options bounds the simulated horizon, [Start, End].
type Options struct {
	Start time.Time
	End   time.Time
}

// Kind names an Event. The values reach the UI — treat them as a public surface.
type Kind string

const (
	KindWindowOpen        Kind = "window-open"
	KindWindowClose       Kind = "window-close"
	KindRotationStart     Kind = "rotation-start"
	KindNodeReady         Kind = "node-ready"
	KindRotationDone      Kind = "rotation-done"
	KindExpireAfterBreach Kind = "expire-after-breach"
	KindBlockedByGate     Kind = "blocked-by-gate"
	KindNoEligibleClaim   Kind = "no-eligible-claim"
)

// Event is one thing that happened on the virtual clock.
//
// KindBlockedByGate and KindNoEligibleClaim are EDGE-TRIGGERED, not level-triggered:
// one is emitted when the gate reason (respectively the census) changes, and Until
// carries the end of the interval it covers. A week-long out-of-window stretch is one
// event, not one per tick.
type Event struct {
	Kind Kind
	At   time.Time
	// Until is the end of a coalesced interval (KindBlockedByGate, KindNoEligibleClaim);
	// zero for an instantaneous event.
	Until time.Time
	// Node is the node the event is about: the rotating node for the rotation events,
	// the breaching node for KindExpireAfterBreach; empty otherwise.
	Node string
	// Replacement is the name of the node that replaced Node (KindRotationDone).
	Replacement string
	// Surgeless marks a rotation that took the window-bounded forceful-fallback path
	// (KindRotationStart): no placeholder, no surge, drain only.
	Surgeless bool
	// Gate is the first start gate that said no (KindBlockedByGate).
	Gate decide.Gate
	// Census says why no claim was eligible (KindNoEligibleClaim).
	Census *selection.Census
}

// Diagnostic explains something the timeline itself cannot: an input clamped, a path
// not modelled, a policy whose findings forbid any rotation at all.
type Diagnostic struct {
	Severity schedule.Severity
	Code     string
	Message  string
}

// Timeline is a simulation result.
type Timeline struct {
	Events []Event
	// Result is the derivation the controller would compute and export for this policy
	// (A, t_rot, t_rot_est, G, C and the feasibility findings) — the header strip. It is
	// policy-derived, so it does NOT follow Env.
	Result schedule.Result
	// Diagnostics is why the timeline looks the way it does. A Fatal finding, an
	// unreachable input and an unmodelled path all land here rather than in error, so
	// the UI can always render something and say why.
	Diagnostics []Diagnostic
	// Partial marks a timeline that stops short of the horizon or of the truth: an
	// unmodelled path was reached, or the step budget ran out.
	Partial bool
}

// Run simulates the fleet over the horizon.
//
// It returns error only for input it cannot run at all — a policy that does not
// validate, an empty or inverted horizon, a negative Env, a non-positive template
// expireAfter. Everything else is a Diagnostic.
func Run(p *policy.Policy, f Fleet, env Env, o Options) (Timeline, error) {
	if p == nil {
		return Timeline{}, errors.New("sim: policy is nil")
	}
	if !o.End.After(o.Start) {
		return Timeline{}, fmt.Errorf("sim: options end %s must be after start %s", o.End, o.Start)
	}
	if env.Provisioning < 0 || env.Drain < 0 {
		return Timeline{}, fmt.Errorf("sim: env durations must be non-negative, got provisioning=%v drain=%v", env.Provisioning, env.Drain)
	}
	if f.ExpireAfter <= 0 {
		return Timeline{}, fmt.Errorf("sim: fleet expireAfter must be positive, got %v", f.ExpireAfter)
	}

	// Default and validate a COPY: the caller's Policy is not ours to mutate, and the
	// browser hands us whatever the YAML said. ApplyDefaults only assigns fresh
	// pointers, so the copy is deep enough.
	pol := *p
	pol.ApplyDefaults()
	if err := pol.Validate(); err != nil {
		return Timeline{}, fmt.Errorf("sim: invalid policy: %w", err)
	}

	sched, err := window.New(pol.MaintenanceWindows)
	if err != nil {
		return Timeline{}, fmt.Errorf("sim: invalid maintenance windows: %w", err)
	}

	res, err := Resolve(&pol, sched, f)
	if err != nil {
		return Timeline{}, err
	}

	tl := Timeline{Result: res.Derived}

	// Env defaults to the policy's resolved forecast estimates, so an untouched
	// simulation is self-consistent (§Env).
	if env.Provisioning == 0 {
		env.Provisioning = res.Derived.ProvisioningEstimate
	}
	if env.Drain == 0 {
		env.Drain = res.Derived.DrainEstimate
	}

	// A provision slower than readyTimeout means the surge attempt is ABANDONED — the
	// failure path, which is not modelled. Refuse to run the timeline rather than
	// pretend every rotation succeeds; the header strip still renders.
	if env.Provisioning > res.ReadyTimeout {
		tl.Partial = true
		tl.Diagnostics = append(tl.Diagnostics, Diagnostic{
			Severity: schedule.Fatal,
			Code:     "EnvProvisioningAboveReadyTimeout",
			Message: fmt.Sprintf("provisioning %v exceeds surge.readyTimeout %v: the surge attempt would be abandoned and the rotation would fail. Failure paths (retryBackoff, failurePause) are not modelled, so no timeline is produced",
				env.Provisioning, res.ReadyTimeout),
		})
		return tl, nil
	}

	r := &run{
		pol:     &pol,
		sched:   sched,
		res:     res,
		env:     env,
		opts:    o,
		poolAnn: map[string]string{},
	}
	r.initFleet(f)
	r.tl = tl
	r.loop()
	sort.SliceStable(r.tl.Events, func(i, j int) bool { return r.tl.Events[i].At.Before(r.tl.Events[j].At) })
	return r.tl, nil
}

// simNode is a fleet slot. A rotation replaces the node in the slot 1:1, so the fleet
// size is constant and gen counts how many generations the slot has been through.
type simNode struct {
	base        string
	gen         int
	createdAt   time.Time
	expireAfter time.Duration
	tgp         *time.Duration
	state       string // "", annotations.StatePending, annotations.StateDraining
	breached    bool
}

func (n *simNode) name() string {
	if n.gen == 0 {
		return n.base
	}
	return fmt.Sprintf("%s-r%d", n.base, n.gen)
}

func (n *simNode) deadline() time.Time { return n.createdAt.Add(n.expireAfter) }

// rotation is the single in-flight rotation (v1 is serial per NodePool:
// surge.maxUnavailable == 1, enforced by policy.Validate).
type rotation struct {
	slot      int
	start     time.Time
	readyAt   time.Time // surge path only
	doneAt    time.Time
	surgeless bool
}

type run struct {
	pol   *policy.Policy
	sched *window.Schedule
	res   Resolved
	env   Env
	opts  Options
	// template carries the NodePool template values a REPLACEMENT node inherits — it is
	// provisioned from the NodePool, so it does not inherit the old node's per-node
	// expireAfter/tGP overrides.
	template Fleet
	nodes    []simNode
	poolAnn  map[string]string
	rot      *rotation
	tl       Timeline

	// coalescing state for the two edge-triggered events
	blockFrom  time.Time
	blockGate  decide.Gate
	censusFrom time.Time
	census     selection.Census
	hasCensus  bool
}

func (r *run) initFleet(f Fleet) {
	r.nodes = make([]simNode, 0, len(f.Nodes))
	for _, n := range f.Nodes {
		sn := simNode{base: n.Name, createdAt: n.CreatedAt, expireAfter: f.ExpireAfter, tgp: f.TGP}
		if n.ExpireAfter != nil {
			sn.expireAfter = *n.ExpireAfter
		}
		if n.TGP != nil {
			sn.tgp = n.TGP
		}
		r.nodes = append(r.nodes, sn)
	}
	r.template = f
}

// drainFor is how long the old node's drain actually takes: Env.Drain, clamped to the
// node's own terminationGracePeriod — the deadline Karpenter force-completes a drain
// at, resolved exactly as selection.LeadTime.For resolves it (unset → DrainFallback).
// A heterogeneous fleet therefore clamps per node.
func (r *run) drainFor(n *simNode) time.Duration {
	bound := schedule.DrainFallback
	if n.tgp != nil {
		bound = *n.tgp
	}
	if r.env.Drain > bound {
		r.diagOnce(Diagnostic{
			Severity: schedule.Warn,
			Code:     "EnvDrainAboveTGP",
			Message: fmt.Sprintf("drain %v exceeds node %s's terminationGracePeriod %v, the deadline Karpenter force-completes a drain at; a drain can never take longer, so the timeline uses %v",
				r.env.Drain, n.name(), bound, bound),
		})
		return bound
	}
	return r.env.Drain
}

func (r *run) diagOnce(d Diagnostic) {
	for _, e := range r.tl.Diagnostics {
		if e.Code == d.Code && e.Message == d.Message {
			return
		}
	}
	r.tl.Diagnostics = append(r.tl.Diagnostics, d)
}

func (r *run) emit(e Event) { r.tl.Events = append(r.tl.Events, e) }
