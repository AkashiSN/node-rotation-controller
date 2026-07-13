<script setup lang="ts">
import { type Env } from './model.ts'
import { useLabels } from './i18n.ts'

// provisioningEstimate / drainEstimate are the RESOLVED policy estimates, read off
// result.* — i.e. what Go actually resolved, not what the YAML happens to say.
const props = defineProps<{ env: Env; provisioningEstimate: string; drainEstimate: string }>()
const emit = defineEmits<{ 'update:env': [Env] }>()
const t = useLabels()

// A blank field STAYS blank in the model. It is never hydrated to the estimate shown
// beside it: that would freeze Env at the estimate the policy had at that moment, and
// a later policy edit would silently stop moving the timeline — exactly the default
// semantics simapi.Env documents ("empty = the corresponding resolved policy estimate").
function patch(part: Partial<Env>) {
  const next: Env = { ...props.env, ...part }
  if (!next.provisioning) delete next.provisioning
  if (!next.drain) delete next.drain
  emit('update:env', next)
}
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.env }}</h3>
    <p class="sim-hint">{{ t.envHint }}</p>

    <fieldset class="sim-form">
      <!-- sim-field-wide: each field takes a full row so the `blank = policy
           estimate: …` hint under it never wraps mid-value. -->
      <label class="sim-field-wide">{{ t.provisioning }}
        <input :value="env.provisioning ?? ''" :placeholder="provisioningEstimate"
               @change="patch({ provisioning: ($event.target as HTMLInputElement).value })" />
        <small>{{ t.envBlank(provisioningEstimate) }}</small>
      </label>
      <label class="sim-field-wide">{{ t.drain }}
        <input :value="env.drain ?? ''" :placeholder="drainEstimate"
               @change="patch({ drain: ($event.target as HTMLInputElement).value })" />
        <small>{{ t.envBlank(drainEstimate) }}</small>
      </label>
    </fieldset>
  </section>
</template>
