// vitest run — component (DOM) tests. See vitest.config.ts for what these can and cannot
// cover: happy-dom has no layout engine, so these are STATE-TRANSITION tests with stubbed
// geometry, not layout tests.
import { mount } from '@vue/test-utils'
import { describe, expect, test, vi } from 'vitest'
import { ref } from 'vue'

// i18n reads the locale off VitePress. The component is the unit here, not the theme.
vi.mock('vitepress', () => ({ useData: () => ({ lang: ref('en-US') }) }))

import TimelineChart from './TimelineChart.vue'
import type { Fleet, Horizon, SimResponse } from './model.ts'

const FLEET: Fleet = {
  expireAfter: '480h',
  terminationGracePeriod: '1h',
  nodes: [
    { name: 'node-1', createdAt: '2026-01-01T00:00:00Z' },
    { name: 'node-2', createdAt: '2026-01-08T00:00:00Z' },
  ],
}
const HORIZON: Horizon = { start: '2026-01-01T00:00:00Z', end: '2026-02-10T00:00:00Z' }
const ms = (iso: string) => new Date(iso).getTime()

const RESPONSE: SimResponse = {
  simulatedThrough: HORIZON.end,
  generations: [
    {
      slot: 0, gen: 0, name: 'node-1', birthMode: 'initial',
      createdAt: '2026-01-01T00:00:00Z', expireAfter: '480h',
      drainCap: '1h', drainCapSource: 'explicit',
      deadline: '2026-01-21T00:00:00Z', eligibilityBoundary: '2026-01-13T00:00:00Z',
    },
    {
      slot: 0, gen: 1, name: 'node-1-r1', birthMode: 'surge', predecessorGen: 0,
      createdAt: '2026-01-14T02:00:00Z', expireAfter: '480h',
      drainCap: '1h', drainCapSource: 'explicit',
      deadline: '2026-02-03T02:00:00Z', eligibilityBoundary: '2026-01-26T00:00:00Z',
      readyAt: '2026-01-14T02:05:00Z',
    },
    {
      slot: 1, gen: 0, name: 'node-2', birthMode: 'initial',
      createdAt: '2026-01-08T00:00:00Z', expireAfter: '480h',
      drainCap: '1h', drainCapSource: 'explicit',
      deadline: '2026-01-28T00:00:00Z', eligibilityBoundary: '2026-01-20T00:00:00Z',
    },
  ],
  rotations: [{
    slot: 0, fromGen: 0, toGen: 1, mode: 'surge',
    start: '2026-01-14T02:00:00Z', ready: '2026-01-14T02:05:00Z', done: '2026-01-14T02:15:00Z',
  }],
  windows: [
    { start: '2026-01-03T02:00:00Z', end: '2026-01-03T06:00:00Z' },
    { start: '2026-01-14T02:00:00Z', end: '2026-01-14T06:00:00Z' },
  ],
  events: [],
}

const mountChart = (response: SimResponse = RESPONSE) =>
  mount(TimelineChart, { props: { response, horizon: HORIZON, fleet: FLEET, timezone: 'UTC' } })

const button = (w: ReturnType<typeof mountChart>, label: string) =>
  w.findAll('button').find(b => b.text() === label)!

