<script setup lang="ts">
import { computed } from 'vue'
import { parseGoDuration, type Fleet, type Horizon, type SimEvent } from './model.ts'
import { useLabels } from './i18n.ts'

const props = defineProps<{ events: SimEvent[]; horizon: Horizon; fleet: Fleet }>()
const t = useLabels()

const W = 1000, ROW = 26, PAD_L = 130, PAD_R = 20, PAD_T = 28, BAND = 14

const t0 = computed(() => new Date(props.horizon.start).getTime())
const t1 = computed(() => new Date(props.horizon.end).getTime())
// A degenerate horizon (end <= start, or either bound not a parseable instant — e.g.
// a user-typed negative expireAfter feeding defaultHorizon) breaks the x()/clip()
// scale: clip() assumes t0 <= t1, and x() would place points far outside the 0..1000
// viewBox. Gate the whole chart on this instead of trying to draw a mis-scaled one.
const horizonValid = computed(() =>
  Number.isFinite(t0.value) && Number.isFinite(t1.value) && t1.value > t0.value)
const x = (ms: number) => PAD_L + ((ms - t0.value) / Math.max(1, t1.value - t0.value)) * (W - PAD_L - PAD_R)
// Clip to the visible horizon: an interval can start before start or run past end,
// and an unclipped band would render outside the plot area.
const clip = (ms: number) => Math.min(Math.max(ms, t0.value), t1.value)
const at = (e: SimEvent) => new Date(e.at).getTime()
// No `until` on an edge-triggered interval event means it was still open at the end
// of the simulated horizon — mirror windows()'s unclosed-window fallback instead of
// falling back to `e.at`, which produced a zero-width band silently dropped below.
const until = (e: SimEvent) => (e.until ? new Date(e.until).getTime() : t1.value)

/** One row per node, including the replacements sim materialises: a replacement is a
 *  new node name, and it becomes the next generation's candidate. */
const rows = computed(() => {
  const names = props.fleet.nodes.map(n => n.name)
  for (const e of props.events) {
    if (e.replacement && !names.includes(e.replacement)) names.push(e.replacement)
  }
  return names
})

const height = computed(() => PAD_T + rows.value.length * ROW + 56)

/** Maintenance-window bands, paired from the window-open / window-close events. */
const windows = computed(() => {
  const out: { x1: number; x2: number }[] = []
  // The wire contract does not promise chronological order. Pairing by array
  // position (not time) would drop a close that precedes its open in the array,
  // leaving the open dangling to hit the unclosed-window fallback below and draw
  // a band across the WHOLE horizon. Sort a local copy — never mutate the prop.
  const sorted = [...props.events].sort((a, b) => at(a) - at(b))
  let open: number | null = null
  for (const e of sorted) {
    if (e.kind === 'window-open') open = at(e)
    if (e.kind === 'window-close' && open !== null) {
      out.push({ x1: x(clip(open)), x2: x(clip(at(e))) })
      open = null
    }
  }
  if (open !== null) out.push({ x1: x(clip(open)), x2: x(t1.value) })
  return out
})

/** blocked-by-gate / no-eligible-claim are EDGE-TRIGGERED and carry an interval
 *  (at..until) — one event for a week-long stretch. They are bands under the axis,
 *  never point marks: drawing them as points would throw away the coalescing that
 *  keeps the payload small and the picture readable. */
const blocked = computed(() =>
  props.events
    .filter(e => e.kind === 'blocked-by-gate' || e.kind === 'no-eligible-claim')
    .map(e => ({
      x1: x(clip(at(e))),
      x2: x(clip(until(e))),
      label: e.gate ?? 'no-eligible-claim',
    }))
    .filter(b => b.x2 > b.x1))

/** Per-row lifetime bar: from the node's createdAt (or the horizon start, for a
 *  replacement created inside it) to its rotation-done (or the horizon end).
 *
 *  For a replacement row (no declared FleetNode), its birth is the `at` of the
 *  event that named it in `replacement` — that lookup is guaranteed to succeed
 *  because `rows` only ever adds a name it saw on such an event. Still resolved
 *  as a total function (fallback to the horizon start) rather than asserted: a
 *  thrown TypeError here would blank the whole page over a single bad row. */
const bars = computed(() => rows.value.map((name, i) => {
  const declared = props.fleet.nodes.find(n => n.name === name)
  const bornEvent = declared ? undefined : props.events.find(e => e.replacement === name)
  const bornMs = declared ? new Date(declared.createdAt).getTime() : bornEvent ? at(bornEvent) : t0.value
  const done = props.events.find(e => e.kind === 'rotation-done' && e.node === name)
  const doneMs = done ? at(done) : t1.value
  const eff = declared?.expireAfter ?? props.fleet.expireAfter
  const deadlineMs = bornMs + (parseGoDuration(eff) ?? 0)
  // A malformed createdAt (or event `at`) parses to NaN, and clip(NaN) stays NaN:
  // an SVG x1="NaN" paints nothing with no explanation. Treat it as absent (null)
  // so the template can skip that mark instead of emitting a garbage attribute.
  return {
    name,
    y: PAD_T + i * ROW,
    x1: Number.isFinite(bornMs) ? x(clip(bornMs)) : null,
    x2: Number.isFinite(doneMs) ? x(clip(doneMs)) : null,
    deadline: Number.isFinite(deadlineMs) && deadlineMs <= t1.value && deadlineMs >= t0.value
      ? x(deadlineMs) : null,
  }
}))

