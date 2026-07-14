// docs/.vitepress/theme/components/simulator/timeline.ts
//
// The timeline's GEOMETRY, in the data's own units (milliseconds and slot indices).
// PURE by design — no Vue, no SVG, no coordinate scaling: TimelineChart.vue keeps the
// x() scale and the rendering, this module keeps every DERIVATION worth getting right,
// so `node --test` can pin them without a DOM.
//
// The governing rule, and the reason this module reads the wire's generations /
// rotations / windows instead of re-deriving them from the events:
//
//   THE CHART MUST NEVER STATE SOMETHING THE SIMULATION DID NOT ESTABLISH.
//
// A horizon artifact is not a policy boundary. A template-derived representative
// (Result.ageThreshold) is not a per-node fact. An absent instant is not a zero-length
// duration — it is drawn open-ended, and the page says WHY it is open (the simulation
// ended while the rotation was in flight, or the response is malformed: opposite
// meanings, so they never share a label).
import {
  parseGoDuration,
  type BirthMode, type DrainCapSource, type Fleet, type Horizon,
  type SimEvent, type SimGeneration, type SimResponse, type SimRotation,
} from './model.ts'

/** A generation's life is three segments. Generation n+1 PROVISIONS while generation n is
 *  still RUNNING — its predecessor's NodeClaim is deleted only at node-ready — and that
 *  coexistence IS make-before-break: the single most important thing this chart has to show.
 *  (Not "provisioning against the drain": the drain begins exactly where the provisioning
 *  ends, so those two are adjacent and never overlap. See segmentsOf.) */
export type SegmentKind = 'provisioning' | 'running' | 'drain'

/** Why a segment has no established end.
 *  - `in-flight`: the simulation simply stopped while it was still running. Legitimate.
 *  - `malformed`: the response contradicts itself. A bug.
 *  They mean opposite things to a reader, so they never share an accessible label. */
export type OpenReason = 'in-flight' | 'malformed'

export interface Segment {
  kind: SegmentKind
  startMs: number
  /** null when the end was never established. NEVER collapsed onto the start: a
   *  zero-length drain is a duration the simulation did not report. */
  endMs: number | null
  /** Where an open segment's hatching runs to (simulatedThrough). */
  openToMs: number
  openReason: OpenReason | null
}

export interface GenerationView {
  slot: number
  gen: number
  name: string
  birthMode: BirthMode
  /** The rotation that produced it has not completed. */
  provisional: boolean
  createdMs: number
  /** Surge births only, and only once Ready. */
  readyMs: number | null
  deadlineMs: number | null
  /** EXCLUSIVE: "eligible AFTER this instant". The trigger is a strict inequality. */
  eligibilityMs: number | null
  drainCapMs: number
  drainCapSource: DrainCapSource
  /** The drain's start — the outgoing rotation's `ready` (surge) or `start` (surge-less). */
  drainStartMs: number | null
  /** drainStart + drainCap: the instant Karpenter force-completes the drain. A CAP drawn
   *  as a dimension, whose endpoint is a DIFFERENT glyph from the drain's actual end —
   *  "the drain took 10m, the cap was 1h" has to be legible as geometry. */
  drainCapEndMs: number | null
  drainEndMs: number | null
  /** expire-after-breach, which the simulator emits AT the deadline by construction. The
   *  chart must keep the glyph legible against the deadline line rather than overdrawing
   *  it — a regression there would silently hide every breach in the run. */
  breachMs: number | null
  segments: Segment[]
}

export interface Row {
  slot: number
  /** The slot's name — its first generation's, which is the node the fleet declared. */
  label: string
  /** False for a slot the response reports but the fleet never declared. Such a row is
   *  APPENDED rather than dropped: a response the page cannot place is still a response
   *  the reader must see. */
  declared: boolean
  generations: GenerationView[]
}

export interface WindowView {
  startMs: number
  endMs: number
  /** The boundary is an artifact of the horizon, not a real transition of the schedule. */
  startClipped: boolean
  endClipped: boolean
}

export interface BlockedBand {
  startMs: number
  endMs: number
  label: string
}

export interface TimelineModel {
  rows: Row[]
  windows: WindowView[]
  blocked: BlockedBand[]
  /** The last instant the simulation processed. A bar alive here CONTINUES; it is not
   *  truncated, and the chart says so explicitly. */
  simulatedThroughMs: number
  /** Contradictions in the response. Surfaced, never painted over. */
  anomalies: string[]
}

const ms = (iso: string | undefined): number | null => {
  if (!iso) return null
  const t = new Date(iso).getTime()
  return Number.isFinite(t) ? t : null
}

const key = (slot: number, gen: number) => `${slot}/${gen}`

/** Index the generations by (slot, gen), keeping the FIRST of any duplicate pair and
 *  reporting the rest: two generations drawn stacked on one row would be unreadable, and
 *  silently dropping one would hide a producer bug. */