describe('the chart draws what the run established, and only that', () => {
  test('one row per slot, whatever the generation count', () => {
    const w = mountChart()
    expect(w.findAll('.sim-row')).toHaveLength(2)
    expect(w.findAll('.sim-rowlabel').map(n => n.text())).toEqual(['node-1', 'node-2'])
  })

  // A rotation lasts minutes; the horizon spans weeks. At the whole-horizon zoom its
  // segments are sub-pixel and are deliberately NOT drawn (an illegible mark is a claim the
  // reader cannot check), so these two mount and then navigate to the rotation — which is
  // what a reader does.
  test('an in-flight boundary is hatched and labelled as in-flight — never a zero-length bar', async () => {
    const inFlight: SimResponse = {
      simulatedThrough: '2026-01-14T02:03:00Z',
      generations: [
        RESPONSE.generations![0],
        { ...RESPONSE.generations![1], readyAt: undefined, provisional: true },
      ],
      rotations: [{ slot: 0, fromGen: 0, toGen: 1, mode: 'surge', start: '2026-01-14T02:00:00Z' }],
    }
    const w = mountChart(inFlight)
    await button(w, 'Next rotation').trigger('click')

    const open = w.findAll('.sim-seg.sim-seg-open')
    expect(open.length).toBeGreaterThan(0)
    // "The simulation ended" and "the response is wrong" mean opposite things to a reader,
    // so they never share an accessible name.
    expect(open.some(n => /still in flight/.test(n.attributes('aria-label') ?? ''))).toBe(true)
    expect(w.findAll('.sim-seg.sim-seg-malformed')).toHaveLength(0)
  })

  test('a malformed boundary gets its OWN label, not the in-flight one', async () => {
    const malformed: SimResponse = {
      simulatedThrough: HORIZON.end,
      generations: [RESPONSE.generations![0], { ...RESPONSE.generations![1], readyAt: undefined }],
      rotations: [{
        slot: 0, fromGen: 0, toGen: 1, mode: 'surge',
        start: '2026-01-14T02:00:00Z', done: '2026-01-14T02:15:00Z',
      }],
    }
    const w = mountChart(malformed)
    await button(w, 'Next rotation').trigger('click')

    const bad = w.findAll('.sim-seg.sim-seg-malformed')
    expect(bad.length).toBeGreaterThan(0)
    expect(bad.some(n => /missing this boundary/.test(n.attributes('aria-label') ?? ''))).toBe(true)
  })

  test('a self-contradicting response is surfaced, never painted over', () => {
    const w = mountChart({
      ...RESPONSE,
      generations: [...RESPONSE.generations!, { ...RESPONSE.generations![0], name: 'dup' }],
    })
    expect(w.find('.sim-warn').text()).toMatch(/duplicate generation/)
  })

  test('the breach glyph is drawn OFFSET from the deadline line it is co-located with', () => {
    const w = mountChart({
      ...RESPONSE,
      events: [{ kind: 'expire-after-breach', at: '2026-01-28T00:00:00Z', node: 'node-2' }],
    })
    const breach = w.find('.sim-breach')
    expect(breach.exists()).toBe(true)
    // The simulator emits the breach AT the deadline by construction, so the glyph must not
    // be overdrawn by the deadline line — a regression there hides every breach in the run.
    const y = Number(breach.find('line').attributes('y1'))
    const deadlineY = Number(w.findAll('.sim-deadline')[0].attributes('y1'))
    expect(y).not.toBe(deadlineY)
  })

  test('a run that reaches no rotation still renders, with the rotation buttons disabled', () => {
    const w = mountChart({ simulatedThrough: HORIZON.end, generations: RESPONSE.generations })
    expect(button(w, 'First rotation').attributes('disabled')).toBeDefined()
    expect(button(w, 'Next rotation').attributes('disabled')).toBeDefined()
    expect(w.findAll('.sim-row')).toHaveLength(2)
  })
})

