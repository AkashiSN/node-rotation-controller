import { test } from 'node:test'
import assert from 'node:assert/strict'
import { barsOf, blockedOf, marksOf, rowsOf, windowsOf } from './timeline.ts'
import type { Fleet, Horizon, SimEvent } from './model.ts'

const HORIZON: Horizon = { start: '2026-01-01T00:00:00Z', end: '2026-01-31T00:00:00Z' }
const ms = (iso: string) => new Date(iso).getTime()

/** A one-node fleet whose template expireAfter is 240h (10d). */
const fleet = (over?: Partial<Fleet>): Fleet => ({
  expireAfter: '240h',
  nodes: [{ name: 'node-1', createdAt: '2026-01-01T00:00:00Z' }],
  ...over,
})

test('a SURGE replacement is born at rotation-start, not rotation-done', () => {
  // THE regression test. internal/sim/loop.go sets the replacement's CreatedAt to
  // `rot.start` on the surge path (Karpenter creates the replacement NodeClaim the
  // moment the placeholder Pod goes pending), and deadlineOf = CreatedAt + expireAfter.
  // Deriving `born` from the rotation-done that NAMES the replacement erases the
  // make-before-break overlap and pushes the deadline tick t_rot too late — the one
  // direction that hides a breach.
  const events: SimEvent[] = [
    { kind: 'rotation-start', at: '2026-01-05T02:00:00Z', node: 'node-1' },
    { kind: 'node-ready', at: '2026-01-05T02:05:00Z', node: 'node-1' },
    { kind: 'rotation-done', at: '2026-01-05T02:20:00Z', node: 'node-1', replacement: 'node-1-2' },
  ]
  const bars = barsOf(events, HORIZON, fleet())
  const repl = bars.find(b => b.name === 'node-1-2')!
  assert.equal(repl.bornMs, ms('2026-01-05T02:00:00Z'), 'born at the rotation-start instant')
  // 240h after birth, NOT 240h after the rotation-done.
  assert.equal(repl.deadlineMs, ms('2026-01-15T02:00:00Z'))
  // And the overlap the product exists to show: the replacement is alive while the old
  // node is still draining.
  const old = bars.find(b => b.name === 'node-1')!
  assert.ok(repl.bornMs! < old.endMs!, 'make-before-break: the bars must overlap')
})

test('a SURGE-LESS replacement is born at rotation-done', () => {
  // The forceful-fallback path has no placeholder Pod, so Karpenter only provisions in
  // response to the evicted Pods: sim pins CreatedAt to rot.doneAt.
  const events: SimEvent[] = [
    { kind: 'rotation-start', at: '2026-01-05T02:00:00Z', node: 'node-1', surgeless: true },
    { kind: 'rotation-done', at: '2026-01-05T02:20:00Z', node: 'node-1', replacement: 'node-1-2', surgeless: true },
  ]
  const repl = barsOf(events, HORIZON, fleet()).find(b => b.name === 'node-1-2')!
  assert.equal(repl.bornMs, ms('2026-01-05T02:20:00Z'))
  assert.equal(repl.deadlineMs, ms('2026-01-15T02:20:00Z'))
})

test('the LATEST rotation-start at or before the done is the birth, across generations', () => {
  // A node name rotates more than once over a long horizon (gen 2 of node-1-2 is
  // node-1-3): matching the FIRST rotation-start for the node would date the second
  // generation's replacement from the first generation's rotation.
  const events: SimEvent[] = [
    { kind: 'rotation-start', at: '2026-01-05T02:00:00Z', node: 'node-1' },
    { kind: 'rotation-done', at: '2026-01-05T02:20:00Z', node: 'node-1', replacement: 'node-1-2' },
    { kind: 'rotation-start', at: '2026-01-15T02:00:00Z', node: 'node-1-2' },
    { kind: 'rotation-done', at: '2026-01-15T02:20:00Z', node: 'node-1-2', replacement: 'node-1-3' },
  ]
  const bars = barsOf(events, HORIZON, fleet())
  assert.equal(bars.find(b => b.name === 'node-1-2')!.bornMs, ms('2026-01-05T02:00:00Z'))
  assert.equal(bars.find(b => b.name === 'node-1-3')!.bornMs, ms('2026-01-15T02:00:00Z'))
})

test('a rotation-done with no matching rotation-start falls back to the done instant', () => {
  // Total function: a truncated event stream must not produce NaN or throw.
  const events: SimEvent[] = [
    { kind: 'rotation-done', at: '2026-01-05T02:20:00Z', node: 'node-1', replacement: 'node-1-2' },
  ]
  const repl = barsOf(events, HORIZON, fleet()).find(b => b.name === 'node-1-2')!
  assert.equal(repl.bornMs, ms('2026-01-05T02:20:00Z'))
})