/** The point marks: rotation-start (surge-less drawn distinctly), node-ready,
 *  rotation-done and — in red — every expire-after-breach. */
const marks = computed(() => props.events
  .filter(e => ['rotation-start', 'node-ready', 'rotation-done', 'expire-after-breach'].includes(e.kind))
  .map(e => {
    const row = rows.value.indexOf(e.node ?? '')
    if (row < 0) return null
    const atMs = at(e)
    // A malformed `at` parses to NaN; skip the mark rather than emit cx="NaN" (see
    // the same treatment in `bars` above).
    if (!Number.isFinite(atMs)) return null
    return {
      kind: e.kind,
      surgeless: e.surgeless === true,
      cx: x(clip(atMs)),
      cy: PAD_T + row * ROW - 4,
      title: `${e.kind}${e.surgeless ? ' (surge-less)' : ''} — ${e.node} @ ${e.at}`,
    }
  })
  .filter((m): m is NonNullable<typeof m> => m !== null))
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.timeline }}</h3>
    <p v-if="!horizonValid" class="sim-empty">{{ t.horizonInvalid }}</p>
    <svg v-else class="sim-svg" :viewBox="`0 0 ${W} ${height}`" role="img" :aria-label="t.timeline">
      <!-- maintenance windows -->
      <rect v-for="(w, i) in windows" :key="`w${i}`" :x="w.x1" :y="PAD_T - 12"
            :width="Math.max(1, w.x2 - w.x1)" :height="rows.length * ROW + 4" class="sim-window" />

      <!-- one row per node -->
      <g v-for="b in bars" :key="b.name">
        <text :x="PAD_L - 8" :y="b.y" text-anchor="end" class="sim-rowlabel">{{ b.name }}</text>
        <line v-if="b.x1 !== null && b.x2 !== null" :x1="b.x1" :y1="b.y - 4" :x2="b.x2" :y2="b.y - 4"
              class="sim-life" />
        <line v-if="b.deadline !== null" :x1="b.deadline" :y1="b.y - 12" :x2="b.deadline" :y2="b.y + 3"
              class="sim-deadline" />
      </g>

      <!-- event marks -->
      <g v-for="(m, i) in marks" :key="`m${i}`">
        <title>{{ m.title }}</title>
        <polygon v-if="m.kind === 'rotation-start'"
                 :points="`${m.cx},${m.cy - 6} ${m.cx - 5},${m.cy + 4} ${m.cx + 5},${m.cy + 4}`"
                 :class="m.surgeless ? 'sim-surgeless' : 'sim-rotation'" />
        <circle v-else-if="m.kind === 'node-ready'" :cx="m.cx" :cy="m.cy" r="3" class="sim-ready" />
        <circle v-else-if="m.kind === 'rotation-done'" :cx="m.cx" :cy="m.cy" r="3" class="sim-done" />
        <g v-else class="sim-breach">
          <line :x1="m.cx - 5" :y1="m.cy - 5" :x2="m.cx + 5" :y2="m.cy + 5" />
          <line :x1="m.cx - 5" :y1="m.cy + 5" :x2="m.cx + 5" :y2="m.cy - 5" />
        </g>
      </g>

      <!-- blocked intervals, under the axis -->
      <g v-for="(b, i) in blocked" :key="`b${i}`">
        <title>{{ b.label }}</title>
        <rect :x="b.x1" :y="PAD_T + rows.length * ROW + 8" :width="Math.max(1, b.x2 - b.x1)" :height="BAND"
              class="sim-blocked" />
      </g>

      <text :x="PAD_L" :y="height - 8" class="sim-axis">{{ horizon.start }}</text>
      <text :x="W - PAD_R" :y="height - 8" text-anchor="end" class="sim-axis">{{ horizon.end }}</text>
    </svg>

    <p v-if="horizonValid" class="sim-legend">
      <span class="k-rotation" /> {{ t.legend.rotation }}
      <span class="k-surgeless" /> {{ t.legend.surgeless }}
      <span class="k-ready" /> {{ t.legend.ready }}
      <span class="k-breach" /> {{ t.legend.breach }}
      <span class="k-window" /> {{ t.legend.window }}
      <span class="k-blocked" /> {{ t.legend.blocked }}
    </p>
  </section>
</template>
