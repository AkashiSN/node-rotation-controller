<script setup lang="ts">
// The symbols the forecast strip reports — A, t_rot, t_rot_est, G, C — with the formula each
// is derived from.
//
// They appeared on the page as bare <dt>s with no definition anywhere, so the strip answered
// a question the visitor had no way to ask (#261). This is a POINTER, not a second source of
// truth: the formulas are restated because a reader needs them where the numbers are, and
// every entry links back to the specification, which remains the one place they are defined.
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'
import { useLabels } from './i18n.ts'

const t = useLabels()
const { lang } = useData()

// The JA site lives under /ja/, and its headings carry their own (Japanese) slugs — the
// anchors are NOT shared across locales. Sending a Japanese reader to the English chapter
// would be the same defect this page is fixing, one level up.
const ja = computed(() => lang.value.startsWith('ja'))
const specBase = computed(() => (ja.value ? '/ja/specification' : '/specification'))
const symbolsHref = computed(() =>
  withBase(`${specBase.value}/01-overview${ja.value ? '#14-用語' : '#14-terminology'}`))
const derivationHref = computed(() =>
  withBase(`${specBase.value}/03-design${ja.value ? '#32-候補選定' : '#32-candidate-selection'}`))

// The formulas are CODE, not prose: they are identical in every locale, and they are the
// spec's own (§1.4, §3.2). Only the definition beside them is translated.
const rows = computed(() => [
  { symbol: 'A', formula: 'A = E − (K·P + t_rot)', def: t.value.symbols.defs.a },
  { symbol: 't_rot', formula: 't_rot = readyTimeout + tGP + buffer', def: t.value.symbols.defs.tRot },
  { symbol: 't_rot_est', formula: 't_rot_est = provisioningEstimate + drainEstimate', def: t.value.symbols.defs.tRotEst },
  { symbol: 'G', formula: 'G = floor(((E − t_rot) − A) / P)', def: t.value.symbols.defs.g },
  { symbol: 'C', formula: 'C = m · ceil(D / (t_rot_est + cooldownAfter))', def: t.value.symbols.defs.c },
])
</script>

<template>
  <details class="sim-symbols">
    <summary>{{ t.symbols.title }}</summary>
    <p class="sim-hint">{{ t.symbols.hint }}</p>
    <dl class="sim-symbol-list">
      <div v-for="r in rows" :key="r.symbol" class="sim-symbol">
        <dt><code class="sim-symbol-name">{{ r.symbol }}</code></dt>
        <dd>
          <code class="sim-symbol-formula">{{ r.formula }}</code>
          <span class="sim-symbol-def">{{ r.def }}</span>
        </dd>
      </div>
    </dl>
    <p class="sim-hint">
      <a :href="symbolsHref">{{ t.symbols.specSymbols }}</a> ·
      <a :href="derivationHref">{{ t.symbols.specDerivation }}</a>
    </p>
  </details>
</template>
