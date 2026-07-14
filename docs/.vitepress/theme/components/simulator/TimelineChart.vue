<script setup lang="ts">
// The timeline. One row per SLOT; generations relay along it, left to right.
//
// Every derivation lives in the pure modules — timeline.ts (slots, segments, windows),
// zoom.ts (the view, the tick ladder, the semantic thresholds), timeutil.ts (wall-clock
// arithmetic in the policy's timezone). This component scales those instants to the
// viewBox, draws them, and routes interaction back into zoom.ts. It decides nothing.
//
// The one rule everything here answers to: THE CHART MUST NEVER STATE SOMETHING THE
// SIMULATION DID NOT ESTABLISH. An absent boundary is hatched, never squashed to a
// zero-length bar; a horizon artifact is never drawn as a policy boundary; the
// template-derived ageThreshold appears on the ruler and in the header, never as a
// per-generation line.
import { computed, ref, watch } from 'vue'
import type { Fleet, Horizon, SimResponse } from './model.ts'
import {
  buildTimeline, rotationInstants, rotationOccasions,
  type GenerationView, type Segment,
} from './timeline.ts'
import {
  SEMANTIC, clampView, fitInstant, fitTo, nextTarget, panBy, prevTarget,
  pxOf, reconcileView, ticksOf, zoomBy, type View,
} from './zoom.ts'
import { formatDuration, formatInstant } from './timeutil.ts'
import { useLabels } from './i18n.ts'
import MinimapStrip from './MinimapStrip.vue'

const props = defineProps<{
  response: SimResponse
  horizon: Horizon
  fleet: Fleet
  timezone: string
}>()
const t = useLabels()

// The viewBox is the coordinate system, and — because happy-dom has no layout engine and
// getBoundingClientRect is stubbed — it is also the width the semantic thresholds are
// measured in. A pixel here is a viewBox unit, which is what the CSS scales.
const W = 1000, PAD_L = 120, PAD_R = 24
const PLOT = W - PAD_L - PAD_R
const AXIS_H = 44, ROW_H = 48, BAR_H = 12, BLOCKED_H = 12

const t0 = computed(() => new Date(props.horizon.start).getTime())
const t1 = computed(() => new Date(props.horizon.end).getTime())
// A degenerate horizon (end <= start, or a bound that is not an instant — a user-typed
// negative expireAfter reaching defaultHorizon) would place every point far outside the
// viewBox. Gate the chart on it rather than draw a mis-scaled one.
const horizonValid = computed(() =>
  Number.isFinite(t0.value) && Number.isFinite(t1.value) && t1.value > t0.value)

const horizonSpan = computed(() => ({ startMs: t0.value, endMs: t1.value }))

const tl = computed(() => buildTimeline(props.response, props.horizon, props.fleet))

// The minimap marks EVERY rotation — it is a density map of the whole run. The BUTTONS step
// by occasion (one window occurrence's worth of rotations), which is a different, coarser
// thing: see rotationOccasions.
const instants = computed(() => rotationInstants(props.response))
const occasions = computed(() => rotationOccasions(props.response, t1.value - t0.value))
const targets = computed(() => occasions.value.map(o => o.atMs))

// THE VIEW — the sub-range on screen. It is not the horizon, and the page says so: the
// horizon is what Go was asked to simulate, and zoom never changes it.
const view = ref<View>({ startMs: t0.value, endMs: t1.value })

// Every keystroke reruns the simulation, and a rerun can move the horizon. zoom.ts owns
// the rule: a view that was the whole horizon stays the whole horizon; any other view keeps
// its instants, clamped. Without this, typing in the fleet form would silently throw away
// the rotation the visitor had zoomed into.
watch(horizonSpan, (after, before) => {
  if (!horizonValid.value) return
  view.value = reconcileView(view.value, before, after)
})

const span = computed(() => Math.max(1, view.value.endMs - view.value.startMs))
const x = (ms: number) => PAD_L + ((ms - view.value.startMs) / span.value) * PLOT
const px = (ms: number) => pxOf(ms, view.value, PLOT)
const msAtX = (px: number) => view.value.startMs + ((px - PAD_L) / PLOT) * span.value

