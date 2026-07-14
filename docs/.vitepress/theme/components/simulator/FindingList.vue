<script setup lang="ts">
// The controller's own findings and diagnostics, given a shape.
//
// The MESSAGE is untouched and untranslated — it comes from internal/schedule and
// internal/sim, and re-wording it here would fork the message catalogue, which is the drift
// the whole simulator exists to prevent. What this component adds is everything AROUND the
// message: severity as a signal rather than a word, the code as a chip, the message as the
// body. A `warn` you can ignore and a `warn` that invalidates the run still read the same at
// a glance otherwise (#261).
import { type Finding } from './model.ts'
import { useLabels } from './i18n.ts'

defineProps<{ findings: Finding[] }>()
const t = useLabels()
</script>

<template>
  <ul class="sim-findings">
    <li v-for="(f, i) in findings" :key="i" :class="['sim-finding', `sim-finding-${f.severity}`]">
      <!-- The glyph is DECORATIVE only in the sense that it is not the message: it still
           carries the severity, so it carries an accessible name. Colour alone would not —
           severity has to survive both a screen reader and a colour-blind reader. -->
      <span class="sim-sev" role="img" :aria-label="t.severity[f.severity]">
        {{ f.severity === 'fatal' ? '✕' : '!' }}
      </span>
      <code class="sim-finding-code">{{ f.code }}</code>
      <span class="sim-finding-msg">{{ f.message }}</span>
    </li>
  </ul>
</template>