function indexGenerations(gens: SimGeneration[]): {
  index: Map<string, SimGeneration>
  anomalies: string[]
} {
  const index = new Map<string, SimGeneration>()
  const anomalies: string[] = []
  for (const g of gens) {
    const k = key(g.slot, g.gen)
    if (index.has(k)) {
      anomalies.push(`duplicate generation (slot ${g.slot}, gen ${g.gen}): kept the first, dropped ${g.name}`)
      continue
    }
    index.set(k, g)
  }
  return { index, anomalies }
}

/** The rotation that takes a generation OUT of its slot, if the run reached it. */
function outgoing(rots: SimRotation[], slot: number, gen: number): SimRotation | undefined {
  return rots.find(r => r.slot === slot && r.fromGen === gen)
}

/** The segments of one generation.
 *
 *  | segment      | from                                  | to                                     |
 *  | provisioning | createdAt (= its rotation's start)     | its own readyAt                        |
 *  | running      | readyAt (surge) / createdAt (others)   | the instant its OWN drain begins        |
 *  | drain        | rotation's ready (surge) / start (s-l) | rotation's done                        |
 *
 *  RUNNING ENDS WHERE THE DRAIN BEGINS — not at the rotation's start, which is what the
 *  design doc's table said. Two reasons, and they are the same reason:
 *
 *  1. The old node is NOT idle between rotation-start and node-ready. Its NodeClaim is
 *     deleted only when the surge node goes Ready (internal/sim/loop.go:113); until then it
 *     is still carrying load. Ending its bar at the rotation's start would leave that span
 *     unaccounted for — a hole in the one node the reader is watching.
 *  2. It is what makes MAKE-BEFORE-BREAK visible at all. The design says "the provisioning
 *     of generation n+1 overlaps the drain of generation n", but with its own boundaries
 *     those two are strictly ADJACENT: provisioning is [start, ready] and the drain is
 *     [ready, done]; they touch at `ready` and never overlap. The span in which two nodes
 *     genuinely coexist is [start, ready] — the replacement PROVISIONING while its
 *     predecessor is still RUNNING — and that is the overlap this chart has to show. */
function segmentsOf(
  g: SimGeneration, rot: SimRotation | undefined, throughMs: number,
): Segment[] {
  const out: Segment[] = []
  const created = ms(g.createdAt)
  if (created === null) return out

  const ready = ms(g.readyAt)
  const start = rot ? ms(rot.start) : null
  const rotReady = rot ? ms(rot.ready) : null
  const done = rot ? ms(rot.done) : null

  // provisioning — the surge path only. An initial node was never provisioned by us, and
  // a surge-less replacement stages no placeholder: it is born already drained-in.
  if (g.birthMode === 'surge') {
    out.push({
      kind: 'provisioning',
      startMs: created,
      endMs: ready,
      openToMs: throughMs,
      // Still provisioning when the simulation stopped: legitimate. A generation that is
      // NOT provisional (its rotation completed) and still has no readyAt is a
      // contradiction — the wire promises a completed surge carries one.
      openReason: ready === null ? (g.provisional ? 'in-flight' : 'malformed') : null,
    })
  }

  // The instant this generation stops carrying load: when its own drain begins.
  const drainFrom = rot ? (rot.mode === 'surge' ? rotReady : start) : null

  // running — from the instant the node is actually carrying load, to the instant its
  // NodeClaim is deleted.
  const runFrom = g.birthMode === 'surge' ? ready : created
  if (runFrom !== null) {
    out.push({
      kind: 'running',
      startMs: runFrom,
      // Ends where the drain begins. With no drain start: either no rotation reached it
      // (the generation was still running when the simulation stopped — known-alive, not
      // unknown, and drawn as a bar that CONTINUES rather than one that was cut off), or a
      // rotation is in flight but its surge is not Ready yet, in which case the node is
      // STILL RUNNING. Only a malformed record (a completed rotation with no `ready`) falls
      // back to the rotation's start, and its drain is hatched as malformed below.
      endMs: drainFrom ?? (rot && done !== null ? start : throughMs),
      openToMs: throughMs,
      openReason: null,
    })
  }

  // drain — the old node's, from the instant its NodeClaim is deleted.
  if (rot) {
    if (drainFrom !== null) {
      out.push({
        kind: 'drain',
        startMs: drainFrom,
        endMs: done,
        openToMs: throughMs,
        openReason: done === null ? 'in-flight' : null,
      })
    } else if (done !== null && start !== null) {
      // A surge that completed but carries no `ready`: the drain's START is unknowable, so
      // the whole span is hatched as malformed rather than guessed at.
      out.push({
        kind: 'drain',
        startMs: start,
        endMs: null,
        openToMs: done,
        openReason: 'malformed',
      })
    }
  }
  return out
}

