// node --test docs/.vitepress/theme/components/simulator/ruler.test.ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { buildRuler, longestLifetimeMs, scaleWithin, worstObservedRotationMs } from './ruler.ts'
import type { Fleet, SimResponse, SimResult } from './model.ts'

const HOUR = 3_600_000
const MIN = 60_000

const FLEET: Fleet = {
  expireAfter: '480h',
  terminationGracePeriod: '1h',
  nodes: [
    { name: 'node-1', createdAt: '2026-01-01T00:00:00Z' },
    { name: 'node-2', createdAt: '2026-01-08T00:00:00Z', expireAfter: '720h' },
  ],
}

const RESULT: SimResult = {
  ageThreshold: '286h43m0s',
  tRot: '1h17m0s',
  tRotEstimate: '15m0s',
  drainEstimate: '10m0s',
  provisioningEstimate: '5m0s',
  g: 2,
  c: 10,
}

const inputs = (resp: SimResponse) => ({
  result: RESULT, fleet: FLEET, resp,
  provisioningMs: 5 * MIN, drainMs: 10 * MIN,
  readyTimeout: '15m', cooldownAfter: '10m', tgp: '1h',
})

test('the denominator is the LONGEST effective expireAfter, not the template', () => {
  // node-2 overrides the template. A heterogeneous fleet has no single lifetime, so the
  // ratio names which one it used.
  assert.equal(longestLifetimeMs(FLEET), 720 * HOUR)
})

test('the numerator is the WORST OBSERVED rotation — never an average, never one in flight', () => {
  const resp: SimResponse = {
    rotations: [
      { slot: 0, fromGen: 0, mode: 'surge', start: '2026-01-14T02:00:00Z', done: '2026-01-14T02:15:00Z' },
      { slot: 1, fromGen: 0, mode: 'surge', start: '2026-01-20T02:00:00Z', done: '2026-01-20T02:40:00Z' },
      // In flight: its duration is not established, and simulatedThrough − start would be a
      // number the simulation never reported.
      { slot: 0, fromGen: 1, mode: 'surge', start: '2026-01-28T02:00:00Z' },
    ],
  }
  assert.equal(worstObservedRotationMs(resp), 40 * MIN)

  const ratio = buildRuler(inputs(resp)).ratio!
  assert.equal(ratio.numeratorMs, 40 * MIN)
  assert.equal(ratio.denominatorMs, 720 * HOUR)
  assert.equal(ratio.forecast, false)
})

test('when nothing completed, the numerator falls back to the FORECAST — and says so', () => {
  const resp: SimResponse = {
    rotations: [{ slot: 0, fromGen: 0, mode: 'surge', start: '2026-01-14T02:00:00Z' }],
  }
  assert.equal(worstObservedRotationMs(resp), null)

  const ratio = buildRuler(inputs(resp)).ratio!
  assert.equal(ratio.numeratorMs, 15 * MIN, 't_rot_est')
  // Mixing a forecast numerator with an actual denominator is acceptable; doing it SILENTLY
  // is not — an operator who reads a forecast as a measurement trusts a run that never
  // happened.
  assert.equal(ratio.forecast, true)
})

test('the two groups are two SCALES: a bar is measured against its own group', () => {
  const { lifecycle, rotation } = buildRuler(inputs({}))

  // Against expireAfter, terminationGracePeriod is 0.2% — five of six bars would have no
  // length on one shared scale, which is a duration list with decoration, not a chart.
  const life = scaleWithin(lifecycle)
  assert.equal(life(lifecycle[0]), 1)
  assert.ok(life(lifecycle[1]) > 0.3, 'ageThreshold is a large fraction of a lifetime')

  const rot = scaleWithin(rotation)
  // t_rot (readyTimeout + tGP + buffer = 1h17m) is the longest quantity on this scale, and
  // the cap it contains is most of it.
  assert.equal(rot(rotation.find(b => b.key === 'tRot')!), 1)
  assert.ok(rot(rotation.find(b => b.key === 'tgp')!) > 0.7)
  // And a 10-minute drain — a sub-pixel sliver on the lifecycle scale — is legible here.
  assert.ok(rot(rotation.find(b => b.key === 'drain')!) > 0.1)
})

test('quantities are typed, because a forecast and a measurement are not the same thing', () => {
  const { lifecycle, rotation } = buildRuler(inputs({}))
  assert.equal(lifecycle.find(b => b.key === 'ageThreshold')!.quantity, 'lifecycle')
  assert.equal(rotation.find(b => b.key === 'tRot')!.quantity, 'bound')
  assert.equal(rotation.find(b => b.key === 'tRotEstimate')!.quantity, 'forecast')
  assert.equal(rotation.find(b => b.key === 'tgp')!.quantity, 'cap')
  assert.equal(rotation.find(b => b.key === 'drain')!.quantity, 'actual')
})

test('a zero-length quantity is dropped rather than drawn as an empty bar', () => {
  const { rotation } = buildRuler({ ...inputs({}), cooldownAfter: '', tgp: '' })
  assert.equal(rotation.find(b => b.key === 'cooldownAfter'), undefined)
  assert.equal(rotation.find(b => b.key === 'tgp'), undefined)
})
