// docs/.vitepress/theme/components/simulator/zoom.ts
//
// The VIEW: which sub-range of the simulated horizon is on screen, the tick ladder that
// labels it, and the thresholds below which an element is too small to mean anything.
// PURE — no Vue, no DOM.
//
// The view and the horizon are DIFFERENT CONCEPTS and the page presents them as such.
// The horizon is the range Go was asked to simulate; the view is the sub-range being
// looked at. Zoom changes the view only. That separation is the structural fix for the
// original chart's two worst symptoms: one linear axis carrying both the aging scale
// (weeks) and the rotation-mechanics scale (minutes), and a right edge that read as
// truncation because there was nothing to say the data continued.
import {
  DAY_MS, HOUR_MS, MINUTE_MS, addLocalDays, formatDate, formatTimeOfDay,
  instantOfLocal, startOfLocalDay, startOfLocalWeek, zonedParts,
} from './timeutil.ts'

export interface View {
  startMs: number
  endMs: number
}

export interface Span {
  startMs: number
  endMs: number
}

export interface Tick {
  ms: number
  label: string
}

export interface TickRows {
  /** The coarse row: dates (or week starts when the fine row is already dates). */
  coarse: Tick[]
  /** The fine row: the ladder step. */
  fine: Tick[]
  stepMs: number
}

/** The ladder. Each step is a unit a reader already thinks in — there is no 3.7-hour
 *  tick. `zoom.ts` picks the FINEST step whose on-screen spacing still clears
 *  MIN_TICK_GAP_PX, so the axis densifies as you zoom in and never overlaps its labels. */
export const STEP_LADDER = [
  7 * DAY_MS, DAY_MS, 6 * HOUR_MS, HOUR_MS, 15 * MINUTE_MS, 5 * MINUTE_MS, MINUTE_MS,
] as const

export const MIN_TICK_GAP_PX = 64

/** The view can never be narrower than the simulator's own decision cadence: below one
 *  minute there is nothing further to resolve, and an unbounded zoom would let a wheel
 *  gesture divide by zero. */
export const MIN_VIEW_MS = MINUTE_MS

/** Semantic zoom. An element is drawn only once it is wide enough to MEAN something; a
 *  sub-pixel drain segment is not a short drain, it is an illegible one. */
export const SEMANTIC = {
  /** provisioning / running / drain segments. */
  segmentPx: 4,
  /** The TGP-cap bracket and the surge-overlap annotation: they carry text. */
  capBracketPx: 24,
  /** Below this a maintenance-window band degrades to a thin tick — never a hairline
   *  rectangle that a reader would mistake for a border. */
  windowBandPx: 3,
  /** Below this a window band is drawn as ONE stripe, not as a fill between two edges.
   *
   *  The edges exist to make a wide band legible without a loud fill (#260). On a band a few
   *  pixels wide they stop being edges: two full-strength strokes 4px apart are a bright bar,
   *  and a whole horizon's worth of them is exactly the glare the soft fill was avoiding. So
   *  a narrow band carries its contrast in a stronger fill instead, as a single mark. Its
   *  clipped-by-horizon dashes go with the edges — at this width a 3-3 dash is not legible as
   *  one, and the tick below `windowBandPx` already drops that distinction for the same
   *  reason. */
  windowEdgePx: 12,
} as const

export const spanOf = (v: Span): number => Math.max(1, v.endMs - v.startMs)

/** How many pixels a duration occupies in the current view. */
export function pxOf(durationMs: number, view: Span, widthPx: number): number {
  return (durationMs / spanOf(view)) * widthPx
}

/** The finest ladder step whose ticks stay at least `minGapPx` apart. */
export function chooseStep(spanMs: number, widthPx: number, minGapPx = MIN_TICK_GAP_PX): number {
  const span = Math.max(1, spanMs)
  const width = Math.max(1, widthPx)
  // Finest first: the first step whose spacing clears the gap is the finest one that does.
  for (let i = STEP_LADDER.length - 1; i >= 0; i--) {
    const step = STEP_LADDER[i]
    if ((step / span) * width >= minGapPx) return step
  }
  // Even a week is too dense: the view spans years. Fall back to the coarsest step; the
  // thinning in ticksOf keeps the labels from colliding.
  return STEP_LADDER[0]
}

/** Every local day boundary in [fromMs, toMs). Calendar days, not 24h chunks: across a
 *  DST transition a day is 23h or 25h, and a fixed-chunk grid would drift off the
 *  weekday column it labels. */
function localDays(fromMs: number, toMs: number, tz: string, stepDays = 1): number[] {
  const out: number[] = []
  let d = startOfLocalDay(fromMs, tz)
  // A guard, not a scroll bar: the view is bounded by the horizon, but a malformed
  // horizon (or a pathological zone) must not spin the render loop forever.
  for (let i = 0; d < toMs && i < 4000; i++) {
    if (d >= fromMs) out.push(d)
    d = addLocalDays(d, stepDays, tz)
  }
  return out
}

/** Thin a tick row down to `max` entries by keeping every k-th. Labels that overlap are
 *  worse than fewer labels. */
function thin(ticks: Tick[], max: number): Tick[] {
  if (ticks.length <= max) return ticks
  const k = Math.ceil(ticks.length / max)
  return ticks.filter((_, i) => i % k === 0)
}

