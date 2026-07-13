// node --test docs/.vitepress/theme/components/simulator/calendar.test.ts
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { CELLS_PER_DAY, buildCalendar, weekdayRows } from './calendar.ts'
import type { WindowView } from './timeline.ts'

const ms = (iso: string) => new Date(iso).getTime()

const win = (start: string, end: string, flags: Partial<WindowView> = {}): WindowView => ({
  startMs: ms(start), endMs: ms(end), startClipped: false, endClipped: false, ...flags,
})

/** The cell a local wall-clock time falls in. 0 = Monday. */
const cellAt = (cal: ReturnType<typeof buildCalendar>, weekday: number, hour: number, minute = 0) =>
  cal.cells[weekday * CELLS_PER_DAY + (hour * 60 + minute) / 15]

test('a weekly window folds to a full ratio in the cells it covers, and zero elsewhere', () => {
  // Saturdays 02:00–06:00 UTC, over four whole calendar weeks.
  const windows = [
    win('2026-01-03T02:00:00Z', '2026-01-03T06:00:00Z'),
    win('2026-01-10T02:00:00Z', '2026-01-10T06:00:00Z'),
    win('2026-01-17T02:00:00Z', '2026-01-17T06:00:00Z'),
    win('2026-01-24T02:00:00Z', '2026-01-24T06:00:00Z'),
  ]
  // Mon 2025-12-29 → Mon 2026-01-26: four whole weeks, and the windows above sit inside.
  const cal = buildCalendar(windows, ms('2025-12-29T00:00:00Z'), ms('2026-01-26T00:00:00Z'), 'UTC')

  assert.equal(cal.wholeWeeks, 4)
  // Saturday is weekday 5 (0 = Monday).
  assert.equal(cellAt(cal, 5, 2).ratio, 1, 'Saturday 02:00 was open in every observed week')
  assert.equal(cellAt(cal, 5, 5, 45).ratio, 1, 'and still open in the last quarter-hour')
  assert.equal(cellAt(cal, 5, 6).ratio, 0, 'the window is END-EXCLUSIVE: 06:00 is shut')
  assert.equal(cellAt(cal, 3, 3).ratio, 0, 'Thursday is never open')
})

test('a slot open in some weeks and not others reports the FRACTION, not a yes/no', () => {
  // Open on two of the four Saturdays.
  const windows = [
    win('2026-01-03T02:00:00Z', '2026-01-03T06:00:00Z'),
    win('2026-01-17T02:00:00Z', '2026-01-17T06:00:00Z'),
  ]
  const cal = buildCalendar(windows, ms('2025-12-29T00:00:00Z'), ms('2026-01-26T00:00:00Z'), 'UTC')
  assert.equal(cal.wholeWeeks, 4)
  assert.equal(cellAt(cal, 5, 3).ratio, 0.5)
})

test('a 5-minute clip is a FIFTH of its cell, not a full one and not nothing', () => {
  // One week, one window of five minutes inside a single 15-minute cell. Any binarisation
  // would have to call this either "open" (a whole cell) or "shut" (nothing); the ratio of
  // durations needs no threshold at all.
  const cal = buildCalendar(
    [win('2026-01-03T02:00:00Z', '2026-01-03T02:05:00Z')],
    ms('2025-12-29T00:00:00Z'), ms('2026-01-05T00:00:00Z'), 'UTC',
  )
  assert.equal(cal.wholeWeeks, 1)
  assert.ok(Math.abs(cellAt(cal, 5, 2).ratio! - 1 / 3) < 1e-9)
})

test('an ALWAYS-OPEN schedule reads 100%, not 0% — clipping is a property of a BOUNDARY', () => {
  // The catastrophic case. An always-open schedule yields exactly ONE interval, clipped at
  // both ends. Excluding clipped intervals wholesale would leave every cell at zero while
  // the window never closed at all.
  const cal = buildCalendar(
    [win('2025-12-20T00:00:00Z', '2026-01-26T00:00:00Z', { startClipped: true, endClipped: true })],
    ms('2025-12-29T00:00:00Z'), ms('2026-01-26T00:00:00Z'), 'UTC',
  )
  for (const row of weekdayRows(cal)) {
    for (const c of row) assert.equal(c.ratio, 1, `cell ${c.weekday}/${c.slot} must read 100%`)
  }
})

test('only WHOLE local weeks count: the partial weeks at the horizon edges are excluded', () => {
  // The span starts on a Thursday and ends on a Tuesday, so the first and last weeks are
  // partial. A half-observed Saturday would otherwise read as a window that is only half
  // open — a fact about the horizon, presented as a fact about the policy.
  const cal = buildCalendar(
    [win('2026-01-10T02:00:00Z', '2026-01-10T06:00:00Z')],
    ms('2026-01-01T00:00:00Z'), ms('2026-01-20T00:00:00Z'), 'UTC',
  )
  // Whole weeks: Mon 2026-01-05 → Mon 2026-01-19. Two.
  assert.equal(cal.wholeWeeks, 2)
  assert.equal(cal.fromMs, ms('2026-01-05T00:00:00Z'))
  assert.equal(cal.toMs, ms('2026-01-19T00:00:00Z'))
  // The window on the 10th (inside) counts; it is one of the two observed Saturdays.
  assert.equal(cellAt(cal, 5, 3).ratio, 0.5)
})