const rows = computed(() => tl.value.rows)
const height = computed(() => AXIS_H + rows.value.length * ROW_H + BLOCKED_H + 28)
const blockedY = computed(() => AXIS_H + rows.value.length * ROW_H + 8)
const rowTop = (i: number) => AXIS_H + i * ROW_H
const barY = (i: number) => rowTop(i) + 18

const ticks = computed(() => ticksOf(view.value, props.timezone, PLOT))

// Semantic zoom: an element is drawn only once it is wide enough to MEAN something. A
// sub-pixel drain is not a short drain, it is an illegible one — and drawing it anyway is
// how the original chart came to show a 4h maintenance window as a 4px sliver.
const visible = (a: number, b: number) => b >= view.value.startMs && a <= view.value.endMs
const wide = (a: number, b: number, min: number) => px(b - a) >= min

const windows = computed(() => tl.value.windows
  .filter(w => visible(w.startMs, w.endMs))
  .map(w => ({
    x1: x(w.startMs),
    x2: x(w.endMs),
    thin: !wide(w.startMs, w.endMs, SEMANTIC.windowBandPx),
    // Too narrow for its edges to BE edges: one stripe, carrying its own contrast.
    narrow: !wide(w.startMs, w.endMs, SEMANTIC.windowEdgePx),
    // A boundary the HORIZON cut into is an artifact, not a transition the schedule made.
    // Each edge carries its own verdict: a window clipped on the left and closed on the
    // right has one of each, and drawing both the same would state something false about
    // the one that is real.
    startClipped: w.startClipped,
    endClipped: w.endClipped,
    title: `${t.value.legend.window}: ${formatInstant(w.startMs, props.timezone)} → ${formatInstant(w.endMs, props.timezone)}`,
  })))

const blocked = computed(() => tl.value.blocked
  .filter(b => visible(b.startMs, b.endMs))
  .map(b => ({ x1: x(b.startMs), x2: x(b.endMs), label: b.label })))

interface DrawnSegment {
  kind: Segment['kind']
  x1: number
  x2: number
  open: boolean
  reason: Segment['openReason']
  title: string
}

function drawSegments(g: GenerationView): DrawnSegment[] {
  const out: DrawnSegment[] = []
  for (const s of g.segments) {
    // An OPEN segment has no established end: it is hatched, running to simulatedThrough —
    // never collapsed onto its start, which would paint a duration the simulation never
    // reported.
    const end = s.endMs ?? s.openToMs
    if (!visible(s.startMs, end)) continue
    if (!wide(s.startMs, end, SEMANTIC.segmentPx) && s.kind !== 'running') continue
    const reasonText = s.openReason === 'in-flight' ? t.value.chart.inFlight
      : s.openReason === 'malformed' ? t.value.chart.malformed
      : ''
    out.push({
      kind: s.kind,
      x1: x(s.startMs),
      x2: x(end),
      open: s.endMs === null,
      reason: s.openReason,
      title: `${g.name} — ${t.value.chart[s.kind]}: ${formatInstant(s.startMs, props.timezone)} → ${
        s.endMs === null ? reasonText : formatInstant(s.endMs, props.timezone)}`,
    })
  }
  return out
}

