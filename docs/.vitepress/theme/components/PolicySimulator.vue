<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useData } from 'vitepress'
import {
  COVERAGE_CHOICES, DEFAULT_COVERAGE, DEFAULT_FLEET, DEFAULT_ENV,
  buildRequest, defaultPolicyYaml, horizonForCoverage, parseGoDuration,
  type Env, type Fleet, type Horizon, type SimResponse,
} from './simulator/model.ts'
import { projectPolicy } from './simulator/policyYaml.ts'
import { isValidTimezone } from './simulator/timeutil.ts'
import { useWasm } from './simulator/useWasm.ts'
import { useLabels } from './simulator/i18n.ts'
import {
  SHARE_PARAM, decodeState, encodeState, shareSupported, ShareTooLargeError, type ShareState,
} from './simulator/share.ts'
import FindingList from './simulator/FindingList.vue'
import PolicyInput from './simulator/PolicyInput.vue'
import FleetInput from './simulator/FleetInput.vue'
import EnvInput from './simulator/EnvInput.vue'
import ResultHeader from './simulator/ResultHeader.vue'
import TimelineChart from './simulator/TimelineChart.vue'
import WindowCalendar from './simulator/WindowCalendar.vue'
import ScaleRuler from './simulator/ScaleRuler.vue'
import DiagnosticsPanel from './simulator/DiagnosticsPanel.vue'

const t = useLabels()
const { loading, ready, error: loadError, load, simulate } = useWasm()

// The SEED only, read once: the locale picks which manifest the page opens on (the Japanese
// page opens on Asia/Tokyo), and from that instant the YAML is authoritative. A later locale
// switch is a full page navigation, so there is nothing to react to — and reacting would be
// wrong anyway: it would throw away the visitor's own edits.
const { lang } = useData()
const policyYAML = ref(defaultPolicyYaml(lang.value))
const fleet = ref<Fleet>(structuredClone(DEFAULT_FLEET))
const env = ref<Env>({ ...DEFAULT_ENV })

// The horizon's pin state machine, made explicit — the old single flag only covered half of
// it, so a coverage choice and a hand-typed instant could disagree with no way back:
//
//   - choosing a coverage multiplier UNPINS: the horizon tracks the fleet again, at that
//     multiple, so adding a node never leaves its deadline off the right edge;
//   - editing an instant by hand PINS: fleet and policy edits stop moving the horizon, and
//     the coverage buttons show as inactive rather than lying about the current span.
const coverage = ref<number>(DEFAULT_COVERAGE)
const horizonPinned = ref(false)
const horizon = ref<Horizon>(horizonForCoverage(DEFAULT_FLEET, DEFAULT_COVERAGE))

watch([fleet, coverage], () => {
  if (!horizonPinned.value) horizon.value = horizonForCoverage(fleet.value, coverage.value)
}, { deep: true })

function chooseCoverage(n: number) {
  horizonPinned.value = false
  coverage.value = n
  horizon.value = horizonForCoverage(fleet.value, n)
}
function pinHorizon(patch: Partial<Horizon>) {
  horizonPinned.value = true
  horizon.value = { ...horizon.value, ...patch }
}

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

// A link the page could not read is reported, not thrown: someone whose chat client ate the
// tail of a URL must still land on a usable simulator.
const shareError = ref('')
const shareNote = ref('')
const canShare = shareSupported()
let noteTimer: ReturnType<typeof setTimeout> | undefined

function currentState(): ShareState {
  return {
    policy: policyYAML.value,
    fleet: fleet.value,
    env: env.value,
    horizon: horizon.value,
  }
}

async function copyShareLink() {
  // Building the link and copying it are split into two tries on purpose: encodeState() can
  // reject on its own — the button is disabled when the Compression Streams API is missing,
  // but a stale render or a runtime that changes mid-session must still land on a message,
  // not an unhandled rejection; and a large-but-valid state (a big fleet or policy YAML) can
  // encode past the ceiling decodeState() itself enforces, which encodeState() now refuses to
  // produce at all — and when EITHER fails, replaceState below must never run, so the link is
  // NOT in the address bar. Telling the user to copy it from there would be false.
  let url: URL
  try {
    url = new URL(window.location.href)
    url.searchParams.set(SHARE_PARAM, await encodeState(currentState()))
  } catch (err) {
    // ShareTooLargeError gets its own, actionable message — "too big" is something a visitor
    // can fix by trimming the fleet or the YAML; the generic buildFailed message is not.
    note(err instanceof ShareTooLargeError ? t.value.share.tooBig : t.value.share.buildFailed)
    return
  }
  try {
    // replaceState, not pushState: Back must still mean "the page I came from", not "my
    // previous edit". It runs inside this second try too: only once it has run is "the link
    // is in the address bar" (copyFailed's claim) actually true.
    window.history.replaceState({}, '', url)
    await navigator.clipboard.writeText(url.toString())
    note(t.value.share.copied)
  } catch {
    note(t.value.share.copyFailed)
  }
}

