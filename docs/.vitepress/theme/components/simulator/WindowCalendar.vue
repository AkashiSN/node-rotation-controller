<script setup lang="ts">
// The maintenance-window calendar: weekday x time-of-day, folded from the OBSERVED window
// occurrences — never from the policy, and never presented as the policy's recurrence.
//
// internal/window evaluates each entry in its OWN timezone and the effective schedule is
// their union, so the entry that produced an occurrence, its timezone and any boundary
// interior to the union are gone by the time the page sees it. A finite sample of what
// actually happened is the truthful thing to show, and the footer says exactly what the
// sample was.
import { computed } from 'vue'
import { buildCalendar, CELL_MINUTES, weekdayRows } from './calendar.ts'
import type { WindowView } from './timeline.ts'
import { formatInstant } from './timeutil.ts'
import { useLabels } from './i18n.ts'

const props = defineProps<{
  windows: WindowView[]
  spanStartMs: number
  spanEndMs: number
  timezone: string
  partial: boolean
}>()
const t = useLabels()

const cal = computed(() =>
  buildCalendar(props.windows, props.spanStartMs, props.spanEndMs, props.timezone))
const rows = computed(() => weekdayRows(cal.value))

const timeOf = (slot: number) => {
  const minutes = slot * CELL_MINUTES
  return `${String(Math.floor(minutes / 60)).padStart(2, '0')}:${String(minutes % 60).padStart(2, '0')}`
}

/** The hour labels across the top: every fourth cell is a whole hour. */
const hours = Array.from({ length: 24 }, (_, h) => h)

function title(weekday: number, slot: number, ratio: number | null, open: number, observed: number): string {
  const day = t.value.chart.weekdays[weekday]
  if (ratio === null) return `${day} ${timeOf(slot)}: ${t.value.chart.calendarUnknown}`
  return t.value.chart.calendarCell(
    day, timeOf(slot), Math.round(ratio * 100), Math.round(open), Math.round(observed),
  )
}
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.chart.calendar }}</h3>

    <p v-if="cal.wholeWeeks === 0" class="sim-empty">{{ t.chart.calendarNoWeeks }}</p>

    <template v-else>
      <div class="sim-cal-scroll">
        <table class="sim-cal">
          <thead>
            <tr>
              <th scope="col" class="sim-cal-corner"></th>
              <!-- Each hour spans four 15-minute cells. -->
              <th v-for="h in hours" :key="h" scope="col" colspan="4" class="sim-cal-hour">
                {{ String(h).padStart(2, '0') }}
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, weekday) in rows" :key="weekday">
              <th scope="row" class="sim-cal-day">{{ t.chart.weekdays[weekday] }}</th>
              <td v-for="cell in row" :key="cell.slot"
                  :class="['sim-cal-cell', { 'sim-cal-unknown': cell.ratio === null }]"
                  :style="cell.ratio === null ? undefined : { '--open': cell.ratio }"
                  tabindex="0"
                  :aria-label="title(weekday, cell.slot, cell.ratio, cell.openMinutes, cell.observedMinutes)"
                  :title="title(weekday, cell.slot, cell.ratio, cell.openMinutes, cell.observedMinutes)" />
            </tr>
          </tbody>
        </table>
      </div>

      <p class="sim-hint">
        {{ t.chart.calendarHint(cal.wholeWeeks, cal.timezone) }}
        <template v-if="partial">{{ t.chart.calendarPartial(formatInstant(spanEndMs, timezone)) }}</template>
      </p>
    </template>
  </section>
</template>
