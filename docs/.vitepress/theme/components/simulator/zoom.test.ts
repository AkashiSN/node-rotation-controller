// node --test docs/.vitepress/theme/components/simulator/zoom.test.ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  MIN_VIEW_MS, SEMANTIC, centreOn, chooseStep, clampView, fitTo, nextTarget, panBy,
  prevTarget, pxOf, reconcileView, ticksOf, zoomBy,
} from './zoom.ts'
import { DAY_MS, HOUR_MS, MINUTE_MS } from './timeutil.ts'

const ms = (iso: string) => new Date(iso).getTime()
const WIDTH = 900

test('the tick ladder picks the FINEST step whose labels still clear the gap', () => {
  // A 40-day view across 900px: a daily tick would sit 22px apart, so days are too dense.
  assert.equal(chooseStep(40 * DAY_MS, WIDTH), 7 * DAY_MS)
  // Two days: hours would be 18px apart; 6h clears the gap.
  assert.equal(chooseStep(2 * DAY_MS, WIDTH), 6 * HOUR_MS)
  // Four hours: 15m ticks are 56px apart — under the 64px gap — so hours it is.
  assert.equal(chooseStep(4 * HOUR_MS, WIDTH), HOUR_MS)
  // Half an hour: minutes are 30px apart, so 5m.
  assert.equal(chooseStep(30 * MINUTE_MS, WIDTH), 5 * MINUTE_MS)
  // Ten minutes: minutes clear the gap comfortably — the finest rung, and the finest the
  // simulator itself resolves.
  assert.equal(chooseStep(10 * MINUTE_MS, WIDTH), MINUTE_MS)
})

test('ticks are labelled in the DISPLAY timezone, never the browser\'s', () => {
  const view = { startMs: ms('2026-01-03T00:00:00Z'), endMs: ms('2026-01-03T06:00:00Z') }
  const { fine } = ticksOf(view, 'Asia/Tokyo', WIDTH)
  // 00:00Z is 09:00 in Tokyo. A UTC-labelled axis would say 00:00 and be wrong for every
  // policy whose window is not in UTC — which is the interesting case.
  assert.equal(fine[0].label, '09:00')
})

test('a DST day still ticks on its own local grid (23h and 25h days)', () => {
  // Europe/Berlin springs forward at 02:00 local on 2026-03-29: 02:00–03:00 does not exist.
  const view = { startMs: ms('2026-03-29T00:00:00Z'), endMs: ms('2026-03-29T04:00:00Z') }
  const { fine } = ticksOf(view, 'Europe/Berlin', WIDTH)
  const labels = fine.map(t => t.label)
  // The hour the clock skipped has no tick of its own: re-anchoring on local midnight is
  // what keeps the grid on the hour instead of drifting to :30 for the rest of the day.
  assert.ok(!labels.includes('02:00'), `02:00 does not exist on this date, got ${labels.join(',')}`)
  assert.ok(labels.includes('03:00'))
  // And every tick is strictly increasing in real time.
  for (let i = 1; i < fine.length; i++) assert.ok(fine[i].ms > fine[i - 1].ms)
})

test('semantic thresholds: an element is drawn only once it is wide enough to mean something', () => {
  const view = { startMs: 0, endMs: 40 * DAY_MS }
  // A 10-minute drain across a 40-day view is a fifth of a pixel. It is not a short drain,
  // it is an illegible one — and the old chart drew it anyway.
  assert.ok(pxOf(10 * MINUTE_MS, view, WIDTH) < SEMANTIC.segmentPx)
  // Zoomed to an hour, the same drain is a third of the width.
  const zoomed = { startMs: 0, endMs: HOUR_MS }
  assert.ok(pxOf(10 * MINUTE_MS, zoomed, WIDTH) > SEMANTIC.capBracketPx)
})

// The units are real: a view is never narrower than the simulator's one-minute cadence, so
// a fixture in bare milliseconds would be testing the clamp, not the logic.
const H = HOUR_MS

test('clampView holds the view inside the horizon, keeping its width where it fits', () => {
  const horizon = { startMs: 0, endMs: 100 * H }
  assert.deepEqual(clampView({ startMs: -50 * H, endMs: -10 * H }, horizon), { startMs: 0, endMs: 40 * H })
  assert.deepEqual(clampView({ startMs: 90 * H, endMs: 130 * H }, horizon), { startMs: 60 * H, endMs: 100 * H })
  // Wider than the horizon: collapse to it rather than showing time that was not simulated.
  assert.deepEqual(clampView({ startMs: -500 * H, endMs: 500 * H }, horizon), { startMs: 0, endMs: 100 * H })
})

