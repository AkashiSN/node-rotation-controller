package simapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/simapi"
)

// The semantic tests live in internal/sim; these pin the wire SHAPE — the RFC3339
// formatting, the enum strings, and the false-vs-absent question. A consumer reads this JSON
// through hand-written types, so "omitted when false" is a contract, not an implementation
// detail: if startClipped is dropped when false, the consumer must be entitled to read a
// missing key as false, and that is only true if the producer never omits a TRUE one.

// raw runs the simulator and returns the response as generic JSON, so a key's ABSENCE is
// observable — a typed decode cannot tell "omitted" from "zero".
func raw(t *testing.T, yaml, req string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(simapi.Simulate(yaml, req), &out); err != nil {
		t.Fatalf("Simulate returned invalid JSON: %v", err)
	}
	return out
}

func objects(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	arr, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s is missing or not an array: %v", key, m[key])
	}
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		o, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s carries a non-object element: %v", key, v)
		}
		out = append(out, o)
	}
	return out
}

func rfc3339(t *testing.T, o map[string]any, key string) time.Time {
	t.Helper()
	s, ok := o[key].(string)
	if !ok {
		t.Fatalf("%s is missing or not a string: %v", key, o[key])
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("%s = %q is not RFC3339: %v", key, s, err)
	}
	return at
}

func wantAbsent(t *testing.T, o map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := o[k]; ok {
			t.Errorf("%s = %v, want the key to be ABSENT: a consumer reads missing as false/none", k, v)
		}
	}
}

// TestWireShapeOfACompletedSurge pins the JSON of the golden path.
func TestWireShapeOfACompletedSurge(t *testing.T) {
	resp := raw(t, policyYAML, request)
	if e, ok := resp["error"]; ok {
		t.Fatalf("error = %v, want none", e)
	}

	// The horizon's end is what was actually simulated, in RFC3339.
	if got, want := resp["simulatedThrough"], "2026-01-22T00:00:00Z"; got != want {
		t.Errorf("simulatedThrough = %v, want %q (the requested end of a run that completed)", got, want)
	}
	if resp["partial"] != false {
		t.Errorf("partial = %v, want false", resp["partial"])
	}

	gens := objects(t, resp, "generations")
	if len(gens) != 2 {
		t.Fatalf("generations = %d, want 2 (the node and its replacement)", len(gens))
	}

	g0, g1 := gens[0], gens[1]
	if g0["birthMode"] != "initial" || g1["birthMode"] != "surge" {
		t.Errorf("birthMode = %q, %q; want the enum strings \"initial\", \"surge\"", g0["birthMode"], g1["birthMode"])
	}
	// An initial node has no predecessor and stages no surge, and its rotation completed:
	// all three keys are absent rather than zero-valued.
	wantAbsent(t, g0, "predecessorGen", "readyAt", "provisional")

	// The trap: generation 0 IS a valid predecessor. A sentinel int would be dropped by
	// omitempty and the replacement would read as having no predecessor at all.
	pred, ok := g1["predecessorGen"].(float64)
	if !ok || pred != 0 {
		t.Errorf("predecessorGen = %v, want 0 to be SERIALIZED (generation 0 is a real predecessor, not an absent one)", g1["predecessorGen"])
	}
	wantAbsent(t, g1, "provisional") // the rotation completed

	// Durations are Go duration strings; instants are RFC3339.
	if g0["expireAfter"] != "720h0m0s" {
		t.Errorf("expireAfter = %v, want a Go duration string", g0["expireAfter"])
	}
	if g0["drainCap"] != "1h0m0s" || g0["drainCapSource"] != "explicit" {
		t.Errorf("drainCap = %v (%v), want 1h0m0s from the node's own tGP (\"explicit\")", g0["drainCap"], g0["drainCapSource"])
	}
	created := rfc3339(t, g0, "createdAt")
	deadline := rfc3339(t, g0, "deadline")
	if want := created.Add(720 * time.Hour); !deadline.Equal(want) {
		t.Errorf("deadline = %s, want createdAt + expireAfter = %s", deadline, want)
	}
	boundary := rfc3339(t, g0, "eligibilityBoundary")
	if !boundary.Before(deadline) {
		t.Errorf("eligibilityBoundary %s is not before the deadline %s", boundary, deadline)
	}
	ready := rfc3339(t, g1, "readyAt") // the surged replacement became Ready

	rots := objects(t, resp, "rotations")
	if len(rots) != 1 {
		t.Fatalf("rotations = %d, want 1", len(rots))
	}
	r0 := rots[0]
	if r0["mode"] != "surge" {
		t.Errorf("mode = %v, want the enum string \"surge\"", r0["mode"])
	}
	if to, ok := r0["toGen"].(float64); !ok || to != 1 {
		t.Errorf("toGen = %v, want 1", r0["toGen"])
	}
	if got := rfc3339(t, r0, "ready"); !got.Equal(ready) {
		t.Errorf("rotation ready = %s, want the replacement's own readyAt %s", got, ready)
	}
	if start, done := rfc3339(t, r0, "start"), rfc3339(t, r0, "done"); !start.Before(done) {
		t.Errorf("rotation start %s is not before done %s", start, done)
	}

	// The window occurrences: the horizon starts and ends out of window, so neither boundary
	// is clipped — and both flags are therefore ABSENT, which is what lets a consumer treat
	// a missing flag as false.
	wins := objects(t, resp, "windows")
	if len(wins) == 0 {
		t.Fatal("windows is empty: the horizon spans three Saturdays")
	}
	for i, w := range wins {
		if s, e := rfc3339(t, w, "start"), rfc3339(t, w, "end"); !s.Before(e) {
			t.Errorf("window[%d] = [%s, %s] is not an interval", i, s, e)
		}
		wantAbsent(t, w, "startClipped", "endClipped")
	}
}

