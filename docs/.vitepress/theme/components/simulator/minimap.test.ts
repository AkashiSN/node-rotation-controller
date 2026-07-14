// node --test docs/.vitepress/theme/components/simulator/minimap.test.ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  HANDLE_MIN_BRUSH_UNITS, MIN_BRUSH_UNITS, brushBox, centreOn, grabAt, moveTo, msAt,
  resizeEdge, wheelPan,
} from './minimap.ts'
import { MIN_VIEW_MS } from './zoom.ts'
import { DAY_MS, HOUR_MS, MINUTE_MS } from './timeutil.ts'

const W = 1000
const T0 = new Date('2026-01-01T00:00:00Z').getTime()
/** Six weeks — the page's default horizon, and the scale at which the old brush vanished. */
const HORIZON = { startMs: T0, endMs: T0 + 42 * DAY_MS }

const at = (offsetMs: number) => T0 + offsetMs
const view = (fromMs: number, spanMs: number) => ({ startMs: at(fromMs), endMs: at(fromMs + spanMs) })

test('the brush spans the whole strip when the view is the whole horizon', () => {
  const box = brushBox(HORIZON, HORIZON, W)
  assert.equal(box.x, 0)
  assert.equal(box.w, W)
})

test('a deep zoom is WIDENED to the floor, and stays centred on the instants it names', () => {
  // 2.5 hours of a six-week horizon: 0.25% of the strip — 2.48 units, which is what used to
  // be clamped to a 2px hairline among the rotation ticks and lost.
  const v = view(20 * DAY_MS, 150 * MINUTE_MS)
  const box = brushBox(v, HORIZON, W)
  assert.equal(box.w, MIN_BRUSH_UNITS)

  // Widened about the CENTRE: the drawn box still points at the view.
  const trueCentre = ((v.startMs + v.endMs) / 2 - T0) / (42 * DAY_MS) * W
  assert.ok(Math.abs(box.x + box.w / 2 - trueCentre) < 1e-9,
    'the widened brush must be centred on the true range, not anchored to its start')
})

test('the widened brush never leaves the strip at either horizon edge', () => {
  const first = brushBox(view(0, MINUTE_MS), HORIZON, W)
  assert.equal(first.x, 0)
  assert.equal(first.w, MIN_BRUSH_UNITS)

  const last = brushBox(
    { startMs: HORIZON.endMs - MINUTE_MS, endMs: HORIZON.endMs }, HORIZON, W)
  assert.equal(last.x + last.w, W)
  assert.equal(last.w, MIN_BRUSH_UNITS)
})

test('a brush too narrow for handles is all MOVE — there is no edge that is not its middle', () => {
  const box = brushBox(view(20 * DAY_MS, 150 * MINUTE_MS), HORIZON, W)
  assert.ok(box.w < HANDLE_MIN_BRUSH_UNITS)
  // Every point of it, including its exact edges, moves the view. Were the handles live
  // here, their two hit zones would cover the brush entirely and it could not be dragged.
  assert.equal(grabAt(box.x, box), 'move')
  assert.equal(grabAt(box.x + box.w, box), 'move')
  assert.equal(grabAt(box.x + box.w / 2, box), 'move')
  assert.equal(grabAt(box.x - 1, box), 'outside')
})

test('a wide brush grabs by its edges to resize and by its middle to move', () => {
  const box = { x: 200, w: 300 }
  assert.equal(grabAt(200, box), 'left')
  assert.equal(grabAt(203, box), 'left')
  assert.equal(grabAt(500, box), 'right')
  assert.equal(grabAt(497, box), 'right')
  assert.equal(grabAt(350, box), 'move')
  assert.equal(grabAt(100, box), 'outside')
  assert.equal(grabAt(900, box), 'outside')
})

test('msAt maps the strip back onto the horizon', () => {
  assert.equal(msAt(0, HORIZON, W), HORIZON.startMs)
  assert.equal(msAt(W, HORIZON, W), HORIZON.endMs)
  assert.equal(msAt(W / 2, HORIZON, W), T0 + 21 * DAY_MS)
})