describe('the view is not the horizon', () => {
  test('it opens on the whole horizon', () => {
    const w = mountChart()
    expect(w.vm.view).toEqual({ startMs: ms(HORIZON.start), endMs: ms(HORIZON.end) })
  })

  test('"next rotation" fits the OCCASION — the window occurrence, both boundaries on screen', async () => {
    const w = mountChart()
    await button(w, 'Next rotation').trigger('click')

    const view = w.vm.view
    const windowStart = ms('2026-01-14T02:00:00Z')
    const windowEnd = ms('2026-01-14T06:00:00Z')
    // BOTH boundaries are on screen. The old landing view (at − 30m … at + 60m) was 2h33m —
    // narrower than the 4h window it sat inside — so neither of them was, and the run read as
    // crowded against the right edge.
    expect(view.startMs).toBeLessThan(windowStart)
    expect(view.endMs).toBeGreaterThan(windowEnd)
    // Still the rotation's scale, not the horizon's: hours, not the 40-day run.
    expect(view.endMs - view.startMs).toBeLessThan(12 * 3_600_000)

    // There is only one occasion in the run: there is nothing to go to next.
    expect(button(w, 'Next rotation').attributes('disabled')).toBeDefined()
    expect(button(w, 'Previous rotation').attributes('disabled')).toBeDefined()
  })

  test('a busy window is ONE click, not one per node: every rotation in it lands on screen', async () => {
    // Four nodes rotating serially inside one occurrence, 25m apart — the default policy's
    // own shape at ten nodes. Stepping by rotation start moved the view by 25 minutes a
    // click (a jitter, not a jump) and cost four clicks to leave the window.
    const starts = [
      '2026-01-14T02:00:00Z', '2026-01-14T02:25:00Z',
      '2026-01-14T02:50:00Z', '2026-01-14T03:15:00Z',
    ]
    const w = mountChart({
      ...RESPONSE,
      rotations: starts.map((start, i) => ({
        slot: i % 2, fromGen: 0, toGen: 1, mode: 'surge' as const, start,
      })),
    })
    await button(w, 'Next rotation').trigger('click')

    const view = w.vm.view
    for (const start of starts) {
      expect(ms(start)).toBeGreaterThanOrEqual(view.startMs)
      expect(ms(start)).toBeLessThanOrEqual(view.endMs)
    }
    // …and there is nothing left to step to inside the occurrence, because it is all in view.
    expect(button(w, 'Next rotation').attributes('disabled')).toBeDefined()
  })

  test('a schedule with no occurrence to fit falls back to the instant — CENTRED', async () => {
    // A continuously-open union (here: a window as wide as the run) has no occurrence
    // narrower than the horizon, so there is nothing to fit. The rotation itself becomes the
    // target, in the middle of the view rather than 40% from the left.
    const w = mountChart({
      ...RESPONSE,
      windows: [{ start: HORIZON.start, end: HORIZON.end }],
    })
    await button(w, 'Next rotation').trigger('click')

    const view = w.vm.view
    const at = ms('2026-01-14T02:00:00Z')
    expect(at - view.startMs).toBe(view.endMs - at)
    expect(view.endMs - view.startMs).toBeLessThan(6 * 3_600_000)
  })

  test('semantic zoom: the TGP cap bracket appears only once it is wide enough to read', async () => {
    const w = mountChart()
    // Across 40 days, a 1h cap is under a pixel. Drawing it would be a claim the reader
    // cannot check.
    expect(w.findAll('.sim-cap')).toHaveLength(0)

    await button(w, 'Next rotation').trigger('click')
    expect(w.findAll('.sim-cap').length).toBeGreaterThan(0)
    expect(w.find('.sim-cap').attributes('aria-label')).toMatch(/terminationGracePeriod/)
  })

  test('the make-before-break span is NAMED once it is legible, not left to be inferred', async () => {
    const w = mountChart()
    // At the whole-horizon zoom the 5-minute overlap is sub-pixel: annotating it there would
    // be a label pointing at nothing.
    expect(w.findAll('.sim-overlap')).toHaveLength(0)

    // The occasion view is the WINDOW (4h), and a 5-minute overlap inside it is ~10px — still
    // below the threshold at which an annotation carrying text is legible. This is the price
    // of fitting the occurrence rather than one rotation start, and it is paid deliberately:
    // the occasion answers "when did this window rotate, and did it fit", and one zoom step
    // answers "what happened inside one rotation".
    await button(w, 'Next rotation').trigger('click')
    expect(w.findAll('.sim-overlap')).toHaveLength(0)

    // One zoom into the rotation's own scale — anchored ON the rotation, the way a wheel
    // gesture over it is — and it is named.
    w.vm.zoom(0.1, ms('2026-01-14T02:00:00Z'))
    await w.vm.$nextTick()
    const overlap = w.findAll('.sim-overlap')
    expect(overlap.length).toBe(1)
    expect(overlap[0].attributes('aria-label')).toMatch(/make-before-break/)
    // Five minutes: the replacement provisioning while the old node still serves.
    expect(overlap[0].attributes('aria-label')).toMatch(/5m/)
  })

  test('the keyboard operates the view: arrows pan, +/- zoom, 0 resets', async () => {
    const w = mountChart()
    const svg = w.find('svg.sim-svg')
    const whole = { ...w.vm.view }

    await svg.trigger('keydown', { key: '+' })
    expect(w.vm.view.endMs - w.vm.view.startMs).toBeLessThan(whole.endMs - whole.startMs)

    const zoomed = { ...w.vm.view }
    await svg.trigger('keydown', { key: 'ArrowRight' })
    expect(w.vm.view.startMs).toBeGreaterThan(zoomed.startMs)

    await svg.trigger('keydown', { key: '0' })
    expect(w.vm.view).toEqual(whole)
  })

  test('the wheel is an accelerator on the same transition', async () => {
    const w = mountChart()
    const before = w.vm.view.endMs - w.vm.view.startMs
    await w.find('svg.sim-svg').trigger('wheel', { deltaY: -100, clientX: 400 })
    expect(w.vm.view.endMs - w.vm.view.startMs).toBeLessThan(before)
  })

  test('a rerun that moves the horizon clamps the view instead of losing it', async () => {
    const w = mountChart()
    await button(w, 'Next rotation').trigger('click')
    const zoomed = { ...w.vm.view }

    // The visitor edits the fleet; the horizon lengthens. Their view is theirs.
    await w.setProps({ horizon: { start: HORIZON.start, end: '2026-03-10T00:00:00Z' } })
    expect(w.vm.view).toEqual(zoomed)

    // But a view that was the WHOLE horizon was never a choice — it tracks.
    await w.find('svg.sim-svg').trigger('keydown', { key: '0' })
    await w.setProps({ horizon: { start: HORIZON.start, end: '2026-04-10T00:00:00Z' } })
    expect(w.vm.view.endMs).toBe(ms('2026-04-10T00:00:00Z'))
  })

  test('the minimap moves the view, and the chart accepts it', async () => {
    const w = mountChart()
    await button(w, 'Next rotation').trigger('click')
    const width = w.vm.view.endMs - w.vm.view.startMs

    w.findComponent({ name: 'MinimapStrip' }).vm.$emit('update:view', {
      startMs: ms('2026-01-20T00:00:00Z'), endMs: ms('2026-01-20T00:00:00Z') + width,
    })
    await w.vm.$nextTick()
    expect(w.vm.view.startMs).toBe(ms('2026-01-20T00:00:00Z'))
  })
})

