// docs/.vitepress/theme/components/simulator/timeline.ts
//
// The timeline's GEOMETRY, in the data's own units (milliseconds and row indices).
// PURE by design — no Vue, no SVG, no coordinate scaling: TimelineChart.vue keeps the
// x() scale and the rendering, this module keeps every DERIVATION worth getting right
// (what a row is, when a node was born, where its deadline falls, how the interval
// events pair up), so `node --test` can pin them without a DOM.
import { parseGoDuration, type Fleet, type Horizon, type SimEvent } from './model.ts'

export interface Bar {
  name: string
  /** Birth instant, clamped to the horizon; null when the source instant is malformed. */
  bornMs: number | null
  /** rotation-done, or the horizon end for a node that never rotated. */
  endMs: number | null
  /** createdAt + effective expireAfter; null when malformed or outside the horizon. */
  deadlineMs: number | null
  /** The node is still alive at the horizon end — either it never rotated, or its
   *  rotation-done falls at/after the horizon end and endMs was clamped in. The chart
   *  marks these bars as CONTINUING (a right chevron) so a lifetime that outlives the
   *  visible window does not read as "cut off" at the right edge. */
  ongoing: boolean
}

export interface Band {
  startMs: number
  endMs: number
}

export interface BlockedBand extends Band {
  label: string
}

export interface Mark {
  kind: SimEvent['kind']
  node: string
  row: number
  surgeless: boolean
  atMs: number
  title: string
}

export interface Timeline {
  rows: string[]
  bars: Bar[]
  windows: Band[]
  blocked: BlockedBand[]
  marks: Mark[]
}

const MARK_KINDS: SimEvent['kind'][] = [
  'rotation-start', 'node-ready', 'rotation-done', 'expire-after-breach',
]

const at = (e: SimEvent) => new Date(e.at).getTime()

/** Clamp an instant to the visible horizon: an interval can start before the horizon
 *  or run past its end, and an unclamped band would render outside the plot area. */
export function clampToHorizon(ms: number, t0: number, t1: number): number {
  return Math.min(Math.max(ms, t0), t1)
}

/** One row per node, including the replacements sim materialises: a replacement is a
 *  new node name, and it becomes the next generation's candidate. */
export function rowsOf(events: SimEvent[], fleet: Fleet): string[] {
  const names = fleet.nodes.map(n => n.name)
  for (const e of events) {
    if (e.replacement && !names.includes(e.replacement)) names.push(e.replacement)
  }
  return names
}

/** The instant a REPLACEMENT node was created, derived the way internal/sim does it.
 *
 *  `Replacement` is carried on the rotation-done event and on no other kind — but the
 *  replacement is NOT born then. On the surge path Karpenter creates the replacement
 *  NodeClaim as soon as the low-priority placeholder Pod goes pending, i.e. at
 *  ROTATION-START, and the provisioning time is what elapses from there to Ready
 *  (internal/sim/loop.go: `createdAt := rot.start`). Only the surge-less
 *  (forceful-fallback) path, where no placeholder exists, has the replacement created
 *  at rotation-done.
 *
 *  Anchoring on rotation-done for a surged replacement would (a) erase the
 *  make-before-break overlap the product exists to demonstrate — old and new bars drawn
 *  end-to-end — and (b) place the deadline tick at doneAt + expireAfter, LATER than the
 *  real deadline (createdAt + expireAfter), so a breach mark could land to the LEFT of
 *  the deadline line the page itself drew. Under-reporting a breach is the one error
 *  direction the simulator must never make.
 *
 *  Total by construction: a rotation-done with no matching rotation-start (truncated or
 *  malformed event stream) falls back to the rotation-done instant rather than throwing.
 */
function bornOfReplacement(done: SimEvent, events: SimEvent[]): number {
  if (done.surgeless === true) return at(done)
  const doneMs = at(done)
  // A node rotates once per generation, so there can be several rotation-starts for the
  // same node name across the horizon: take the LATEST one at or before this done.
  let best: number | null = null
  for (const e of events) {
    if (e.kind !== 'rotation-start' || e.node !== done.node) continue
    const ms = at(e)
    if (!Number.isFinite(ms) || ms > doneMs) continue
    if (best === null || ms > best) best = ms
  }
  return best ?? doneMs
}

/** Per-row lifetime bar: from the node's createdAt to its rotation-done (or the horizon
 *  end, for a node that never rotates). */
