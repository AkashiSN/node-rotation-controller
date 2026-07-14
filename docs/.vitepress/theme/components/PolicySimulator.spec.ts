// vitest run — the page shell: the horizon's PIN STATE MACHINE, and the display timezone.
//
// The old page carried a single `horizonPinned` flag that covered only half the machine, so
// a coverage choice and a hand-typed instant could disagree with no way back. These pin the
// whole of it.
import { mount } from '@vue/test-utils'
import { randomBytes } from 'node:crypto'
import { describe, expect, test, vi } from 'vitest'
import { ref } from 'vue'

vi.mock('vitepress', () => ({
  useData: () => ({ lang: ref('en-US') }),
  withBase: (p: string) => p,
}))

// The wasm module is 3.4 MB and is not the unit under test: what matters here is which
// horizon the page ASKS it for.
//
// useWasm() runs once per mount, synchronously, during that component's own setup — so
// give each call a PRIVATE calls array, captured into `latestCalls`, rather than one
// array shared by every mount in the file. A shared array is not just cosmetically
// noisy: a PRIOR test's component can still have a debounced schedule() timer pending
// on the real clock (nothing in the page cancels it on unmount), and that stray call
// can land, in real time, in the middle of a LATER test's own mount+flush — at exactly
// the index that test would otherwise mistake for its own first call.
let latestCalls: { policy: string; request: any }[] = []
vi.mock('./simulator/useWasm.ts', () => ({
  useWasm: () => {
    const calls: { policy: string; request: any }[] = []
    latestCalls = calls
    return {
      loading: ref(false),
      ready: ref(true),
      error: ref(''),
      load: async () => {},
      simulate: (policy: string, request: any) => {
        calls.push({ policy, request })
        // A fleet whose first node is named this way stands in for a policy the controller
        // REJECTS: a response with no `result` at all, the same shape simulate() returns for
        // input it cannot run. One test below needs exactly this to exist without reaching
        // into the wasm module for real.
        if (request.fleet.nodes[0]?.name === 'reject-me') {
          return { error: 'the controller rejected this policy', events: [], generations: [], rotations: [], windows: [], partial: false }
        }
        return { result: RESULT, events: [], generations: [], rotations: [], windows: [], partial: false }
      },
    }
  },
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
    // mountPage() just ran this component's setup synchronously, so latestCalls is now
    // THIS mount's own private array — not the one any earlier test's component still
    // writes into.
    const myCalls = latestCalls
    // onMounted awaits decodeState(), which pipes through a real DecompressionStream — the
    // number of micro/macrotask turns that takes is not fixed (it depends on the platform's
    // stream implementation and the event loop's current load), so a fixed-tick flush() races
    // it and occasionally samples state before the decode has landed. Poll for the CONDITION
    // (the first simulate() call having happened) instead of a tick count.
    await vi.waitUntil(() => myCalls.length > 0)
    const vm = w.vm as any
    expect(vm.policyYAML).toBe(state.policy)
    expect(vm.fleet.nodes[0].name).toBe('shared-1')
    expect(vm.env.provisioning).toBe('7m')
    expect(vm.horizon).toEqual(state.horizon)
    expect(vm.horizonPinned).toBe(true)

    // The decode must land BEFORE the first simulate() call, not just before the settled
    // state: if the decode ever slipped to after run(), the page would flash the DEFAULT
    // policy and fleet to the wasm module (and, briefly, on screen) before jumping to the
    // shared state — the very flash the onMounted comment says must not happen.
    const firstCall = myCalls[0]
    expect(firstCall.policy).toBe(state.policy)
    expect(firstCall.request.fleet.nodes[0].name).toBe('shared-1')
  })

  test('a damaged link is NOT a broken page: the defaults render, and the page says why', async () => {
    visit('?s=!!!truncated-by-a-chat-client')
    const w = mountPage()
    // Same non-fixed decode latency as above applies to the failure path too: poll for the
    // rendered banner (which only appears once shareError is set AND Vue has flushed the
    // DOM) rather than a tick count that can undershoot on a slow run.
    await vi.waitUntil(() => w.text().includes('Could not read the shared link'))
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
    const myCalls = latestCalls
    // No ?s= param here, so decodeState() never runs, but onMounted still awaits load()
    // before its first run() — poll for that first simulate() call to know the mount has
    // settled before reading history.length as the "before" baseline.
    await vi.waitUntil(() => myCalls.length > 0)
    // The constraint under test is replaceState, never pushState — and asserting only the
    // URL's final shape cannot tell the two apart, since both would leave the same
    // window.location.search behind. History LENGTH is what distinguishes them.
    const historyLengthBefore = window.history.length
    await shareButton(w).trigger('click')
    // copyShareLink() is itself async (encodeState() awaits a CompressionStream pipeline
    // before clipboard.writeText() resolves); poll for the write landing instead of a tick
    // count that can undershoot before the promise chain settles.
    await vi.waitUntil(() => written.length > 0)

    expect(written).toHaveLength(1)
    expect(written[0]).toContain('/ja/simulator?s=')
    expect(window.location.search).toBe(new URL(written[0]).search)  // replaceState, same URL
    expect(window.history.length).toBe(historyLengthBefore)          // pushState would grow this
    // And the link round trips back to what the page is showing.
    const value = new URL(written[0]).searchParams.get('s')!
    const back = await decodeState(value)
    expect('state' in back && back.state.policy).toBe(DEFAULT_POLICY_YAML)
  })

  test('an oversized state fails to build a link, and never touches the clipboard or the address bar', async () => {
    // The reviewer's exact reproduction: a large-but-fully-valid policy YAML (nothing a
    // cluster would reject) grows the state past encodeState's own ceiling. The button must
    // not be allowed to claim success — no replaceState, no clipboard write, no "Copied".
    visit('')
    const written: string[] = []
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: async (v: string) => { written.push(v) } },
    })
    const w = mountPage()
    const myCalls = latestCalls
    await vi.waitUntil(() => myCalls.length > 0)
    const vm = w.vm as any
    vm.policyYAML = DEFAULT_POLICY_YAML + randomBytes(15000).toString('hex')
    await w.vm.$nextTick()
    const searchBefore = window.location.search

    await shareButton(w).trigger('click')
    // copyShareLink()'s try/catch around encodeState() is async; poll for the rendered
    // "too large" banner instead of a fixed tick count, so a slow encode still gets caught.
    await vi.waitUntil(() => w.text().includes('too large to fit in a share link'))

    expect(written).toHaveLength(0)
    expect(window.location.search).toBe(searchBefore)
    expect(w.text()).toContain('too large to fit in a share link')
  })

  test('an unknown link VERSION gets its own message, distinct from "damaged"', async () => {
    // decodeState() distinguishes the two error codes; this is the other half of that
    // contract — the page must actually SHOW the distinction, not collapse both into the
    // same "damaged" banner.
    const value = await deflated(JSON.stringify({
      v: 99,
      policy: DEFAULT_POLICY_YAML,
      fleet: { expireAfter: '480h', nodes: [] },
      env: {},
      horizon: { start: '2026-01-01T00:00:00Z', end: '2026-02-01T00:00:00Z' },
    }))
    visit(`?s=${value}`)
    const w = mountPage()
    // Poll for the rendered version banner rather than a fixed tick count — same
    // non-fixed decode latency as the other decodeState() paths.
    await vi.waitUntil(() => w.text().includes('newer version of the simulator'))
    expect(w.text()).toContain('newer version of the simulator')
    expect(w.text()).not.toContain('Could not read the shared link')
  })

  test('the share button is available even when the run has NO result — a rejected policy is exactly what someone wants to share', async () => {
    const state = {
      policy: DEFAULT_POLICY_YAML,
      fleet: { expireAfter: '720h', nodes: [{ name: 'reject-me', createdAt: '2026-03-01T00:00:00Z' }] },
      env: {},
      horizon: { start: '2026-03-01T00:00:00Z', end: '2026-04-01T00:00:00Z' },
    }
    visit(`?s=${await encodeState(state)}`)
    const w = mountPage()
    const myCalls = latestCalls
    // Poll for the run against the rejecting fleet actually having happened, rather than a
    // fixed tick count, before checking that a resultless run still exposes the share button.
    await vi.waitUntil(() => myCalls.length > 0)
    expect((w.vm as any).result).toBeUndefined()  // the mocked simulate() rejected this fleet
    expect(shareButton(w)).toBeTruthy()
  })
})

/** Encode a raw string the way the codec does, so this suite's version test can forge a
 *  payload decodeState() itself would never construct. */
async function deflated(text: string): Promise<string> {
  const stream = new Blob([text]).stream().pipeThrough(new CompressionStream('deflate-raw'))
  const bytes = new Uint8Array(await new Response(stream).arrayBuffer())
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}
