<script setup lang="ts">
// The minimap: the WHOLE horizon, with the current view drawn on it as a brush.
//
// It is what makes "view ≠ horizon" visible rather than merely true. The original chart's
// right edge read as truncation because there was nothing on screen to say the data
// continued past it; here, a view at either edge shows the rest of the run beside it.
//
// It is also a CONTROL, not a picture: the wheel scrolls it, the brush drags, the handles
// resize. Every one of those decisions lives in minimap.ts — this component maps client
// coordinates into viewBox units, calls it, and emits. It decides nothing.
//
// THE CURTAIN, NOT A FILL. The brush used to be a translucent teal rectangle among teal
// rotation ticks and teal window bands, two pixels wide at any real zoom (#260). Dimming
// everything OUTSIDE the view instead makes the view the brightest thing on the strip at
// every width — which a fill can never be.
import { computed, ref } from 'vue'
import {
  HANDLE_MIN_BRUSH_UNITS, brushBox, centreOn, grabAt, moveTo, msAt, resizeEdge, wheelPan,
  type Grab,
} from './minimap.ts'
import type { WindowView } from './timeline.ts'
import { formatInstant } from './timeutil.ts'
import { useLabels } from './i18n.ts'
import { panBy, spanOf, type Span, type View } from './zoom.ts'

const props = defineProps<{
  horizon: Span
  view: View
  targets: number[]
  windows: WindowView[]
  timezone: string
}>()
const emit = defineEmits<{ 'update:view': [View] }>()
const t = useLabels()

const W = 1000, H = 28

const span = computed(() => Math.max(1, props.horizon.endMs - props.horizon.startMs))
const x = (ms: number) => ((ms - props.horizon.startMs) / span.value) * W

const brush = computed(() => brushBox(props.view, props.horizon, W))
/** Handles are an affordance, and an affordance that covers the thing it sits on is a trap:
 *  below this width there would be no brush left to grab for a MOVE. Semantic zoom, same as
 *  the chart. */
const handles = computed(() => brush.value.w >= HANDLE_MIN_BRUSH_UNITS)

const bands = computed(() => props.windows.map(w => ({
  x: x(w.startMs),
  w: Math.max(1, x(w.endMs) - x(w.startMs)),
})))

const marks = computed(() => props.targets.map(ms => x(ms)))

const viewLabel = computed(() =>
  `${formatInstant(props.view.startMs, props.timezone)} → ${formatInstant(props.view.endMs, props.timezone)}`)

const root = ref<SVGSVGElement | null>(null)

/** A client x, in viewBox units. Returns null where there is no layout to measure — which
 *  is every test: happy-dom stubs getBoundingClientRect. The gestures are therefore tested
 *  in minimap.ts, against instants, not against pixels. */
function unitsAt(clientX: number): number | null {
  const box = root.value?.getBoundingClientRect()
  if (!box || !box.width) return null
  return ((clientX - box.left) / box.width) * W
}

/** What the pointer took hold of, and — for a move — where inside the view it took hold, so
 *  the instant under it stays under it for the whole drag. */
const drag = ref<{ mode: Exclude<Grab, 'outside'>; offsetMs: number } | null>(null)

const cursor = computed(() => {
  if (drag.value?.mode === 'left' || drag.value?.mode === 'right') return 'ew-resize'
  return drag.value ? 'grabbing' : 'pointer'
})

function onDown(e: PointerEvent) {
  const units = unitsAt(e.clientX)
  if (units === null) return
  const at = msAt(units, props.horizon, W)
  const grab = grabAt(units, brush.value)
  ;(e.target as Element).setPointerCapture?.(e.pointerId)

  if (grab === 'outside') {
    // A click on the track: go there, keeping the zoom. The drag then continues as a move,
    // grabbed at the brush's middle — which is where the pointer now is.
    emit('update:view', centreOn(props.view, at, props.horizon))
    drag.value = { mode: 'move', offsetMs: spanOf(props.view) / 2 }
    return
  }
  drag.value = { mode: grab, offsetMs: at - props.view.startMs }
}

