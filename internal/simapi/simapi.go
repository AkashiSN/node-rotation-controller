// Package simapi is the wire format of the browser policy simulator: it decodes
// a RotationPolicy manifest and a simulation request, runs internal/sim, and
// encodes the timeline as JSON.
//
// It is the whole of cmd/wasm's logic. cmd/wasm itself is only syscall/js glue,
// which cannot be built or tested on a host GOOS — so everything worth testing
// lives here, in a pure package that `go test ./...` covers.
//
// The policy travels as YAML and goes through the CONTROLLER'S OWN path
// (crd.ToPolicy → policy.ApplyDefaults → policy.Validate), so the defaults the
// page shows and the errors it reports are the ones a cluster would produce.
// Everything else travels as one JSON request object.
//
// # Wire shapes
//
// Durations are Go duration strings ("5m", "1h30m"); instants are RFC3339. The
// design sketch's signature was simulate(policyYAML, fleetJSON, envJSON), which
// has no room for the horizon — rather than smuggle Options into the env object,
// the three non-YAML inputs travel together as Request{fleet, env, options}.
package simapi

import (
	"encoding/json"
	"fmt"
	"time"

	"sigs.k8s.io/yaml"

	nrv1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/crd"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/sim"
)

// Request is everything the simulator needs besides the policy YAML.
type Request struct {
	Fleet   Fleet   `json:"fleet"`
	Env     Env     `json:"env"`
	Options Options `json:"options"`
}

// Fleet is the simulated NodePool: the template values every node inherits, plus
// the nodes themselves.
type Fleet struct {
	// ExpireAfter is the NodePool template's expireAfter. Required and positive:
	// it is the backstop the whole simulation is about.
	ExpireAfter string `json:"expireAfter"`
	// TerminationGracePeriod is the NodePool template's terminationGracePeriod;
	// empty means unset, and the drain bound falls back to schedule.DrainFallback.
	TerminationGracePeriod string `json:"terminationGracePeriod,omitempty"`
	Nodes                  []Node `json:"nodes"`
}

// Node is one node of the fleet. ExpireAfter and TerminationGracePeriod override
// the Fleet template for this node (a heterogeneous fleet); empty inherits it.
type Node struct {
	Name                   string `json:"name"`
	CreatedAt              string `json:"createdAt"`
	ExpireAfter            string `json:"expireAfter,omitempty"`
	TerminationGracePeriod string `json:"terminationGracePeriod,omitempty"`
}

// Env is the virtual world's ACTUAL durations, NOT the policy's forecast
// estimates (surge.provisioningEstimate / surge.drainEstimate produce t_rot_est
// and C; these decide when node-ready and rotation-done fire). An empty field
// defaults to the corresponding resolved policy estimate, so an untouched
// simulation is self-consistent; moving them apart is the interesting case.
type Env struct {
	Provisioning string `json:"provisioning,omitempty"`
	Drain        string `json:"drain,omitempty"`
}

// Options bounds the simulated horizon, [Start, End].
type Options struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Response is the JSON the page renders. Error is set only for input that cannot
// be run at all — an unparseable policy, a policy the controller would reject, a
// malformed request. Everything else (an unmodelled path, a clamped input, a
// policy whose findings forbid any rotation) is a Diagnostic, so the page can
// always render the header strip and say why the timeline looks as it does.
type Response struct {
	Error  string  `json:"error,omitempty"`
	Result *Result `json:"result,omitempty"`
	Events []Event `json:"events,omitempty"`
	// Generations, Rotations and Windows are the run's derived structure — the facts a
	// consumer cannot honestly recover from the events (see the types). Events remains the
	// backwards-compatible contract: a page that knows nothing of these fields keeps
	// working, and the two representations agree wherever they overlap.
	Generations []Generation     `json:"generations,omitempty"`
	Rotations   []Rotation       `json:"rotations,omitempty"`
	Windows     []WindowInterval `json:"windows,omitempty"`
	Diagnostics []Diagnostic     `json:"diagnostics,omitempty"`
	Partial     bool             `json:"partial"`
	// SimulatedThrough (RFC3339) is the last instant the simulation actually PROCESSED —
	// Options.End on a normal run, earlier when the step budget was exhausted. Nothing in
	// the response lies beyond it. It is absent only when no timeline was produced at all.
	SimulatedThrough string `json:"simulatedThrough,omitempty"`
}

