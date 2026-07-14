<script setup lang="ts">
import { computed } from 'vue'
import { type SimResult } from './model.ts'
import { buildDerivation } from './derivation.ts'
import { useLabels } from './i18n.ts'
import FindingList from './FindingList.vue'
import SymbolReference from './SymbolReference.vue'

const props = defineProps<{ result: SimResult }>()
const t = useLabels()

// The rows are DATA (derivation.ts): the component renders them and computes nothing. The
// value on each row is the wasm module's own — see derivation.ts for why that matters.
const rows = computed(() => buildDerivation(props.result, {
  overrideNote: t.value.derivation.overrideNote,
  fallbackMark: t.value.derivation.fallbackMark,
}))
</script>

<template>
  <section class="sim-block sim-header">
    <h3>{{ t.forecast }}</h3>
    <p class="sim-hint">{{ t.forecastHint }}</p>

    <!-- The derivation, not only its result: the inputs a reader would otherwise have to
         scroll to find — and P, D and a fallback tGP, which are written nowhere in the
         manifest at all — appear inside the formula that used them (#266). -->
    <dl class="sim-derivation">
      <div v-for="r in rows" :key="r.symbol" class="sim-derivation-row">
        <dt><code class="sim-symbol-name">{{ r.symbol }}</code></dt>
        <dd>
          <code class="sim-derivation-formula">{{ r.formula }}</code>
          <code v-if="r.substitution" class="sim-derivation-sub">{{ r.substitution }}</code>
          <span v-else-if="r.note" class="sim-derivation-note">{{ r.note }}</span>
        </dd>
        <dd class="sim-derivation-value">{{ r.value }}</dd>
      </div>
    </dl>

    <!-- Beside the numbers, not on another page: the strip is where a reader first meets
         these symbols, so it is where their definitions have to be reachable. -->
    <SymbolReference />

    <!-- The controller's own English messages, verbatim in both locales. -->
    <FindingList v-if="result.findings?.length" :findings="result.findings" />
  </section>
</template>
