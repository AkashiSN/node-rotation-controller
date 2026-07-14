// vitest run — the page's EXPLANATIONS: the policy form's groups, the severity treatment a
// finding gets, and the symbol reference beside the forecast strip (#261).
//
// These are DOM claims, so they are made against a mounted component — but they are claims
// about STRUCTURE and CONTENT (which control sits in which group, which severity a finding
// carries, where a link points), never about layout: happy-dom has no layout engine, and a
// test that pretended otherwise would be asserting nothing.
import { mount } from '@vue/test-utils'
import { describe, expect, test, vi } from 'vitest'

const locale = vi.hoisted(() => ({ value: 'en-US' }))
vi.mock('vitepress', () => ({
  useData: () => ({ lang: locale }),
  withBase: (p: string) => `/node-rotation-controller${p}`,
}))

import { DEFAULT_POLICY_YAML } from './model.ts'
import FindingList from './FindingList.vue'
import PolicyInput from './PolicyInput.vue'
import ResultHeader from './ResultHeader.vue'
import SymbolReference from './SymbolReference.vue'

const RESULT = {
  ageThreshold: '310h43m0s', tRot: '1h17m0s', tRotEstimate: '15m0s',
  drainEstimate: '10m0s', provisioningEstimate: '5m0s', g: 2, c: 10,
  inputs: {
    e: '480h0m0s', tgp: '1h0m0s', tgpFallback: false, p: '84h0m0s', windowLen: '4h0m0s',
    buffer: '2m0s', readyTimeout: '15m0s', cooldownAfter: '10m0s',
    k: 2, m: 1, nodeCount: 3, ageThresholdOverride: false,
  },
}

describe('the policy form reads as a form', () => {
  // A label's NAME is its own text, not its control's: `.text()` on a <label> wrapping the
  // timezone <select> returns the label plus all 400-odd option names.
  const nameOf = (el: Element) =>
    [...el.childNodes].filter(n => n.nodeType === 3).map(n => n.textContent?.trim())
      .filter(Boolean).join(' ')
  const groupsOf = (w: ReturnType<typeof mount>) =>
    w.findAll('.sim-group').map(g => ({
      legend: g.find('legend').text(),
      fields: g.findAll(':scope > .sim-form > label').map(l => nameOf(l.element)),
    }))

  test('the eleven controls are grouped the way the POLICY is: window / derivation / surge', () => {
    const w0 = mount(PolicyInput, { props: { yaml: DEFAULT_POLICY_YAML } })
    const groups = groupsOf(w0)
    expect(groups).toHaveLength(3)
    expect(groups.map(g => g.legend)).toEqual([
      expect.stringContaining('Maintenance window'),
      expect.stringContaining('Derivation'),
      expect.stringContaining('Surge'),
    ])
    // The window group owns the schedule, the derivation group the two fields that set how
    // early a node is picked, and the surge group the mechanics of one rotation.
    expect(groups[0].fields).toEqual(['Timezone', 'Window start', 'Window end'])
    expect(groups[0].fields.length + groups[1].fields.length + groups[2].fields.length).toBe(10)
    // …and the weekday enum, which is a checkbox grid rather than a single field.
    expect(w0.findAll('.sim-group')[0].findAll('.sim-days-grid label')).toHaveLength(7)
    expect(groups[1].fields).toEqual(['minRotationChances (K)', 'ageThreshold'])
    expect(groups[2].fields).toEqual([
      'provisioningEstimate', 'drainEstimate', 'readyTimeout', 'cooldownAfter', 'Forceful fallback',
    ])
  })

  test('every label goes through the catalogue — including the two that used to be bare literals', () => {
    // provisioningEstimate and drainEstimate were hardcoded in the template, so they were the
    // only two fields that could never be translated. They come from i18n now, which is what
    // this asserts: switch the locale, and the fields that ARE translated change with it.
    locale.value = 'ja-JP'
    try {
      const w = mount(PolicyInput, { props: { yaml: DEFAULT_POLICY_YAML } })
      const groups = groupsOf(w)
      expect(groups[0].legend).toContain('メンテナンス窓')
      expect(groups[0].fields).toContain('タイムゾーン')
      // The Go field names stay identifiers in both locales — they are what you type in the
      // YAML, and a translated `cooldownAfter` would be a lie about the schema.
      expect(groups[2].fields).toContain('provisioningEstimate')
    } finally {
      locale.value = 'en-US'
    }
  })

  test('a YAML the browser parser rejects still greys out every group at once', () => {
    // Grouping the fields into nested fieldsets must not cost the disable: a disabled
    // fieldset disables every form control INSIDE it, however deeply nested, and that is what
    // greys the form out while the YAML is mid-edit and unparseable.
    //
    // The assertion is on the fieldset, not on `input.disabled`: that property reflects the
    // input's OWN attribute — it is false under a disabled ancestor in a real browser too —
    // so asserting it would be asserting the wrong thing and would pass for the wrong reason.
    const w = mount(PolicyInput, { props: { yaml: 'not: [valid' } })
    const outer = w.find('fieldset.sim-policy-form')
    expect(outer.attributes('disabled')).toBeDefined()
    // Every control is inside it, so every control is disabled.
    expect(w.findAll('input, select')).toHaveLength(outer.findAll('input, select').length)

    // …and a valid YAML leaves the form live.
    const ok = mount(PolicyInput, { props: { yaml: DEFAULT_POLICY_YAML } })
    expect(ok.find('fieldset.sim-policy-form').attributes('disabled')).toBeUndefined()
  })
})

