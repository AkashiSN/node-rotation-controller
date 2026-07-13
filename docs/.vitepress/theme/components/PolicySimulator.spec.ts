// vitest run — the page shell: the horizon's PIN STATE MACHINE, and the display timezone.
//
// The old page carried a single `horizonPinned` flag that covered only half the machine, so
// a coverage choice and a hand-typed instant could disagree with no way back. These pin the
// whole of it.
import { mount } from '@vue/test-utils'
import { describe, expect, test, vi } from 'vitest'
import { ref } from 'vue'

vi.mock('vitepress', () => ({
  useData: () => ({ lang: ref('en-US') }),
  withBase: (p: string) => p,
}))

// The wasm module is 3.4 MB and is not the unit under test: what matters here is which
// horizon the page ASKS it for.
const calls: { policy: string; request: any }[] = []
vi.mock('./simulator/useWasm.ts', () => ({
  useWasm: () => ({
    loading: ref(false),
    ready: ref(true),
    error: ref(''),
    load: async () => {},
    simulate: (policy: string, request: any) => {
      calls.push({ policy, request })
      return { result: RESULT, events: [], generations: [], rotations: [], windows: [], partial: false }
    },
  }),
}))

const RESULT = {
  ageThreshold: '286h43m0s', tRot: '1h17m0s', tRotEstimate: '15m0s',
  drainEstimate: '10m0s', provisioningEstimate: '5m0s', g: 2, c: 10,
}

import { DEFAULT_POLICY_YAML } from './simulator/model.ts'
import { decodeState, encodeState } from './simulator/share.ts'
import PolicySimulator from './PolicySimulator.vue'

const mountPage = () => mount(PolicySimulator)
const coverageButton = (w: ReturnType<typeof mountPage>, label: string) =>
  w.findAll('button').find(b => b.text() === label)!

describe('the horizon control', () => {
  test('it is a LIFETIME COVERAGE multiplier, and 2x is the default a visitor lands on', () => {
    const w = mountPage()
    // The default fleet is 3 nodes spread over 168h with expireAfter 480h, so 2x reaches the
    // last node's SECOND deadline — a second generation, and any breach, on screen without
    // the visitor doing anything.
    const horizon = (w.vm as any).horizon
    const start = new Date(horizon.start).getTime()
    const end = new Date(horizon.end).getTime()
    expect(end - start).toBe((168 + 2 * 480) * 3_600_000)
    expect(coverageButton(w, '2x').attributes('aria-pressed')).toBe('true')
  })

  test('choosing a coverage multiplier moves the horizon and keeps it tracking the fleet', async () => {
    const w = mountPage()
    await coverageButton(w, '3x').trigger('click')
    const horizon = (w.vm as any).horizon
    const span = new Date(horizon.end).getTime() - new Date(horizon.start).getTime()
    expect(span).toBe((168 + 3 * 480) * 3_600_000)
    expect(coverageButton(w, '3x').attributes('aria-pressed')).toBe('true')
    expect(coverageButton(w, '2x').attributes('aria-pressed')).toBe('false')
  })

  test('editing an instant by hand PINS the horizon; a coverage button unpins it', async () => {
    const w = mountPage()
    // The raw instants live behind the <details> escape hatch: [0] is start, [1] is end.
    const end = w.findAll('.sim-advanced input')[1]

    await end.setValue('2026-06-01T00:00:00Z')
    await end.trigger('change')
    expect((w.vm as any).horizonPinned).toBe(true)
    expect((w.vm as any).horizon.end).toBe('2026-06-01T00:00:00Z')
    // The coverage buttons must not claim to describe a span they no longer set.
    expect(coverageButton(w, '2x').attributes('aria-pressed')).toBe('false')

    await coverageButton(w, '1x').trigger('click')
    expect((w.vm as any).horizonPinned).toBe(false)
    expect((w.vm as any).horizon.end).not.toBe('2026-06-01T00:00:00Z')
  })
})