test('a drag PRESERVES the grab offset — the instant under the pointer stays under it', () => {
  const v = view(10 * DAY_MS, 6 * HOUR_MS)
  // Grabbed a quarter of the way into the brush; the pointer then travels two days right.
  const offset = 1.5 * HOUR_MS
  const grabbed = v.startMs + offset
  const moved = moveTo(v, grabbed + 2 * DAY_MS, offset, HORIZON)

  assert.equal(moved.startMs, at(12 * DAY_MS))
  assert.equal(moved.endMs - moved.startMs, 6 * HOUR_MS, 'a move never changes the zoom')
  // The bug this replaces: the old handler re-centred on the pointer, so the same grab
  // would have jumped the view's start by half its width the instant it was touched.
  assert.notEqual(moved.startMs, grabbed + 2 * DAY_MS - 3 * HOUR_MS)
})

test('a drag clamps at the horizon without changing the view width', () => {
  const v = view(41 * DAY_MS, 6 * HOUR_MS)
  const moved = moveTo(v, HORIZON.endMs + 10 * DAY_MS, 0, HORIZON)
  assert.equal(moved.endMs, HORIZON.endMs)
  assert.equal(moved.endMs - moved.startMs, 6 * HOUR_MS)
})

test('a click on the track centres the view there, keeping its width', () => {
  const v = view(10 * DAY_MS, 6 * HOUR_MS)
  const jumped = centreOn(v, at(30 * DAY_MS), HORIZON)
  assert.equal((jumped.startMs + jumped.endMs) / 2, at(30 * DAY_MS))
  assert.equal(jumped.endMs - jumped.startMs, 6 * HOUR_MS)
})

test('resizing an edge leaves the OPPOSITE edge exactly where it was', () => {
  const v = view(10 * DAY_MS, 6 * HOUR_MS)

  const left = resizeEdge(v, 'left', at(9 * DAY_MS), HORIZON)
  assert.equal(left.startMs, at(9 * DAY_MS))
  assert.equal(left.endMs, v.endMs)

  const right = resizeEdge(v, 'right', at(11 * DAY_MS), HORIZON)
  assert.equal(right.startMs, v.startMs)
  assert.equal(right.endMs, at(11 * DAY_MS))
})

test('an edge cannot cross its opposite, nor shrink the view below the decision cadence', () => {
  const v = view(10 * DAY_MS, 6 * HOUR_MS)

  // Dragged far past the right edge: it stops one minute short of it.
  const left = resizeEdge(v, 'left', at(30 * DAY_MS), HORIZON)
  assert.equal(left.endMs - left.startMs, MIN_VIEW_MS)
  assert.equal(left.endMs, v.endMs)

  const right = resizeEdge(v, 'right', at(0), HORIZON)
  assert.equal(right.endMs - right.startMs, MIN_VIEW_MS)
  assert.equal(right.startMs, v.startMs)
})

test('an edge cannot leave the horizon', () => {
  const v = view(1 * DAY_MS, 6 * HOUR_MS)
  assert.equal(resizeEdge(v, 'left', T0 - 10 * DAY_MS, HORIZON).startMs, HORIZON.startMs)

  const late = view(41 * DAY_MS, 6 * HOUR_MS)
  assert.equal(resizeEdge(late, 'right', HORIZON.endMs + DAY_MS, HORIZON).endMs, HORIZON.endMs)
})

test('the wheel PANS: one screenful per 400px, at every zoom level, width untouched', () => {
  const wide = view(10 * DAY_MS, 6 * HOUR_MS)
  const pannedWide = wheelPan(wide, 400, HORIZON)
  assert.equal(pannedWide.startMs, wide.startMs + 6 * HOUR_MS)
  assert.equal(pannedWide.endMs - pannedWide.startMs, 6 * HOUR_MS)

  // The same gesture on a ten-minute view moves ten minutes — the gesture means the same
  // thing to the reader whatever they are looking at.
  const tight = view(10 * DAY_MS, 10 * MINUTE_MS)
  const pannedTight = wheelPan(tight, 400, HORIZON)
  assert.equal(pannedTight.startMs, tight.startMs + 10 * MINUTE_MS)
  assert.equal(pannedTight.endMs - pannedTight.startMs, 10 * MINUTE_MS)

  // Backwards, and a partial gesture.
  assert.equal(wheelPan(wide, -200, HORIZON).startMs, wide.startMs - 3 * HOUR_MS)
})

test('the wheel clamps at the horizon rather than dragging the view out of it', () => {
  const v = view(41 * DAY_MS + 20 * HOUR_MS, 3 * HOUR_MS)
  const panned = wheelPan(v, 4000, HORIZON)
  assert.equal(panned.endMs, HORIZON.endMs)
  assert.equal(panned.endMs - panned.startMs, 3 * HOUR_MS)
})
