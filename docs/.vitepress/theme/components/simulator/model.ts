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

/** How a generation came into existence. A single "surgeless" boolean cannot tell an
 *  initial node from a surged replacement — both would be false. */
export type BirthMode = 'initial' | 'surge' | 'surgeless'

/** Where a generation's drain cap came from: its own terminationGracePeriod, or the
 *  controller's fixed fallback. The page must never re-derive that constant. */
export type DrainCapSource = 'explicit' | 'fallback'

/** One generation of one fleet slot (internal/sim). It carries the facts the event
 *  stream cannot: `node-ready` is emitted under the OLD node's name, so a replacement's
 *  own ready instant is unrecoverable from the events, and the eligibility boundary
 *  depends on the claim's own tGP, which no event carries. */
export interface SimGeneration {
  slot: number
  gen: number
  name: string
  birthMode: BirthMode
  /** Absent for an initial node. 0 is a REAL predecessor, not an absent one. */
  predecessorGen?: number
  createdAt: string
  expireAfter: string
  drainCap: string
  drainCapSource: DrainCapSource
  deadline: string
  /** EXCLUSIVE: the trigger is a strict inequality, so a node exactly at this instant is
   *  not yet eligible. It is labelled "eligible after", never as an event. */
  eligibilityBoundary: string
  /** Only on the surge path, and only once the replacement became Ready. */
  readyAt?: string
  /** A replacement whose rotation has not completed. Omitted when false. */
  provisional?: boolean
}

export type RotationMode = 'surge' | 'surgeless'

/** The relay between two generations of a slot. */
export interface SimRotation {
  slot: number
  fromGen: number
  /** Absent while the produced generation does not exist yet: a surge-less rotation
   *  still draining has staged no replacement (it is born at `done`). */
  toGen?: number
  mode: RotationMode
  start: string
  /** ABSENT — not zero — while the rotation is in flight; always absent surge-less. */
  ready?: string
  done?: string
}

/** One OBSERVED occurrence of the effective (union) window schedule. The clipped flags
 *  mark a boundary that is an artifact of the horizon rather than a real transition of
 *  the schedule; both are omitted when false. */
export interface SimWindow {
  start: string
  end: string
  startClipped?: boolean
  endClipped?: boolean
}

/** What simulate() returns. `error` is set ONLY for input that cannot be run at
 *  all; everything else is a diagnostic, so the page can always render something. */
export interface SimResponse {
  error?: string
  result?: SimResult
  events?: SimEvent[]
  generations?: SimGeneration[]
  rotations?: SimRotation[]
  windows?: SimWindow[]
  diagnostics?: Diagnostic[]
  partial?: boolean
  /** The last instant the simulation actually PROCESSED — the requested end on a normal
   *  run, earlier when the step budget was exhausted. Nothing in the response lies beyond
   *  it, and a bar still alive there CONTINUES rather than being truncated. */
  simulatedThrough?: string
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
  // firstCreatedAt flows straight from the node-generator UI field; a malformed
  // value must fall back rather than mint "Invalid Date" strings into the fleet.
  const parsedFirst = new Date(firstCreatedAt).getTime()
  const first = Number.isNaN(parsedFirst) ? new Date(DEFAULT_FIRST_CREATED_AT).getTime() : parsedFirst
  const total = parseGoDuration(spread) ?? 0
  const step = count > 1 ? total / (count - 1) : 0
  return Array.from({ length: count }, (_, i) => ({
    name: `node-${i + 1}`,
    createdAt: new Date(first + i * step).toISOString(),
  }))
}

/** The horizon a visitor lands on: DEFAULT_COVERAGE lifetimes of the fleet. */
export function defaultHorizon(fleet: Fleet): Horizon {
  return horizonForCoverage(fleet, DEFAULT_COVERAGE)
}

/** The horizon-control multipliers: how many EFFECTIVE NODE LIFETIMES the horizon
 *  covers. Deliberately not "generations": staggered createdAt, per-node expireAfter
 *  overrides, window waits, cooldown, the forceful fallback and a fatal policy all break
 *  that equivalence, so a control promising N generations would be lying whenever any of
 *  them applied. */
export const COVERAGE_CHOICES = [1, 2, 3] as const
export type Coverage = (typeof COVERAGE_CHOICES)[number]

/** 2x: one full expireAfter generation past the first, so a second generation — and a
 *  breach, when the policy cannot make it — is on screen without the visitor doing
 *  anything. */
export const DEFAULT_COVERAGE: Coverage = 2

/** The horizon that covers `coverage` EFFECTIVE node lifetimes: from the earliest node
 *  to the last node's coverage-th deadline.
 *
 *  Each node contributes createdAt + coverage x its EFFECTIVE expireAfter (its own
 *  override, else the template) — a heterogeneous fleet would otherwise push the
 *  overriding node's deadline off the right edge, and the page would report no breach for
 *  time it never simulated. The multiplier generalises exactly that bound and must not
 *  regress it. */
export function horizonForCoverage(fleet: Fleet, coverage: number): Horizon {
  const template = parseGoDuration(fleet.expireAfter) ?? 0
  const n = Number.isFinite(coverage) && coverage > 0 ? coverage : DEFAULT_COVERAGE
  // fleet.nodes CAN be empty: the UI has a node-count field a visitor can set
  // to 0, and generateNodes(0, ...) returns []. Math.min/max(...[]) is
  // +/-Infinity, and new Date(Infinity).toISOString() throws RangeError — inside
  // a Vue watcher that would blank the whole page with no message. Fall back to
  // a fixed, reproducible anchor so this function stays total.
  //
  // The same applies to a MALFORMED createdAt: FleetInput.vue exposes it as a
  // raw editable text input, so a visitor can type "bad" into it, and
  // PolicySimulator.vue recomputes the horizon on every keystroke while
  // unpinned. The horizon is presentation only — validity of the fleet is the
  // wasm module's to judge, and it must see the malformed value unchanged so
  // its own error reaches the user. So: bound the horizon on the nodes that DO
  // parse, and only fall back to the fixed anchor when NONE of them do (which
  // subsumes the empty-fleet case below).
  const parseable = fleet.nodes
    .map(n => ({ n, createdAt: new Date(n.createdAt).getTime() }))
    .filter(({ createdAt }) => !Number.isNaN(createdAt))
  if (parseable.length === 0) {
    const start = new Date(DEFAULT_FIRST_CREATED_AT).getTime()
    return { start: new Date(start).toISOString(), end: new Date(start + n * template).toISOString() }
  }
  const created = parseable.map(({ createdAt }) => createdAt)
  const ends = parseable.map(({ n: node, createdAt }) =>
    createdAt + n * (parseGoDuration(node.expireAfter ?? '') ?? template))
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
