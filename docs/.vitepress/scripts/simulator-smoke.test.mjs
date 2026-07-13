// docs/.vitepress/scripts/simulator-smoke.test.mjs
// Load the real wasm module the page loads, and call it with the page's own
// defaults. This is the gate that says the front door opens: a default template
// the controller would reject, or an env the simulator calls Fatal, ships a page
// whose first render is an error.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  DEFAULT_POLICY_YAML, DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon, buildRequest,
} from '../theme/components/simulator/model.ts'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')

async function loadSimulate() {
  // wasm_exec.js is a classic script that defines globalThis.Go.
  const glue = await readFile(resolve(root, 'docs/public/wasm_exec.js'), 'utf8')
  new Function(glue).call(globalThis)
  const go = new globalThis.Go()
  const wasm = await readFile(resolve(root, 'docs/public/simulator.wasm'))
  const { instance } = await WebAssembly.instantiate(wasm, go.importObject)
  go.run(instance)                       // registers globalThis.simulate; blocks in select{}
  return globalThis.simulate
}

test('the page defaults produce a timeline, not an error', async () => {
  const simulate = await loadSimulate()
  const req = buildRequest(DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon(DEFAULT_FLEET))
  const out = JSON.parse(simulate(DEFAULT_POLICY_YAML, JSON.stringify(req)))

  assert.equal(out.error, undefined, `the default template must be runnable: ${out.error}`)
  assert.ok(out.result, 'a result is required')
  assert.ok(out.result.c >= 1, 'the default policy must rotate at least one node per window')
  assert.ok(out.result.g >= 1, 'the default policy must guarantee at least one chance')
  assert.ok(out.events.some(e => e.kind === 'rotation-done'),
    'the default fleet must complete at least one rotation inside the default horizon')
  assert.equal(out.partial, false, 'the default horizon must fit the step budget')
})

test('simulate never throws — a bad manifest comes back as an error string', async () => {
  const simulate = await loadSimulate()
  const req = buildRequest(DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon(DEFAULT_FLEET))
  const bad = DEFAULT_POLICY_YAML.replace('apiVersion: noderotation.io/v1alpha1', 'apiVersion: apps/v1')
  const out = JSON.parse(simulate(bad, JSON.stringify(req)))
  assert.ok(out.error, 'a manifest the cluster would reject must not produce a timeline')
  assert.equal(out.result, undefined)
})