test("a replacement's deadline uses the TEMPLATE expireAfter; a declared override uses its own", () => {
  // A replacement is provisioned from the NodePool template, so it can never inherit the
  // old node's per-node override.
  const f = fleet({
    nodes: [{ name: 'node-1', createdAt: '2026-01-01T00:00:00Z', expireAfter: '72h' }],
  })
  const events: SimEvent[] = [
    { kind: 'rotation-start', at: '2026-01-03T02:00:00Z', node: 'node-1' },
    { kind: 'rotation-done', at: '2026-01-03T02:20:00Z', node: 'node-1', replacement: 'node-1-2' },
  ]
  const bars = barsOf(events, HORIZON, f)
  assert.equal(bars.find(b => b.name === 'node-1')!.deadlineMs, ms('2026-01-04T00:00:00Z'), '72h override')
  assert.equal(bars.find(b => b.name === 'node-1-2')!.deadlineMs, ms('2026-01-13T02:00:00Z'), '240h template')
})

test('a deadline beyond the horizon is reported as absent, not drawn off-scale', () => {
  const f = fleet({ expireAfter: '2400h' })
  assert.equal(barsOf([], HORIZON, f)[0].deadlineMs, null)
})

test('a malformed instant is skipped rather than emitted as NaN', () => {
  const f = fleet({ nodes: [{ name: 'node-1', createdAt: 'not-a-date' }] })
  const bar = barsOf([], HORIZON, f)[0]
  assert.equal(bar.bornMs, null)
  assert.equal(bar.deadlineMs, null)
  const marks = marksOf(
    [{ kind: 'rotation-start', at: 'not-a-date', node: 'node-1' }], HORIZON, f)
  assert.deepEqual(marks, [], 'a mark with a malformed `at` must not reach the SVG')
})

test('blocked events stay INTERVAL bands, clipped to the horizon', () => {
  const events: SimEvent[] = [
    {
      kind: 'blocked-by-gate', at: '2025-12-25T00:00:00Z', until: '2026-01-04T00:00:00Z',
      gate: 'cooldown',
    },
  ]
  const [b] = blockedOf(events, HORIZON)
  assert.equal(b.startMs, ms(HORIZON.start), 'clipped to the horizon start')
  assert.equal(b.endMs, ms('2026-01-04T00:00:00Z'))
  assert.ok(b.endMs > b.startMs, 'an interval, never a point')
  assert.equal(b.label, 'cooldown')
})

test('a blocked event with no `until` runs to the horizon end', () => {
  const events: SimEvent[] = [{ kind: 'no-eligible-claim', at: '2026-01-20T00:00:00Z' }]
  const [b] = blockedOf(events, HORIZON)
  assert.equal(b.startMs, ms('2026-01-20T00:00:00Z'))
  assert.equal(b.endMs, ms(HORIZON.end), 'still open at the end of the simulated horizon')
  assert.equal(b.label, 'no-eligible-claim')
})

test('out-of-order events still pair windows correctly', () => {
  // The wire contract does not promise chronological order; pairing by array position
  // would leave the open dangling and band the WHOLE horizon.
  const events: SimEvent[] = [
    { kind: 'window-close', at: '2026-01-03T06:00:00Z' },
    { kind: 'window-open', at: '2026-01-03T02:00:00Z' },
  ]
  const w = windowsOf(events, HORIZON)
  assert.equal(w.length, 1)
  assert.equal(w[0].startMs, ms('2026-01-03T02:00:00Z'))
  assert.equal(w[0].endMs, ms('2026-01-03T06:00:00Z'))
})

test('an unclosed window bands to the horizon end', () => {
  const w = windowsOf([{ kind: 'window-open', at: '2026-01-30T02:00:00Z' }], HORIZON)
  assert.deepEqual(w, [{ startMs: ms('2026-01-30T02:00:00Z'), endMs: ms(HORIZON.end) }])
})

test('rows list the declared nodes then each replacement, once', () => {
  const events: SimEvent[] = [
    { kind: 'rotation-done', at: '2026-01-05T02:20:00Z', node: 'node-1', replacement: 'node-1-2' },
    { kind: 'rotation-done', at: '2026-01-15T02:20:00Z', node: 'node-1-2', replacement: 'node-1-3' },
  ]
  assert.deepEqual(rowsOf(events, fleet()), ['node-1', 'node-1-2', 'node-1-3'])
})