// Generation is one generation of one fleet slot. A slot's node count is constant and its
// generations relay along it, so a consumer draws one row per slot — not one per node name.
type Generation struct {
	Slot      int    `json:"slot"`
	Gen       int    `json:"gen"`
	Name      string `json:"name"`
	BirthMode string `json:"birthMode"` // "initial" | "surge" | "surgeless"
	// PredecessorGen is absent for an initial node. A pointer, not a sentinel: generation 0
	// is a valid predecessor and must serialize as 0, not be dropped.
	PredecessorGen *int   `json:"predecessorGen,omitempty"`
	CreatedAt      string `json:"createdAt"`
	ExpireAfter    string `json:"expireAfter"`
	// DrainCap is the bound this node's drain is held to; DrainCapSource says whether it is
	// the node's own terminationGracePeriod or the fixed fallback, so the consumer never
	// re-derives that constant.
	DrainCap       string `json:"drainCap"`
	DrainCapSource string `json:"drainCapSource"` // "explicit" | "fallback"
	Deadline       string `json:"deadline"`
	// EligibilityBoundary is EXCLUSIVE: the trigger is a strict inequality, so a node
	// exactly at this instant is not yet eligible. Label it "eligible after", never as an
	// event.
	EligibilityBoundary string `json:"eligibilityBoundary"`
	// ReadyAt is set only for a surged replacement that became Ready; absent while it is
	// still provisioning, and absent for the initial and surge-less births.
	ReadyAt string `json:"readyAt,omitempty"`
	// Provisional marks a replacement whose rotation has not completed. Omitted when false:
	// a consumer treats missing as false.
	Provisional bool `json:"provisional,omitempty"`
}

// Rotation relates two generations of a slot, so no consumer pairs events to find it.
type Rotation struct {
	Slot    int `json:"slot"`
	FromGen int `json:"fromGen"`
	// ToGen is absent while the produced generation does not exist yet — a surge-less
	// rotation still draining at simulatedThrough has staged no replacement, and naming one
	// would assert a node the simulation never created.
	ToGen *int   `json:"toGen,omitempty"`
	Mode  string `json:"mode"` // "surge" | "surgeless"
	Start string `json:"start"`
	// Ready and Done are ABSENT while the rotation is in flight — absent, not zero-length.
	// Ready is always absent on the surge-less path, which stages no surge.
	Ready string `json:"ready,omitempty"`
	Done  string `json:"done,omitempty"`
}

// WindowInterval is one observed occurrence of the effective (union) window schedule.
// The clipped flags mark a boundary that is an artifact of the horizon rather than a real
// transition; both are omitted when false, and a consumer treats missing as false.
type WindowInterval struct {
	Start        string `json:"start"`
	End          string `json:"end"`
	StartClipped bool   `json:"startClipped,omitempty"`
	EndClipped   bool   `json:"endClipped,omitempty"`
}

// Result is the derivation the controller would compute and export for this
// policy (spec §3.2) — the page's header strip. It is policy-derived and does
// NOT follow Env.
type Result struct {
	AgeThreshold         string    `json:"ageThreshold"` // A
	TRot                 string    `json:"tRot"`         // deadline-side rotation cost bound
	TRotEstimate         string    `json:"tRotEstimate"` // layer-2 forecast (ADR-0003)
	DrainEstimate        string    `json:"drainEstimate"`
	ProvisioningEstimate string    `json:"provisioningEstimate"`
	G                    int       `json:"g"`                // guaranteed rotation chances
	C                    int       `json:"c"`                // throughput per window occurrence
	Inputs               *Inputs   `json:"inputs,omitempty"` // what the above was derived FROM (#266)
	Findings             []Finding `json:"findings,omitempty"`
}

