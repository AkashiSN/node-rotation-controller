<script setup lang="ts">
import { type SimResult } from './model.ts'
import { useLabels } from './i18n.ts'

defineProps<{ result: SimResult }>()
const t = useLabels()
</script>

<template>
  <section class="sim-header">
    <h3>{{ t.forecast }}</h3>
    <p class="sim-hint">{{ t.forecastHint }}</p>

    <dl class="sim-strip">
      <div><dt>A (ageThreshold)</dt><dd>{{ result.ageThreshold }}</dd></div>
      <div><dt>t_rot</dt><dd>{{ result.tRot }}</dd></div>
      <div><dt>t_rot_est</dt><dd>{{ result.tRotEstimate }}</dd></div>
      <div><dt>G</dt><dd>{{ result.g }}</dd></div>
      <div><dt>C</dt><dd>{{ result.c }}</dd></div>
    </dl>

    <!-- The controller's own English messages, verbatim in both locales. -->
    <ul class="sim-findings">
      <li v-for="(f, i) in result.findings ?? []" :key="i" :class="`sim-${f.severity}`">
        <strong>{{ f.severity }}</strong> <code>{{ f.code }}</code> {{ f.message }}
      </li>
    </ul>
  </section>
</template>
