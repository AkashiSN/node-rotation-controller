<script setup lang="ts">
import { type Diagnostic } from './model.ts'
import { useLabels } from './i18n.ts'

defineProps<{ diagnostics: Diagnostic[]; partial: boolean }>()
const t = useLabels()
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.diagnostics }}</h3>
    <p v-if="partial" class="sim-fatal sim-banner">{{ t.partial }}</p>
    <ul v-if="diagnostics.length" class="sim-findings">
      <li v-for="(d, i) in diagnostics" :key="i" :class="`sim-${d.severity}`">
        <strong>{{ d.severity }}</strong> <code>{{ d.code }}</code> {{ d.message }}
      </li>
    </ul>
    <p v-else>{{ t.noDiagnostics }}</p>
  </section>
</template>
