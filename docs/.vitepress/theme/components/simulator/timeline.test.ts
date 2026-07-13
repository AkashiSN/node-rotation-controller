// node --test docs/.vitepress/theme/components/simulator/timeline.test.ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { blockedOf, buildTimeline, rotationInstants, windowsOf } from './timeline.ts'
import type { Fleet, Horizon, SimResponse } from './model.ts'

const FLEET: Fleet = {
  expireAfter: '480h',
  terminationGracePeriod: '1h',
  nodes: [
    { name: 'node-1', createdAt: '2026-01-01T00:00:00Z' },
    { name: 'node-2', createdAt: '2026-01-08T00:00:00Z' },
  ],
}

const HORIZON: Horizon = { start: '2026-01-01T00:00:00Z', end: '2026-02-10T00:00:00Z' }

const ms = (iso: string) => new Date(iso).getTime()

/** A slot's initial generation, with the fields the wire always carries. */
function gen0(slot: number, name: string, createdAt: string, deadline: string) {
  return {
    slot, gen: 0, name, birthMode: 'initial' as const,
    createdAt, expireAfter: '480h',
    drainCap: '1h', drainCapSource: 'explicit' as const,
    deadline, eligibilityBoundary: '2026-01-15T00:00:00Z',
  }
}

/** A surged replacement of slot 0. */
function surged(gen: number, name: string, createdAt: string, readyAt?: string) {
  return {
    slot: 0, gen, name, birthMode: 'surge' as const, predecessorGen: gen - 1,
    createdAt, expireAfter: '480h',
    drainCap: '1h', drainCapSource: 'explicit' as const,
    deadline: '2026-02-03T02:00:00Z', eligibilityBoundary: '2026-01-28T00:00:00Z',
    ...(readyAt ? { readyAt } : {}),
  }
}

test('one row per SLOT: a slot that rotated twice is still ONE row', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [
      gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'),
      surged(1, 'node-1-r1', '2026-01-14T02:00:00Z', '2026-01-14T02:05:00Z'),
      surged(2, 'node-1-r2', '2026-01-28T02:00:00Z', '2026-01-28T02:05:00Z'),
      gen0(1, 'node-2', '2026-01-08T00:00:00Z', '2026-01-28T00:00:00Z'),
    ],
    rotations: [
      {
        slot: 0, fromGen: 0, toGen: 1, mode: 'surge',
        start: '2026-01-14T02:00:00Z', ready: '2026-01-14T02:05:00Z', done: '2026-01-14T02:15:00Z',
      },
      {
        slot: 0, fromGen: 1, toGen: 2, mode: 'surge',
        start: '2026-01-28T02:00:00Z', ready: '2026-01-28T02:05:00Z', done: '2026-01-28T02:15:00Z',
      },
    ],
  }
  const tl = buildTimeline(resp, HORIZON, FLEET)

  // The old chart gave each generation its own row, so a rotated node looked like an
  // unrelated node appearing from nowhere — "why did node-1 rotate twice?".
  assert.equal(tl.rows.length, 2, 'the row count is the fleet size, not the generation count')
  assert.deepEqual(tl.rows.map(r => r.label), ['node-1', 'node-2'])
  assert.deepEqual(tl.rows[0].generations.map(g => g.name), ['node-1', 'node-1-r1', 'node-1-r2'])
  assert.equal(tl.anomalies.length, 0)
})

test('gen n+1 PROVISIONS while gen n is still RUNNING — that coexistence is make-before-break', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [
      gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'),
      surged(1, 'node-1-r1', '2026-01-14T02:00:00Z', '2026-01-14T02:05:00Z'),
    ],
    rotations: [{
      slot: 0, fromGen: 0, toGen: 1, mode: 'surge',
      start: '2026-01-14T02:00:00Z', ready: '2026-01-14T02:05:00Z', done: '2026-01-14T02:15:00Z',
    }],
  }
  const [old, next] = buildTimeline(resp, HORIZON, FLEET).rows[0].generations

  const drain = old.segments.find(s => s.kind === 'drain')!
  const provisioning = next.segments.find(s => s.kind === 'provisioning')!

  // The old node's drain starts when the surge node is READY — its NodeClaim is deleted
  // then — and the replacement had been provisioning since the rotation's start, five
  // minutes earlier. That overlap is the whole point of the chart.
  assert.equal(drain.startMs, ms('2026-01-14T02:05:00Z'))
  assert.equal(drain.endMs, ms('2026-01-14T02:15:00Z'))
  assert.equal(provisioning.startMs, ms('2026-01-14T02:00:00Z'))
  assert.equal(provisioning.endMs, ms('2026-01-14T02:05:00Z'))
  // THE OVERLAP: the old node keeps RUNNING until its NodeClaim is deleted at `ready`, and
  // the replacement is provisioning throughout that span. Two nodes, coexisting — that is
  // make-before-break, and it is what the chart exists to show.
  const running = old.segments.find(s => s.kind === 'running')!
  assert.equal(running.startMs, ms('2026-01-01T00:00:00Z'))
  assert.equal(running.endMs, ms('2026-01-14T02:05:00Z'), 'running ends where the DRAIN begins, not at the rotation start')
  const overlap = Math.min(running.endMs!, provisioning.endMs!) - Math.max(running.startMs, provisioning.startMs)
  assert.equal(overlap, 5 * 60_000, 'the replacement provisioned for 5 minutes while the old node still ran')
})

