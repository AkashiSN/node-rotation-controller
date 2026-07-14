<script setup lang="ts">
import { type SimResult } from './model.ts'
import { useLabels } from './i18n.ts'
import FindingList from './FindingList.vue'
import SymbolReference from './SymbolReference.vue'

defineProps<{ result: SimResult }>()
const t = useLabels()
</script>

<template>
  <section class="sim-block sim-header">
    <h3>{{ t.forecast }}</h3>
    <p class="sim-hint">{{ t.forecastHint }}</p>

    <dl class="sim-strip">
      <div><dt>A (ageThreshold)</dt><dd>{{ result.ageThreshold }}</dd></div>
      <div><dt>t_rot</dt><dd>{{ result.tRot }}</dd></div>
      <div><dt>t_rot_est</dt><dd>{{ result.tRotEstimate }}</dd></div>
      <div><dt>G</dt><dd>{{ result.g }}</dd></div>
      <div><dt>C</dt><dd>{{ result.c }}</dd></div>
    </dl>

    <!-- Beside the numbers, not on another page: the strip is where a reader first meets
         these symbols, so it is where their definitions have to be reachable. -->
    <SymbolReference />

    <!-- The controller's own English messages, verbatim in both locales. -->
    <FindingList v-if="result.findings?.length" :findings="result.findings" />
  </section>
</template>