// TestWireShapeOfClippedWindows: a TRUE clipped flag is always present. The absent-means-
// false contract is only sound if the producer never omits a true one.
func TestWireShapeOfClippedWindows(t *testing.T) {
	// A horizon that opens inside the Saturday window and closes inside the next one.
	req := strings.Replace(request,
		`"options": {"start": "2026-01-01T00:00:00Z", "end": "2026-01-22T00:00:00Z"}`,
		`"options": {"start": "2026-01-03T03:00:00Z", "end": "2026-01-10T04:00:00Z"}`, 1)
	resp := raw(t, policyYAML, req)

	wins := objects(t, resp, "windows")
	if len(wins) != 2 {
		t.Fatalf("windows = %d, want 2 (the clipped occurrence at each end)", len(wins))
	}
	if wins[0]["startClipped"] != true {
		t.Errorf("windows[0].startClipped = %v, want true: the simulation began inside an already-open window", wins[0]["startClipped"])
	}
	wantAbsent(t, wins[0], "endClipped") // it closed for real, at 06:00
	if wins[1]["endClipped"] != true {
		t.Errorf("windows[1].endClipped = %v, want true: the window was still open when the simulation stopped", wins[1]["endClipped"])
	}
	wantAbsent(t, wins[1], "startClipped")

	// And the window that never closed emits no window-close event — only the interval says
	// where observation stopped.
	var closes int
	for _, e := range objects(t, resp, "events") {
		if e["kind"] == "window-close" {
			closes++
		}
	}
	if closes != 1 {
		t.Errorf("window-close events = %d, want 1: only the occurrence that genuinely closed", closes)
	}
}