test('TGP is a CAP: the cap endpoint is a different instant from the drain end', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z')],
    rotations: [{
      slot: 0, fromGen: 0, toGen: 1, mode: 'surge',
      start: '2026-01-14T02:00:00Z', ready: '2026-01-14T02:05:00Z', done: '2026-01-14T02:15:00Z',
    }],
  }
  const g = buildTimeline(resp, HORIZON, FLEET).rows[0].generations[0]

  // The drain took 10m; the cap was 1h. Both are on the record, as DIFFERENT instants, so
  // "the drain took 10m, the cap was 1h" is legible as geometry rather than as a marker
  // sitting at the drain's end.
  assert.equal(g.drainStartMs, ms('2026-01-14T02:05:00Z'))
  assert.equal(g.drainEndMs, ms('2026-01-14T02:15:00Z'))
  assert.equal(g.drainCapEndMs, ms('2026-01-14T03:05:00Z'))
  assert.equal(g.drainCapSource, 'explicit')
})

test('an in-flight surge: the replacement is provisional and its boundary is OPEN, not zero', () => {
  const through = '2026-01-14T02:03:00Z' // mid-provisioning
  const resp: SimResponse = {
    simulatedThrough: through,
    generations: [
      gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'),
      { ...surged(1, 'node-1-r1', '2026-01-14T02:00:00Z'), provisional: true },
    ],
    rotations: [{ slot: 0, fromGen: 0, toGen: 1, mode: 'surge', start: '2026-01-14T02:00:00Z' }],
  }
  const [old, next] = buildTimeline(resp, HORIZON, FLEET).rows[0].generations

  assert.equal(next.provisional, true)
  const provisioning = next.segments.find(s => s.kind === 'provisioning')!
  assert.equal(provisioning.endMs, null, 'an absent boundary is UNKNOWN, never zero-length')
  assert.equal(provisioning.openReason, 'in-flight')
  assert.equal(provisioning.openToMs, ms(through))

  // The surge never went Ready, so the old node's drain never started — there is no drain
  // segment at all, and certainly not a zero-length one at the rotation's start.
  assert.equal(old.segments.find(s => s.kind === 'drain'), undefined)
  assert.equal(old.drainStartMs, null)
})

test('in-flight and malformed are DIFFERENT: a completed surge with no ready is a bug, not a truncation', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [
      gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'),
      // No readyAt, yet the rotation below is DONE: the wire contradicts itself.
      surged(1, 'node-1-r1', '2026-01-14T02:00:00Z'),
    ],
    rotations: [{
      slot: 0, fromGen: 0, toGen: 1, mode: 'surge',
      start: '2026-01-14T02:00:00Z', done: '2026-01-14T02:15:00Z',
    }],
  }
  const [old, next] = buildTimeline(resp, HORIZON, FLEET).rows[0].generations

  assert.equal(next.segments.find(s => s.kind === 'provisioning')!.openReason, 'malformed')
  // Without `ready` the drain's START is unknowable, so the span is hatched as malformed
  // rather than guessed at. The two reasons never share a label: to a reader they mean
  // opposite things — "the simulation ended" versus "this response is wrong".
  const drain = old.segments.find(s => s.kind === 'drain')!
  assert.equal(drain.openReason, 'malformed')
  assert.equal(drain.endMs, null)
})

