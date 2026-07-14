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
  DEFAULT_POLICY_YAML, DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon, defaultPolicyYaml,
  buildRequest,
} from '../theme/components/simulator/model.ts'
import { buildTimeline } from '../theme/components/simulator/timeline.ts'
import { buildCalendar } from '../theme/components/simulator/calendar.ts'

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
  // The front door must be clean, not merely runnable: a default that WARNS
  // (e.g. HardCapExceeded from an expireAfter the cluster would reject) is
  // still a defect even though it is not a hard `error`. No findings, and no
  // `fatal`-severity diagnostic, may survive on the shipped defaults.
  assert.ok(
    !out.result.findings || out.result.findings.length === 0,
    `the shipped defaults must produce no findings: ${JSON.stringify(out.result.findings)}`,
  )
  assert.ok(
    !out.diagnostics || out.diagnostics.every(d => d.severity !== 'fatal'),
    `the shipped defaults must produce no fatal diagnostics: ${JSON.stringify(out.diagnostics)}`,
  )
})

// The Japanese page opens on a DIFFERENT seed manifest (Asia/Tokyo), so it is a second front
// door — and a front door is only open if the controller admits the manifest behind it. The
// zone shifts the window's wall clock against the fixed horizon anchor, so this is not
// redundant with the test above: it is the same gate, on the other door.
test('the JAPANESE page defaults produce a clean timeline too', async () => {
  const simulate = await loadSimulate()
  const req = buildRequest(DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon(DEFAULT_FLEET))
  const out = JSON.parse(simulate(defaultPolicyYaml('ja'), JSON.stringify(req)))

  assert.equal(out.error, undefined, `the ja default template must be runnable: ${out.error}`)
  assert.ok(out.result, 'a result is required')
  assert.ok(out.events.some(e => e.kind === 'rotation-done'),
    'the ja default fleet must complete at least one rotation inside the default horizon')
  assert.equal(out.partial, false)
  assert.ok(
    !out.result.findings || out.result.findings.length === 0,
    `the ja defaults must produce no findings: ${JSON.stringify(out.result.findings)}`,
  )
  assert.ok(
    !out.diagnostics || out.diagnostics.every(d => d.severity !== 'fatal'),
    `the ja defaults must produce no fatal diagnostics: ${JSON.stringify(out.diagnostics)}`,
  )
})

// The chart is built from the wire's generations / rotations / windows, not from the event
// stream. A fixture can be wrong about the wire; the REAL module cannot. This is the test
// that says the page and the simulator still agree about what a run looks like.
test('the real response drives the chart: one row per slot, and a visible make-before-break overlap', async () => {
  const simulate = await loadSimulate()
  const horizon = defaultHorizon(DEFAULT_FLEET)
  const req = buildRequest(DEFAULT_FLEET, DEFAULT_ENV, horizon)
  const out = JSON.parse(simulate(DEFAULT_POLICY_YAML, JSON.stringify(req)))

  assert.ok(out.simulatedThrough, 'the wire must say how far it actually simulated')
  assert.ok(out.generations?.length, 'the wire must carry the generations the chart draws')
  assert.ok(out.windows?.length, 'and the observed window occurrences')

  const tl = buildTimeline(out, horizon, DEFAULT_FLEET)
  assert.equal(tl.rows.length, DEFAULT_FLEET.nodes.length,
    'one row per SLOT: the row count is the fleet size, whatever the generation count')
  assert.deepEqual(tl.anomalies, [], 'the producer and the page must agree')

  // Every instant the chart will scale to a coordinate has to be a number: an SVG x="NaN"
  // paints nothing, with no explanation.
  for (const row of tl.rows) {
    for (const g of row.generations) {
      assert.ok(Number.isFinite(g.createdMs), `${g.name} has no createdAt`)
      assert.ok(Number.isFinite(g.deadlineMs), `${g.name} has no deadline`)
      assert.ok(Number.isFinite(g.eligibilityMs), `${g.name} has no eligibility boundary`)
      for (const s of g.segments) {
        assert.ok(Number.isFinite(s.startMs), `${g.name}: ${s.kind} has no start`)
        assert.ok(s.endMs === null || s.endMs >= s.startMs, `${g.name}: ${s.kind} ends before it starts`)
      }
    }
  }

  // The overlap the whole redesign exists to show: somewhere in this run, a replacement was
  // PROVISIONING while its predecessor was still RUNNING. Two nodes, coexisting — that is
  // make-before-break. (Not "provisioning against the drain": the old node's NodeClaim is
  // deleted at node-ready, so its drain begins exactly where the provisioning ends. Those
  // two are adjacent, never overlapping.)
  const overlaps = tl.rows.flatMap(row => row.generations.flatMap((g, i) => {
    const next = row.generations[i + 1]
    if (!next) return []
    const running = g.segments.find(s => s.kind === 'running')
    const prov = next.segments.find(s => s.kind === 'provisioning')
    if (!running || !prov || prov.endMs === null || running.endMs === null) return []
    return [Math.min(prov.endMs, running.endMs) - Math.max(prov.startMs, running.startMs)]
  }))
  assert.ok(overlaps.some(ms => ms > 0),
    'no replacement provisioned while its predecessor was still running — make-before-break is invisible')

  // And the calendar folds the same run into whole weeks without dividing by zero.
  const cal = buildCalendar(
    out.windows.map(w => ({
      startMs: new Date(w.start).getTime(),
      endMs: new Date(w.end).getTime(),
      startClipped: w.startClipped === true,
      endClipped: w.endClipped === true,
    })),
    new Date(horizon.start).getTime(),
    new Date(out.simulatedThrough).getTime(),
    'UTC',
  )
  assert.ok(cal.wholeWeeks >= 1, 'the default horizon spans whole weeks')
  const open = cal.cells.filter(c => (c.ratio ?? 0) > 0)
  assert.ok(open.length > 0, 'the default policy opens a window somewhere in the grid')
  assert.ok(cal.cells.every(c => c.ratio === null || (c.ratio >= 0 && c.ratio <= 1)))
})

test('simulate never throws — a bad manifest comes back as an error string', async () => {
  const simulate = await loadSimulate()
  const req = buildRequest(DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon(DEFAULT_FLEET))
  const bad = DEFAULT_POLICY_YAML.replace('apiVersion: noderotation.io/v1alpha1', 'apiVersion: apps/v1')
  const out = JSON.parse(simulate(bad, JSON.stringify(req)))
  assert.ok(out.error, 'a manifest the cluster would reject must not produce a timeline')
  assert.equal(out.result, undefined)
})