// Inputs is what Result was derived FROM (schedule.Inputs) — the values the page substitutes
// into the formulas beside each symbol (#266). It is here because the page must NOT re-derive
// them: P (the worst-case period between window occurrences) and D (one occurrence's length)
// are resolved from the schedule, and TGP silently falls back to schedule.DrainFallback when
// the template leaves terminationGracePeriod unset — none of the three is written in the
// manifest a visitor is reading.
//
// ProvisioningEstimate and DrainEstimate are deliberately NOT repeated here: Result already
// carries them, resolved.
type Inputs struct {
	E             string `json:"e"`             // expireAfter (the fleet template's representative value)
	TGP           string `json:"tgp"`           // terminationGracePeriod, fallback already applied
	TGPIsFallback bool   `json:"tgpFallback"`   // the value above is schedule.DrainFallback, not the operator's
	P             string `json:"p"`             // worst-case window period
	WindowLen     string `json:"windowLen"`     // D
	Buffer        string `json:"buffer"`        // the fixed slack in t_rot; the page must not re-declare the constant
	ReadyTimeout  string `json:"readyTimeout"`  // surge.readyTimeout, resolved
	Cooldown      string `json:"cooldownAfter"` // surge.cooldownAfter, resolved
	K             int    `json:"k"`             // minRotationChances
	M             int    `json:"m"`             // surge.maxUnavailable
	// AgeThresholdOverride marks an A that was GIVEN, not derived: A = E − (K·P + t_rot) does
	// not hold for it, and a page that printed that equation anyway would be lying.
	AgeThresholdOverride bool `json:"ageThresholdOverride"`
}