describe('a finding is a signal, not a sentence', () => {
  const findings = [
    { severity: 'warn' as const, code: 'AVeryAggressive', message: 'ageThreshold is below the window period' },
    { severity: 'fatal' as const, code: 'ANonPositive', message: 'the schedule cannot guarantee even K chances' },
  ]

  test('severity is carried by a NAMED glyph and a class — never by colour alone', () => {
    const w = mount(FindingList, { props: { findings } })
    const items = w.findAll('.sim-finding')
    expect(items).toHaveLength(2)
    expect(items[0].classes()).toContain('sim-finding-warn')
    expect(items[1].classes()).toContain('sim-finding-fatal')
    // The glyph has an accessible name, so severity survives a screen reader too.
    expect(items[0].find('.sim-sev').attributes('aria-label')).toBe('Warning')
    expect(items[1].find('.sim-sev').attributes('aria-label')).toBe('Fatal')
  })

  test('the code is a chip and the MESSAGE is untouched — the controller\'s own words, verbatim', () => {
    const w = mount(FindingList, { props: { findings } })
    const first = w.findAll('.sim-finding')[0]
    expect(first.find('.sim-finding-code').text()).toBe('AVeryAggressive')
    expect(first.find('.sim-finding-msg').text()).toBe(findings[0].message)

    // Verbatim in BOTH locales: re-wording a message here would fork the catalogue the wasm
    // module owns, which is the drift the whole design exists to prevent.
    locale.value = 'ja-JP'
    try {
      const ja = mount(FindingList, { props: { findings } })
      expect(ja.find('.sim-finding-msg').text()).toBe(findings[0].message)
      expect(ja.find('.sim-sev').attributes('aria-label')).toBe('警告')
    } finally {
      locale.value = 'en-US'
    }
  })
})

describe('the symbols the strip reports are defined ON the page', () => {
  test('the strip IS the derivation: formula, this run substituted into it, and the value', () => {
    const w = mount(ResultHeader, { props: { result: RESULT } })
    const rows = w.findAll('.sim-derivation-row')
    expect(rows.map(r => r.find('.sim-symbol-name').text()))
      .toEqual(['A', 't_rot', 't_rot_est', 'G', 'C'])
    // The symbolic formula element itself — the thing #266 adds — not just its class name in
    // a selector elsewhere. Deleting <code class="sim-derivation-formula"> or renaming it
    // must fail here.
    expect(rows.map(r => r.find('.sim-derivation-formula').text())).toEqual([
      'A = E − (K·P + t_rot)',
      't_rot = readyTimeout + tGP + buffer',
      't_rot_est = provisioningEstimate + drainEstimate',
      'G = floor(((E − t_rot) − A) / P)',
      'C = m · ceil(D / (t_rot_est + cooldownAfter))',
    ])
    expect(rows.map(r => r.find('.sim-derivation-sub').text())).toEqual([
      '480h − (2·84h + 1h17m)',
      '15m + 1h + 2m',
      '5m + 10m',
      'floor(((480h − 1h17m) − 310h43m) / 84h)',
      '1 · ceil(4h / (15m + 10m))',
    ])
    expect(rows.map(r => r.find('.sim-derivation-value').text()))
      .toEqual(['310h43m', '1h17m', '15m', '2', '10'])
  })

  test('an overridden ageThreshold shows a note where the equation would be', () => {
    const overridden = {
      ...RESULT, ageThreshold: '240h0m0s',
      inputs: { ...RESULT.inputs, ageThresholdOverride: true },
    }
    const rows = mount(ResultHeader, { props: { result: overridden } }).findAll('.sim-derivation-row')
    expect(rows[0].find('.sim-derivation-sub').exists()).toBe(false)
    expect(rows[0].find('.sim-derivation-note').text()).toContain('spec.ageThreshold')
    expect(rows[0].find('.sim-derivation-value').text()).toBe('240h')
  })

  test('all five symbols get a definition, not just the two links to the spec', () => {
    const defs = mount(SymbolReference).findAll('.sim-symbol')
    expect(defs).toHaveLength(5)
    expect(defs.map(d => d.find('.sim-symbol-name').text()))
      .toEqual(['A', 't_rot', 't_rot_est', 'G', 'C'])
  })

  test('it links to the specification — and a Japanese reader lands on the JAPANESE chapter', () => {
    // The anchors are NOT shared across locales (the JA headings carry Japanese slugs), so
    // sending a Japanese reader to the English chapter would be the very defect this fixes.
    const en = mount(SymbolReference).findAll('a').map(a => a.attributes('href'))
    expect(en).toEqual([
      '/node-rotation-controller/specification/01-overview#14-terminology',
      '/node-rotation-controller/specification/03-design#32-candidate-selection',
    ])

    locale.value = 'ja-JP'
    try {
      const ja = mount(SymbolReference).findAll('a').map(a => a.attributes('href'))
      expect(ja).toEqual([
        '/node-rotation-controller/ja/specification/01-overview#14-用語',
        '/node-rotation-controller/ja/specification/03-design#32-候補選定',
      ])
    } finally {
      locale.value = 'en-US'
    }
  })
})
