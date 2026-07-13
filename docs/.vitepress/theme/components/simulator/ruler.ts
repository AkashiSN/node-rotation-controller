// docs/.vitepress/theme/components/simulator/ruler.ts
//
// The duration ruler: two GROUPED linear scales, and the ratio that bridges them. PURE —
// no Vue, no DOM.
//
// One linear scale cannot work. Against an expireAfter of 480h, terminationGracePeriod
// (1h) is 0.2% of the width, and t_rot, drain (10m) and provisioning (5m) are effectively
// zero. Six bars of which five have no length is a duration list with decoration, not a
// chart — and it is the same scale collision that made the timeline unreadable.
//
// The quantities are NOT alike, and the ruler says which is which: lifecycle offsets
// (expireAfter, A), a conservative deadline-side bound (t_rot), a CAP (tGP), actual
// environment durations, and the policy's FORECAST estimates. A forecast and an actual are
// never drawn as the same kind of thing.
import { parseGoDuration, type Fleet, type SimResponse, type SimResult } from './model.ts'

/** What a bar on the ruler MEANS. The distinction is load-bearing: an operator who reads a
 *  forecast as a measurement will trust a policy the run never validated. */
export type Quantity =
  | 'lifecycle'  // an offset from the node's birth: expireAfter, ageThreshold
  | 'bound'      // a conservative deadline-side bound the controller reserves: t_rot
  | 'cap'        // a ceiling, not a duration anything took: terminationGracePeriod
  | 'actual'     // what the simulated world actually did: Env.provisioning / Env.drain
  | 'forecast'   // what the policy predicts: provisioningEstimate, drainEstimate, t_rot_est
  | 'policy'     // a knob's value: readyTimeout, cooldownAfter

export interface Bar {
  key: string
  ms: number
  quantity: Quantity
}

export interface RulerModel {
  /** The aging scale: weeks. */
  lifecycle: Bar[]
  /** The rotation-mechanics scale: minutes. */
  rotation: Bar[]
  /** The bridge between the two scales — the reason the timeline needs zoom at all. */
  ratio: RatioSentence | null
}

/** "The worst rotation observed in this run took 15m — 0.1% of the longest node lifetime
 *  (480h)." Its terms are pinned, because "N% of a lifetime" is ambiguous in a
 *  heterogeneous fleet, and because a forecast numerator silently mixed with an actual
 *  denominator is exactly the kind of quiet lie this page exists not to tell. */
export interface RatioSentence {
  /** The WORST OBSERVED rotation (the longest COMPLETED done − start), or the forecast when
   *  nothing completed. Never an average, never a representative. */
  numeratorMs: number
  /** The LONGEST effective expireAfter in the fleet. */
  denominatorMs: number
  fraction: number
  /** True when nothing completed and the numerator is t_rot_est — a FORECAST. The sentence
   *  must say so; mixing a forecast numerator with an actual denominator is acceptable,
   *  doing it silently is not. */
  forecast: boolean
}

/** The longest EFFECTIVE expireAfter in the fleet: a per-node override where there is one,
 *  the template otherwise. The denominator of the ratio, and the ruler's full width. */
export function longestLifetimeMs(fleet: Fleet): number {
  const template = parseGoDuration(fleet.expireAfter) ?? 0
  const perNode = fleet.nodes.map(n => parseGoDuration(n.expireAfter ?? '') ?? template)
  return Math.max(template, ...perNode, 0)
}

/** The longest COMPLETED rotation in the run. An in-flight one is not a measurement: its
 *  duration is not known yet, and taking `simulatedThrough − start` would report a number
 *  the simulation never established. */
export function worstObservedRotationMs(resp: SimResponse): number | null {
  let worst: number | null = null
  for (const r of resp.rotations ?? []) {
    if (!r.done) continue
    const start = new Date(r.start).getTime()
    const done = new Date(r.done).getTime()
    if (!Number.isFinite(start) || !Number.isFinite(done) || done < start) continue
    if (worst === null || done - start > worst) worst = done - start
  }
  return worst
}

export interface RulerInputs {
  result: SimResult
  fleet: Fleet
  resp: SimResponse
  /** The virtual world's ACTUAL durations, already resolved: a blank env field means the
   *  policy's own estimate, and the caller resolves that (the page shows the resolved value
   *  in the env form too). */
  provisioningMs: number
  drainMs: number
  /** The policy knobs the result does not carry, from the YAML projection. */
  readyTimeout: string
  cooldownAfter: string
  /** The NodePool template's terminationGracePeriod — a CAP. */
  tgp: string
}

export function buildRuler(inp: RulerInputs): RulerModel {
  const { result, fleet, resp } = inp
  const dur = (s: string) => parseGoDuration(s) ?? 0

  const lifecycle: Bar[] = [
    { key: 'expireAfter', ms: longestLifetimeMs(fleet), quantity: 'lifecycle' },
    // A is the TEMPLATE-derived representative the controller exports. It appears here and
    // in the header, and NOWHERE on the timeline: drawn per generation it would be a
    // per-node fact, which it is not (the lead time adds each claim's own tGP).
    { key: 'ageThreshold', ms: dur(result.ageThreshold), quantity: 'lifecycle' },
  ]

  const rotation: Bar[] = [
    { key: 'tRot', ms: dur(result.tRot), quantity: 'bound' },
    { key: 'tRotEstimate', ms: dur(result.tRotEstimate), quantity: 'forecast' },
    { key: 'readyTimeout', ms: dur(inp.readyTimeout), quantity: 'policy' },
    { key: 'cooldownAfter', ms: dur(inp.cooldownAfter), quantity: 'policy' },
    { key: 'tgp', ms: dur(inp.tgp), quantity: 'cap' },
    { key: 'provisioning', ms: inp.provisioningMs, quantity: 'actual' },
    { key: 'drain', ms: inp.drainMs, quantity: 'actual' },
  ].filter(b => b.ms > 0)

  const denominatorMs = longestLifetimeMs(fleet)
  const observed = worstObservedRotationMs(resp)
  const forecastMs = dur(result.tRotEstimate)
  const numeratorMs = observed ?? forecastMs
  const ratio: RatioSentence | null = denominatorMs > 0 && numeratorMs > 0
    ? {
        numeratorMs,
        denominatorMs,
        fraction: numeratorMs / denominatorMs,
        forecast: observed === null,
      }
    : null

  return { lifecycle, rotation, ratio }
}

/** A bar's width as a fraction of its group's longest bar — each group is its OWN linear
 *  scale, which is the whole point. */
export function scaleWithin(bars: Bar[]): (bar: Bar) => number {
  const max = Math.max(1, ...bars.map(b => b.ms))
  return (bar: Bar) => bar.ms / max
}