function note(message: string) {
  shareNote.value = message
  clearTimeout(noteTimer)
  noteTimer = setTimeout(() => (shareNote.value = ''), 4000)
}

onMounted(async () => {
  const value = new URLSearchParams(window.location.search).get(SHARE_PARAM)
  if (value) {
    const got = await decodeState(value)
    if ('error' in got) {
      // 'version' gets its own message: a link from a newer simulator is not "damaged", and
      // conflating the two would make the version→message distinction decodeState() carries
      // dead code.
      shareError.value = got.error.code === 'version' ? t.value.share.badLinkVersion : t.value.share.badLink
    } else {
      policyYAML.value = got.state.policy
      fleet.value = got.state.fleet
      env.value = got.state.env
      // PINNED: the sharer chose this span, so the fleet watcher must not move it.
      horizonPinned.value = true
      horizon.value = got.state.horizon
    }
  }
  await load()
  run()
})
watch([policyYAML, fleet, env, horizon], schedule, { deep: true })

const result = computed(() => response.value.result)
const diagnostics = computed(() => response.value.diagnostics ?? [])
// A PARTIAL run can return a result and NO events at all (e.g. env.provisioning above
// readyTimeout: every rotation times out and the sim gives up). The page would then show a
// forecast strip over an empty timeline — reading as "nothing ever rotates" — while the only
// explanation sat at the bottom of the page. Surface the fatal diagnostics ABOVE the chart,
// in the controller's own words, verbatim.
const fatals = computed(() => diagnostics.value.filter(d => d.severity === 'fatal'))

const policyForm = computed(() => projectPolicy(policyYAML.value).form)

// The DISPLAY TIMEZONE is the policy's — maintenanceWindows[0].timezone — never the
// browser's, and it is always shown as a label. A zone this runtime does not know degrades
// to UTC rather than throwing inside a render.
const timezone = computed(() => {
  const tz = policyForm.value.timezone
  return tz && isValidTimezone(tz) ? tz : 'UTC'
})

// The env fields are BLANK by default, and blank means "the policy's own estimate". The
// ruler needs the resolved value, and resolving it here — rather than hydrating the form —
// is what keeps a later policy edit still able to move it.
const provisioningMs = computed(() =>
  parseGoDuration(env.value.provisioning || result.value?.provisioningEstimate || '') ?? 0)
const drainMs = computed(() =>
  parseGoDuration(env.value.drain || result.value?.drainEstimate || '') ?? 0)

const simulatedThroughMs = computed(() => {
  const at = new Date(response.value.simulatedThrough ?? horizon.value.end).getTime()
  return Number.isFinite(at) ? at : new Date(horizon.value.end).getTime()
})
const horizonStartMs = computed(() => new Date(horizon.value.start).getTime())
</script>

