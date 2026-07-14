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

// BirthMode says how a generation came into existence. A single "surgeless" boolean
// cannot distinguish an initial node from a surged replacement — both would be false.
type BirthMode string

const (
	BirthInitial   BirthMode = "initial"   // a node the fleet started with
	BirthSurge     BirthMode = "surge"     // a surged replacement, born at its rotation's start
	BirthSurgeless BirthMode = "surgeless" // a forceful-fallback replacement, born at its rotation's done
)

// DrainCapSource says where a generation's drain cap came from, so a consumer never has
// to re-derive schedule.DrainFallback for itself.
type DrainCapSource string

const (
	DrainCapExplicit DrainCapSource = "explicit" // the node's own terminationGracePeriod
	DrainCapFallback DrainCapSource = "fallback" // tGP unset — schedule.DrainFallback
)

// Generation is one generation of one fleet slot: everything a consumer needs to place it
// on a timeline without re-implementing policy semantics.
//
// It exists because the event stream cannot carry these facts. node-ready is emitted under
// the OLD node's name (the generation has not incremented yet), so a replacement's own
// ready instant is not recoverable from the events; and the eligibility boundary depends on
// the claim's own tGP, which no event carries.
type Generation struct {
	Slot int    // the fleet slot; the slot's node count is constant, generations relay along it
	Gen  int    // 0 for an initial node, incrementing once per completed rotation
	Name string // the node name at this generation ("node-a", "node-a-r1", …)
	// BirthMode is how this generation was created.
	BirthMode BirthMode
	// PredecessorGen is the generation this one replaced; nil for an initial node. A
	// pointer, not a sentinel: 0 is a valid predecessor.
	PredecessorGen *int
	CreatedAt      time.Time
	// ExpireAfter is the EFFECTIVE value: the declared node's override, else the NodePool
	// template's (a replacement is provisioned from the template, so it inherits it).
	ExpireAfter time.Duration
	// DrainCap is the bound this generation's drain is actually held to — the instant
	// Karpenter force-completes it — and DrainCapSource says where it came from.
	DrainCap       time.Duration
	DrainCapSource DrainCapSource
	// Deadline is CreatedAt + ExpireAfter: the instant Karpenter's forceful expiration
	// takes the node if the rotation has not completed.
	Deadline time.Time
	// EligibilityBoundary is the instant this generation must be STRICTLY past before it
	// can be picked. Deliberately not an "eligibleAt": selection.triggered is a strict
	// inequality (age > expireAfter − leadTime), so no first eligible instant exists —
	// only a boundary the claim must be past. It is per-generation because the lead time
	// adds the claim's OWN tGP; Result.A is a template-derived representative and must
	// never be presented as a per-node fact.
	EligibilityBoundary time.Time
	// ReadyAt is when this generation's node became Ready. Set only on the surge path
	// (BirthSurge); zero while the surge is still provisioning, and zero for the initial
	// and surge-less births, which stage no surge at all.
	ReadyAt time.Time
	// Provisional marks a generation whose rotation has not completed: the replacement
	// NodeClaim exists (its CreatedAt is the rotation's start) but the old node has not
	// been retired yet. Without it, a surge still in flight at the horizon's end would
	// produce no record at all — and the make-before-break overlap, the whole point of the
	// timeline, would vanish exactly in the case a reader most wants to see.
	Provisional bool
}

// RotationMode is the path a rotation took.
type RotationMode string

const (
	RotationSurge     RotationMode = "surge"
	RotationSurgeless RotationMode = "surgeless"
)

// Rotation relates two generations of a slot, so a consumer never has to pair events to
// find the relay.
type Rotation struct {
	Slot    int
	FromGen int
	// ToGen is the generation this rotation produced; nil while it does not exist yet.
	//
	// On the surge path the replacement is born AT the start (its CreatedAt is the
	// rotation-start instant), so ToGen names a provisional generation from the start. On
	// the SURGE-LESS path there is no placeholder: the replacement is born at Done, so
	// until then no such generation exists and ToGen is nil. Naming a generation that the
	// simulation has not created — with a CreatedAt it never reached — would report time
	// that was never simulated, the very defect the partial-run sweep fixes.
	ToGen *int
	Mode  RotationMode
	Start time.Time
	// Ready is the instant the surge node became Ready and the old NodeClaim was deleted;
	// zero while the surge is in flight, and always zero on the surge-less path.
	Ready time.Time
	// Done is the instant the old node finished draining; zero while the rotation is in
	// flight — absent, not zero-length.
	Done time.Time
}

