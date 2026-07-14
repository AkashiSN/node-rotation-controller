// node --test — the derivation rows: the page SUBSTITUTES, it never computes. Pure, so the
// rules that matter (the override note, the tGP fallback marker, a response with no inputs)
// are covered here rather than through a DOM.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { buildDerivation, tidyDuration } from './derivation.ts'
import type { SimResult } from './model.ts'

const LABELS = {
  overrideNote: 'the value given in spec.ageThreshold (auto derives E − (K·P + t_rot))',
  fallbackMark: 'fallback',
}

const RESULT: SimResult = {
  ageThreshold: '310h43m0s', tRot: '1h17m0s', tRotEstimate: '15m0s',
  drainEstimate: '10m0s', provisioningEstimate: '5m0s', g: 2, c: 10,
  inputs: {
    e: '480h0m0s', tgp: '1h0m0s', tgpFallback: false, p: '84h0m0s', windowLen: '4h0m0s',
    buffer: '2m0s', readyTimeout: '15m0s', cooldownAfter: '10m0s',
    k: 2, m: 1, nodeCount: 3, ageThresholdOverride: false,
  },
}

test('a Go duration reads as the operator wrote it, without the zero units', () => {
  assert.equal(tidyDuration('480h0m0s'), '480h')
  assert.equal(tidyDuration('1h17m0s'), '1h17m')
  assert.equal(tidyDuration('15m0s'), '15m')
  assert.equal(tidyDuration('0s'), '0s')      // a real zero stays a zero
  assert.equal(tidyDuration(''), '—')          // absent, not zero
})

test('a NEGATIVE duration keeps its sign — a fatal A must not read as positive', () => {
  // schedule.Derive's ANonPositive case: A = E − (K·P + t_rot) went negative. The strip is
  // shown precisely to surface this; losing the "-" would make a fatal config look plausible.
  assert.equal(tidyDuration('-1h17m0s'), '-1h17m')
})

test('sub-second components (Go can emit these) pass through untouched', () => {
  assert.equal(tidyDuration('500ms'), '500ms')
  assert.equal(tidyDuration('1.5s'), '1.5s')
})

test('every symbol carries its formula, this run substituted into it, and its value', () => {
  const rows = buildDerivation(RESULT, LABELS)
  assert.deepEqual(rows.map(r => r.symbol), ['A', 't_rot', 't_rot_est', 'G', 'C'])
  // The formulas are the specification's own (§1.4/§3.2), verbatim — pinned here so a
  // corrupted or deleted formula fails a test, not just a reader's trust.
  assert.deepEqual(rows.map(r => r.formula), [
    'A = E − (K·P + t_rot)',
    't_rot = readyTimeout + tGP + buffer',
    't_rot_est = provisioningEstimate + drainEstimate',
    'G = floor(((E − t_rot) − A) / P)',
    'C = m · ceil(D / (t_rot_est + cooldownAfter))',
  ])
  assert.deepEqual(rows.map(r => r.value), ['310h43m', '1h17m', '15m', '2', '10'])
  assert.deepEqual(rows.map(r => r.substitution), [
    '480h − (2·84h + 1h17m)',
    '15m + 1h + 2m',
    '5m + 10m',
    'floor(((480h − 1h17m) − 310h43m) / 84h)',
    '1 · ceil(4h / (15m + 10m))',
  ])
  // No row explains itself away when the derivation actually holds.
  assert.deepEqual(rows.map(r => r.note), ['', '', '', '', ''])
})

test('an OVERRIDDEN ageThreshold prints a note, never an equation that does not hold', () => {
  const rows = buildDerivation(
    { ...RESULT, ageThreshold: '240h0m0s', inputs: { ...RESULT.inputs!, ageThresholdOverride: true } },
    LABELS,
  )
  const a = rows[0]
  assert.equal(a.substitution, '')
  assert.equal(a.note, LABELS.overrideNote)
  assert.equal(a.value, '240h')
  // G is still derived — against the A that was actually used.
  assert.equal(rows[3].substitution, 'floor(((480h − 1h17m) − 240h) / 84h)')
})

test('a tGP the operator never wrote is MARKED as the fallback', () => {
  const rows = buildDerivation(
    { ...RESULT, inputs: { ...RESULT.inputs!, tgp: '1h0m0s', tgpFallback: true } },
    LABELS,
  )
  assert.equal(rows[1].substitution, '15m + 1h (fallback) + 2m')
})

test('a FATAL negative ageThreshold renders as negative, never laundered positive', () => {
  // The wire still carries this run (ANonPositive is a Fatal finding, not a dropped response):
  // the strip's job is to show the operator exactly the value that tripped the guard.
  const rows = buildDerivation({ ...RESULT, ageThreshold: '-1h17m0s' }, LABELS)
  assert.equal(rows[0].value, '-1h17m')
})

test('a response with no inputs still renders — the values, and no equations', () => {
  const rows = buildDerivation({ ...RESULT, inputs: undefined }, LABELS)
  assert.deepEqual(rows.map(r => r.value), ['310h43m', '1h17m', '15m', '2', '10'])
  assert.deepEqual(rows.map(r => r.substitution), ['', '', '', '', ''])
  // The symbolic formulas do not depend on the run, so they stay — the exact same five as
  // when inputs ARE present.
  assert.deepEqual(rows.map(r => r.formula), [
    'A = E − (K·P + t_rot)',
    't_rot = readyTimeout + tGP + buffer',
    't_rot_est = provisioningEstimate + drainEstimate',
    'G = floor(((E − t_rot) − A) / P)',
    'C = m · ceil(D / (t_rot_est + cooldownAfter))',
  ])
})
