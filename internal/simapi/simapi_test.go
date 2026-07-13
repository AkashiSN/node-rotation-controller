package simapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/simapi"
)

// policyYAML is a RotationPolicy manifest an operator could apply as-is: a
// weekly 4-hour window, expireAfter 720h on the fleet side.
const policyYAML = `apiVersion: noderotation.io/v1alpha1
kind: RotationPolicy
metadata:
  name: weekly
spec:
  nodePoolSelector:
    matchLabels:
      workload: api
  minRotationChances: 2
  maintenanceWindows:
    - timezone: UTC
      days: [Sat]
      start: "02:00"
      end: "06:00"
`

// request rotates a single 30-day-old node over a 21-day horizon.
const request = `{
  "fleet": {
    "expireAfter": "720h",
    "terminationGracePeriod": "1h",
    "nodes": [{"name": "node-a", "createdAt": "2026-01-01T00:00:00Z"}]
  },
  "env": {"provisioning": "5m", "drain": "10m"},
  "options": {"start": "2026-01-01T00:00:00Z", "end": "2026-01-22T00:00:00Z"}
}`

func mustParse(t *testing.T, ts string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("event timestamp %q is not RFC3339: %v", ts, err)
	}
	return at
}

func run(t *testing.T, yaml, req string) simapi.Response {
	t.Helper()
	var resp simapi.Response
	if err := json.Unmarshal(simapi.Simulate(yaml, req), &resp); err != nil {
		t.Fatalf("Simulate returned invalid JSON: %v", err)
	}
	return resp
}

func TestSimulateReturnsTimelineAndDerivation(t *testing.T) {
	resp := run(t, policyYAML, request)
	if resp.Error != "" {
		t.Fatalf("Simulate error = %q, want none", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("Result is nil: the header strip is what the page renders even when nothing rotates")
	}
	// The derivation is policy-derived, so it must be there whatever the timeline
	// does — and expressed in the duration strings the page renders.
	if resp.Result.AgeThreshold == "" || resp.Result.TRot == "" || resp.Result.TRotEstimate == "" {
		t.Errorf("derived durations missing: %+v", resp.Result)
	}
	if resp.Result.C < 1 {
		t.Errorf("C = %d, want >= 1", resp.Result.C)
	}

	// The node crosses its ageThreshold inside the horizon, so it rotates: the
	// three surge events must be there, in order, with the replacement named.
	var kinds []string
	for _, e := range resp.Events {
		switch e.Kind {
		case "rotation-start", "node-ready", "rotation-done":
			kinds = append(kinds, e.Kind)
		}
	}
	if got := strings.Join(kinds, ","); got != "rotation-start,node-ready,rotation-done" {
		t.Errorf("surge events = %q, want rotation-start,node-ready,rotation-done", got)
	}
	for _, e := range resp.Events {
		if e.Kind == "rotation-done" && e.Replacement == "" {
			t.Error("rotation-done carries no replacement name")
		}
		if e.Kind == "rotation-start" && e.Surgeless {
			t.Error("rotation-start marked surgeless with forcefulFallback disabled")
		}
	}
}

func TestSimulateCoalescedEventCarriesUntilAndGate(t *testing.T) {
	resp := run(t, policyYAML, request)
	var blocked *simapi.Event
	for i := range resp.Events {
		if resp.Events[i].Kind == "blocked-by-gate" {
			blocked = &resp.Events[i]
			break
		}
	}
	if blocked == nil {
		t.Fatal("no blocked-by-gate event: the horizon starts out of window")
	}
	if blocked.Gate != "outOfWindow" {
		t.Errorf("gate = %q, want outOfWindow (the typed reason reaches the UI verbatim)", blocked.Gate)
	}
	if blocked.Until == "" {
		t.Error("coalesced event carries no until: the UI cannot draw the interval it covers")
	}
}

func TestSimulateReportsPolicyErrorsAsTheControllerWouldPhraseThem(t *testing.T) {
	// An overnight window passes the CRD's HH:MM pattern but fails the runtime
	// validation — the browser must show the controller's own message.
	bad := strings.Replace(policyYAML, `end: "06:00"`, `end: "01:00"`, 1)
	resp := run(t, bad, request)
	if resp.Error == "" {
		t.Fatal("Simulate accepted an end-before-start window, want an error")
	}
	if resp.Result != nil || resp.Events != nil {
		t.Error("an un-runnable policy must produce no timeline")
	}
}

func TestSimulateRejectsUnknownPolicyFields(t *testing.T) {
	// A typo the CRD's structural schema would reject at admission must not be
	// silently ignored in the simulator either.
	bad := strings.Replace(policyYAML, "  minRotationChances: 2", "  minRotationChance: 2", 1)
	if resp := run(t, bad, request); resp.Error == "" {
		t.Fatal("Simulate accepted an unknown policy field, want an error")
	}
}

func TestSimulateRejectsAManifestTheClusterWouldNotAdmit(t *testing.T) {
	// The boundary's promise is that a manifest which simulates is a manifest the
	// cluster would accept. A wrong (or absent) apiVersion/kind means Kubernetes
	// would never have admitted this as a RotationPolicy, so producing a timeline
	// for it would tell the operator their policy works when it would not apply.
	for name, yaml := range map[string]string{
		"wrong apiVersion":   strings.Replace(policyYAML, "apiVersion: noderotation.io/v1alpha1", "apiVersion: apps/v1", 1),
		"missing apiVersion": strings.Replace(policyYAML, "apiVersion: noderotation.io/v1alpha1\n", "", 1),
		"wrong kind":         strings.Replace(policyYAML, "kind: RotationPolicy", "kind: Deployment", 1),
		"missing kind":       strings.Replace(policyYAML, "kind: RotationPolicy\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			resp := run(t, yaml, request)
			if resp.Error == "" {
				t.Fatalf("Simulate accepted a manifest with a %s, want an error", name)
			}
			if resp.Result != nil || resp.Events != nil {
				t.Error("a manifest the cluster would reject must produce no timeline")
			}
		})
	}
}

