// docs/.vitepress/theme/components/simulator/model.ts
//
// The simulator page's data model: the wire shapes of internal/simapi, the Go
// duration arithmetic the page needs, and the defaults a visitor lands on.
//
// PURE by design — no Vue, no VitePress. Every rule worth getting right (the
// horizon, the node generator, duration parsing) lives here, where `node --test`
// can cover it without a DOM.

/** One node of the simulated fleet. expireAfter / terminationGracePeriod override
 *  the NodePool template for this node; blank inherits it. */
export interface FleetNode {
  name: string
  createdAt: string
  expireAfter?: string
  terminationGracePeriod?: string
}

/** The simulated NodePool: the template values a replacement inherits, plus the nodes. */
export interface Fleet {
  expireAfter: string
  terminationGracePeriod?: string
  nodes: FleetNode[]
}

/** The virtual world's ACTUAL durations — NOT the policy's forecast estimates.
 *  A blank field means "use the policy's own resolved estimate", and MUST stay
 *  blank in the model: hydrating it to the displayed estimate would freeze it at
 *  the value the policy had at that moment, so later policy edits would stop
 *  moving the timeline. */
export interface Env {
  provisioning?: string
  drain?: string
}

/** The simulated horizon, [start, end]. */
export interface Horizon {
  start: string
  end: string
}

export interface SimRequest {
  fleet: Fleet
  env: Env
  options: Horizon
}

export interface Finding {
  severity: 'warn' | 'fatal'
  code: string
  message: string
}

export type Diagnostic = Finding

export interface SimResult {
  ageThreshold: string
  tRot: string
  tRotEstimate: string
  drainEstimate: string
  provisioningEstimate: string
  g: number
  c: number
  findings?: Finding[]
}

export type EventKind =
  | 'window-open' | 'window-close' | 'rotation-start' | 'node-ready' | 'rotation-done'
  | 'expire-after-breach' | 'blocked-by-gate' | 'no-eligible-claim'

export interface SimEvent {
  kind: EventKind
  at: string
  until?: string
  node?: string
  replacement?: string
  surgeless?: boolean
  gate?: string
  census?: Record<string, number>
}

/** What simulate() returns. `error` is set ONLY for input that cannot be run at
 *  all; everything else is a diagnostic, so the page can always render something. */
export interface SimResponse {
  error?: string
  result?: SimResult
  events?: SimEvent[]
  diagnostics?: Diagnostic[]
  partial?: boolean
}

const UNITS: Record<string, number> = {
  ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000,
}

/** Parse a Go duration string to milliseconds; null when it is not one.
 *  Note Go has NO day unit — "7d" is not a duration and must not be invented. */
export function parseGoDuration(s: string): number | null {
  if (!s) return null
  const m = s.trim().match(/^-?(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/)
  if (!m) return null
  let ms = 0
  for (const [, value, unit] of s.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)) {
    ms += parseFloat(value) * UNITS[unit]
  }
  return s.trim().startsWith('-') ? -ms : ms
}

/** N nodes named node-1..node-N, their createdAt spread evenly over `spread`
 *  (a Go duration; "0s" puts them all at the same instant). */
export function generateNodes(count: number, firstCreatedAt: string, spread: string): FleetNode[] {
  const first = new Date(firstCreatedAt).getTime()
  const total = parseGoDuration(spread) ?? 0
  const step = count > 1 ? total / (count - 1) : 0
  return Array.from({ length: count }, (_, i) => ({
    name: `node-${i + 1}`,
    createdAt: new Date(first + i * step).toISOString(),
  }))
}

/** The horizon a visitor lands on: from the earliest node to the last node's
 *  SECOND-generation deadline, so at least one full expireAfter generation is on
 *  screen and a breach is visible.
 *
 *  Each node contributes createdAt + 2 x its EFFECTIVE expireAfter (its own
 *  override, else the template) — a heterogeneous fleet would otherwise push the
 *  overriding node's deadline off the right edge, and the page would report no
 *  breach for time it never simulated. */
