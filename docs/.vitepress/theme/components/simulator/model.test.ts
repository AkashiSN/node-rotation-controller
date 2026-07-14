import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  parseGoDuration, generateNodes, defaultHorizon, defaultPolicyYaml, buildRequest,
  horizonForCoverage, DEFAULT_COVERAGE, DEFAULT_POLICY_YAML, DEFAULT_FLEET, DEFAULT_ENV,
  DEFAULT_FIRST_CREATED_AT,
} from './model.ts'

test('lifetime coverage is a multiple of the LONGEST effective lifetime, not of the template', () => {
  // A heterogeneous fleet: node-b's own expireAfter is longer than the template's. Bounding
  // the horizon on the TEMPLATE would push node-b's deadline off the right edge, and the
  // page would then report no breach for time it never simulated.
  const fleet = {
    expireAfter: '480h',
    nodes: [
      { name: 'node-a', createdAt: '2026-01-01T00:00:00Z' },
      { name: 'node-b', createdAt: '2026-01-01T00:00:00Z', expireAfter: '720h' },
    ],
  }
  const h = horizonForCoverage(fleet, 2)
  assert.equal(new Date(h.end).getTime() - new Date(h.start).getTime(), 2 * 720 * 3600_000)

  // The multiplier is the control; 1x is one lifetime, 3x is three.
  assert.equal(
    new Date(horizonForCoverage(fleet, 1).end).getTime() - new Date(h.start).getTime(),
    720 * 3600_000,
  )
  // And the default a visitor lands on is the 2x that puts a SECOND generation on screen.
  assert.deepEqual(defaultHorizon(fleet), horizonForCoverage(fleet, DEFAULT_COVERAGE))
})

test('parseGoDuration handles the units Go emits', () => {
  assert.equal(parseGoDuration('720h'), 720 * 3600_000)
  assert.equal(parseGoDuration('1h30m'), 90 * 60_000)
  assert.equal(parseGoDuration('382h43m0s'), (382 * 3600 + 43 * 60) * 1000)
  assert.equal(parseGoDuration('500ms'), 500)
  // Go has no day unit: "7d" is not a duration, and pretending it is would send
  // the wasm module a string it rejects.
  assert.equal(parseGoDuration('7d'), null)
  assert.equal(parseGoDuration(''), null)
  assert.equal(parseGoDuration('abc'), null)
})

test('generateNodes spreads createdAt evenly across the spread', () => {
  const nodes = generateNodes(3, '2026-01-01T00:00:00Z', '168h')
  assert.deepEqual(nodes.map(n => n.name), ['node-01', 'node-02', 'node-03'])
  assert.equal(nodes[0].createdAt, '2026-01-01T00:00:00.000Z')
  assert.equal(nodes[1].createdAt, '2026-01-04T12:00:00.000Z')
  assert.equal(nodes[2].createdAt, '2026-01-08T00:00:00.000Z')
})

test('generated names are FIXED WIDTH, so lexicographic order is creation order', () => {
  // The property that matters, at every size the UI and a share link can reach: selection
  // breaks deadline/creation ties on the name, lexicographically. An unpadded generator
  // ordered node-1, node-10, node-2 — correct controller behaviour, but as the example a
  // visitor lands on it reads as a bug in the simulator.
  for (const count of [1, 3, 9, 10, 26, 50, 200]) {
    const names = generateNodes(count, '2026-01-01T00:00:00Z', '168h').map(n => n.name)
    assert.deepEqual(names, [...names].sort(),
      `a fleet of ${count} does not rotate in creation order: ${names.slice(0, 12).join(', ')}`)
    assert.equal(new Set(names.map(n => n.length)).size, 1, `a fleet of ${count} mixes name widths`)
  }
  assert.equal(generateNodes(10, '2026-01-01T00:00:00Z', '0s')[9].name, 'node-10')
  assert.equal(generateNodes(100, '2026-01-01T00:00:00Z', '0s')[0].name, 'node-001')
})

test('generateNodes with one node puts it at the first instant', () => {
  const nodes = generateNodes(1, '2026-01-01T00:00:00Z', '168h')
  assert.equal(nodes.length, 1)
  assert.equal(nodes[0].createdAt, '2026-01-01T00:00:00.000Z')
})

test('defaultHorizon spans the last node TWO of its OWN expireAfter generations', () => {
  // A heterogeneous fleet: node-2 overrides expireAfter. The horizon must follow
  // each node's EFFECTIVE expireAfter (its override, else the template), or the
  // overriding node's second-generation deadline falls off the right edge and the
  // page reports "no breach" for a window it never simulated.
  const h = defaultHorizon({
    expireAfter: '720h',
    nodes: [
      { name: 'node-1', createdAt: '2026-01-01T00:00:00Z' },
      { name: 'node-2', createdAt: '2026-01-08T00:00:00Z', expireAfter: '1440h' },
    ],
  })
  assert.equal(h.start, '2026-01-01T00:00:00.000Z')          // earliest createdAt
  assert.equal(h.end, '2026-05-08T00:00:00.000Z')            // 2026-01-08 + 2*1440h (120 days)
})

