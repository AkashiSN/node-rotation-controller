<script setup lang="ts">
import { computed } from 'vue'
import { projectPolicy, applyPolicyEdit, type PolicyForm } from './policyYaml.ts'
import { useLabels } from './i18n.ts'

const props = defineProps<{ yaml: string }>()
const emit = defineEmits<{ 'update:yaml': [string] }>()
const t = useLabels()

const projection = computed(() => projectPolicy(props.yaml))
const form = computed(() => projection.value.form)
// A YAML the browser parser rejects DISABLES the form (the last good projection stays
// on screen, greyed) — but the raw YAML is still what the page sends to simulate(),
// so the error the user reads is the controller's own.
const broken = computed(() => projection.value.error !== undefined)

function edit(field: keyof PolicyForm, value: unknown) {
  emit('update:yaml', applyPolicyEdit(props.yaml, field, value))
}
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.policy }}</h3>
    <p class="sim-hint">{{ t.policyYamlHint }}</p>

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
               @change="edit('minRotationChances', Number(($event.target as HTMLInputElement).value))" />
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

    <textarea class="sim-yaml" spellcheck="false" :value="yaml"
              @input="emit('update:yaml', ($event.target as HTMLTextAreaElement).value)" />
  </section>
</template>