function viewOf(
  g: SimGeneration, rot: SimRotation | undefined, throughMs: number, breachMs: number | null,
): GenerationView {
  const created = ms(g.createdAt) ?? NaN
  const start = rot ? ms(rot.start) : null
  const rotReady = rot ? ms(rot.ready) : null
  const drainStart = rot ? (rot.mode === 'surge' ? rotReady : start) : null
  const cap = parseGoDuration(g.drainCap) ?? 0
  return {
    slot: g.slot,
    gen: g.gen,
    name: g.name,
    birthMode: g.birthMode,
    provisional: g.provisional === true,
    createdMs: created,
    readyMs: ms(g.readyAt),
    deadlineMs: ms(g.deadline),
    eligibilityMs: ms(g.eligibilityBoundary),
    drainCapMs: cap,
    drainCapSource: g.drainCapSource,
    drainStartMs: drainStart,
    drainCapEndMs: drainStart === null ? null : drainStart + cap,
    drainEndMs: rot ? ms(rot.done) : null,
    breachMs,
    segments: segmentsOf(g, rot, throughMs),
  }
}

/** One row per SLOT. The row count is the fleet's node count and does not grow with the
 *  horizon — which is what fixes "why did node-1 rotate twice": it did not; its slot
 *  relayed through three generations, left to right along one row. */
export function buildTimeline(resp: SimResponse, horizon: Horizon, fleet: Fleet): TimelineModel {
  const t0 = new Date(horizon.start).getTime()
  const t1 = new Date(horizon.end).getTime()
  const throughMs = ms(resp.simulatedThrough) ?? (Number.isFinite(t1) ? t1 : t0)

  const gens = resp.generations ?? []
  const rots = resp.rotations ?? []
  const { index, anomalies } = indexGenerations(gens)

  // The breach the simulator reports lands AT the node's deadline by construction, so it
  // is keyed by NAME. A replacement's name can collide with a declared node's, so the
  // (slot, gen) that owns it is resolved through the generation records — which are unique
  // by construction — and a name that maps to more than one is reported rather than
  // silently attached to the wrong row.
  const breaches = new Map<string, number>()
  for (const e of resp.events ?? []) {
    if (e.kind !== 'expire-after-breach' || !e.node) continue
    const at = ms(e.at)
    if (at === null) continue
    breaches.set(`${e.node}@${at}`, at)
  }
  const breachOf = (g: SimGeneration): number | null => {
    const deadline = ms(g.deadline)
    if (deadline === null) return null
    return breaches.has(`${g.name}@${deadline}`) ? deadline : null
  }

  const bySlot = new Map<number, SimGeneration[]>()
  for (const g of index.values()) {
    const list = bySlot.get(g.slot) ?? []
    list.push(g)
    bySlot.set(g.slot, list)
  }

  const rows: Row[] = []
  const emit = (slot: number, declared: boolean, fallbackLabel: string) => {
    const list = (bySlot.get(slot) ?? []).slice().sort((a, b) =>
      a.gen - b.gen ||
      (ms(a.createdAt) ?? 0) - (ms(b.createdAt) ?? 0) ||
      a.name.localeCompare(b.name))
    rows.push({
      slot,
      label: list[0]?.name ?? fallbackLabel,
      declared,
      generations: list.map(g => viewOf(g, outgoing(rots, g.slot, g.gen), throughMs, breachOf(g))),
    })
  }

  // The declared fleet, in its own order, so the rows never reshuffle as the run changes.
  fleet.nodes.forEach((n, i) => emit(i, true, n.name))
  // Then any slot the response reports that the fleet did not declare.
  for (const slot of [...bySlot.keys()].sort((a, b) => a - b)) {
    if (slot < fleet.nodes.length) continue
    anomalies.push(`slot ${slot} is not a declared fleet node; its row is appended`)
    emit(slot, false, `slot ${slot}`)
  }

  for (const r of rots) {
    if (r.toGen !== undefined && !index.has(key(r.slot, r.toGen))) {
      anomalies.push(`rotation (slot ${r.slot}, gen ${r.fromGen} → ${r.toGen}) names a generation that is not in the response`)
    }
  }

  return {
    rows,
    windows: windowsOf(resp),
    blocked: blockedOf(resp.events ?? [], throughMs),
    simulatedThroughMs: throughMs,
    anomalies,
  }
}

/** The OBSERVED window occurrences, straight from the wire. They are no longer paired
 *  from the events: pairing cannot express a clipped boundary, and the clipped flags are
 *  what keep a horizon artifact from being drawn as a real opening or closing. */
