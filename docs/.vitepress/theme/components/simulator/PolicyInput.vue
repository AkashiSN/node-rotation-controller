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
        <fieldset :disabled="broken" class="sim-form">
          <label>{{ t.timezone }}
            <input :value="form.timezone" @change="edit('timezone', ($event.target as HTMLInputElement).value)" />
          </label>
          <label>{{ t.days }}
            <input :value="form.days.join(',')"
                   @change="edit('days', ($event.target as HTMLInputElement).value.split(',').map(s => s.trim()).filter(Boolean))" />
          </label>
          <label>{{ t.windowStart }}
            <input :value="form.start" @change="edit('start', ($event.target as HTMLInputElement).value)" />
          </label>
          <label>{{ t.windowEnd }}
            <input :value="form.end" @change="edit('end', ($event.target as HTMLInputElement).value)" />
          </label>
          <label>{{ t.minRotationChances }}
            <input type="number" min="1" :value="form.minRotationChances ?? 2"
                   @change="onMinRotationChancesChange(($event.target as HTMLInputElement).value)" />
          </label>
          <label>{{ t.ageThreshold }}
            <input :value="form.ageThreshold" @change="edit('ageThreshold', ($event.target as HTMLInputElement).value)" />
          </label>
          <label>provisioningEstimate
            <input :value="form.provisioningEstimate" @change="edit('provisioningEstimate', ($event.target as HTMLInputElement).value)" />
          </label>
          <label>drainEstimate
            <input :value="form.drainEstimate" @change="edit('drainEstimate', ($event.target as HTMLInputElement).value)" />
          </label>
          <label>{{ t.cooldownAfter }}
            <input :value="form.cooldownAfter" @change="edit('cooldownAfter', ($event.target as HTMLInputElement).value)" />
          </label>
          <label>{{ t.readyTimeout }}
            <input :value="form.readyTimeout" @change="edit('readyTimeout', ($event.target as HTMLInputElement).value)" />
          </label>
          <label class="sim-check">
            <input type="checkbox" :checked="form.forcefulFallback"
                   @change="edit('forcefulFallback', ($event.target as HTMLInputElement).checked)" />
            {{ t.forcefulFallback }}
          </label>
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