describe('the chart is not the sole carrier of the result', () => {
  test('a visually-hidden table restates the run, and is re-rendered on a rerun', async () => {
    const w = mountChart()
    const table = () => w.find('.sim-sr-only table')
    expect(table().text()).toContain('node-1-r1')

    // The hidden recipe must sit on the WRAPPER: a <table> ignores `width: 1px` (auto table
    // layout floors it at min-content), so a `table.sim-sr-only` would stay ~2200px wide and
    // give the page a horizontal scrollbar into empty space (#250).
    expect(w.find('table.sim-sr-only').exists()).toBe(false)

    // A rerun with a different fleet: the table follows the data, not the first render.
    await w.setProps({
      response: { simulatedThrough: HORIZON.end, generations: [RESPONSE.generations![2]] },
    })
    expect(table().text()).not.toContain('node-1-r1')
    expect(table().text()).toContain('node-2')
  })

  test('every focusable mark carries an accessible name', () => {
    const w = mountChart()
    for (const el of w.findAll('[tabindex="0"][role="graphics-symbol"]')) {
      expect(el.attributes('aria-label')).toBeTruthy()
    }
  })

  test('a degenerate horizon says so rather than drawing a mis-scaled chart', () => {
    const w = mount(TimelineChart, {
      props: {
        response: RESPONSE, fleet: FLEET, timezone: 'UTC',
        horizon: { start: '2026-02-10T00:00:00Z', end: '2026-01-01T00:00:00Z' },
      },
    })
    expect(w.find('.sim-empty').exists()).toBe(true)
    expect(w.find('svg.sim-svg').exists()).toBe(false)
  })
})