export function defaultHorizon(fleet: Fleet): Horizon {
  const template = parseGoDuration(fleet.expireAfter) ?? 0
  // fleet.nodes CAN be empty: the UI has a node-count field a visitor can set
  // to 0, and generateNodes(0, ...) returns []. Math.min/max(...[]) is
  // +/-Infinity, and new Date(Infinity).toISOString() throws RangeError — inside
  // a Vue watcher that would blank the whole page with no message. Fall back to
  // a fixed, reproducible anchor so this function stays total.
  if (fleet.nodes.length === 0) {
    const start = new Date(DEFAULT_FIRST_CREATED_AT).getTime()
    return { start: new Date(start).toISOString(), end: new Date(start + 2 * template).toISOString() }
  }
  const created = fleet.nodes.map(n => new Date(n.createdAt).getTime())
  const ends = fleet.nodes.map((n, i) =>
    created[i] + 2 * (parseGoDuration(n.expireAfter ?? '') ?? template))
  return {
    start: new Date(Math.min(...created)).toISOString(),
    end: new Date(Math.max(...ends)).toISOString(),
  }
}

/** The second argument of simulate(): {fleet, env, options} as one object. */
export function buildRequest(fleet: Fleet, env: Env, horizon: Horizon): SimRequest {
  const clean: Env = {}
  if (env.provisioning) clean.provisioning = env.provisioning
  if (env.drain) clean.drain = env.drain
  return { fleet, env: clean, options: horizon }
}

/** The manifest the page opens on. It is a FULL RotationPolicy — apiVersion and
 *  kind are both required exactly, because a manifest the cluster would not admit
 *  must not produce a timeline. */
export const DEFAULT_POLICY_YAML = `apiVersion: noderotation.io/v1alpha1
kind: RotationPolicy
metadata:
  name: weekly
spec:
  nodePoolSelector:
    matchLabels:
      workload: api
  ageThreshold: auto
  minRotationChances: 2
  maintenanceWindows:
    # Two occurrences a week, not one: with minRotationChances=2, a single
    # weekly window (worst-case period P = 168h) forces K*P = 336h, which
    # only clears the AVeryAggressive warning (needs ageThreshold A >= P) at
    # an expireAfter beyond the Auto Mode hard cap (§1.1) — there is no
    # expireAfter that is simultaneously under the cap and free of that
    # warning against a single-day window. Adding Wed halves P and keeps A
    # comfortably above it at expireAfter = 480h (see DEFAULT_FLEET below).
    - timezone: UTC
      days: [Wed, Sat]
      start: "02:00"
      end: "06:00"
  surge:
    readyTimeout: 15m
    cooldownAfter: 10m
    provisioningEstimate: 5m
    drainEstimate: 10m
    forcefulFallback:
      enabled: false
`

/** A fixed date, not `now`: the page a visitor lands on must be reproducible, and
 *  the smoke test asserts against exactly these defaults. Shared by DEFAULT_FLEET
 *  and defaultHorizon's empty-fleet fallback so the two cannot drift apart. */
export const DEFAULT_FIRST_CREATED_AT = '2026-01-01T00:00:00Z'

// 480h (20d), not 720h (30d): the EKS Auto Mode hard cap is 21d (504h) on
// expireAfter + terminationGracePeriod combined (spec §1.1). 480h + the 1h
// terminationGracePeriod below leaves 23h of headroom under that cap, so the
// page a visitor lands on demonstrates a policy Auto Mode would actually
// admit instead of opening on a HardCapExceeded warning about our own
// default. Do not round this back up to 720h.
export const DEFAULT_FLEET: Fleet = {
  expireAfter: '480h',
  terminationGracePeriod: '1h',
  nodes: generateNodes(3, DEFAULT_FIRST_CREATED_AT, '168h'),
}

/** Blank on purpose: blank means "the policy's own estimates", which is what makes
 *  an untouched simulation self-consistent. */
export const DEFAULT_ENV: Env = {}