test('defaultHorizon is total: an empty fleet must not throw', () => {
  // generateNodes(0, ...) is reachable from the UI (a node-count field a user
  // can set to 0) and returns []; Math.min(...[]) is Infinity, and
  // new Date(Infinity).toISOString() throws RangeError inside a Vue watcher,
  // blanking the whole page. defaultHorizon must stay total over this input.
  const h = defaultHorizon({ expireAfter: '720h', nodes: [] })
  assert.ok(!Number.isNaN(new Date(h.start).getTime()))
  assert.ok(!Number.isNaN(new Date(h.end).getTime()))
  assert.ok(new Date(h.end) > new Date(h.start))
})

test('defaultHorizon is total: a malformed createdAt must not throw', () => {
  // FleetInput.vue exposes createdAt as a raw editable text input, and
  // PolicySimulator.vue recomputes the horizon in a watch while unpinned. A
  // node with an unparseable createdAt maps to NaN through new Date(...).getTime(),
  // and new Date(NaN).toISOString() throws RangeError — inside that watcher that
  // would blank the whole page. The malformed value itself must NOT be touched:
  // it stays in the fleet and reaches simulate(), so the user reads the wasm
  // module's own error about it rather than a silently "fixed" value.
  const h = defaultHorizon({ expireAfter: '480h', nodes: [{ name: 'node-1', createdAt: 'bad' }] })
  assert.ok(!Number.isNaN(new Date(h.start).getTime()))
  assert.ok(!Number.isNaN(new Date(h.end).getTime()))
  assert.ok(new Date(h.end) > new Date(h.start))
})

test('defaultHorizon with a mixed fleet bounds on the valid node, not the anchor', () => {
  // A malformed node must be ignored when computing bounds, not fall back to
  // silently dragging the whole horizon to the fixed anchor: the valid node's
  // own createdAt/expireAfter must still determine start and end.
  const h = defaultHorizon({
    expireAfter: '720h',
    nodes: [
      { name: 'node-1', createdAt: '2026-01-01T00:00:00Z' },
      { name: 'node-2', createdAt: 'bad' },
    ],
  })
  assert.equal(h.start, '2026-01-01T00:00:00.000Z')
  assert.equal(h.end, '2026-03-02T00:00:00.000Z') // 2026-01-01 + 2*720h (60 days)
})

test('generateNodes with a malformed firstCreatedAt falls back to the anchor', () => {
  // Reachable from the same UI path; must not emit "Invalid Date" strings into
  // the fleet the page hands to simulate().
  const nodes = generateNodes(2, 'not-a-date', '168h')
  assert.ok(!Number.isNaN(new Date(nodes[0].createdAt).getTime()))
  assert.ok(!Number.isNaN(new Date(nodes[1].createdAt).getTime()))
  assert.equal(nodes[0].createdAt, new Date(DEFAULT_FIRST_CREATED_AT).toISOString())
})

test('the shipped defaults are internally consistent', () => {
  const req = buildRequest(DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon(DEFAULT_FLEET))
  assert.equal(req.fleet.nodes.length, 3)
  assert.ok(parseGoDuration(req.fleet.expireAfter)! > 0)
  // Env is NOT the estimates: the default env is blank, which is what makes the
  // wasm module fall back to the policy's own resolved estimates.
  assert.deepEqual(req.env, {})
  assert.ok(new Date(req.options.end) > new Date(req.options.start))
})

test('the default policy template carries both apiVersion and kind', () => {
  // Both are required exactly: a manifest the cluster would never admit must not
  // produce a timeline, so a default template missing either would ship a page
  // that opens on an error.
  assert.match(DEFAULT_POLICY_YAML, /^apiVersion: noderotation\.io\/v1alpha1$/m)
  assert.match(DEFAULT_POLICY_YAML, /^kind: RotationPolicy$/m)
})

test('the seed manifest reads as a manifest: no comments', () => {
  // It is the first thing a visitor reads, and the form rewrites the YAML around it. The
  // rationale for the schedule lives in model.ts, and the symbols are explained on the page.
  assert.ok(!DEFAULT_POLICY_YAML.includes('#'), 'the seed manifest must carry no comments')
})

test('the JAPANESE page seeds Asia/Tokyo; every other locale seeds UTC', () => {
  // The whole page renders in the POLICY's zone, so a UTC seed makes the Japanese page a
  // timezone-conversion exercise before it is anything else.
  assert.match(defaultPolicyYaml('ja'), /^ {4}- timezone: Asia\/Tokyo$/m)
  assert.match(defaultPolicyYaml('ja-JP'), /^ {4}- timezone: Asia\/Tokyo$/m)
  assert.equal(defaultPolicyYaml('en-US'), DEFAULT_POLICY_YAML)
  assert.match(DEFAULT_POLICY_YAML, /^ {4}- timezone: UTC$/m)

  // The zone is the ONLY difference: the seed is one manifest, not two that can drift.
  assert.equal(defaultPolicyYaml('ja').replace('Asia/Tokyo', 'UTC'), DEFAULT_POLICY_YAML)
})