// Finding is one feasibility result from schedule.Derive, with its English
// message verbatim — the page shows the controller's own words.
type Finding struct {
	Severity string `json:"severity"` // "warn" | "fatal"
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// Event is one thing that happened on the virtual clock. Kind values are
// sim.Kind verbatim.
//
// blocked-by-gate and no-eligible-claim are EDGE-TRIGGERED: one event per change
// of the reason (respectively the census), with Until carrying the end of the
// interval it covers. A week-long out-of-window stretch is one event.
type Event struct {
	Kind        string  `json:"kind"`
	At          string  `json:"at"`
	Until       string  `json:"until,omitempty"`
	Node        string  `json:"node,omitempty"`
	Replacement string  `json:"replacement,omitempty"`
	Surgeless   bool    `json:"surgeless,omitempty"`
	Gate        string  `json:"gate,omitempty"`
	Census      *Census `json:"census,omitempty"`
}

// Census says why no claim was eligible: the breakdown by the first eligibility
// check each one failed.
type Census struct {
	Total        int `json:"total"`
	Eligible     int `json:"eligible"`
	OptedOut     int `json:"optedOut"`
	Deleting     int `json:"deleting"`
	NotReady     int `json:"notReady"`
	InFlight     int `json:"inFlight"`
	Terminal     int `json:"terminal"`
	InBackoff    int `json:"inBackoff"`
	NotTriggered int `json:"notTriggered"`
}

// Diagnostic explains something the timeline cannot: an input clamped, a path not
// modelled, a policy whose findings forbid any rotation at all.
type Diagnostic struct {
	Severity string `json:"severity"` // "warn" | "fatal"
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// Simulate is the wasm entry point. It never fails: input it cannot run comes
// back as Response.Error, so the caller always has a JSON object to render.
func Simulate(policyYAML, requestJSON string) []byte {
	resp, err := simulate(policyYAML, requestJSON)
	if err != nil {
		resp = Response{Error: err.Error()}
	}
	out, err := json.Marshal(resp)
	if err != nil {
		// Unreachable: every field is a plain scalar, slice or struct of them.
		return fmt.Appendf(nil, `{"error":%q}`, "encoding the result failed: "+err.Error())
	}
	return out
}

func simulate(policyYAML, requestJSON string) (Response, error) {
	pol, err := decodePolicy(policyYAML)
	if err != nil {
		return Response{}, err
	}

	var req Request
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return Response{}, fmt.Errorf("request is not valid JSON: %w", err)
	}
	fleet, env, opts, err := req.decode()
	if err != nil {
		return Response{}, err
	}

	tl, err := sim.Run(pol, fleet, env, opts)
	if err != nil {
		return Response{}, err
	}
	return toResponse(tl), nil
}

// kind is the only kind this boundary runs. The apiVersion it pairs with is
// nrv1.GroupVersion, so a manifest that names another API is rejected here rather
// than simulated as if the cluster would have admitted it.
const kind = "RotationPolicy"

// decodePolicy takes the RotationPolicy manifest an operator would apply and runs
// it through the controller's own conversion, defaulting and validation. It is as
// strict as admission would be: the apiVersion and kind must be this CRD's, and a
// misspelled field is an error rather than a value silently defaulted away.
func decodePolicy(policyYAML string) (*policy.Policy, error) {
	var rp nrv1.RotationPolicy
	if err := yaml.UnmarshalStrict([]byte(policyYAML), &rp); err != nil {
		return nil, fmt.Errorf("policy YAML is not a valid RotationPolicy: %w", err)
	}
	if want := nrv1.GroupVersion.String(); rp.APIVersion != want {
		return nil, fmt.Errorf("policy apiVersion is %q, want %s: the simulator runs the RotationPolicy CRD, and a cluster would not admit this manifest either", rp.APIVersion, want)
	}
	if rp.Kind != kind {
		return nil, fmt.Errorf("policy kind is %q, want %s", rp.Kind, kind)
	}
	p, err := crd.ToPolicy(rp.Spec)
	if err != nil {
		return nil, fmt.Errorf("policy is invalid: %w", err)
	}
	return p, nil
}

func (r Request) decode() (sim.Fleet, sim.Env, sim.Options, error) {
	var (
		fleet sim.Fleet
		env   sim.Env
		opts  sim.Options
	)

	expireAfter, err := duration("fleet.expireAfter", r.Fleet.ExpireAfter)
	if err != nil {
		return fleet, env, opts, err
	}
	if expireAfter == nil {
		return fleet, env, opts, fmt.Errorf("fleet.expireAfter is required: it is the backstop the simulation is about")
	}
	tgp, err := duration("fleet.terminationGracePeriod", r.Fleet.TerminationGracePeriod)
	if err != nil {
		return fleet, env, opts, err
	}
	fleet = sim.Fleet{ExpireAfter: *expireAfter, TGP: tgp}

	for i, n := range r.Fleet.Nodes {
		node := sim.Node{Name: n.Name}
		field := fmt.Sprintf("fleet.nodes[%d]", i)
		if node.CreatedAt, err = instant(field+".createdAt", n.CreatedAt); err != nil {
			return fleet, env, opts, err
		}
		if node.ExpireAfter, err = duration(field+".expireAfter", n.ExpireAfter); err != nil {
			return fleet, env, opts, err
		}
		if node.TGP, err = duration(field+".terminationGracePeriod", n.TerminationGracePeriod); err != nil {
			return fleet, env, opts, err
		}
		fleet.Nodes = append(fleet.Nodes, node)
	}

	// An omitted Env field stays zero, which sim reads as "use the policy's own
	// resolved estimate" — not as "instantaneous".
	prov, err := duration("env.provisioning", r.Env.Provisioning)
	if err != nil {
		return fleet, env, opts, err
	}
	drain, err := duration("env.drain", r.Env.Drain)
	if err != nil {
		return fleet, env, opts, err
	}
	if prov != nil {
		env.Provisioning = *prov
	}
	if drain != nil {
		env.Drain = *drain
	}

	if opts.Start, err = instant("options.start", r.Options.Start); err != nil {
		return fleet, env, opts, err
	}
	if opts.End, err = instant("options.end", r.Options.End); err != nil {
		return fleet, env, opts, err
	}
	return fleet, env, opts, nil
}

// duration parses an optional Go duration string. Empty is nil (unset), which the
// callers read as "inherit" or "use the policy estimate" — never as zero.
func duration(field, s string) (*time.Duration, error) {
	if s == "" {
		return nil, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a duration (want e.g. \"720h\", \"15m\"): %w", field, s, err)
	}
	return &d, nil
}

func instant(field, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("%s is required (RFC3339, e.g. \"2026-01-01T00:00:00Z\")", field)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %q is not an RFC3339 instant: %w", field, s, err)
	}
	return t, nil
}