test('a horizon shorter than one whole week says so instead of dividing by zero', () => {
  const cal = buildCalendar(
    [win('2026-01-03T02:00:00Z', '2026-01-03T06:00:00Z')],
    ms('2026-01-02T00:00:00Z'), ms('2026-01-05T00:00:00Z'), 'UTC',
  )
  assert.equal(cal.wholeWeeks, 0)
  for (const c of cal.cells) assert.equal(c.ratio, null, 'unobserved is UNKNOWN, not zero')
})

test('a DST week is 167h or 169h: the hour the clock SKIPS is unobserved, never "shut"', () => {
  // Europe/Berlin springs forward at 02:00 local on Sunday 2026-03-29: local 02:00–03:00
  // never happens. Observed minutes there are zero, so the cells are UNKNOWN — which is a
  // different claim from "the maintenance window was closed".
  // The span is one LOCAL week: Mon 23 Mar 00:00 CET (= 22 Mar 23:00Z) → Mon 30 Mar 00:00
  // CEST (= 29 Mar 22:00Z). That is 167 real hours, and a 7 x 24h chunk would not be a week.
  const cal = buildCalendar([], ms('2026-03-22T23:00:00Z'), ms('2026-03-29T22:00:00Z'), 'Europe/Berlin')
  assert.equal(cal.wholeWeeks, 1)
  const skipped = cellAt(cal, 6, 2, 30) // Sunday 02:30 local — the hour that did not exist
  assert.equal(skipped.observedMinutes, 0)
  assert.equal(skipped.ratio, null)
  // The week really is 167 hours long, and every minute of it landed in some cell.
  const observed = cal.cells.reduce((sum, c) => sum + c.observedMinutes, 0)
  assert.equal(observed, 167 * 60)
})

test('a DST fall-back week is 169h: the repeated hour is observed TWICE', () => {
  // Berlin falls back at 03:00 local on Sunday 2026-10-25: local 02:00–03:00 happens twice.
  // One local week: Mon 19 Oct 00:00 CEST (= 18 Oct 22:00Z) → Mon 26 Oct 00:00 CET (= 25 Oct 23:00Z).
  const cal = buildCalendar([], ms('2026-10-18T22:00:00Z'), ms('2026-10-25T23:00:00Z'), 'Europe/Berlin')
  assert.equal(cal.wholeWeeks, 1)
  const repeated = cellAt(cal, 6, 2, 30)
  assert.equal(repeated.observedMinutes, 30, 'the 15-minute cell was lived through twice')
  const observed = cal.cells.reduce((sum, c) => sum + c.observedMinutes, 0)
  assert.equal(observed, 169 * 60)
})

test('a window crossing midnight splits into the cells it actually occupies', () => {
  const cal = buildCalendar(
    [win('2026-01-03T23:00:00Z', '2026-01-04T01:00:00Z')],
    ms('2025-12-29T00:00:00Z'), ms('2026-01-05T00:00:00Z'), 'UTC',
  )
  assert.equal(cellAt(cal, 5, 23).ratio, 1, 'Saturday 23:00 is open')
  assert.equal(cellAt(cal, 6, 0).ratio, 1, 'and it continues into Sunday 00:00')
  assert.equal(cellAt(cal, 6, 1).ratio, 0, 'but not past 01:00')
})

test('the grid is keyed to the DISPLAY timezone, not to UTC', () => {
  // 02:00–06:00 UTC on Saturday is 11:00–15:00 on Saturday in Tokyo. The week is a TOKYO
  // week: Mon 00:00 JST is 15:00Z the day before.
  const cal = buildCalendar(
    [win('2026-01-03T02:00:00Z', '2026-01-03T06:00:00Z')],
    ms('2025-12-28T15:00:00Z'), ms('2026-01-04T15:00:00Z'), 'Asia/Tokyo',
  )
  assert.equal(cellAt(cal, 5, 11).ratio, 1)
  assert.equal(cellAt(cal, 5, 2).ratio, 0)
})

test('an interval ending exactly at simulatedThrough is counted up to that instant', () => {
  const cal = buildCalendar(
    [win('2026-01-03T02:00:00Z', '2026-01-05T00:00:00Z', { endClipped: true })],
    ms('2025-12-29T00:00:00Z'), ms('2026-01-05T00:00:00Z'), 'UTC',
  )
  assert.equal(cal.wholeWeeks, 1)
  assert.equal(cellAt(cal, 5, 2).ratio, 1)
  assert.equal(cellAt(cal, 6, 23, 45).ratio, 1, 'the last observed cell of the week is open')
})
