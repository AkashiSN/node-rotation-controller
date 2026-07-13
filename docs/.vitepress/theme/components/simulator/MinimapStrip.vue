<script setup lang="ts">
// The minimap: the WHOLE horizon, with the current view drawn on it as a brush.
//
// It is what makes "view ≠ horizon" visible rather than merely true. The original chart's
// right edge read as truncation because there was nothing on screen to say the data
// continued past it; here, a view at either edge shows the rest of the run beside it.
import { computed, ref } from 'vue'
import type { WindowView } from './timeline.ts'
import type { Span, View } from './zoom.ts'

const props = defineProps<{
  horizon: Span
  view: View
  targets: number[]
  windows: WindowView[]
}>()
const emit = defineEmits<{ 'update:view': [View] }>()

const W = 1000, H = 28

const span = computed(() => Math.max(1, props.horizon.endMs - props.horizon.startMs))
const x = (ms: number) => ((ms - props.horizon.startMs) / span.value) * W
const msAt = (fraction: number) => props.horizon.startMs + fraction * span.value

const brush = computed(() => ({
  x: x(props.view.startMs),
  w: Math.max(2, x(props.view.endMs) - x(props.view.startMs)),
}))

const bands = computed(() => props.windows.map(w => ({
  x: x(w.startMs),
  w: Math.max(1, x(w.endMs) - x(w.startMs)),
})))

const marks = computed(() => props.targets.map(ms => x(ms)))

const root = ref<SVGSVGElement | null>(null)
const dragging = ref(false)

/** Centre the view on the clicked instant, keeping its width — the minimap moves the view,
 *  it does not resize it. */
function moveTo(e: PointerEvent) {
  const box = root.value?.getBoundingClientRect()
  if (!box || !box.width) return
  const at = msAt((e.clientX - box.left) / box.width)
  const width = props.view.endMs - props.view.startMs
  emit('update:view', { startMs: at - width / 2, endMs: at + width / 2 })
}

function onDown(e: PointerEvent) {
  dragging.value = true
  ;(e.target as Element).setPointerCapture?.(e.pointerId)
  moveTo(e)
}
function onMove(e: PointerEvent) {
  if (dragging.value) moveTo(e)
}
function onUp(e: PointerEvent) {
  dragging.value = false
  ;(e.target as Element).releasePointerCapture?.(e.pointerId)
}
</script>

<template>
  <svg ref="root" class="sim-minimap" :viewBox="`0 0 ${W} ${H}`" role="img"
       @pointerdown="onDown" @pointermove="onMove" @pointerup="onUp" @pointercancel="onUp">
    <rect x="0" y="0" :width="W" :height="H" class="sim-minimap-bg" />
    <rect v-for="(b, i) in bands" :key="`b${i}`" :x="b.x" y="0" :width="b.w" :height="H"
          class="sim-minimap-window" />
    <line v-for="(m, i) in marks" :key="`m${i}`" :x1="m" y1="4" :x2="m" :y2="H - 4"
          class="sim-minimap-rotation" />
    <rect :x="brush.x" y="0" :width="brush.w" :height="H" class="sim-minimap-brush" />
  </svg>
</template>
