<script setup lang="ts">
import { computed } from 'vue'
import type { Fleet, Horizon, SimEvent } from './model.ts'
import { buildTimeline } from './timeline.ts'
import { useLabels } from './i18n.ts'

const props = defineProps<{ events: SimEvent[]; horizon: Horizon; fleet: Fleet }>()
const t = useLabels()

// PAD_R leaves a clear right-hand gutter so bars that reach the horizon end do not sit
// flush against the SVG edge (which read as "cut off"); the ongoing chevron is drawn
// INTO this gutter to show the lifetime continues past the visible window.
const W = 1000, ROW = 26, PAD_L = 130, PAD_R = 40, PAD_T = 28, BAND = 14

const t0 = computed(() => new Date(props.horizon.start).getTime())
const t1 = computed(() => new Date(props.horizon.end).getTime())
// A degenerate horizon (end <= start, or either bound not a parseable instant — e.g.
// a user-typed negative expireAfter feeding defaultHorizon) breaks the x() scale: it
// would place points far outside the 0..1000 viewBox. Gate the whole chart on this
// instead of trying to draw a mis-scaled one.
const horizonValid = computed(() =>
  Number.isFinite(t0.value) && Number.isFinite(t1.value) && t1.value > t0.value)
const x = (ms: number) => PAD_L + ((ms - t0.value) / Math.max(1, t1.value - t0.value)) * (W - PAD_L - PAD_R)

// Every DERIVATION (rows, births, deadlines, window/blocked pairing, marks) lives in
// the pure timeline.ts, where node --test pins it — notably the rule that a surged
// replacement is born at its rotation-START, not at the rotation-done that names it.
// This component only scales the resulting instants to the viewBox and draws them.
const tl = computed(() => buildTimeline(props.events, props.horizon, props.fleet))

const rows = computed(() => tl.value.rows)
const height = computed(() => PAD_T + rows.value.length * ROW + 56)

const windows = computed(() => tl.value.windows.map(w => ({ x1: x(w.startMs), x2: x(w.endMs) })))
const blocked = computed(() => tl.value.blocked.map(b => ({ x1: x(b.startMs), x2: x(b.endMs), label: b.label })))
const bars = computed(() => tl.value.bars.map((b, i) => ({
  name: b.name,
  ongoing: b.ongoing,
  y: PAD_T + i * ROW,
  x1: b.bornMs === null ? null : x(b.bornMs),
  x2: b.endMs === null ? null : x(b.endMs),
  deadline: b.deadlineMs === null ? null : x(b.deadlineMs),
})))
const marks = computed(() => tl.value.marks.map(m => ({
  kind: m.kind,
  surgeless: m.surgeless,
  title: m.title,
  cx: x(m.atMs),
  cy: PAD_T + m.row * ROW - 4,
})))
</script>

<template>
  <section class="sim-block">
    <div class="sim-chart-head">
      <h3>{{ t.timeline }}</h3>
      <!-- The legend is a KEY: every glyph here is drawn with the same class as the mark
           it explains, so its shape and colour always match the chart below (a plain
           colour swatch cannot tell a teal triangle from a teal dot). Every glyph the
           chart can draw appears here, and nothing here is absent from the chart. -->
      <div v-if="horizonValid" class="sim-legend">
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><line class="sim-life" x1="1" y1="7" x2="19" y2="7" /></svg>{{ t.legend.life }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><polygon class="sim-rotation" points="10,3 5,11 15,11" /></svg>{{ t.legend.rotation }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><polygon class="sim-surgeless" points="10,3 5,11 15,11" /></svg>{{ t.legend.surgeless }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><circle class="sim-ready" cx="10" cy="7" r="3" /></svg>{{ t.legend.ready }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><circle class="sim-done" cx="10" cy="7" r="3" /></svg>{{ t.legend.done }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><line class="sim-deadline" x1="10" y1="1" x2="10" y2="13" /></svg>{{ t.legend.deadline }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><g class="sim-breach"><line x1="6" y1="3" x2="14" y2="11" /><line x1="6" y1="11" x2="14" y2="3" /></g></svg>{{ t.legend.breach }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-window" x="2" y="2" width="16" height="10" /></svg>{{ t.legend.window }}</span>
        <span class="sim-key"><svg viewBox="0 0 20 14" class="sim-glyph"><rect class="sim-blocked" x="2" y="2" width="16" height="10" /></svg>{{ t.legend.blocked }}</span>
      </div>
    </div>
    <p v-if="!horizonValid" class="sim-empty">{{ t.horizonInvalid }}</p>
    <!-- The chart never shrinks below the width where its row labels are legible:
         .sim-svg carries a min-width, and this wrapper scrolls horizontally so the
         PAGE never does. -->
    <div v-else class="sim-chart-scroll">
    <svg class="sim-svg" :viewBox="`0 0 ${W} ${height}`" role="img" :aria-label="t.timeline">
      <!-- maintenance windows -->
      <rect v-for="(w, i) in windows" :key="`w${i}`" :x="w.x1" :y="PAD_T - 12"
            :width="Math.max(1, w.x2 - w.x1)" :height="rows.length * ROW + 4" class="sim-window" />

      <!-- one row per node -->
      <g v-for="b in bars" :key="b.name">
        <text :x="PAD_L - 8" :y="b.y" text-anchor="end" class="sim-rowlabel">{{ b.name }}</text>
        <line v-if="b.x1 !== null && b.x2 !== null" :x1="b.x1" :y1="b.y - 4" :x2="b.x2" :y2="b.y - 4"
              class="sim-life" />
        <!-- lifetime continues past the visible horizon: a chevron in the right gutter,
             so an ongoing bar does not read as cut off at the edge. -->
        <polygon v-if="b.ongoing && b.x2 !== null"
                 :points="`${b.x2},${b.y - 8} ${b.x2 + 8},${b.y - 4} ${b.x2},${b.y}`"
                 class="sim-ongoing" />
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
    </div>
  </section>
</template>
