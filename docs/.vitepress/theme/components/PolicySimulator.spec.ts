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