export function barsOf(events: SimEvent[], horizon: Horizon, fleet: Fleet): Bar[] {
  const t0 = new Date(horizon.start).getTime()
  const t1 = new Date(horizon.end).getTime()
  return rowsOf(events, fleet).map(name => {
    const declared = fleet.nodes.find(n => n.name === name)
    // `rows` only ever adds a name it saw on a rotation-done's `replacement`, so this
    // lookup succeeds for every undeclared row; it is still resolved as a total
    // function (fallback to the horizon start) rather than asserted, because a thrown
    // TypeError here would blank the whole page over a single bad row.
    const bornEvent = declared ? undefined : events.find(e => e.replacement === name)
    const bornMs = declared
      ? new Date(declared.createdAt).getTime()
      : bornEvent ? bornOfReplacement(bornEvent, events) : t0
    const done = events.find(e => e.kind === 'rotation-done' && e.node === name)
    const doneMs = done ? at(done) : NaN
    const endMs = Number.isFinite(doneMs) ? doneMs : t1
    // Alive at the horizon end: no rotation-done at all, or a done that lands beyond the
    // window (so endMs was clamped back in). Such a bar continues past the right edge and
    // must not look truncated there.
    const ongoing = !Number.isFinite(doneMs) || doneMs >= t1
    // A replacement is provisioned from the NodePool TEMPLATE, so it inherits the
    // template's expireAfter — only a declared node can carry a per-node override
    // (internal/sim/loop.go pins the same rule).
    const eff = declared?.expireAfter ?? fleet.expireAfter
    const deadlineMs = bornMs + (parseGoDuration(eff) ?? 0)
    // A malformed createdAt (or event `at`) parses to NaN, and clamping keeps it NaN:
    // an SVG x1="NaN" paints nothing with no explanation. Report it as absent (null) so
    // the caller can skip the mark instead of emitting a garbage attribute.
    return {
      name,
      bornMs: Number.isFinite(bornMs) ? clampToHorizon(bornMs, t0, t1) : null,
      endMs: Number.isFinite(endMs) ? clampToHorizon(endMs, t0, t1) : null,
      deadlineMs: Number.isFinite(deadlineMs) && deadlineMs >= t0 && deadlineMs <= t1
        ? deadlineMs : null,
      ongoing,
    }
  })
}

/** Maintenance-window bands, paired from the window-open / window-close events. */
export function windowsOf(events: SimEvent[], horizon: Horizon): Band[] {
  const t0 = new Date(horizon.start).getTime()
  const t1 = new Date(horizon.end).getTime()
  const out: Band[] = []
  // The wire contract does not promise chronological order. Pairing by array position
  // (not time) would drop a close that precedes its open in the array, leaving the open
  // dangling to hit the unclosed-window fallback below and draw a band across the WHOLE
  // horizon. Sort a local copy — never mutate the caller's array.
  const sorted = [...events].sort((a, b) => at(a) - at(b))
  let open: number | null = null
  for (const e of sorted) {
    if (e.kind === 'window-open') open = at(e)
    if (e.kind === 'window-close' && open !== null) {
      out.push({ startMs: clampToHorizon(open, t0, t1), endMs: clampToHorizon(at(e), t0, t1) })
      open = null
    }
  }
  // An open with no close was still open at the end of the simulated horizon.
  if (open !== null) out.push({ startMs: clampToHorizon(open, t0, t1), endMs: t1 })
  return out.filter(b => Number.isFinite(b.startMs) && Number.isFinite(b.endMs))
}

/** blocked-by-gate / no-eligible-claim are EDGE-TRIGGERED and carry an interval
 *  (at..until) — one event for a week-long stretch. They are bands, never point marks:
 *  drawing them as points would throw away the coalescing that keeps the payload small
 *  and the picture readable. */
export function blockedOf(events: SimEvent[], horizon: Horizon): BlockedBand[] {
  const t0 = new Date(horizon.start).getTime()
  const t1 = new Date(horizon.end).getTime()
  // No `until` means the interval was still open at the end of the simulated horizon —
  // mirror windowsOf()'s unclosed-window fallback rather than falling back to `e.at`,
  // which produces a zero-width band that the width filter below silently drops.
  const until = (e: SimEvent) => (e.until ? new Date(e.until).getTime() : t1)
  return events
    .filter(e => e.kind === 'blocked-by-gate' || e.kind === 'no-eligible-claim')
    .map(e => ({
      startMs: clampToHorizon(at(e), t0, t1),
      endMs: clampToHorizon(until(e), t0, t1),
      label: e.gate ?? 'no-eligible-claim',
    }))
    .filter(b => Number.isFinite(b.startMs) && Number.isFinite(b.endMs) && b.endMs > b.startMs)
}

/** The point marks: rotation-start (surge-less distinguishable), node-ready,
 *  rotation-done and — in red — every expire-after-breach. */
export function marksOf(events: SimEvent[], horizon: Horizon, fleet: Fleet): Mark[] {
  const t0 = new Date(horizon.start).getTime()
  const t1 = new Date(horizon.end).getTime()
  const rows = rowsOf(events, fleet)
  const out: Mark[] = []
  for (const e of events) {
    if (!MARK_KINDS.includes(e.kind)) continue
    const node = e.node ?? ''
    const row = rows.indexOf(node)
    if (row < 0) continue
    const atMs = at(e)
    // A malformed `at` parses to NaN; skip the mark rather than emit cx="NaN" (see the
    // same treatment in barsOf above).
    if (!Number.isFinite(atMs)) continue
    out.push({
      kind: e.kind,
      node,
      row,
      surgeless: e.surgeless === true,
      atMs: clampToHorizon(atMs, t0, t1),
      title: `${e.kind}${e.surgeless ? ' (surge-less)' : ''} — ${node} @ ${e.at}`,
    })
  }
  return out
}

export function buildTimeline(events: SimEvent[], horizon: Horizon, fleet: Fleet): Timeline {
  return {
    rows: rowsOf(events, fleet),
    bars: barsOf(events, horizon, fleet),
    windows: windowsOf(events, horizon),
    blocked: blockedOf(events, horizon),
    marks: marksOf(events, horizon, fleet),
  }
}
