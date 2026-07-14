<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { projectPolicy, applyPolicyEdit, type PolicyForm } from './policyYaml.ts'
import { useLabels } from './i18n.ts'

const props = defineProps<{ yaml: string }>()
const emit = defineEmits<{ 'update:yaml': [string] }>()
const t = useLabels()

const projection = computed(() => projectPolicy(props.yaml))
// A YAML the browser parser rejects DISABLES the form (the last good projection stays
// on screen, greyed) — but the raw YAML is still what the page sends to simulate(),
// so the error the user reads is the controller's own.
//
// `projectPolicy` itself returns a BLANK form on a parse error (see policyYaml.ts),
// so showing `projection.value.form` directly would snap every field to empty the
// instant the user types an invalid character mid-edit. Freeze on the last
// SUCCESSFUL projection instead — `broken` still disables the fieldset, so the
// frozen values read as "greyed out", not as live.
const lastGoodForm = ref<PolicyForm>(projection.value.form)
watch(projection, (p) => {
  if (!p.error) lastGoodForm.value = p.form
})
const form = computed(() => lastGoodForm.value)
const broken = computed(() => projection.value.error !== undefined)

function edit(field: keyof PolicyForm, value: unknown) {
  emit('update:yaml', applyPolicyEdit(props.yaml, field, value))
}

// The CRD restricts maintenanceWindows[].days to this exact enum
// (Mon;Tue;Wed;Thu;Fri;Sat;Sun — rotationpolicy_types.go), so the field is a
// fixed multi-select rather than free text.
const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'] as const

function dayChecked(day: string): boolean {
  // ParseWeekday is case-insensitive, so match that way — a source YAML with
  // `sat` must show its checkbox ticked.
  return form.value.days.some((d) => d.toLowerCase() === day.toLowerCase())
}

function toggleDay(day: string, checked: boolean) {
  const present = form.value.days
  const has = (d: string) => present.some((x) => x.toLowerCase() === d.toLowerCase())
  // Rebuild in canonical Mon..Sun order regardless of click order…
  const canonical = WEEKDAYS.filter((d) => (d === day ? checked : has(d)))
  // …but carry through any day the grid does not model (an unusual case or
  // spelling in the source YAML), so a toggle never silently drops it.
  const extras = present.filter(
    (x) => !WEEKDAYS.some((d) => d.toLowerCase() === x.toLowerCase()),
  )
  edit('days', [...canonical, ...extras])
}

// `timezone` is an IANA tz database name. Intl.supportedValuesOf('timeZone') is
// the browser's own fixed IANA set — the exact domain the field accepts — so we
// offer it as a dropdown. Guard the call (unavailable in older engines / SSR)
// and always keep the YAML's current value selectable even if the engine's list
// omits it (e.g. "UTC"): the YAML is authoritative and must not be rewritten by
// merely rendering the form.
const timezones = computed<string[]>(() => {
  let zones: string[] = []
  try {
    zones = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] })
      .supportedValuesOf?.('timeZone') ?? []
  } catch {
    zones = []
  }
  const current = form.value.timezone
  if (current && !zones.includes(current)) zones = [current, ...zones]
  return zones
})

function onMinRotationChancesChange(raw: string) {
  // `Number('')` is `0`, not NaN — clearing the field would otherwise write
  // `minRotationChances: 0` into the YAML, a value the CRD's `minimum: 1`
  // rejects. Leave the YAML untouched while the field is empty; the strict
  // decode error (if any) still comes from the wasm module, not this form.
  if (raw === '') return
  edit('minRotationChances', Number(raw))
}
</script>

<template>
  <section class="sim-block sim-policy">
    <h3>{{ t.policy }}</h3>
    <p class="sim-hint">{{ t.policyYamlHint }}</p>

    <div class="sim-policy-grid">
      <div>
        <!-- Eleven controls in one flat row read as a pile, not as a form. They are grouped
             the way the POLICY itself is — window / derivation / surge — so a reader who has
             met the spec's three layers recognises them here, and one who has not can still
             see that these fields answer three different questions (#261).

             Nested fieldsets: the OUTER one carries `disabled`, so a YAML the browser parser
             rejects greys out every group at once, while each inner one is a real group with
             a real <legend> rather than a heading that merely looks like one. -->
        <fieldset :disabled="broken" class="sim-policy-form">
          <fieldset class="sim-group">
            <legend>{{ t.policyGroups.window }}</legend>
            <div class="sim-form">
              <label>{{ t.timezone }}
                <select :value="form.timezone" @change="edit('timezone', ($event.target as HTMLSelectElement).value)">
                  <option v-for="tz in timezones" :key="tz" :value="tz">{{ tz }}</option>
                </select>
              </label>
              <div class="sim-days">
                <span>{{ t.days }}</span>
                <div class="sim-days-grid">
                  <label v-for="d in WEEKDAYS" :key="d">
                    <input type="checkbox" :checked="dayChecked(d)"
                           @change="toggleDay(d, ($event.target as HTMLInputElement).checked)" />
                    {{ d }}
                  </label>
                </div>
              </div>
              <label>{{ t.windowStart }}
                <input type="time" :value="form.start" @change="edit('start', ($event.target as HTMLInputElement).value)" />
              </label>
              <label>{{ t.windowEnd }}
                <input type="time" :value="form.end" @change="edit('end', ($event.target as HTMLInputElement).value)" />
              </label>
            </div>
          </fieldset>

          <fieldset class="sim-group">
            <legend>{{ t.policyGroups.derivation }}</legend>
            <div class="sim-form">
              <label>{{ t.minRotationChances }}
                <input type="number" min="1" :value="form.minRotationChances ?? 2"
                       @change="onMinRotationChancesChange(($event.target as HTMLInputElement).value)" />
              </label>
              <label>{{ t.ageThreshold }}
                <input :value="form.ageThreshold" @change="edit('ageThreshold', ($event.target as HTMLInputElement).value)" />
              </label>
            </div>
          </fieldset>

          <fieldset class="sim-group">
            <legend>{{ t.policyGroups.surge }}</legend>
            <div class="sim-form">
              <label>{{ t.provisioningEstimate }}
                <input :value="form.provisioningEstimate" @change="edit('provisioningEstimate', ($event.target as HTMLInputElement).value)" />
              </label>
              <label>{{ t.drainEstimate }}
                <input :value="form.drainEstimate" @change="edit('drainEstimate', ($event.target as HTMLInputElement).value)" />
              </label>
              <label>{{ t.readyTimeout }}
                <input :value="form.readyTimeout" @change="edit('readyTimeout', ($event.target as HTMLInputElement).value)" />
              </label>
              <label>{{ t.cooldownAfter }}
                <input :value="form.cooldownAfter" @change="edit('cooldownAfter', ($event.target as HTMLInputElement).value)" />
              </label>
              <label class="sim-check">
                <input type="checkbox" :checked="form.forcefulFallback"
                       @change="edit('forcefulFallback', ($event.target as HTMLInputElement).checked)" />
                {{ t.forcefulFallback }}
              </label>
            </div>
          </fieldset>
        </fieldset>

        <p v-if="form.extraWindows > 0" class="sim-hint">{{ t.extraWindows(form.extraWindows) }}</p>
      </div>

      <!-- wrap="off": a soft-wrapped YAML line renders as broken indentation, which
           reads as corrupt YAML. Long lines scroll inside the box instead. -->
      <textarea class="sim-yaml" spellcheck="false" wrap="off" :value="yaml"
                @input="emit('update:yaml', ($event.target as HTMLTextAreaElement).value)" />
    </div>
  </section>
</template>