function onMove(e: PointerEvent) {
  const d = drag.value
  if (!d) return
  const units = unitsAt(e.clientX)
  if (units === null) return
  const at = msAt(units, props.horizon, W)
  emit('update:view', d.mode === 'move'
    ? moveTo(props.view, at, d.offsetMs, props.horizon)
    : resizeEdge(props.view, d.mode, at, props.horizon))
}

function onUp(e: PointerEvent) {
  drag.value = null
  ;(e.target as Element).releasePointerCapture?.(e.pointerId)
}

/** The wheel PANS — it does not zoom. A reader who reaches for the minimap is asking to move
 *  along the run at the scale they already chose; the chart's own wheel is where zoom lives.
 *  A trackpad's horizontal axis says the same thing, so whichever axis dominates is honoured. */
function onWheel(e: WheelEvent) {
  e.preventDefault()
  const delta = Math.abs(e.deltaX) > Math.abs(e.deltaY) ? e.deltaX : e.deltaY
  emit('update:view', wheelPan(props.view, delta, props.horizon))
}

/** Scoped to the focused strip, so the arrow keys do not steal the page's scroll. */
function onKeydown(e: KeyboardEvent) {
  const step = spanOf(props.view) * 0.25
  switch (e.key) {
    case 'ArrowLeft': emit('update:view', panBy(props.view, -step, props.horizon)); break
    case 'ArrowRight': emit('update:view', panBy(props.view, step, props.horizon)); break
    default: return
  }
  e.preventDefault()
}
</script>

<template>
  <!-- preserveAspectRatio="none" is LOAD-BEARING, not decoration. The strip is 1000x28 in a
       box whose width is the container's and whose height is pinned to 28px by CSS, so the
       default (xMidYMid meet) scales the content UNIFORMLY to fit the height — leaving it
       1000px wide and CENTRED, with a letterbox either side. Every gesture here maps a client
       x through getBoundingClientRect().width, which then disagreed with where the graphic
       actually was: at a 1112px-wide strip, a click 100px in landed on unit 90 by that
       arithmetic and on unit 44 in the SVG's own coordinates. The old click-to-centre carried
       the same error. Stretching the x axis makes the naive mapping the true one — and the
       strip finally spans the width it is measured against. -->
  <svg ref="root" class="sim-minimap" :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none"
       :style="{ cursor }"
       tabindex="0" role="group" :aria-label="`${t.chart.minimap}: ${viewLabel}`"
       @pointerdown="onDown" @pointermove="onMove" @pointerup="onUp" @pointercancel="onUp"
       @wheel="onWheel" @keydown="onKeydown">
    <title>{{ t.chart.minimapHint }}</title>
    <rect x="0" y="0" :width="W" :height="H" class="sim-minimap-bg" />
    <rect v-for="(b, i) in bands" :key="`b${i}`" :x="b.x" y="0" :width="b.w" :height="H"
          class="sim-minimap-window" />
    <line v-for="(m, i) in marks" :key="`m${i}`" :x1="m" y1="4" :x2="m" :y2="H - 4"
          class="sim-minimap-rotation" />

    <!-- THE CURTAIN: what is NOT on screen is dimmed, so what is on screen is found. -->
    <rect x="0" y="0" :width="brush.x" :height="H" class="sim-minimap-curtain" />
    <rect :x="brush.x + brush.w" y="0" :width="Math.max(0, W - brush.x - brush.w)" :height="H"
          class="sim-minimap-curtain" />

    <rect :x="brush.x" y="0.75" :width="brush.w" :height="H - 1.5" class="sim-minimap-brush">
      <title>{{ viewLabel }}</title>
    </rect>
    <!-- The handles are drawn only where they fit; see HANDLE_MIN_BRUSH_UNITS. They are LINES,
         not rects: the x axis is stretched (see above), so a rect's width and its corner
         radius would stretch with it, and a handle would grow fatter the wider the page.
         A non-scaling stroke stays the width it says it is. -->
    <g v-if="handles" class="sim-minimap-handle">
      <line :x1="brush.x" y1="6" :x2="brush.x" :y2="H - 6" />
      <line :x1="brush.x + brush.w" y1="6" :x2="brush.x + brush.w" :y2="H - 6" />
    </g>
  </svg>
</template>
