<script setup lang="ts">
import { type Diagnostic } from './model.ts'
import { useLabels } from './i18n.ts'
import FindingList from './FindingList.vue'

defineProps<{ diagnostics: Diagnostic[]; partial: boolean }>()
const t = useLabels()
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.diagnostics }}</h3>
    <p v-if="partial" class="sim-banner sim-banner-fatal">{{ t.partial }}</p>
    <FindingList v-if="diagnostics.length" :findings="diagnostics" />
    <p v-else>{{ t.noDiagnostics }}</p>
  </section>
</template>