/** The two permanent tick rows, in the DISPLAY timezone — never the browser's. */
export function ticksOf(view: Span, tz: string, widthPx: number): TickRows {
  const span = spanOf(view)
  const stepMs = chooseStep(span, widthPx)
  const maxTicks = Math.max(2, Math.floor(widthPx / MIN_TICK_GAP_PX))

  let fine: Tick[]
  let coarse: Tick[]

  if (stepMs >= 7 * DAY_MS) {
    // Weeks: both rows are calendar boundaries. The coarse row keeps the month readable.
    const weeks: number[] = []
    let w = startOfLocalWeek(view.startMs, tz)
    for (let i = 0; w < view.endMs && i < 600; i++) {
      if (w >= view.startMs) weeks.push(w)
      w = addLocalDays(w, 7, tz)
    }
    fine = weeks.map(ms => ({ ms, label: formatDate(ms, tz) }))
    coarse = fine
  } else if (stepMs >= DAY_MS) {
    fine = localDays(view.startMs, view.endMs, tz).map(ms => ({ ms, label: formatDate(ms, tz) }))
    coarse = localDays(view.startMs, view.endMs, tz)
      .filter(ms => zonedParts(ms, tz).weekday === 0)
      .map(ms => ({ ms, label: formatDate(ms, tz) }))
  } else {
    const stepMin = Math.max(1, Math.round(stepMs / MINUTE_MS))
    const seen = new Set<number>()
    fine = []
    // Re-anchor on each local midnight rather than stepping a fixed offset across the
    // whole view: after a DST transition a fixed grid would sit at :30 past every hour.
    for (const day of localDays(view.startMs - DAY_MS, view.endMs, tz)) {
      const p = zonedParts(day, tz)
      for (let minute = 0; minute < 24 * 60; minute += stepMin) {
        const at = instantOfLocal(p.y, p.mo, p.d, Math.floor(minute / 60), minute % 60, tz)
        // A local time a spring-forward SKIPPED resolves to the instant the clock jumped
        // to — the same instant as the next tick. Draw it once.
        if (at < view.startMs || at > view.endMs || seen.has(at)) continue
        seen.add(at)
        fine.push({ ms: at, label: formatTimeOfDay(at, tz) })
      }
    }
    fine.sort((a, b) => a.ms - b.ms)
    coarse = localDays(view.startMs, view.endMs, tz).map(ms => ({ ms, label: formatDate(ms, tz) }))
  }

  return { fine: thin(fine, maxTicks), coarse: thin(coarse, Math.max(2, maxTicks)), stepMs }
}

/** Hold a view inside the horizon, preserving its duration where it fits and collapsing
 *  to the horizon when it does not. */
export function clampView(view: View, horizon: Span): View {
  const hSpan = spanOf(horizon)
  const width = Math.min(Math.max(MIN_VIEW_MS, view.endMs - view.startMs), hSpan)
  let start = view.startMs
  if (start < horizon.startMs) start = horizon.startMs
  if (start + width > horizon.endMs) start = horizon.endMs - width
  return { startMs: start, endMs: start + width }
}

/** Every keystroke reruns the simulation, and a rerun can MOVE the horizon. The rule is
 *  explicit so a view is never silently lost:
 *
 *  - a view that was the whole horizon stays the whole horizon (it was not a choice, it
 *    was the default);
 *  - any other view keeps its instants, clamped into the new bounds. */
export function reconcileView(view: View, before: Span, after: Span): View {
  const wasWhole = view.startMs <= before.startMs && view.endMs >= before.endMs
  if (wasWhole) return { startMs: after.startMs, endMs: after.endMs }
  return clampView(view, after)
}

/** Zoom about an anchor instant (the pointer, or the view's centre). factor < 1 zooms IN. */
export function zoomBy(view: View, factor: number, anchorMs: number, horizon: Span): View {
  const span = spanOf(view)
  const next = Math.max(MIN_VIEW_MS, Math.min(span * factor, spanOf(horizon)))
  // Keep the anchor under the same fraction of the width, so the instant under the
  // pointer does not slide out from under it.
  const frac = Math.min(1, Math.max(0, (anchorMs - view.startMs) / span))
  const start = anchorMs - frac * next
  return clampView({ startMs: start, endMs: start + next }, horizon)
}

export function panBy(view: View, deltaMs: number, horizon: Span): View {
  return clampView({ startMs: view.startMs + deltaMs, endMs: view.endMs + deltaMs }, horizon)
}

/** Fit an interval, with breathing room on each side. A zero-width target (a rotation
 *  whose start and end coincide) still yields a usable view rather than collapsing. */
export function fitTo(startMs: number, endMs: number, horizon: Span, padFrac = 0.35): View {
  const width = Math.max(MIN_VIEW_MS, endMs - startMs)
  const pad = width * padFrac
  return clampView({ startMs: startMs - pad, endMs: endMs + pad }, horizon)
}

/** Centre the view on an instant, keeping its current width — used by the rotation
 *  buttons, so stepping between rotations does not also change the zoom level. */
export function centreOn(view: View, atMs: number, horizon: Span): View {
  const half = spanOf(view) / 2
  return clampView({ startMs: atMs - half, endMs: atMs + half }, horizon)
}

/** The first target strictly after / before an instant; null at the bounds, which is what
 *  disables the button rather than silently doing nothing. */
export function nextTarget(targets: number[], afterMs: number): number | null {
  for (const t of targets) if (t > afterMs) return t
  return null
}

export function prevTarget(targets: number[], beforeMs: number): number | null {
  for (let i = targets.length - 1; i >= 0; i--) if (targets[i] < beforeMs) return targets[i]
  return null
}
