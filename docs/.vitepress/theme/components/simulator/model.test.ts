import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  parseGoDuration, generateNodes, defaultHorizon, buildRequest,
  DEFAULT_POLICY_YAML, DEFAULT_FLEET, DEFAULT_ENV,
} from './model.ts'

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
  assert.deepEqual(nodes.map(n => n.name), ['node-1', 'node-2', 'node-3'])
  assert.equal(nodes[0].createdAt, '2026-01-01T00:00:00.000Z')
  assert.equal(nodes[1].createdAt, '2026-01-04T12:00:00.000Z')
  assert.equal(nodes[2].createdAt, '2026-01-08T00:00:00.000Z')
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