test('a surge-less rotation in flight: no ready, no replacement, no invented drain start', () => {
  const through = '2026-01-14T02:10:00Z'
  const resp: SimResponse = {
    simulatedThrough: through,
    generations: [gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z')],
    rotations: [{ slot: 0, fromGen: 0, mode: 'surgeless', start: '2026-01-14T02:00:00Z' }],
  }
  const row = buildTimeline(resp, HORIZON, FLEET).rows[0]

  // The surge-less path stages no placeholder, so the drain begins at the rotation's START.
  const drain = row.generations[0].segments.find(s => s.kind === 'drain')!
  assert.equal(drain.startMs, ms('2026-01-14T02:00:00Z'))
  assert.equal(drain.endMs, null)
  assert.equal(drain.openReason, 'in-flight')
  assert.equal(drain.openToMs, ms(through))
  // And no replacement is invented: it is born at `done`, which never came.
  assert.equal(row.generations.length, 1)
})

test('a generation that never rotates runs to simulatedThrough — continuing, not truncated', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z')],
    rotations: [],
  }
  const g = buildTimeline(resp, HORIZON, FLEET).rows[0].generations[0]
  const running = g.segments.find(s => s.kind === 'running')!
  assert.equal(running.endMs, ms(HORIZON.end))
  assert.equal(running.openReason, null, 'known-alive is not unknown: it ran, and the run ended')
})

test('the breach sits exactly ON the deadline, attached to the generation that owns it', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z')],
    events: [{ kind: 'expire-after-breach', at: '2026-01-21T00:00:00Z', node: 'node-1' }],
  }
  const g = buildTimeline(resp, HORIZON, FLEET).rows[0].generations[0]
  // The simulator emits the breach AT the deadline by construction, so "no breach right of
  // the deadline" is vacuous and is NOT claimed. What the chart owes the reader is that the
  // two glyphs stay distinguishable — a regression there hides every breach in the run.
  assert.equal(g.breachMs, g.deadlineMs)
})

test('a duplicate (slot, gen) is reported, never drawn stacked', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [
      gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'),
      gen0(0, 'node-1-dup', '2026-01-02T00:00:00Z', '2026-01-22T00:00:00Z'),
    ],
  }
  const tl = buildTimeline(resp, HORIZON, FLEET)
  assert.equal(tl.rows[0].generations.length, 1)
  assert.equal(tl.rows[0].generations[0].name, 'node-1')
  assert.match(tl.anomalies[0], /duplicate generation/)
})

test('a slot the fleet never declared gets its own appended row, and is reported', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [
      gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'),
      gen0(1, 'node-2', '2026-01-08T00:00:00Z', '2026-01-28T00:00:00Z'),
      gen0(7, 'ghost', '2026-01-08T00:00:00Z', '2026-01-28T00:00:00Z'),
    ],
  }
  const tl = buildTimeline(resp, HORIZON, FLEET)
  assert.equal(tl.rows.length, 3, 'an unplaceable row is APPENDED, never dropped')
  assert.equal(tl.rows[2].declared, false)
  assert.equal(tl.rows[2].label, 'ghost')
  assert.match(tl.anomalies.join(' '), /slot 7/)
})

test('a replacement whose name collides with a declared node is still placed by (slot, gen)', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [
      gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'),
      gen0(1, 'node-2', '2026-01-08T00:00:00Z', '2026-01-28T00:00:00Z'),
      // A replacement in slot 0 that happens to carry the declared node-2's name.
      surged(1, 'node-2', '2026-01-14T02:00:00Z', '2026-01-14T02:05:00Z'),
    ],
  }
  const tl = buildTimeline(resp, HORIZON, FLEET)
  assert.equal(tl.rows[0].generations.length, 2, 'the collision lands in slot 0, by (slot, gen)')
  assert.equal(tl.rows[1].generations.length, 1)
})

test('a rotation naming a generation the response does not carry is reported', () => {
  const resp: SimResponse = {
    simulatedThrough: HORIZON.end,
    generations: [gen0(0, 'node-1', '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z')],
    rotations: [{
      slot: 0, fromGen: 0, toGen: 1, mode: 'surge',
      start: '2026-01-14T02:00:00Z', ready: '2026-01-14T02:05:00Z', done: '2026-01-14T02:15:00Z',
    }],
  }
  assert.match(buildTimeline(resp, HORIZON, FLEET).anomalies.join(' '), /names a generation/)
})