test('a rerun moves the horizon: a whole-horizon view stays whole, any other view keeps its instants', () => {
  const before = { startMs: 0, endMs: 100 * H }
  const after = { startMs: 0, endMs: 200 * H }

  // The default view was not a choice — it was the whole horizon. It stays the whole one.
  assert.deepEqual(reconcileView({ startMs: 0, endMs: 100 * H }, before, after), { startMs: 0, endMs: 200 * H })

  // A view the visitor chose is theirs: keep the instants, clamped.
  assert.deepEqual(reconcileView({ startMs: 40 * H, endMs: 60 * H }, before, after), { startMs: 40 * H, endMs: 60 * H })

  // A view longer than the NEW horizon collapses to it instead of scaling to nothing.
  const shrunk = { startMs: 0, endMs: 10 * H }
  assert.deepEqual(reconcileView({ startMs: 40 * H, endMs: 60 * H }, before, shrunk), { startMs: 0, endMs: 10 * H })
})

test('zoom keeps the anchored instant under the pointer, and never goes below one minute', () => {
  const horizon = { startMs: 0, endMs: 10 * DAY_MS }
  const view = { startMs: 0, endMs: DAY_MS }
  const anchor = view.startMs + DAY_MS / 4 // a quarter in

  const zoomed = zoomBy(view, 0.5, anchor, horizon)
  assert.equal(zoomed.endMs - zoomed.startMs, DAY_MS / 2)
  const fracBefore = (anchor - view.startMs) / DAY_MS
  const fracAfter = (anchor - zoomed.startMs) / (zoomed.endMs - zoomed.startMs)
  assert.ok(Math.abs(fracBefore - fracAfter) < 1e-9, 'the instant under the pointer does not slide')

  // A zoom that would resolve below the simulator's own one-minute cadence is capped.
  const deep = zoomBy({ startMs: 0, endMs: 2 * MINUTE_MS }, 0.01, MINUTE_MS, horizon)
  assert.equal(deep.endMs - deep.startMs, MIN_VIEW_MS)

  // Zooming out is bounded by the horizon, not by the view's own arithmetic.
  const out = zoomBy(view, 100, anchor, horizon)
  assert.deepEqual(out, { startMs: 0, endMs: 10 * DAY_MS })
})

test('pan is clamped at the horizon bounds instead of running off the end', () => {
  const horizon = { startMs: 0, endMs: 100 * H }
  assert.deepEqual(panBy({ startMs: 80 * H, endMs: 100 * H }, 50 * H, horizon), { startMs: 80 * H, endMs: 100 * H })
  assert.deepEqual(panBy({ startMs: 0, endMs: 20 * H }, -50 * H, horizon), { startMs: 0, endMs: 20 * H })
})

test('fitTo pads its target, and a zero-width target still yields a usable view', () => {
  const horizon = { startMs: 0, endMs: 10 * DAY_MS }
  const fitted = fitTo(DAY_MS, 2 * DAY_MS, horizon)
  assert.ok(fitted.startMs < DAY_MS && fitted.endMs > 2 * DAY_MS, 'the target has breathing room')

  // A rotation whose start and end coincide (a zero-length interval) must not collapse the
  // view to nothing — the reader would be looking at an empty chart with no way back.
  const degenerate = fitTo(DAY_MS, DAY_MS, horizon)
  assert.ok(degenerate.endMs - degenerate.startMs >= MIN_VIEW_MS)
})

test('the rotation buttons are disabled at the bounds rather than silently doing nothing', () => {
  const targets = [ms('2026-01-14T02:00:00Z'), ms('2026-01-20T02:00:00Z'), ms('2026-01-28T02:00:00Z')]
  assert.equal(nextTarget(targets, targets[1]), targets[2])
  assert.equal(nextTarget(targets, targets[2]), null, 'past the last rotation there is nothing to go to')
  assert.equal(prevTarget(targets, targets[1]), targets[0])
  assert.equal(prevTarget(targets, targets[0]), null)
})

test('centreOn keeps the zoom level: stepping between rotations does not also rescale', () => {
  const horizon = { startMs: 0, endMs: 10 * DAY_MS }
  const view = { startMs: 0, endMs: DAY_MS }
  const centred = centreOn(view, 5 * DAY_MS, horizon)
  assert.equal(centred.endMs - centred.startMs, DAY_MS)
  assert.equal(centred.startMs, 5 * DAY_MS - DAY_MS / 2)
})
