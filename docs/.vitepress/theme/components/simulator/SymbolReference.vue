<script setup lang="ts">
// The symbols the forecast strip reports — A, t_rot, t_rot_est, G, C — with what each MEANS.
//
// The strip itself carries the formulas now (#266) — this file's job is the meanings the
// strip has no room for, plus a POINTER back to the specification, which remains the one
// place both the symbols and their derivation are defined.
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

const rows = computed(() => [
  { symbol: 'A', def: t.value.symbols.defs.a },
  { symbol: 't_rot', def: t.value.symbols.defs.tRot },
  { symbol: 't_rot_est', def: t.value.symbols.defs.tRotEst },
  { symbol: 'G', def: t.value.symbols.defs.g },
  { symbol: 'C', def: t.value.symbols.defs.c },
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
