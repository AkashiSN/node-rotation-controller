// docs/.vitepress/theme/components/simulator/calendar.ts
//
// The maintenance-window calendar: a weekday x time-of-day grid folded from the OBSERVED
// window occurrences. PURE — no Vue, no DOM.
//
// It is not the policy's recurrence, and it must never be presented as one. internal/window
// evaluates EACH entry in its own timezone and the effective schedule is their UNION, so
// the entry that produced an occurrence, its timezone, and any boundary interior to the
// union are all gone by the time the page sees it. A finite sample of what actually
// happened is the truthful thing to show.
//
// A cell reports an OPEN-TIME RATIO:
//
//     open minutes observed in this cell / minutes observed in this cell
//
// not a k-of-N week count. A week count has to binarise "was the window open in this
// 15-minute cell", and every threshold lies: any-overlap turns a one-minute clip into a
// full cell, full-coverage erases it. A ratio of real elapsed durations needs no threshold
// — and it handles DST by construction, because the cell that occurred twice has twice the
// observed minutes and the one that never existed has zero.
import type { WindowView } from './timeline.ts'
import {
  MINUTE_MS, addLocalDays, localMinuteOfDay, startOfLocalWeek, zonedParts,
} from './timeutil.ts'

/** 15 minutes: fine enough for a window edge at :15, coarse enough for a 7 x 96 grid. */
export const CELL_MINUTES = 15
export const CELLS_PER_DAY = (24 * 60) / CELL_MINUTES

export interface Cell {
  /** 0 = Monday … 6 = Sunday, in the display timezone. */
  weekday: number
  /** 0 … 95: the cell's index within the local day. */
  slot: number
  openMinutes: number
  observedMinutes: number
  /** open / observed, or null when the cell was never observed (a DST-skipped hour, or a
   *  run with no whole week in it). A null cell is drawn as UNKNOWN, never as zero. */
  ratio: number | null
}

export interface Calendar {
  cells: Cell[]
  /** Whole local calendar weeks in the observed span. Zero is a legitimate answer — the
   *  grid says so instead of dividing by it. */
  wholeWeeks: number
  /** The span the denominators were taken over: whole weeks only. The partial weeks at the
   *  horizon's edges are excluded, and the footer says so. */
  fromMs: number
  toMs: number
  timezone: string
}

const cellIndex = (weekday: number, slot: number) => weekday * CELLS_PER_DAY + slot

function emptyCells(): Cell[] {
  const cells: Cell[] = []
  for (let weekday = 0; weekday < 7; weekday++) {
    for (let slot = 0; slot < CELLS_PER_DAY; slot++) {
      cells.push({ weekday, slot, openMinutes: 0, observedMinutes: 0, ratio: null })
    }
  }
  return cells
}

/** Walk [fromMs, toMs) in real time, attributing each stretch to the (weekday, slot) cell
 *  its WALL CLOCK fell in, and hand the caller the real minutes it lasted.
 *
 *  Real elapsed time on one side and wall-clock cells on the other is what makes DST free:
 *  the local hour a fall-back repeats is entered twice and accumulates twice the minutes;
 *  the one a spring-forward skips is never entered and accumulates none. The next boundary
 *  is recomputed from the actual wall clock at every step, so a transition landing inside a
 *  cell cannot desynchronise the walk. */
function walk(
  fromMs: number, toMs: number, tz: string,
  add: (weekday: number, slot: number, minutes: number) => void,
): void {
  let t = fromMs
  // Bounded: a 15-minute grid over a decade is ~350k steps. Every iteration advances by at
  // least one millisecond, so a pathological zone cannot stall the walk either.
  for (let i = 0; t < toMs && i < 500_000; i++) {
    const p = zonedParts(t, tz)
    const minuteOfDay = localMinuteOfDay(t, tz)
    const slot = Math.floor(minuteOfDay / CELL_MINUTES)
    // Seconds matter: a window that ends at 05:59:30 (a horizon a visitor typed) must not
    // be rounded up into the next cell.
    const intoCellMs = (minuteOfDay % CELL_MINUTES) * MINUTE_MS
      + p.s * 1000 + (((t % 1000) + 1000) % 1000)
    const untilCellEnd = CELL_MINUTES * MINUTE_MS - intoCellMs
    const next = Math.min(t + untilCellEnd, toMs)
    if (next <= t) break
    add(p.weekday, slot, (next - t) / MINUTE_MS)
    t = next
  }
}

/** Fold the observed window intervals into the grid.
 *
 *  `spanStartMs` / `spanEndMs` are the run's own bounds — its start and simulatedThrough
 *  — NOT the requested horizon end: a partial run must not have its denominators taken
 *  over time it never simulated. */
export function buildCalendar(
  windows: WindowView[], spanStartMs: number, spanEndMs: number, tz: string,
): Calendar {
  const cells = emptyCells()
  const empty: Calendar = {
    cells, wholeWeeks: 0, fromMs: spanStartMs, toMs: spanStartMs, timezone: tz,
  }
  if (!Number.isFinite(spanStartMs) || !Number.isFinite(spanEndMs) || spanEndMs <= spanStartMs) {
    return empty
  }

  // Whole LOCAL CALENDAR weeks (Monday 00:00 → Monday 00:00), not 7 x 24h chunks anchored
  // on the horizon's start: a DST week is 167h or 169h, and a fixed chunk would not line up
  // with the weekday grid it is the denominator for. The partial weeks at each edge are
  // excluded from BOTH numerator and denominator — a half-observed Saturday would otherwise
  // read as a window that is only half open.
  const firstWeek = startOfLocalWeek(spanStartMs, tz)
  const from = firstWeek >= spanStartMs ? firstWeek : addLocalDays(firstWeek, 7, tz)
  let to = startOfLocalWeek(spanEndMs, tz)
  if (to > spanEndMs) to = addLocalDays(to, -7, tz) // defensive; startOfLocalWeek never overshoots
  if (to <= from) return empty

  let wholeWeeks = 0
  for (let w = from; w < to; w = addLocalDays(w, 7, tz)) wholeWeeks++

  walk(from, to, tz, (weekday, slot, minutes) => {
    cells[cellIndex(weekday, slot)].observedMinutes += minutes
  })

  for (const win of windows) {
    const start = Math.max(win.startMs, from)
    const end = Math.min(win.endMs, to)
    // A CLIPPED interval is NOT excluded — clipping is a property of a BOUNDARY, not of the
    // interval's interior. Excluding it wholesale would be catastrophic in the obvious
    // case: an always-open schedule is one interval, clipped at both ends, and every cell
    // would read 0% despite the window never having closed.
    if (end <= start) continue
    walk(start, end, tz, (weekday, slot, minutes) => {
      cells[cellIndex(weekday, slot)].openMinutes += minutes
    })
  }

  for (const c of cells) {
    // Never observed (a DST-skipped local hour): unknown, not zero. They are opposite
    // claims — "the window was shut" versus "this wall-clock time did not happen".
    c.ratio = c.observedMinutes > 0
      ? Math.min(1, c.openMinutes / c.observedMinutes)
      : null
  }

  return { cells, wholeWeeks, fromMs: from, toMs: to, timezone: tz }
}

/** The rows a grid renders: one per weekday, each with its 96 cells in order. */
export function weekdayRows(cal: Calendar): Cell[][] {
  return Array.from({ length: 7 }, (_, weekday) =>
    cal.cells.slice(weekday * CELLS_PER_DAY, (weekday + 1) * CELLS_PER_DAY))
}
