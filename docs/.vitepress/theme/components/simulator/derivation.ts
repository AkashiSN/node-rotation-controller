// docs/.vitepress/theme/components/simulator/derivation.ts
//
// The forecast strip's rows: each symbol, the formula the controller derives it from, that
// formula with THIS run's values substituted into it, and the value Go computed.
//
// PURE by design — no Vue, no DOM.
//
// THE PAGE PERFORMS NO ARITHMETIC. Every function here interpolates strings; none of them
// adds, divides, floors or ceils. The `value` on each row is the wasm module's own, verbatim.
// A page that recomputed A, G or C in TypeScript would be a second implementation of the very
// derivation this project keeps single — and two implementations can disagree, which is the
// one thing a simulator must never do.
import type { SimResult } from './model.ts'

/** The two strings a row may need that are NOT code: everything else on a row (the symbols,
 *  the formulas, the values) is identical in every locale. */
export interface DerivationLabels {
  /** Why the A row shows no equation: A was given, not derived. */
  overrideNote: string
  /** Marks a tGP that is the controller's fixed fallback, not the operator's value. */
  fallbackMark: string
}

export interface DerivationRow {
  symbol: string
  /** The symbolic formula (the spec's own, §1.4/§3.2) — always present. */
  formula: string
  /** The formula with this run's values in it; '' when there is none to show. */
  substitution: string
  /** Shown INSTEAD of a substitution, when an equation would not hold. '' otherwise. */
  note: string
  /** What the controller computed. Never recomputed here. */
  value: string
}

/** A Go duration as an operator would write it: the zero units dropped. "480h0m0s" → "480h",
 *  "1h17m0s" → "1h17m". A real zero stays "0s"; an ABSENT value is "—", never "0s" — the two
 *  mean different things and the strip must not conflate them.
 *
 *  String surgery, not arithmetic: it splits the string into its (value, unit) components and
 *  drops the trailing ones whose value is 0 — it never touches a digit, so it cannot change
 *  the value it is handed. At least one component always survives, so "0s" stays "0s" rather
 *  than collapsing to "". A negative duration carries exactly ONE leading "-" for the whole
 *  value (Go's own `time.Duration.String()` convention, e.g. "-1h17m0s") — never one per
 *  component. Peel it off, tidy the magnitude, then put it back: a fatal negative A (the
 *  ANonPositive case the strip exists to surface) must never round-trip through here looking
 *  positive. */
export function tidyDuration(s: string): string {
  if (!s) return '—'
  const negative = s.startsWith('-')
  const magnitude = negative ? s.slice(1) : s
  const parts = [...magnitude.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)]
  if (parts.length === 0) return s
  let end = parts.length
  while (end > 1 && parseFloat(parts[end - 1][1]) === 0) end--
  const tidy = parts.slice(0, end).map(([full]) => full).join('')
  return negative ? `-${tidy}` : tidy
}

/** The five rows, in the order the controller derives them. */
export function buildDerivation(result: SimResult, labels: DerivationLabels): DerivationRow[] {
  const inputs = result.inputs
  const a = tidyDuration(result.ageThreshold)
  const tRot = tidyDuration(result.tRot)
  const tRotEst = tidyDuration(result.tRotEstimate)
  const prov = tidyDuration(result.provisioningEstimate)
  const drain = tidyDuration(result.drainEstimate)

  // No inputs (an older artifact, or a response that carried none): the symbolic formulas
  // still hold — they are the spec's — but nothing may be substituted into them.
  if (!inputs) {
    return [
      row('A', 'A = E − (K·P + t_rot)', '', '', a),
      row('t_rot', 't_rot = readyTimeout + tGP + buffer', '', '', tRot),
      row('t_rot_est', 't_rot_est = provisioningEstimate + drainEstimate', '', '', tRotEst),
      row('G', 'G = floor(((E − t_rot) − A) / P)', '', '', String(result.g)),
      row('C', 'C = m · ceil(D / (t_rot_est + cooldownAfter))', '', '', String(result.c)),
    ]
  }

  const e = tidyDuration(inputs.e)
  const p = tidyDuration(inputs.p)
  const d = tidyDuration(inputs.windowLen)
  const buffer = tidyDuration(inputs.buffer)
  // The tGP the derivation USED. When it is the controller's fallback it appears nowhere in
  // the YAML, so the row says so rather than presenting it as something the operator wrote.
  const tgp = inputs.tgpFallback
    ? `${tidyDuration(inputs.tgp)} (${labels.fallbackMark})`
    : tidyDuration(inputs.tgp)
  const readyTimeout = tidyDuration(inputs.readyTimeout)
  const cooldown = tidyDuration(inputs.cooldownAfter)

  return [
    inputs.ageThresholdOverride
      // An explicit ageThreshold is ECHOED BACK, not derived. Printing
      // "A = E − (K·P + t_rot)" with this run's numbers would be an equation that is false.
      ? row('A', 'A = E − (K·P + t_rot)', '', labels.overrideNote, a)
      : row('A', 'A = E − (K·P + t_rot)', `${e} − (${inputs.k}·${p} + ${tRot})`, '', a),
    row('t_rot', 't_rot = readyTimeout + tGP + buffer',
      `${readyTimeout} + ${tgp} + ${buffer}`, '', tRot),
    row('t_rot_est', 't_rot_est = provisioningEstimate + drainEstimate',
      `${prov} + ${drain}`, '', tRotEst),
    // G is derived against the A the run ACTUALLY used — the override included.
    row('G', 'G = floor(((E − t_rot) − A) / P)',
      `floor(((${e} − ${tRot}) − ${a}) / ${p})`, '', String(result.g)),
    row('C', 'C = m · ceil(D / (t_rot_est + cooldownAfter))',
      `${inputs.m} · ceil(${d} / (${tRotEst} + ${cooldown}))`, '', String(result.c)),
  ]
}

const row = (symbol: string, formula: string, substitution: string, note: string, value: string):
  DerivationRow => ({ symbol, formula, substitution, note, value })
