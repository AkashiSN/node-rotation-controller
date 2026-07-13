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
import { parseDocument } from 'yaml'

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
  const days = doc.getIn(['spec', 'maintenanceWindows', 0, 'days'], true) as { toJSON?: () => string[] } | undefined

  return {
    form: {
      timezone: str(PATHS.timezone),
      days: days?.toJSON ? days.toJSON() : [],
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
  doc.setIn(PATHS[field], value)
  // yaml's default stringify pads flow collections ("[ Sat ]"), which would
  // reformat every untouched flow-style `days: [Sat]` on ANY unrelated edit.
  // Disable the padding so an edit changes only the field it touches.
  return doc.toString({ flowCollectionPadding: false })
}