const drawn = computed(() => rows.value.map((row, i) => ({
  slot: row.slot,
  label: row.label,
  declared: row.declared,
  y: rowTop(i),
  barY: barY(i),
  generations: row.generations.map(g => {
    const drainStart = g.drainStartMs
    const capEnd = g.drainCapEndMs
    // The TGP bracket is a DIMENSION over [drainStart, drainStart + drainCap] — a cap, not
    // a marker at the drain's end. It carries text, so it is drawn only where there is room
    // for it to be read.
    const showCap = drainStart !== null && capEnd !== null
      && visible(drainStart, capEnd) && wide(drainStart, capEnd, SEMANTIC.capBracketPx)
    const eligible = g.eligibilityMs !== null && g.deadlineMs !== null
      && visible(g.eligibilityMs, g.deadlineMs)
    // MAKE-BEFORE-BREAK, annotated. The span in which two nodes coexist is this
    // generation's provisioning: its predecessor is still running throughout it (its
    // NodeClaim is deleted only at node-ready). It is the payload of the whole page, so once
    // there is room to read it, it is named rather than left for the reader to infer.
    const prov = g.segments.find(s => s.kind === 'provisioning')
    const showOverlap = g.birthMode === 'surge' && prov !== undefined && prov.endMs !== null
      && visible(prov.startMs, prov.endMs) && wide(prov.startMs, prov.endMs, SEMANTIC.capBracketPx)
    return {
      key: `${g.slot}/${g.gen}`,
      name: g.name,
      provisional: g.provisional,
      segments: drawSegments(g),
      // The eligibility region's left edge is EXCLUSIVE: "eligible after". The trigger is a
      // strict inequality, so the boundary instant itself is not yet eligible.
      eligibleX1: eligible ? x(g.eligibilityMs!) : null,
      eligibleX2: eligible ? x(g.deadlineMs!) : null,
      deadlineX: g.deadlineMs !== null && visible(g.deadlineMs, g.deadlineMs) ? x(g.deadlineMs) : null,
      deadlineTitle: `${g.name} — ${t.value.chart.deadline}: ${
        g.deadlineMs === null ? '—' : formatInstant(g.deadlineMs, props.timezone)}`,
      eligibleTitle: `${g.name} — ${t.value.chart.eligibleAfter}: ${
        g.eligibilityMs === null ? '—' : formatInstant(g.eligibilityMs, props.timezone)}`,
      // The breach is emitted AT the deadline by construction, so the two are co-located and
      // the glyph is OFFSET rather than overdrawn — a regression here would silently hide
      // every breach in the run.
      breachX: g.breachMs !== null && visible(g.breachMs, g.breachMs) ? x(g.breachMs) : null,
      breachTitle: `${g.name} — ${t.value.chart.breach}`,
      capX1: showCap ? x(drainStart!) : null,
      capX2: showCap ? x(capEnd!) : null,
      capTitle: `${g.name} — ${g.drainCapSource === 'fallback' ? t.value.chart.drainCapFallback : t.value.chart.drainCap}: ${formatDuration(g.drainCapMs)}`,
      overlapX1: showOverlap ? x(prov!.startMs) : null,
      overlapX2: showOverlap ? x(prov!.endMs!) : null,
      overlapTitle: `${t.value.chart.overlap} (${formatDuration(prov ? (prov.endMs ?? prov.startMs) - prov.startMs : 0)})`,
    }
  }),
})))

/** A bar still alive at simulatedThrough CONTINUES — it was not cut off. The marker says
 *  so explicitly, because a bar that simply stops at the edge reads as truncation, which
 *  was one of the original chart's worst lies. */
const continuesX = computed(() => {
  const through = tl.value.simulatedThroughMs
  return through <= view.value.endMs && through >= view.value.startMs ? x(through) : null
})

const viewLabel = computed(() =>
  `${formatInstant(view.value.startMs, props.timezone)} → ${formatInstant(view.value.endMs, props.timezone)}`)

// ——— interaction ————————————————————————————————————————————————————————————
//
// BUTTON-FIRST. A page whose whole purpose is teaching cannot hide its payload behind an
// undiscoverable gesture, so every view change has an explicit control; the wheel, the drag
// and the keyboard are accelerators on top of them.

const centre = computed(() => (view.value.startMs + view.value.endMs) / 2)

/** The occasion the buttons last took us to, while it is still on screen. */
const visited = ref<number | null>(null)

/** Where "next" and "previous" count from.
 *
 *  - After a button jump, from the occasion we actually landed on — so "previous" steps to
 *    the one BEFORE it rather than re-fitting the same one.
 *  - While the whole horizon is on screen, from before the first instant: counting from the
 *    horizon's midpoint would silently skip every rotation in the first half of the run,
 *    which is most of them.
 *  - Otherwise (the visitor panned or zoomed by hand), from what they are looking at. */
const cursor = computed(() => {
  const at = visited.value
  if (at !== null && at >= view.value.startMs && at <= view.value.endMs) return at
  return span.value >= t1.value - t0.value ? view.value.startMs - 1 : centre.value
})
const canPrev = computed(() => prevTarget(targets.value, cursor.value) !== null)
const canNext = computed(() => nextTarget(targets.value, cursor.value) !== null)

