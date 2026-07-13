// docs/.vitepress/theme/components/simulator/policyYaml.ts
//
// The YAML ⇄ form projection. The YAML textarea is AUTHORITATIVE: the form is a
// projection of it, and a form edit MUTATES the parsed document (yaml's AST) in
// place rather than rebuilding a manifest from the form's own fields.
//
// That distinction is the whole point. A rebuild would silently drop every key the
// form does not model — an unsupported field, a field added in a future CRD version,
// a second maintenanceWindows entry — and so turn a manifest Go's strict decoder
// REJECTS into one it accepts, as a side effect of an unrelated form edit. Mutating
// the AST cannot do that, and it preserves comments and key order for free.
//
// This parser drives the form and nothing else. It never gates the simulate() call:
// the raw YAML is always what is sent, so the errors a user sees are the
// controller's own (strict decode, apiVersion + kind exact).
import { isSeq, parseDocument } from 'yaml'

export interface PolicyForm {
  timezone: string
  days: string[]
  start: string
  end: string
  minRotationChances: number | null
  ageThreshold: string
  provisioningEstimate: string
  drainEstimate: string
  cooldownAfter: string
  readyTimeout: string
  forcefulFallback: boolean
  /** How many maintenanceWindows entries beyond the first. The form edits
   *  entry [0] only; the rest are the YAML's, and survive untouched. */
  extraWindows: number
}

/** Where each form field lives in the manifest. The form edits maintenanceWindows[0]
 *  only — the effective window is the UNION of all entries (spec §3.1), and the
 *  others stay the YAML's business. */
const PATHS: Record<Exclude<keyof PolicyForm, 'extraWindows'>, (string | number)[]> = {
  timezone: ['spec', 'maintenanceWindows', 0, 'timezone'],
  days: ['spec', 'maintenanceWindows', 0, 'days'],
  start: ['spec', 'maintenanceWindows', 0, 'start'],
  end: ['spec', 'maintenanceWindows', 0, 'end'],
  minRotationChances: ['spec', 'minRotationChances'],
  ageThreshold: ['spec', 'ageThreshold'],
  provisioningEstimate: ['spec', 'surge', 'provisioningEstimate'],
  drainEstimate: ['spec', 'surge', 'drainEstimate'],
  cooldownAfter: ['spec', 'surge', 'cooldownAfter'],
  readyTimeout: ['spec', 'surge', 'readyTimeout'],
  forcefulFallback: ['spec', 'surge', 'forcefulFallback', 'enabled'],
}

/** The OPTIONAL fields: clearing one means "use the default", and the way a CRD says
 *  that is an ABSENT key — not an empty string. Writing `cooldownAfter: ""` produces a
 *  manifest Go rejects outright (`time: invalid duration ""`), which blanks the result
 *  and the timeline over what is an ordinary edit. So a cleared optional field is
 *  DELETED from the document.
 *
 *  The window's timezone/days/start/end are REQUIRED and deliberately absent from this
 *  set: clearing one is a real error, and the user must read it in the controller's own
 *  words rather than have the page quietly drop the key. (minRotationChances is guarded
 *  in the form itself, which leaves the YAML untouched while the field is empty.) */
const OPTIONAL: ReadonlySet<keyof PolicyForm> = new Set([
  'ageThreshold', 'provisioningEstimate', 'drainEstimate', 'cooldownAfter', 'readyTimeout',
])

const EMPTY: PolicyForm = {
  timezone: '', days: [], start: '', end: '',
  minRotationChances: null, ageThreshold: '',
  provisioningEstimate: '', drainEstimate: '', cooldownAfter: '', readyTimeout: '',
  forcefulFallback: false, extraWindows: 0,
}

/** Project the form off the YAML. A YAML the parser rejects yields `error` and an
 *  empty form — the caller keeps showing the last good projection, greyed out, and
 *  still sends the raw YAML to simulate() so the user sees Go's error. */
export function projectPolicy(yamlText: string): { form: PolicyForm; error?: string } {
  let doc
  try {
    doc = parseDocument(yamlText)
  } catch (e) {
    return { form: { ...EMPTY }, error: e instanceof Error ? e.message : String(e) }
  }
  if (doc.errors.length > 0) {
    return { form: { ...EMPTY }, error: doc.errors[0].message }
  }

  const get = (path: (string | number)[]) => doc.getIn(path)
  const str = (path: (string | number)[]) => {
    const v = get(path)
    return v === undefined || v === null ? '' : String(v)
  }

  const windows = doc.getIn(['spec', 'maintenanceWindows']) as { items?: unknown[] } | undefined
  // The YAML is the user's, and `days: Sat` (a bare scalar) is a valid but easy typo
  // for `days: [Sat]` — projecting it would hand the form a string where it promises
  // string[]. Only an actual sequence projects to its items; a scalar, null, or
  // absent `days` all project to `[]`. The raw YAML still goes to the Go decoder
  // unchanged, so the real error (the user's typo) is still reported there.
  const daysNode = doc.getIn(['spec', 'maintenanceWindows', 0, 'days'], true)
  const days = isSeq(daysNode) ? ((daysNode.toJSON() as string[]) ?? []) : []

  return {
    form: {
      timezone: str(PATHS.timezone),
      days,
      start: str(PATHS.start),
      end: str(PATHS.end),
      minRotationChances: get(PATHS.minRotationChances) as number ?? null,
      ageThreshold: str(PATHS.ageThreshold),
      provisioningEstimate: str(PATHS.provisioningEstimate),
      drainEstimate: str(PATHS.drainEstimate),
      cooldownAfter: str(PATHS.cooldownAfter),
      readyTimeout: str(PATHS.readyTimeout),
      forcefulFallback: get(PATHS.forcefulFallback) === true,
      extraWindows: Math.max(0, (windows?.items?.length ?? 0) - 1),
    },
  }
}

/** Write one form field back into the manifest, mutating the parsed document and
 *  re-serialising it. Everything the form does not model is carried through
 *  untouched — including the keys that make Go reject the manifest. */
export function applyPolicyEdit(yamlText: string, field: keyof PolicyForm, value: unknown): string {
  if (field === 'extraWindows') return yamlText   // derived, not editable
  const doc = parseDocument(yamlText)
  if (doc.errors.length > 0) return yamlText      // the form is disabled anyway
  if (value === '' && OPTIONAL.has(field)) {
    doc.deleteIn(PATHS[field])                    // cleared optional field => "use the default"
  } else {
    doc.setIn(PATHS[field], value)
  }
  // yaml's default stringify pads flow collections ("[ Sat ]"), which would
  // reformat every untouched flow-style `days: [Sat]` on ANY unrelated edit.
  // Disable the padding so an edit changes only the field it touches.
  //
  // yaml's default `lineWidth: 80` also line-folds any plain scalar that crosses
  // 80 columns and contains whitespace — e.g. a long unmodeled description — onto
  // multiple lines, again on ANY unrelated edit. `lineWidth: 0` disables folding.
  // This does NOT preserve the source's original indent width (yaml re-emits with
  // its own default indent); that's not achievable through this API and is out of
  // scope here.
  return doc.toString({ flowCollectionPadding: false, lineWidth: 0 })
}