func TestSimulateRejectsMalformedRequest(t *testing.T) {
	for name, req := range map[string]string{
		"not JSON": `{`,
		"no fleet expireAfter": `{"fleet":{"nodes":[{"name":"a","createdAt":"2026-01-01T00:00:00Z"}]},
			"options":{"start":"2026-01-01T00:00:00Z","end":"2026-01-08T00:00:00Z"}}`,
		"bad duration": `{"fleet":{"expireAfter":"720","nodes":[]},
			"options":{"start":"2026-01-01T00:00:00Z","end":"2026-01-08T00:00:00Z"}}`,
		"bad timestamp": `{"fleet":{"expireAfter":"720h","nodes":[{"name":"a","createdAt":"nope"}]},
			"options":{"start":"2026-01-01T00:00:00Z","end":"2026-01-08T00:00:00Z"}}`,
		"inverted horizon": `{"fleet":{"expireAfter":"720h","nodes":[]},
			"options":{"start":"2026-01-08T00:00:00Z","end":"2026-01-01T00:00:00Z"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if resp := run(t, policyYAML, req); resp.Error == "" {
				t.Fatalf("Simulate accepted %s, want an error", name)
			}
		})
	}
}

func TestSimulateSurfacesFatalDiagnosticAsPartial(t *testing.T) {
	// Provisioning slower than surge.readyTimeout is the failure path, which sim
	// does not model: the response must say so rather than fake a timeline.
	req := strings.Replace(request, `"provisioning": "5m"`, `"provisioning": "2h"`, 1)
	resp := run(t, policyYAML, req)
	if resp.Error != "" {
		t.Fatalf("Simulate error = %q; an unmodelled path is a diagnostic, not an error", resp.Error)
	}
	if !resp.Partial {
		t.Error("partial = false, want true when an unmodelled path is reached")
	}
	if len(resp.Diagnostics) == 0 || resp.Diagnostics[0].Severity != "fatal" {
		t.Fatalf("diagnostics = %+v, want a fatal one", resp.Diagnostics)
	}
	if resp.Result == nil {
		t.Error("Result dropped: the header strip must render even when the timeline cannot")
	}
}

func TestSimulateEnvDefaultsToThePolicyEstimates(t *testing.T) {
	// An omitted env is not "zero duration" — it is the policy's own forecast
	// estimates, so an untouched simulation is self-consistent.
	req := strings.Replace(request, `"env": {"provisioning": "5m", "drain": "10m"},`, "", 1)
	resp := run(t, policyYAML, req)
	if resp.Error != "" {
		t.Fatalf("Simulate error = %q, want none", resp.Error)
	}
	var start, ready string
	for _, e := range resp.Events {
		switch e.Kind {
		case "rotation-start":
			if start == "" {
				start = e.At
			}
		case "node-ready":
			if ready == "" {
				ready = e.At
			}
		}
	}
	if start == "" || ready == "" {
		t.Fatal("no rotation happened with a defaulted env")
	}
	// The default provisioning is the resolved provisioningEstimate, so node-ready
	// lands exactly that far after rotation-start.
	gotGap := mustParse(t, ready).Sub(mustParse(t, start)).String()
	if want := resp.Result.ProvisioningEstimate; gotGap != want {
		t.Errorf("node-ready - rotation-start = %s, want the policy's provisioningEstimate %s", gotGap, want)
	}
}
