<script setup lang="ts">
// The duration ruler: two GROUPED scales, bridged by one honest sentence.
//
// A single linear scale cannot work — against an expireAfter of 480h, terminationGracePeriod
// is 0.2% of the width and drain and provisioning are effectively zero. Six bars of which
// five have no length is a duration list with decoration. The same scale collision is what
// made the timeline unreadable, so the ruler is where the page NAMES it.
import { computed } from 'vue'
import type { Fleet, SimResponse, SimResult } from './model.ts'
import { buildRuler, scaleWithin } from './ruler.ts'
import { formatDuration } from './timeutil.ts'
import { useLabels } from './i18n.ts'

const props = defineProps<{
  result: SimResult
  fleet: Fleet
  response: SimResponse
  provisioningMs: number
  drainMs: number
  readyTimeout: string
  cooldownAfter: string
}>()
const t = useLabels()

const model = computed(() => buildRuler({
  result: props.result,
  fleet: props.fleet,
  resp: props.response,
  provisioningMs: props.provisioningMs,
  drainMs: props.drainMs,
  readyTimeout: props.readyTimeout,
  cooldownAfter: props.cooldownAfter,
  tgp: props.fleet.terminationGracePeriod ?? '',
}))

const groups = computed(() => [
  { key: 'lifecycle', title: t.value.chart.rulerLifecycle, bars: model.value.lifecycle },
  { key: 'rotation', title: t.value.chart.rulerRotation, bars: model.value.rotation },
].map(g => {
  const scale = scaleWithin(g.bars)
  return {
    ...g,
    bars: g.bars.map(b => ({
      ...b,
      pct: Math.max(0.4, scale(b) * 100), // a hairline, so a real-but-tiny bar is still THERE
      label: formatDuration(b.ms),
      note: t.value.chart.quantity[b.quantity],
    })),
  }
}))

const ratio = computed(() => {
  const r = model.value.ratio
  if (!r) return null
  const pct = r.fraction < 0.001
    ? `${(r.fraction * 100).toFixed(3)}%`
    : `${(r.fraction * 100).toFixed(1)}%`
  const rotation = formatDuration(r.numeratorMs)
  const lifetime = formatDuration(r.denominatorMs)
  // A forecast numerator mixed with an actual denominator is acceptable. Doing it SILENTLY
  // is not: an operator who reads a forecast as a measurement trusts a run that never
  // happened.
  return r.forecast
    ? t.value.chart.rulerRatioForecast(rotation, lifetime, pct)
    : t.value.chart.rulerRatio(rotation, lifetime, pct)
})
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.chart.ruler }}</h3>
    <div class="sim-ruler">
      <div v-for="g in groups" :key="g.key" class="sim-ruler-group">
        <h4>{{ g.title }}</h4>
        <div v-for="b in g.bars" :key="b.key" class="sim-ruler-row">
          <span class="sim-ruler-name" :title="b.note">{{ b.key }}</span>
          <span class="sim-ruler-track">
            <span :class="['sim-ruler-bar', `sim-q-${b.quantity}`]" :style="{ width: `${b.pct}%` }" />
          </span>
          <span class="sim-ruler-value">{{ b.label }}</span>
          <span class="sim-ruler-note">{{ b.note }}</span>
        </div>
      </div>
    </div>
    <p v-if="ratio" class="sim-hint sim-ruler-ratio">{{ ratio }}</p>
  </section>
</template>
