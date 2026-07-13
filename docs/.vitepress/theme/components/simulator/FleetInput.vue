<script setup lang="ts">
import { ref, watch } from 'vue'
import { generateNodes, type Fleet } from './model.ts'
import { useLabels } from './i18n.ts'

const props = defineProps<{ fleet: Fleet }>()
const emit = defineEmits<{ 'update:fleet': [Fleet] }>()
const t = useLabels()

const count = ref(props.fleet.nodes.length)
const first = ref(props.fleet.nodes[0]?.createdAt ?? '2026-01-01T00:00:00Z')
// `spread` has no representation in a `Fleet` (it only drives the interval `generateNodes`
// spaces new nodes by) — it stays a purely local generator input and is never resynced.
const spread = ref('168h')

// The orchestrator owns `fleet` and can replace it wholesale (e.g. loading a preset),
// not just via this component's own patch()/regenerate() emits. Without this watcher
// `count`/`first` would go stale, and the next Generate click would stomp the
// orchestrator's fleet using the old seed values. Assigning the refs here never emits
// by itself — only the explicit regenerate() click does — so resyncing to a value that
// already matches (including the case where our own emit round-trips back through
// `props.fleet`) is a no-op, not a feedback loop.
watch(
  () => props.fleet,
  (f) => {
    count.value = f.nodes.length
    first.value = f.nodes[0]?.createdAt ?? '2026-01-01T00:00:00Z'
  },
)

function regenerate() {
  // `min="1"` on the <input type="number"> below is advisory only — it does not
  // stop a user from typing "0" (or clearing the field, which v-model.number
  // coerces to NaN-ish 0). generateNodes(0, ...) returns [], and an empty fleet
  // is a state defaultHorizon() already had to be hardened against; clamp here
  // so the page can never produce one in the first place.
  emit('update:fleet', { ...props.fleet, nodes: generateNodes(Math.max(1, count.value), first.value, spread.value) })
}
function patch(part: Partial<Fleet>) {
  emit('update:fleet', { ...props.fleet, ...part })
}
function patchNode(i: number, part: Partial<Fleet['nodes'][number]>) {
  const nodes = props.fleet.nodes.map((n, j) => (j === i ? { ...n, ...part } : n))
  emit('update:fleet', { ...props.fleet, nodes })
}
</script>

<template>
  <section class="sim-block">
    <h3>{{ t.fleet }}</h3>

    <fieldset class="sim-form">
      <label>{{ t.expireAfter }}
        <input :value="fleet.expireAfter" @change="patch({ expireAfter: ($event.target as HTMLInputElement).value })" />
      </label>
      <label>{{ t.tgp }}
        <input :value="fleet.terminationGracePeriod ?? ''"
               @change="patch({ terminationGracePeriod: ($event.target as HTMLInputElement).value || undefined })" />
      </label>
    </fieldset>

    <fieldset class="sim-form">
      <label>{{ t.nodeCount }}<input type="number" min="1" max="50" v-model.number="count" /></label>
      <label>{{ t.firstCreatedAt }}<input v-model="first" /></label>
      <label>{{ t.spread }}<input v-model="spread" /></label>
      <button type="button" @click="regenerate">{{ t.generate }}</button>
    </fieldset>

    <table class="sim-table">
      <thead>
        <tr><th>{{ t.nodeName }}</th><th>{{ t.createdAt }}</th><th>expireAfter</th><th>tGP</th></tr>
      </thead>
      <tbody>
        <tr v-for="(n, i) in fleet.nodes" :key="i">
          <td><input :value="n.name" @change="patchNode(i, { name: ($event.target as HTMLInputElement).value })" /></td>
          <td><input :value="n.createdAt" @change="patchNode(i, { createdAt: ($event.target as HTMLInputElement).value })" /></td>
          <td><input :value="n.expireAfter ?? ''" :placeholder="fleet.expireAfter"
                     @change="patchNode(i, { expireAfter: ($event.target as HTMLInputElement).value || undefined })" /></td>
          <td><input :value="n.terminationGracePeriod ?? ''" :placeholder="fleet.terminationGracePeriod ?? ''"
                     @change="patchNode(i, { terminationGracePeriod: ($event.target as HTMLInputElement).value || undefined })" /></td>
        </tr>
      </tbody>
    </table>
  </section>
</template>
