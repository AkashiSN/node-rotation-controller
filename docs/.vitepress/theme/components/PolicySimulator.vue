<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import {
  DEFAULT_POLICY_YAML, DEFAULT_FLEET, DEFAULT_ENV, defaultHorizon, buildRequest,
  type Env, type Fleet, type Horizon, type SimResponse,
} from './simulator/model.ts'
import { useWasm } from './simulator/useWasm.ts'
import { useLabels } from './simulator/i18n.ts'
import PolicyInput from './simulator/PolicyInput.vue'
import FleetInput from './simulator/FleetInput.vue'
import EnvInput from './simulator/EnvInput.vue'
import ResultHeader from './simulator/ResultHeader.vue'
import TimelineChart from './simulator/TimelineChart.vue'
import DiagnosticsPanel from './simulator/DiagnosticsPanel.vue'

const t = useLabels()
const { loading, ready, error: loadError, load, simulate } = useWasm()

const policyYAML = ref(DEFAULT_POLICY_YAML)
const fleet = ref<Fleet>(structuredClone(DEFAULT_FLEET))
const env = ref<Env>({ ...DEFAULT_ENV })
const horizon = ref<Horizon>(defaultHorizon(DEFAULT_FLEET))
// Once the user edits either end, the horizon is theirs: policy and fleet edits stop
// moving it. Until then it tracks the fleet, so adding a node never leaves its
// deadline off the right edge.
const horizonPinned = ref(false)

watch(fleet, f => { if (!horizonPinned.value) horizon.value = defaultHorizon(f) }, { deep: true })

const response = ref<SimResponse>({})
let timer: ReturnType<typeof setTimeout> | undefined

function run() {
  if (!ready.value) return
  response.value = simulate(policyYAML.value, buildRequest(fleet.value, env.value, horizon.value))
}
function schedule() {
  clearTimeout(timer)
  timer = setTimeout(run, 200)
}

onMounted(async () => { await load(); run() })
watch([policyYAML, fleet, env, horizon], schedule, { deep: true })

const result = computed(() => response.value.result)
const events = computed(() => response.value.events ?? [])
const diagnostics = computed(() => response.value.diagnostics ?? [])
</script>

<template>
  <div class="policy-simulator">
    <p v-if="loading">{{ t.loading }}</p>
    <div v-else-if="loadError" class="sim-fatal sim-banner">
      {{ t.loadFailed }} <code>{{ loadError }}</code>
      <button type="button" @click="load().then(run)">{{ t.retry }}</button>
    </div>

    <template v-else>
      <!-- The controller's own error, verbatim: an unparseable policy, or one the
           cluster would reject. The page still renders, and says why. -->
      <p v-if="response.error" class="sim-fatal sim-banner"><code>{{ response.error }}</code></p>

      <ResultHeader v-if="result" :result="result" />
      <TimelineChart v-if="result" :events="events" :horizon="horizon" :fleet="fleet" />

      <div class="sim-inputs">
        <PolicyInput v-model:yaml="policyYAML" />
        <FleetInput v-model:fleet="fleet" />
        <div>
          <EnvInput v-model:env="env"
                    :provisioning-estimate="result?.provisioningEstimate ?? ''"
                    :drain-estimate="result?.drainEstimate ?? ''" />
          <section class="sim-block">
            <h3>{{ t.horizon }}</h3>
            <fieldset class="sim-form">
              <label>{{ t.start }}
                <input :value="horizon.start"
                       @change="horizonPinned = true; horizon = { ...horizon, start: ($event.target as HTMLInputElement).value }" />
              </label>
              <label>{{ t.end }}
                <input :value="horizon.end"
                       @change="horizonPinned = true; horizon = { ...horizon, end: ($event.target as HTMLInputElement).value }" />
              </label>
            </fieldset>
          </section>
        </div>
      </div>

      <DiagnosticsPanel :diagnostics="diagnostics" :partial="response.partial === true" />
    </template>
  </div>
</template>