describe('the display timezone is the POLICY\'s, never the browser\'s', () => {
  test('it comes from maintenanceWindows[0].timezone', async () => {
    const w = mountPage()
    expect((w.vm as any).timezone).toBe('UTC')

    ;(w.vm as any).policyYAML = (w.vm as any).policyYAML.replace('timezone: UTC', 'timezone: Asia/Tokyo')
    await w.vm.$nextTick()
    expect((w.vm as any).timezone).toBe('Asia/Tokyo')
  })

  test('a timezone this runtime does not know degrades to UTC instead of throwing mid-render', async () => {
    const w = mountPage()
    ;(w.vm as any).policyYAML = (w.vm as any).policyYAML.replace('timezone: UTC', 'timezone: Mars/Olympus')
    await w.vm.$nextTick()
    expect((w.vm as any).timezone).toBe('UTC')
    // And the page still renders: the policy's own error is the Go decoder's to report.
    expect(w.find('.policy-simulator').exists()).toBe(true)
  })
})

describe('a run is shareable as a link', () => {
  // The page reads window.location on mount. happy-dom gives us a real one to set.
  const visit = (search: string) => {
    window.history.replaceState({}, '', `/ja/simulator${search}`)
  }
  const flush = async () => { await new Promise(r => setTimeout(r, 0)); await new Promise(r => setTimeout(r, 0)) }
  const shareButton = (w: ReturnType<typeof mountPage>) =>
    w.findAll('button').find(b => b.text().includes('Copy share link'))!

  test('a link seeds the inputs, and its horizon arrives PINNED', async () => {
    // The sharer chose that span explicitly; a coverage button must not silently move it.
    const state = {
      policy: 'apiVersion: noderotation.io/v1alpha1\nkind: RotationPolicy\n',
      fleet: { expireAfter: '720h', nodes: [{ name: 'shared-1', createdAt: '2026-03-01T00:00:00Z' }] },
      env: { provisioning: '7m', drain: '' },
      horizon: { start: '2026-03-01T00:00:00Z', end: '2026-04-01T00:00:00Z' },
    }
    visit(`?s=${await encodeState(state)}`)
    const w = mountPage()
    await flush()
    const vm = w.vm as any
    expect(vm.policyYAML).toBe(state.policy)
    expect(vm.fleet.nodes[0].name).toBe('shared-1')
    expect(vm.env.provisioning).toBe('7m')
    expect(vm.horizon).toEqual(state.horizon)
    expect(vm.horizonPinned).toBe(true)
  })

  test('a damaged link is NOT a broken page: the defaults render, and the page says why', async () => {
    visit('?s=!!!truncated-by-a-chat-client')
    const w = mountPage()
    await flush()
    const vm = w.vm as any
    expect(vm.policyYAML).toBe(DEFAULT_POLICY_YAML)     // the defaults, not an empty page
    expect(vm.shareError).toBeTruthy()
    expect(w.text()).toContain('Could not read the shared link')
  })

  test('copying writes the link to the clipboard AND the address bar, without a history entry', async () => {
    visit('')
    const written: string[] = []
    // navigator.clipboard is a getter-only accessor (real browsers included): a plain
    // Object.assign throws "has only a getter" in strict mode, so redefine the property.
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: async (v: string) => { written.push(v) } },
    })
    const w = mountPage()
    await flush()
    await shareButton(w).trigger('click')
    await flush()

    expect(written).toHaveLength(1)
    expect(written[0]).toContain('/ja/simulator?s=')
    expect(window.location.search).toBe(new URL(written[0]).search)  // replaceState, same URL
    // And the link round trips back to what the page is showing.
    const value = new URL(written[0]).searchParams.get('s')!
    const back = await decodeState(value)
    expect('state' in back && back.state.policy).toBe(DEFAULT_POLICY_YAML)
  })
})
