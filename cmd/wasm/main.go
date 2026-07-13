//go:build js && wasm

// Command wasm is the browser policy simulator's WebAssembly module: it exposes
// internal/simapi to JavaScript and does nothing else.
//
// It exists so the docs-site simulator runs the CONTROLLER'S OWN code — the §3.2
// derivation, the candidate selection and the start gates — rather than a
// TypeScript re-implementation that would drift from it silently. Everything
// below the js boundary is pure Go shared with the reconcile loop.
//
// The module must never link sigs.k8s.io/karpenter: its scheme and reflect
// metadata cost ~6 MB gzipped that a browser would pay for on every page load,
// and nothing here uses it. `make wasm-guard` fails the build if it reappears.
//
// Usage from JavaScript:
//
//	const go = new Go();
//	const {instance} = await WebAssembly.instantiateStreaming(fetch("simulator.wasm"), go.importObject);
//	go.run(instance);                                    // registers globalThis.simulate
//	const out = JSON.parse(simulate(policyYAML, JSON.stringify({fleet, env, options})));
//
// simulate returns a JSON string and never throws: input it cannot run comes back
// as {"error": "..."} so the page always has something to render.
package main

import (
	"syscall/js"

	"github.com/AkashiSN/node-rotation-controller/internal/simapi"
)

func main() {
	js.Global().Set("simulate", js.FuncOf(simulate))
	// go.run() returns as soon as main does, which would tear down the module and
	// its exported functions. Block forever: the page calls simulate() from JS.
	select {}
}

// simulate is the js.Func bound to globalThis.simulate(policyYAML, requestJSON).
func simulate(_ js.Value, args []js.Value) any {
	if len(args) != 2 || args[0].Type() != js.TypeString || args[1].Type() != js.TypeString {
		return `{"error":"simulate(policyYAML, requestJSON): expected two string arguments"}`
	}
	return string(simapi.Simulate(args[0].String(), args[1].String()))
}
