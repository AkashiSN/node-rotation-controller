import { test } from 'node:test'
import assert from 'node:assert/strict'
import { projectPolicy, applyPolicyEdit } from './policyYaml.ts'
import { DEFAULT_POLICY_YAML } from './model.ts'

test('projectPolicy reads the form fields off a manifest', () => {
  const { form, error } = projectPolicy(DEFAULT_POLICY_YAML)
  assert.equal(error, undefined)
  assert.equal(form.timezone, 'UTC')
  assert.deepEqual(form.days, ['Wed', 'Sat'])
  assert.equal(form.start, '02:00')
  assert.equal(form.end, '06:00')
  assert.equal(form.minRotationChances, 2)
  assert.equal(form.ageThreshold, 'auto')
  assert.equal(form.provisioningEstimate, '5m')
  assert.equal(form.drainEstimate, '10m')
  assert.equal(form.cooldownAfter, '10m')
  assert.equal(form.readyTimeout, '15m')
  assert.equal(form.forcefulFallback, false)
  assert.equal(form.extraWindows, 0)
})

test('projectPolicy reports unparseable YAML instead of throwing', () => {
  const { error } = projectPolicy('spec:\n  - : :\n   bad')
  assert.ok(error, 'a YAML the parser rejects must surface as an error, not an exception')
})

test('applyPolicyEdit writes a field back into the manifest', () => {
  const out = applyPolicyEdit(DEFAULT_POLICY_YAML, 'cooldownAfter', '1m')
  assert.equal(projectPolicy(out).form.cooldownAfter, '1m')
  assert.equal(projectPolicy(out).form.readyTimeout, '15m', 'untouched fields must survive')
})

test('applyPolicyEdit toggles forcefulFallback as a boolean, not a string', () => {
  const out = applyPolicyEdit(DEFAULT_POLICY_YAML, 'forcefulFallback', true)
  assert.match(out, /enabled: true/)
  assert.equal(projectPolicy(out).form.forcefulFallback, true)
})

test('a form edit PRESERVES an unknown field, so Go still rejects the manifest', () => {
  // THE test of this module. A rebuild-from-form-fields implementation would drop
  // `bogusField` here and hand the wasm module a manifest that now passes strict
  // decoding — a Go-strict rejection silently converted into a valid manifest by an
  // unrelated edit. Mutating the AST cannot do that.
  const withUnknown = DEFAULT_POLICY_YAML.replace('  ageThreshold: auto\n', '  ageThreshold: auto\n  bogusField: 1\n')
  const out = applyPolicyEdit(withUnknown, 'cooldownAfter', '1m')
  assert.match(out, /bogusField: 1/)
})

test('projectPolicy always projects days as a string[], even for a bare scalar or empty value', () => {
  // `days: Sat` is valid YAML (a bare scalar) and an easy typo for `days: [Sat]`.
  // The form's contract is `string[]`; a consumer that does `form.days.join(',')`
  // must not throw or receive a raw string mistaken for an array.
  const scalar = projectPolicy(DEFAULT_POLICY_YAML.replace('days: [Wed, Sat]', 'days: Sat'))
  assert.equal(scalar.error, undefined)
  assert.deepEqual(scalar.form.days, [])

  const empty = projectPolicy(DEFAULT_POLICY_YAML.replace('days: [Wed, Sat]', 'days:'))
  assert.equal(empty.error, undefined)
  assert.deepEqual(empty.form.days, [])
})

test('applyPolicyEdit does not re-wrap a long untouched scalar elsewhere in the manifest', () => {
  // yaml's default `lineWidth: 80` line-folds a plain scalar at whitespace once it
  // crosses the width — a value with no spaces (e.g. all-digit) can't be folded and
  // so wouldn't reproduce the bug; this one has spaces, like a real description.
  const longValue = Array.from({ length: 20 }, (_, i) => `word${i}`).join(' ')
  assert.equal(longValue.length, 129, 'sanity: exercise the >80-char fold threshold')
  const withLongScalar = DEFAULT_POLICY_YAML.replace(
    '  ageThreshold: auto\n',
    `  ageThreshold: auto\n  someLongUnknownField: ${longValue}\n`,
  )
  const out = applyPolicyEdit(withLongScalar, 'cooldownAfter', '1m')
  assert.match(
    out,
    new RegExp(`someLongUnknownField: ${longValue}\\n`),
    'a long untouched scalar must survive on a single line, not be folded by an unrelated edit',
  )
})

test('a form edit preserves comments and a second maintenance window', () => {
  const commented = `apiVersion: noderotation.io/v1alpha1
kind: RotationPolicy
metadata:
  name: weekly
spec:
  # the union of both entries is the effective window
  nodePoolSelector:
    matchLabels:
      workload: api
  minRotationChances: 2
  maintenanceWindows:
    - timezone: UTC
      days: [Sat]
      start: "02:00"
      end: "06:00"
    - timezone: Asia/Tokyo
      days: [Sun]
      start: "01:00"
      end: "03:00"
`
  assert.equal(projectPolicy(commented).form.extraWindows, 1)
  const out = applyPolicyEdit(commented, 'timezone', 'Asia/Tokyo')
  assert.match(out, /# the union of both entries/, 'comments must survive a form edit')
  assert.match(out, /days: \[Sun\]/, 'the second window must survive a form edit')
  assert.equal(projectPolicy(out).form.timezone, 'Asia/Tokyo')
})