func toResponse(tl sim.Timeline) Response {
	resp := Response{
		Result:           toResult(tl.Result, tl.Inputs),
		Partial:          tl.Partial,
		SimulatedThrough: stamp(tl.SimulatedThrough),
	}
	for _, e := range tl.Events {
		ev := Event{
			Kind:        string(e.Kind),
			At:          stamp(e.At),
			Until:       stamp(e.Until),
			Node:        e.Node,
			Replacement: e.Replacement,
			Surgeless:   e.Surgeless,
			Gate:        string(e.Gate),
		}
		if e.Census != nil {
			c := *e.Census
			ev.Census = &Census{
				Total: c.Total, Eligible: c.Eligible, OptedOut: c.OptedOut,
				Deleting: c.Deleting, NotReady: c.NotReady, InFlight: c.InFlight,
				Terminal: c.Terminal, InBackoff: c.InBackoff, NotTriggered: c.NotTriggered,
			}
		}
		resp.Events = append(resp.Events, ev)
	}
	for _, g := range tl.Generations {
		resp.Generations = append(resp.Generations, Generation{
			Slot:                g.Slot,
			Gen:                 g.Gen,
			Name:                g.Name,
			BirthMode:           string(g.BirthMode),
			PredecessorGen:      g.PredecessorGen,
			CreatedAt:           stamp(g.CreatedAt),
			ExpireAfter:         g.ExpireAfter.String(),
			DrainCap:            g.DrainCap.String(),
			DrainCapSource:      string(g.DrainCapSource),
			Deadline:            stamp(g.Deadline),
			EligibilityBoundary: stamp(g.EligibilityBoundary),
			ReadyAt:             stamp(g.ReadyAt),
			Provisional:         g.Provisional,
		})
	}
	for _, rt := range tl.Rotations {
		resp.Rotations = append(resp.Rotations, Rotation{
			Slot:    rt.Slot,
			FromGen: rt.FromGen,
			ToGen:   rt.ToGen,
			Mode:    string(rt.Mode),
			Start:   stamp(rt.Start),
			Ready:   stamp(rt.Ready),
			Done:    stamp(rt.Done),
		})
	}
	for _, w := range tl.Windows {
		resp.Windows = append(resp.Windows, WindowInterval{
			Start:        stamp(w.Start),
			End:          stamp(w.End),
			StartClipped: w.StartClipped,
			EndClipped:   w.EndClipped,
		})
	}
	for _, d := range tl.Diagnostics {
		resp.Diagnostics = append(resp.Diagnostics, Diagnostic{
			Severity: d.Severity.String(),
			Code:     d.Code,
			Message:  d.Message,
		})
	}
	return resp
}

func toResult(r schedule.Result, in schedule.Inputs) *Result {
	out := &Result{
		AgeThreshold:         r.A.String(),
		TRot:                 r.TRot.String(),
		TRotEstimate:         r.TRotEst.String(),
		DrainEstimate:        r.DrainEstimate.String(),
		ProvisioningEstimate: r.ProvisioningEstimate.String(),
		G:                    r.G,
		C:                    r.C,
		Inputs: &Inputs{
			E:                    in.E.String(),
			TGP:                  in.TGP.String(),
			TGPIsFallback:        in.TGPWasUnset,
			P:                    in.P.String(),
			WindowLen:            in.WindowLen.String(),
			Buffer:               schedule.Buffer.String(),
			ReadyTimeout:         in.ReadyTimeout.String(),
			Cooldown:             in.Cooldown.String(),
			K:                    in.K,
			M:                    in.MaxUnavailable,
			AgeThresholdOverride: in.Override != nil,
		},
	}
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, Finding{
			Severity: f.Severity.String(),
			Code:     f.Code,
			Message:  f.Message,
		})
	}
	return out
}

// stamp renders an instant for the wire; a zero time (an event with no coalesced
// interval) renders as "", which the omitempty on Until drops.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