// TestWireShapeOfAnInFlightSurgelessRotation pins the absent-not-zero contract on the path
// where it matters most: a surge-less rotation still draining has no ready, no done — and no
// replacement generation to name, because its replacement is born at done.
func TestWireShapeOfAnInFlightSurgelessRotation(t *testing.T) {
	// forcefulFallback on, an always-open window so the deadline race is not gated, and a
	// node 30m from its deadline with a drain that outruns the horizon.
	yaml := `apiVersion: noderotation.io/v1alpha1
kind: RotationPolicy
metadata:
  name: fallback
spec:
  nodePoolSelector:
    matchLabels:
      workload: api
  minRotationChances: 2
  maintenanceWindows:
    - timezone: UTC
      days: [Mon, Tue, Wed, Thu, Fri, Sat, Sun]
      start: "00:00"
      end: "23:59"
  surge:
    forcefulFallback:
      enabled: true
`
	req := `{
	  "fleet": {
	    "expireAfter": "720h",
	    "terminationGracePeriod": "1h",
	    "nodes": [{"name": "node-a", "createdAt": "2026-01-01T00:00:00Z"}]
	  },
	  "env": {"provisioning": "5m", "drain": "45m"},
	  "options": {"start": "2026-01-30T23:30:00Z", "end": "2026-01-30T23:50:00Z"}
	}`
	resp := raw(t, yaml, req)
	if e, ok := resp["error"]; ok {
		t.Fatalf("error = %v, want none", e)
	}

	rots := objects(t, resp, "rotations")
	if len(rots) != 1 {
		t.Fatalf("rotations = %d, want 1 in-flight rotation: %v", len(rots), resp)
	}
	r0 := rots[0]
	if r0["mode"] != "surgeless" {
		t.Fatalf("mode = %v, want the enum string \"surgeless\"; this test needs the fallback path", r0["mode"])
	}
	// ready: never on this path. done: not yet. toGen: the replacement is born at done, so
	// naming one would assert a node the simulation never created.
	wantAbsent(t, r0, "ready", "done", "toGen")

	gens := objects(t, resp, "generations")
	if len(gens) != 1 {
		t.Fatalf("generations = %d, want only the initial node: %v", len(gens), gens)
	}
}

// TestWireShapeOfAnInFlightSurge: the surged replacement IS on the wire while its rotation is
// in flight — marked provisional, so a consumer never mistakes it for a settled generation.
func TestWireShapeOfAnInFlightSurge(t *testing.T) {
	// The node's window opens Saturday 02:00; a horizon that ends at 02:05 catches the
	// rotation mid-flight (provisioning 5m, drain 10m).
	req := strings.Replace(request,
		`"options": {"start": "2026-01-01T00:00:00Z", "end": "2026-01-22T00:00:00Z"}`,
		`"options": {"start": "2026-01-01T00:00:00Z", "end": "2026-01-17T02:05:00Z"}`, 1)
	resp := raw(t, policyYAML, req)

	rots := objects(t, resp, "rotations")
	if len(rots) != 1 {
		t.Fatalf("rotations = %d, want 1: %v", len(rots), rots)
	}
	wantAbsent(t, rots[0], "done")
	if to, ok := rots[0]["toGen"].(float64); !ok || to != 1 {
		t.Fatalf("toGen = %v, want 1: the surged replacement exists from the rotation's start", rots[0]["toGen"])
	}

	gens := objects(t, resp, "generations")
	if len(gens) != 2 {
		t.Fatalf("generations = %d, want 2: an in-flight surge must still yield its replacement, or the make-before-break overlap vanishes exactly where a reader wants to see it", len(gens))
	}
	if gens[1]["provisional"] != true {
		t.Errorf("provisional = %v, want true", gens[1]["provisional"])
	}
	if gens[1]["birthMode"] != "surge" {
		t.Errorf("birthMode = %v, want \"surge\"", gens[1]["birthMode"])
	}
}

// TestWireCarriesNoTimelineForUnrunnableInput: the derived structure follows the timeline. A
// manifest the cluster would reject produces none of it.
func TestWireCarriesNoTimelineForUnrunnableInput(t *testing.T) {
	bad := strings.Replace(policyYAML, `end: "06:00"`, `end: "01:00"`, 1)
	resp := raw(t, bad, request)
	if resp["error"] == nil {
		t.Fatal("Simulate accepted an end-before-start window, want an error")
	}
	wantAbsent(t, resp, "generations", "rotations", "windows", "simulatedThrough", "result", "events")
}