function goTo(at: number | null) {
  if (at === null) return
  const occasion = occasions.value.find(o => o.atMs === at)
  if (!occasion) return
  // Fit the OCCASION: the window occurrence, centred, with both of its boundaries and every
  // rotation it holds on screen at once. Not the rotation instant — at the defaults an
  // occurrence holds up to ten of them, ~25m apart, and a view fitted to one of them was
  // both narrower than the window it sat in and off-centre inside it.
  //
  // A drain may run past the window's close; that bar continues past the right edge, and the
  // chart already says so (the continuation mark). Widening the fit to swallow it would move
  // the window off centre for a boundary the reader is not looking for.
  view.value = occasion.windowless
    ? fitInstant(occasion.atMs, horizonSpan.value)
    : fitTo(occasion.startMs, occasion.endMs, horizonSpan.value)
  visited.value = at
}
const first = () => goTo(targets.value[0] ?? null)
const prev = () => goTo(prevTarget(targets.value, cursor.value))
const next = () => goTo(nextTarget(targets.value, cursor.value))
function fitWindow() {
  const w = tl.value.windows.find(w => w.endMs >= view.value.startMs) ?? tl.value.windows[0]
  if (!w) return
  view.value = fitTo(w.startMs, w.endMs, horizonSpan.value)
}
const reset = () => { view.value = { startMs: t0.value, endMs: t1.value } }
const zoom = (factor: number, at = centre.value) => {
  view.value = zoomBy(view.value, factor, at, horizonSpan.value)
}

const svg = ref<SVGSVGElement | null>(null)

/** Map a client x to an instant. happy-dom has no layout, so getBoundingClientRect is
 *  stubbed there and this degrades to the view's centre — which is exactly why the tests
 *  are state-transition tests and not layout tests. */
function instantAtEvent(e: { clientX: number }): number {
  const box = svg.value?.getBoundingClientRect()
  if (!box || !box.width) return centre.value
  return msAtX(((e.clientX - box.left) / box.width) * W)
}

function onWheel(e: WheelEvent) {
  e.preventDefault()
  zoom(e.deltaY > 0 ? 1.25 : 0.8, instantAtEvent(e))
}

const dragging = ref<{ x: number; startMs: number } | null>(null)
function onPointerDown(e: PointerEvent) {
  dragging.value = { x: e.clientX, startMs: view.value.startMs }
  ;(e.target as Element).setPointerCapture?.(e.pointerId)
}
function onPointerMove(e: PointerEvent) {
  const d = dragging.value
  if (!d) return
  const box = svg.value?.getBoundingClientRect()
  const width = box?.width || W
  const deltaMs = -((e.clientX - d.x) / width) * span.value
  view.value = clampView(
    { startMs: d.startMs + deltaMs, endMs: d.startMs + deltaMs + span.value },
    horizonSpan.value,
  )
}
function onPointerUp(e: PointerEvent) {
  dragging.value = null
  ;(e.target as Element).releasePointerCapture?.(e.pointerId)
}

/** Keyboard, scoped to the focused chart so the arrow keys do not steal page scroll. */
function onKeydown(e: KeyboardEvent) {
  const step = span.value * 0.25
  switch (e.key) {
    case 'ArrowLeft': view.value = panBy(view.value, -step, horizonSpan.value); break
    case 'ArrowRight': view.value = panBy(view.value, step, horizonSpan.value); break
    case '+': case '=': zoom(0.8); break
    case '-': case '_': zoom(1.25); break
    case '0': reset(); break
    default: return
  }
  e.preventDefault()
}

// The tests reach the view through this rather than through pixel geometry: happy-dom
// cannot lay anything out, so what is testable is the STATE TRANSITION, honestly labelled.
defineExpose({ view, reset, zoom })
</script>