// WindowInterval is one OBSERVED occurrence of the effective maintenance-window schedule
// (the union of every policy entry, each evaluated in its own timezone).
//
// The clipped flags mark a boundary that is an artifact of the horizon rather than a real
// transition of the schedule. They live on the interval, not on the events, because
// clipping is symmetric: an event boolean could express a clipped start but not a clipped
// end, and it would leave the consumer pairing events itself.
type WindowInterval struct {
	Start time.Time
	End   time.Time
	// StartClipped: the simulation began inside an already-open window. It is NOT simply
	// InWindow(Start) — the horizon may legitimately begin exactly at a union boundary,
	// which is a real opening. The condition is InWindow(Start) && InWindow(Start−1ns).
	StartClipped bool
	// EndClipped: the window was still open at SimulatedThrough. An interval that genuinely
	// closed exactly at SimulatedThrough is not clipped.
	EndClipped bool
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
	// Generations, Rotations and Windows are the derived structure of the run: the facts
	// a consumer would otherwise have to re-derive from the events by re-implementing
	// policy semantics. Events stays as the backwards-compatible contract; these are
	// additive, and the two agree wherever they overlap.
	Generations []Generation
	Rotations   []Rotation
	Windows     []WindowInterval
	// SimulatedThrough is the last instant the loop actually PROCESSED. On a normal run it
	// is Options.End; when the step budget is exhausted it is earlier. Nothing in the
	// timeline — no event At/Until, no window End — lies beyond it: a partial run that
	// reported breaches and interval ends from time it never reached would be a response
	// that contradicts itself.
	SimulatedThrough time.Time
	// Result is the derivation the controller would compute and export for this policy
	// (A, t_rot, t_rot_est, G, C and the feasibility findings) — the header strip. It is
	// policy-derived, so it does NOT follow Env.
	Result schedule.Result
	// Inputs is what Result was derived FROM (schedule.Inputs). Result alone cannot explain
	// itself: P, D and the tGP fallback are resolved from the schedule and the template, not
	// stated in the policy.
	Inputs schedule.Inputs
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

	tl := Timeline{Result: res.Derived, Inputs: res.Inputs}

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
	r.tl = tl // before initFleet: the initial generations are recorded on it
	r.initFleet(f)
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
	// rotIdx and genIdx point at the records this rotation completes in place, as its
	// instants become facts: Timeline.Rotations[rotIdx], and the provisional replacement
	// Timeline.Generations[genIdx] (-1 on the surge-less path, which materializes its
	// replacement only at done).
	rotIdx int
	genIdx int
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

	// winOpen says whether the last Timeline.Windows entry is still open.
	winOpen bool
}

func (r *run) initFleet(f Fleet) {
	r.template = f
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
	for i := range r.nodes {
		r.recordGeneration(i, &r.nodes[i], BirthInitial, nil, false)
	}
}

// recordGeneration appends the record for the generation currently occupying a slot and
// returns its index. Every generation — initial, surged, surge-less — goes through here,
// so a provisional record and the completed one it becomes cannot drift apart.
func (r *run) recordGeneration(slot int, n *simNode, birth BirthMode, predecessor *int, provisional bool) int {
	bound, src := r.drainCap(n)
	r.tl.Generations = append(r.tl.Generations, Generation{
		Slot:                slot,
		Gen:                 n.gen,
		Name:                n.name(),
		BirthMode:           birth,
		PredecessorGen:      predecessor,
		CreatedAt:           n.createdAt,
		ExpireAfter:         n.expireAfter,
		DrainCap:            bound,
		DrainCapSource:      src,
		Deadline:            n.deadline(),
		EligibilityBoundary: r.eligibilityBoundary(n),
		Provisional:         provisional,
	})
	return len(r.tl.Generations) - 1
}

// eligibilityBoundary is the instant a node must be strictly past to be picked, read off
// selection's own trigger (selection.triggered, spec §3.2): an explicit ageThreshold is an
// age offset from CreatedAt; auto mode is the node's deadline minus ITS OWN lead time —
// LeadTime.For adds the claim's tGP, which is why this is a per-generation fact.
func (r *run) eligibilityBoundary(n *simNode) time.Time {
	if r.res.Override != nil {
		return n.createdAt.Add(*r.res.Override)
	}
	c := selection.Claim{TGP: n.tgp}
	return n.deadline().Add(-r.res.LeadTime.For(&c))
}

// drainCap is the bound the node's drain is held to — the deadline Karpenter
// force-completes a drain at — resolved exactly as selection.LeadTime.For resolves it.
func (r *run) drainCap(n *simNode) (time.Duration, DrainCapSource) {
	if n.tgp != nil {
		return *n.tgp, DrainCapExplicit
	}
	return schedule.DrainFallback, DrainCapFallback
}

// openWindow / closeWindow record the observed occurrences of the window schedule
// alongside the window-open / window-close events.
func (r *run) openWindow(at time.Time) {
	r.tl.Windows = append(r.tl.Windows, WindowInterval{
		Start: at,
		// A start inside a window that was ALREADY open one nanosecond earlier is the
		// horizon cutting into an occurrence; a start exactly at a union boundary is a real
		// opening. InWindow(at) alone cannot tell them apart.
		StartClipped: r.sched.InWindow(at) && r.sched.InWindow(at.Add(-time.Nanosecond)),
	})
	r.winOpen = true
}

func (r *run) closeWindow(at time.Time, clipped bool) {
	if !r.winOpen {
		return
	}
	w := &r.tl.Windows[len(r.tl.Windows)-1]
	w.End, w.EndClipped = at, clipped
	r.winOpen = false
}

// drainFor is how long the old node's drain actually takes: Env.Drain, clamped to the
// node's own terminationGracePeriod — the deadline Karpenter force-completes a drain
// at, resolved exactly as selection.LeadTime.For resolves it (unset → DrainFallback).
// A heterogeneous fleet therefore clamps per node.
func (r *run) drainFor(n *simNode) time.Duration {
	bound, _ := r.drainCap(n)
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