<template>
  <div class="policy-simulator">
    <!-- Hoisted OUTSIDE the loading/loadError/else split: this is a statement about the
         URL, not about the wasm module, and a corrupt link plus a failed wasm load can
         both be true at once. A visitor must be told about the unreadable link even
         while staring at the load-failed banner. -->
    <p v-if="shareError" class="sim-banner sim-banner-warn">{{ shareError }}</p>

    <p v-if="loading">{{ t.loading }}</p>
    <div v-else-if="loadError" class="sim-banner sim-banner-fatal">
      {{ t.loadFailed }} <code>{{ loadError }}</code>
      <button type="button" @click="load().then(run)">{{ t.retry }}</button>
    </div>

    <template v-else>
      <!-- The controller's own error, verbatim: an unparseable policy, or one the
           cluster would reject. The page still renders, and says why. -->
      <p v-if="response.error" class="sim-banner sim-banner-fatal"><code>{{ response.error }}</code></p>

      <ResultHeader v-if="result" :result="result" />

      <!-- Why the timeline below may be empty or short — read before the picture, not
           after it. The messages are the controller's own, rendered verbatim. -->
      <div v-if="response.partial" class="sim-banner sim-banner-fatal">
        <p>{{ t.partial }}</p>
        <FindingList v-if="fatals.length" :findings="fatals" />
      </div>

      <!-- The share control is about the LINK; everything above is about the RUN. They sat
           in one undifferentiated row, so "Copied" read as part of the button and a warning
           above it read as part of the share block (#261). The control gets its own region,
           and its note its own line inside that region — a status about the link can then
           never be mistaken for a statement about the simulation.

           Not gated on `result`: a policy the controller REJECTS is exactly the run someone
           wants to share to ask "why won't this validate?" — the link carries the QUESTION
           (policy/fleet/env/horizon), not the answer, so it is shareable whether or not a
           result exists. This block sits inside `template v-else`, so wasm has already
           loaded by the time it renders. -->
      <section class="sim-share" :aria-label="t.share.copy">
        <button type="button" class="sim-btn" :disabled="!canShare"
                :title="canShare ? undefined : t.share.unsupported" @click="copyShareLink">
          {{ t.share.copy }}
        </button>
        <p v-if="shareNote" class="sim-share-note" role="status">{{ shareNote }}</p>
        <p v-else-if="!canShare" class="sim-share-note">{{ t.share.unsupported }}</p>
      </section>

      <TimelineChart v-if="result" :response="response" :horizon="horizon" :fleet="fleet"
                     :timezone="timezone" />

      <WindowCalendar v-if="result" :windows="(response.windows ?? []).map(w => ({
                        startMs: new Date(w.start).getTime(),
                        endMs: new Date(w.end).getTime(),
                        startClipped: w.startClipped === true,
                        endClipped: w.endClipped === true,
                      }))"
                      :span-start-ms="horizonStartMs" :span-end-ms="simulatedThroughMs"
                      :timezone="timezone" :partial="response.partial === true" />

      <ScaleRuler v-if="result" :result="result" :fleet="fleet" :response="response"
                  :provisioning-ms="provisioningMs" :drain-ms="drainMs"
                  :ready-timeout="policyForm.readyTimeout" :cooldown-after="policyForm.cooldownAfter" />

      <!-- PolicyInput stays OUTSIDE the .sim-inputs grid: it owns a full-width row,
           while the grid's explicit tracks (Fleet widest — it holds full RFC 3339
           instants) are sized for exactly these three blocks. -->
      <PolicyInput v-model:yaml="policyYAML" />

      <div class="sim-inputs">
        <FleetInput v-model:fleet="fleet" />
        <EnvInput v-model:env="env"
                  :provisioning-estimate="result?.provisioningEstimate ?? ''"
                  :drain-estimate="result?.drainEstimate ?? ''" />
        <section class="sim-block">
          <h3>{{ t.horizon }}</h3>
          <p class="sim-hint">{{ t.chart.coverageHint }}</p>
          <div class="sim-controls">
            <span class="sim-ruler-name">{{ t.chart.coverage }}</span>
            <button v-for="n in COVERAGE_CHOICES" :key="n" type="button"
                    :class="['sim-btn', { 'sim-btn-on': !horizonPinned && coverage === n }]"
                    :aria-pressed="!horizonPinned && coverage === n"
                    @click="chooseCoverage(n)">{{ t.chart.coverageOption(n) }}</button>
          </div>
          <p class="sim-hint"><code>{{ horizon.end }}</code></p>
          <p v-if="horizonPinned" class="sim-hint">{{ t.chart.pinned }}</p>

          <!-- The raw instants stay, behind a details, as the escape hatch for reproducing
               an exact span. Editing one PINS the horizon. -->
          <details class="sim-advanced">
            <summary>{{ t.chart.advanced }}</summary>
            <fieldset class="sim-form">
              <label class="sim-field-wide">{{ t.start }}
                <input :value="horizon.start"
                       @change="pinHorizon({ start: ($event.target as HTMLInputElement).value })" />
              </label>
              <label class="sim-field-wide">{{ t.end }}
                <input :value="horizon.end"
                       @change="pinHorizon({ end: ($event.target as HTMLInputElement).value })" />
              </label>
            </fieldset>
          </details>
        </section>
      </div>

      <DiagnosticsPanel :diagnostics="diagnostics" :partial="response.partial === true" />
    </template>
  </div>
</template>
