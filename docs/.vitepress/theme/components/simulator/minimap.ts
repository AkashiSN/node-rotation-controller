// docs/.vitepress/theme/components/simulator/minimap.ts
//
// The minimap's geometry and its gestures. PURE — no Vue, no DOM.
//
// The minimap is NOT the chart. The chart obeys one rule above all others: never state
// something the simulation did not establish. The minimap is a CONTROL — a widget for
// moving the view — and a control may round its own geometry up to stay usable. That
// licence is taken in exactly one place (MIN_BRUSH_UNITS) and stated there.
import { MIN_VIEW_MS, clampView, spanOf, type Span, type View } from './zoom.ts'

export interface Box {
  /** Left edge, in viewBox units. */
  x: number
  /** Width, in viewBox units. */
  w: number
}

/** What a pointer-down landed on. */
export type Grab = 'left' | 'right' | 'move' | 'outside'

/** The brush's floor, in viewBox units (~11 screen px at a 1440px-wide strip).
 *
 *  A 2.5-hour view of a six-week horizon is 0.25% of the strip: two units, a hairline
 *  among the rotation ticks. Below this floor the brush stops being findable at all, so it
 *  is WIDENED — centred on the true range, which it therefore overstates by a few pixels.
 *  That is the widget licence: the reader is being shown where they are, not being told a
 *  fact about the run. Nothing downstream reads the drawn box as a duration. */
export const MIN_BRUSH_UNITS = 8

/** Handles are drawn only once the brush is this wide.
 *
 *  Two 6-unit hit zones on a 12-unit brush would leave nothing to grab for a MOVE — the
 *  resize affordance would eat the primary gesture. Same rule as the chart's semantic zoom,
 *  and the same reason: an element too small to mean anything is not drawn. */
export const HANDLE_MIN_BRUSH_UNITS = 20

/** How close to an edge counts as grabbing its handle, in viewBox units. */
export const HANDLE_HIT_UNITS = 6

/** One screenful of pan per this many pixels of wheel, at every zoom level. Proportional
 *  to the VIEW rather than the horizon: a gesture means the same thing to the reader
 *  whether they are looking at six weeks or ten minutes. */
export const WHEEL_SCREENFUL_PX = 400

/** Where the brush is DRAWN: the true range widened to the floor and held inside [0, width]. */
export function brushBox(view: Span, horizon: Span, width: number): Box {
  const hSpan = spanOf(horizon)
  const unit = (ms: number) => ((ms - horizon.startMs) / hSpan) * width
  const trueX = unit(view.startMs)
  const trueW = unit(view.endMs) - unit(view.startMs)

  const w = Math.min(width, Math.max(MIN_BRUSH_UNITS, trueW))
  // Widen about the CENTRE, so the brush still points at the instants the reader is on.
  const centred = trueX + trueW / 2 - w / 2
  const x = Math.min(Math.max(0, centred), width - w)
  return { x, w }
}

/** What a pointer-down at `xUnits` grabbed, given the DRAWN box — what can be seen is what
 *  can be grabbed. A brush too narrow for handles is all `move`: there is no edge to take
 *  hold of that would not also be its middle. */
export function grabAt(xUnits: number, box: Box): Grab {
  const hasHandles = box.w >= HANDLE_MIN_BRUSH_UNITS
  if (hasHandles) {
    if (Math.abs(xUnits - box.x) <= HANDLE_HIT_UNITS) return 'left'
    if (Math.abs(xUnits - (box.x + box.w)) <= HANDLE_HIT_UNITS) return 'right'
  }
  if (xUnits >= box.x && xUnits <= box.x + box.w) return 'move'
  return 'outside'
}

/** The instant at a viewBox x. */
export function msAt(xUnits: number, horizon: Span, width: number): number {
  return horizon.startMs + (xUnits / Math.max(1, width)) * spanOf(horizon)
}

/** Move the view so that `atMs` sits `offsetMs` from its start — the instant under the
 *  pointer stays under the pointer for the whole drag. Grabbing the brush must not yank it
 *  out from under the hand that grabbed it. */
export function moveTo(view: View, atMs: number, offsetMs: number, horizon: Span): View {
  const span = spanOf(view)
  const start = atMs - offsetMs
  return clampView({ startMs: start, endMs: start + span }, horizon)
}

/** Centre the view on an instant, keeping its width — a click on the track, away from the
 *  brush. The width is the reader's zoom level and this gesture does not touch it. */
export function centreOn(view: View, atMs: number, horizon: Span): View {
  const half = spanOf(view) / 2
  return clampView({ startMs: atMs - half, endMs: atMs + half }, horizon)
}

/** Drag one edge. The OTHER edge does not move: this is a resize, not a pan. The dragged
 *  edge cannot cross its opposite, shrink the view below the simulator's decision cadence,
 *  or leave the horizon. */
export function resizeEdge(view: View, edge: 'left' | 'right', atMs: number, horizon: Span): View {
  if (edge === 'left') {
    const max = view.endMs - MIN_VIEW_MS
    const start = Math.min(Math.max(atMs, horizon.startMs), max)
    return { startMs: start, endMs: view.endMs }
  }
  const min = view.startMs + MIN_VIEW_MS
  const end = Math.max(Math.min(atMs, horizon.endMs), min)
  return { startMs: view.startMs, endMs: end }
}

/** Wheel: PAN, width preserved. The reader asked to move along the run, not to change how
 *  much of it they can see — that is what the chart's own wheel is for. */
export function wheelPan(view: View, deltaPx: number, horizon: Span): View {
  const deltaMs = (deltaPx / WHEEL_SCREENFUL_PX) * spanOf(view)
  return clampView(
    { startMs: view.startMs + deltaMs, endMs: view.endMs + deltaMs },
    horizon,
  )
}