test('windows carry their clipped boundaries; a malformed one is dropped', () => {
  const wins = windowsOf({
    windows: [
      { start: '2026-01-03T03:00:00Z', end: '2026-01-03T06:00:00Z', startClipped: true },
      { start: '2026-01-07T02:00:00Z', end: '2026-01-07T06:00:00Z' },
      { start: 'nonsense', end: '2026-01-07T06:00:00Z' },
    ],
  })
  assert.equal(wins.length, 2)
  assert.equal(wins[0].startClipped, true)
  assert.equal(wins[0].endClipped, false, 'absent means false — the wire omits a false flag')
  assert.equal(wins[1].startClipped, false)
})

test('a blocked interval with no `until` runs to simulatedThrough, not to a zero-width band', () => {
  const through = ms('2026-01-20T00:00:00Z')
  const bands = blockedOf([
    { kind: 'blocked-by-gate', at: '2026-01-02T06:00:00Z', until: '2026-01-03T02:00:00Z', gate: 'outOfWindow' },
    { kind: 'no-eligible-claim', at: '2026-01-19T02:00:00Z' },
  ], through)
  assert.equal(bands.length, 2)
  assert.equal(bands[1].endMs, through)
  assert.equal(bands[1].label, 'no-eligible-claim')
})

test('rotationInstants are the navigation targets, sorted', () => {
  const at = rotationInstants({
    rotations: [
      { slot: 1, fromGen: 0, mode: 'surge', start: '2026-01-20T02:00:00Z' },
      { slot: 0, fromGen: 0, mode: 'surge', start: '2026-01-14T02:00:00Z' },
    ],
  })
  assert.deepEqual(at, [ms('2026-01-14T02:00:00Z'), ms('2026-01-20T02:00:00Z')])
})

test('an empty response still yields the fleet rows — the page never blanks', () => {
  const tl = buildTimeline({}, HORIZON, FLEET)
  assert.deepEqual(tl.rows.map(r => r.label), ['node-1', 'node-2'])
  assert.equal(tl.simulatedThroughMs, ms(HORIZON.end))
})

test('50 nodes over a 3x horizon: the build stays linear, and the row count stays the fleet size', () => {
  // Both counts grow: 50 slots, and several generations each. The rows must NOT — that is
  // the property the whole redesign turns on. And the build must not go quadratic in
  // (generations x rotations), which is exactly what a naive "find my rotation" scan per
  // generation would do on every keystroke of the fleet form.
  const SLOTS = 50, GENS = 6
  const fleet: Fleet = {
    expireAfter: '480h',
    terminationGracePeriod: '1h',
    nodes: Array.from({ length: SLOTS }, (_, i) => ({
      name: `node-${i}`, createdAt: '2026-01-01T00:00:00Z',
    })),
  }
  const generations = []
  const rotations = []
  for (let slot = 0; slot < SLOTS; slot++) {
    generations.push(gen0(slot, `node-${slot}`, '2026-01-01T00:00:00Z', '2026-01-21T00:00:00Z'))
    for (let gen = 1; gen < GENS; gen++) {
      const day = 10 + gen * 10
      const start = `2026-01-${String(day).padStart(2, '0')}T02:00:00Z`
      generations.push({
        slot, gen, name: `node-${slot}-r${gen}`, birthMode: 'surge' as const, predecessorGen: gen - 1,
        createdAt: start, expireAfter: '480h',
        drainCap: '1h', drainCapSource: 'explicit' as const,
        deadline: '2026-03-01T02:00:00Z', eligibilityBoundary: '2026-02-20T02:00:00Z',
        readyAt: `2026-01-${String(day).padStart(2, '0')}T02:05:00Z`,
      })
      rotations.push({
        slot, fromGen: gen - 1, toGen: gen, mode: 'surge' as const,
        start, ready: `2026-01-${String(day).padStart(2, '0')}T02:05:00Z`,
        done: `2026-01-${String(day).padStart(2, '0')}T02:15:00Z`,
      })
    }
  }

  const began = performance.now()
  const tl = buildTimeline(
    { simulatedThrough: HORIZON.end, generations, rotations },
    HORIZON,
    fleet,
  )
  const elapsed = performance.now() - began

  assert.equal(tl.rows.length, SLOTS, 'the row count is the fleet size, not the generation count')
  assert.equal(tl.rows[0].generations.length, GENS)
  assert.equal(tl.anomalies.length, 0)
  // A generous ceiling — this is a guard against a quadratic regression, not a benchmark.
  assert.ok(elapsed < 250, `buildTimeline took ${elapsed.toFixed(0)}ms for ${SLOTS} slots`)
})