export function windowsOf(resp: SimResponse): WindowView[] {
  const out: WindowView[] = []
  for (const w of resp.windows ?? []) {
    const startMs = ms(w.start)
    const endMs = ms(w.end)
    if (startMs === null || endMs === null || endMs < startMs) continue
    out.push({
      startMs,
      endMs,
      startClipped: w.startClipped === true,
      endClipped: w.endClipped === true,
    })
  }
  return out.sort((a, b) => a.startMs - b.startMs)
}

/** blocked-by-gate / no-eligible-claim are EDGE-TRIGGERED and carry an interval
 *  (at..until) — one event for a week-long stretch. They are bands, never point marks:
 *  drawing them as points would throw away the coalescing that keeps the payload small
 *  and the picture readable. */
export function blockedOf(events: SimEvent[], throughMs: number): BlockedBand[] {
  const out: BlockedBand[] = []
  for (const e of events) {
    if (e.kind !== 'blocked-by-gate' && e.kind !== 'no-eligible-claim') continue
    const startMs = ms(e.at)
    if (startMs === null) continue
    // No `until` means the interval was still open when the simulation stopped — it runs
    // to simulatedThrough, never to a zero-width band at `at`.
    const endMs = ms(e.until) ?? throughMs
    if (endMs <= startMs) continue
    out.push({ startMs, endMs, label: e.gate ?? 'no-eligible-claim' })
  }
  return out.sort((a, b) => a.startMs - b.startMs)
}

/** Every rotation start in the run, sorted. The minimap marks all of them — it is a density
 *  map of the whole run — but the buttons do NOT step through them one by one; see
 *  rotationOccasions below. */
export function rotationInstants(resp: SimResponse): number[] {
  return (resp.rotations ?? [])
    .map(r => ms(r.start))
    .filter((v): v is number => v !== null)
    .sort((a, b) => a - b)
}

/** A ROTATION OCCASION: one maintenance-window occurrence in which at least one rotation
 *  started, with every rotation it holds.
 *
 *  This — not the individual rotation start — is what *first / previous / next rotation*
 *  navigates. Inside one occurrence the controller rotates serially, one node every
 *  `t_rot_est + cooldownAfter` (~25m at the defaults), so the next rotation start is already
 *  on screen: stepping to it nudged the view by 25 minutes and read as a jitter, not a jump,
 *  and leaving a busy window cost one click per node that rotated in it (#261). An occasion
 *  is the unit a reader actually wants: fit it, and there is nothing left to step to inside
 *  it, because it is all in view. */
export interface RotationOccasion {
  /** The instant the buttons count from: the FIRST rotation start in this occasion. It is a
   *  real instant of the run, so `prev`/`next` order the occasions by when rotation actually
   *  began — not by a window boundary the rotation may sit far inside. */
  atMs: number
  /** The interval to fit. The window occurrence, or the rotation instant itself when there
   *  is no occurrence to fit (see `windowless`). */
  startMs: number
  endMs: number
  /** How many rotations start inside it. */
  count: number
  /** No window occurrence bounds this rotation: either the schedule admits rotation at any
   *  time (a continuously-open union has no occurrence narrower than the run), or the
   *  response reports a rotation outside every window it also reports. The view then falls
   *  back to the rotation instant — CENTRED, which the old `at − 30m … at + 60m` was not. */
  windowless: boolean
}

/** Group the run's rotation starts into occasions.
 *
 *  `horizonSpanMs` is what makes a continuously-open schedule fall back gracefully: an
 *  occurrence as wide as the run is nothing to zoom to, so its rotations are treated as
 *  windowless rather than "fitted" to a view that cannot move. */
export function rotationOccasions(resp: SimResponse, horizonSpanMs: number): RotationOccasion[] {
  const windows = windowsOf(resp)          // sorted, and disjoint by construction (a union)
  const starts = rotationInstants(resp)    // sorted
  const out: RotationOccasion[] = []
  const byWindow = new Map<number, RotationOccasion>()
  let wi = 0

  for (const at of starts) {
    // The starts are sorted, so the window pointer only ever moves forward.
    while (wi < windows.length && windows[wi].endMs < at) wi++
    const w = windows[wi]
    const inside = w !== undefined && w.startMs <= at && at <= w.endMs
    // A window that spans the whole run gives the view nowhere to go.
    const fittable = inside && w.endMs - w.startMs < horizonSpanMs

    if (!fittable) {
      out.push({ atMs: at, startMs: at, endMs: at, count: 1, windowless: true })
      continue
    }
    const held = byWindow.get(wi)
    if (held) {
      held.count++
      continue
    }
    const occasion: RotationOccasion = {
      atMs: at, startMs: w.startMs, endMs: w.endMs, count: 1, windowless: false,
    }
    byWindow.set(wi, occasion)
    out.push(occasion)
  }
  // `starts` is sorted and the pointer is monotone, so `out` is already in order — but the
  // buttons' whole contract is that it is, so say it rather than rely on it.
  return out.sort((a, b) => a.atMs - b.atMs)
}