<template>
  <section class="sim-block">
    <div class="sim-chart-head">
      <h3>{{ t.timeline }}</h3>
      <div v-if="horizonValid" class="sim-legend">
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-seg-provisioning" x="1" y="4" width="18" height="7" /></svg>{{ t.chart.provisioning }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-seg-running" x="1" y="4" width="18" height="7" /></svg>{{ t.chart.running }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-seg-drain" x="1" y="4" width="18" height="7" /></svg>{{ t.chart.drain }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-seg-open" x="1" y="4" width="18" height="7" /></svg>{{ t.chart.inFlight }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><line class="sim-deadline" x1="10" y1="1" x2="10" y2="13" /></svg>{{ t.chart.deadline }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><g class="sim-breach"><line x1="6" y1="3" x2="14" y2="11" /><line x1="6" y1="11" x2="14" y2="3" /></g></svg>{{ t.legend.breach }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-overlap-band" x="1" y="2" width="18" height="10" /></svg>{{ t.chart.overlap }}</span>
        <!-- Each key is drawn with the same marks as the thing it explains — fill AND edge,
             since the edge is now what makes the region legible. -->
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-eligible" x="1" y="2" width="18" height="10" /><line class="sim-eligible-edge" x1="1" y1="2" x2="1" y2="12" /></svg>{{ t.chart.eligibleAfter }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-window" x="1" y="2" width="18" height="10" /><line class="sim-window-edge" x1="1" y1="2" x2="1" y2="12" /><line class="sim-window-edge" x1="19" y1="2" x2="19" y2="12" /></svg>{{ t.legend.window }}</span>
      </div>
    </div>

    <p v-if="!horizonValid" class="sim-empty">{{ t.horizonInvalid }}</p>

    <template v-else>
      <!-- The response contradicting itself is not something to paint over. -->
      <p v-if="tl.anomalies.length" class="sim-warn sim-banner sim-banner-warn">
        {{ t.chart.anomalies }}
        <span v-for="(a, i) in tl.anomalies" :key="i"><code>{{ a }}</code> </span>
      </p>

      <div class="sim-controls">
        <button type="button" class="sim-btn" :disabled="!targets.length" @click="first">{{ t.chart.firstRotation }}</button>
        <button type="button" class="sim-btn" :disabled="!canPrev" @click="prev">{{ t.chart.prevRotation }}</button>
        <button type="button" class="sim-btn" :disabled="!canNext" @click="next">{{ t.chart.nextRotation }}</button>
        <button type="button" class="sim-btn" :disabled="!tl.windows.length" @click="fitWindow">{{ t.chart.fitWindow }}</button>
        <button type="button" class="sim-btn" @click="zoom(0.8)">{{ t.chart.zoomIn }}</button>
        <button type="button" class="sim-btn" :disabled="span >= t1 - t0" @click="zoom(1.25)">{{ t.chart.zoomOut }}</button>
        <button type="button" class="sim-btn" @click="reset">{{ t.chart.reset }}</button>
      </div>
      <p class="sim-hint">{{ t.chart.rotationHint }}</p>
      <p class="sim-hint">{{ t.chart.viewHint }}</p>
      <p class="sim-view-label"><strong>{{ t.chart.view }}:</strong> <code>{{ viewLabel }}</code> <span class="sim-tz">({{ timezone }})</span></p>

      <MinimapStrip :horizon="horizonSpan" :view="view" :targets="instants" :windows="tl.windows"
                    :timezone="timezone" @update:view="v => (view = clampView(v, horizonSpan))" />

      <div class="sim-chart-scroll">
        <svg ref="svg" class="sim-svg" :viewBox="`0 0 ${W} ${height}`" tabindex="0"
             role="application" :aria-label="t.timeline"
             @wheel="onWheel" @pointerdown="onPointerDown" @pointermove="onPointerMove"
             @pointerup="onPointerUp" @pointercancel="onPointerUp" @keydown="onKeydown">
          <defs>
            <!-- The plot is clipped so a bar that runs past the VIEW does not paint over the
                 row labels; the data itself is never clipped to the view. -->
            <clipPath id="sim-plot-clip">
              <rect :x="PAD_L" y="0" :width="PLOT" :height="height" />
            </clipPath>
            <pattern id="sim-hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
              <line x1="0" y1="0" x2="0" y2="6" class="sim-hatch-line" />
            </pattern>
          </defs>

          <!-- axis: two permanent rows, in the DISPLAY timezone -->
          <g class="sim-axis-rows">
            <text v-for="tick in ticks.coarse" :key="`c${tick.ms}`" :x="x(tick.ms)" y="14"
                  class="sim-axis sim-axis-coarse">{{ tick.label }}</text>
            <text v-for="tick in ticks.fine" :key="`f${tick.ms}`" :x="x(tick.ms)" y="30"
                  class="sim-axis sim-axis-fine">{{ tick.label }}</text>
          </g>

          <g clip-path="url(#sim-plot-clip)">
            <line v-for="tick in ticks.fine" :key="`g${tick.ms}`" :x1="x(tick.ms)" :y1="AXIS_H - 8"
                  :x2="x(tick.ms)" :y2="height - 24" class="sim-gridline" />

            <!-- maintenance windows: below 3px a band degrades to a TICK, never a hairline
                 rectangle a reader would mistake for a border.
                 The band's FILL only says "something is here" — it is a background, and a
                 background that shouts fights the bars drawn on it. Its vertical EDGES
                 carry the contrast (a fill over the dark theme cannot clear 3:1 at any
                 alpha a background may honestly use; a stroke clears it easily — #260).
                 The horizontal edges are the plot's, not the window's, and are never
                 drawn: the schedule set no such boundary. -->
            <g v-for="(w, i) in windows" :key="`w${i}`">
              <title>{{ w.title }}</title>
              <template v-if="!w.thin">
                <rect :x="w.x1" :y="AXIS_H - 8" :width="Math.max(1, w.x2 - w.x1)"
                      :height="rows.length * ROW_H + 4"
                      :class="['sim-window', { 'sim-window-narrow': w.narrow }]" />
                <template v-if="!w.narrow">
                  <line :x1="w.x1" :y1="AXIS_H - 8" :x2="w.x1" :y2="AXIS_H + rows.length * ROW_H - 4"
                        :class="['sim-window-edge', { 'sim-window-edge-clipped': w.startClipped }]" />
                  <line :x1="w.x2" :y1="AXIS_H - 8" :x2="w.x2" :y2="AXIS_H + rows.length * ROW_H - 4"
                        :class="['sim-window-edge', { 'sim-window-edge-clipped': w.endClipped }]" />
                </template>
              </template>
              <line v-else :x1="w.x1" :y1="AXIS_H - 8" :x2="w.x1" :y2="AXIS_H + rows.length * ROW_H - 4"
                    class="sim-window-tick" />
            </g>

            <!-- simulatedThrough: the data STOPS here; it was not cut off -->
            <line v-if="continuesX !== null" :x1="continuesX" :y1="AXIS_H - 8" :x2="continuesX"
                  :y2="height - 24" class="sim-through" />

            <g v-for="row in drawn" :key="row.slot" class="sim-row">
              <g v-for="g in row.generations" :key="g.key">
                <!-- eligible AFTER: a region, from the boundary to the deadline. Its left
                     edge is drawn DASHED and its right edge not at all: "after" is a strict
                     inequality — the boundary instant is not itself eligible — and the
                     region's right end is the deadline, which has its own glyph. -->
                <template v-if="g.eligibleX1 !== null">
                  <rect :x="g.eligibleX1" :y="row.y + 8"
                        :width="Math.max(1, g.eligibleX2! - g.eligibleX1)" :height="BAR_H + 16"
                        class="sim-eligible" tabindex="0" role="graphics-symbol"
                        :aria-label="g.eligibleTitle"><title>{{ g.eligibleTitle }}</title></rect>
                  <line :x1="g.eligibleX1" :y1="row.y + 8" :x2="g.eligibleX1"
                        :y2="row.y + 8 + BAR_H + 16" class="sim-eligible-edge" />
                </template>

                <!-- the generation's own segments -->
                <g v-for="(s, i) in g.segments" :key="i">
                  <rect :x="s.x1" :y="row.barY" :width="Math.max(1, s.x2 - s.x1)" :height="BAR_H"
                        :class="['sim-seg', `sim-seg-${s.kind}`, { 'sim-seg-open': s.open, 'sim-seg-malformed': s.reason === 'malformed' }]"
                        tabindex="0" role="graphics-symbol" :aria-label="s.title">
                    <title>{{ s.title }}</title>
                  </rect>
                </g>

                <!-- the TGP CAP, as a dimension: the actual drain end and the cap endpoint
                     are DIFFERENT glyphs, so "the drain took 10m, the cap was 1h" is
                     legible as geometry -->
                <g v-if="g.capX1 !== null" class="sim-cap" tabindex="0" role="graphics-symbol"
                   :aria-label="g.capTitle">
                  <title>{{ g.capTitle }}</title>
                  <line :x1="g.capX1" :y1="row.barY + BAR_H + 6" :x2="g.capX2!" :y2="row.barY + BAR_H + 6" />
                  <line :x1="g.capX1" :y1="row.barY + BAR_H + 2" :x2="g.capX1" :y2="row.barY + BAR_H + 10" />
                  <line :x1="g.capX2!" :y1="row.barY + BAR_H + 2" :x2="g.capX2!" :y2="row.barY + BAR_H + 10" />
                </g>

                <!-- MAKE-BEFORE-BREAK: the span in which the replacement is coming up while
                     its predecessor is still serving. The payload of the page — named, not
                     left to be inferred from two bars that happen to overlap. -->
                <g v-if="g.overlapX1 !== null" class="sim-overlap" tabindex="0"
                   role="graphics-symbol" :aria-label="g.overlapTitle">
                  <title>{{ g.overlapTitle }}</title>
                  <rect :x="g.overlapX1" :y="row.y + 6" :width="Math.max(1, g.overlapX2! - g.overlapX1)"
                        :height="ROW_H - 16" class="sim-overlap-band" />
                </g>

                <!-- the deadline, and the breach that lands exactly ON it — offset, never
                     overdrawn -->
                <line v-if="g.deadlineX !== null" :x1="g.deadlineX" :y1="row.y + 4"
                      :x2="g.deadlineX" :y2="row.y + ROW_H - 8" class="sim-deadline">
                  <title>{{ g.deadlineTitle }}</title>
                </line>
                <g v-if="g.breachX !== null" class="sim-breach" tabindex="0" role="graphics-symbol"
                   :aria-label="g.breachTitle">
                  <title>{{ g.breachTitle }}</title>
                  <line :x1="g.breachX - 5" :y1="row.y + 1" :x2="g.breachX + 5" :y2="row.y + 11" />
                  <line :x1="g.breachX - 5" :y1="row.y + 11" :x2="g.breachX + 5" :y2="row.y + 1" />
                </g>
              </g>
            </g>

            <g v-for="(b, i) in blocked" :key="`b${i}`">
              <title>{{ b.label }}</title>
              <rect :x="b.x1" :y="blockedY" :width="Math.max(1, b.x2 - b.x1)" :height="BLOCKED_H"
                    class="sim-blocked" />
            </g>
          </g>

          <!-- row labels sit OUTSIDE the clip, so a panned bar never paints over them -->
          <g v-for="row in drawn" :key="`l${row.slot}`">
            <text :x="PAD_L - 10" :y="row.barY + BAR_H - 1" text-anchor="end"
                  :class="['sim-rowlabel', { 'sim-rowlabel-undeclared': !row.declared }]">{{ row.label }}</text>
          </g>
        </svg>
      </div>

      <!-- The chart is not the sole carrier of the result: a visually-hidden table restates
           it, and is re-rendered on every rerun.

           The hidden recipe goes on this WRAPPER, not on the <table>: under auto table
           layout a table's used width cannot go below its min-content width, so `width: 1px`
           is ignored and the table stays as wide as its longest row (~2200px). `clip` only
           suppresses painting, so that absolutely-positioned box still contributed scrollable
           overflow — the page gained a horizontal scrollbar into an empty region (#250). A
           <div> honours the 1px and clips the table inside it. -->
      <div class="sim-sr-only">
        <table>
          <caption>{{ t.chart.table }}</caption>
          <thead>
            <tr>
              <th>{{ t.chart.slot }}</th><th>{{ t.chart.generation }}</th><th>{{ t.nodeName }}</th>
              <th>{{ t.createdAt }}</th><th>{{ t.chart.deadline }}</th><th>{{ t.chart.eligibleAfter }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in tl.rows" :key="row.slot">
              <td>{{ row.slot }}</td>
              <td>{{ row.generations.length }}</td>
              <td>{{ row.generations.map(g => g.name).join(', ') }}</td>
              <td>{{ row.generations.map(g => formatInstant(g.createdMs, timezone)).join(', ') }}</td>
              <td>{{ row.generations.map(g => g.deadlineMs === null ? '—' : formatInstant(g.deadlineMs, timezone)).join(', ') }}</td>
              <td>{{ row.generations.map(g => g.eligibilityMs === null ? '—' : formatInstant(g.eligibilityMs, timezone)).join(', ') }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>
