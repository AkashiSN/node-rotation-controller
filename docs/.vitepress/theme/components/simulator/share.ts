// docs/.vitepress/theme/components/simulator/share.ts
//
// The ?s= codec: the page's INPUTS as a link.
//
// PURE by design — no Vue, no VitePress, no window. It uses only globals that both the
// browser and Node have (Compression Streams, TextEncoder, btoa/atob), which is what keeps
// it on `node --test` alongside the rest of the simulator's core.
//
// What a link carries is the QUESTION (policy, fleet, env, horizon), never the ANSWER. The
// receiver re-runs the wasm module, so a link cannot show a timeline the current controller
// would not produce, and it keeps meaning what it says as that controller evolves.
import type { Env, Fleet, Horizon } from './model.ts'

/** The query parameter a shared link travels in. NOT the hash: `#` is VitePress's
 *  heading-anchor namespace, and a state blob there would collide with a heading link. */
export const SHARE_PARAM = 's'

/** The payload version. It exists so a future change of shape is DETECTED (and refused)
 *  rather than silently misread as v1. */
const VERSION = 1

export interface ShareState {
  policy: string
  fleet: Fleet
  env: Env
  horizon: Horizon
}

export type DecodeResult = { state: ShareState } | { error: string }

/** Is the Compression Streams API present? Gates the button. SSR-safe: no window access. */
export function shareSupported(): boolean {
  return typeof CompressionStream === 'function' && typeof DecompressionStream === 'function'
}

/** Encode to the value of `?s=`. Rejects only where the codec itself is unavailable. */
export async function encodeState(state: ShareState): Promise<string> {
  if (!shareSupported()) throw new Error('this browser cannot compress the link')
  const json = JSON.stringify({ v: VERSION, ...state })
  return base64urlEncode(await deflate(json))
}

/** Decode `?s=`. NEVER throws — an unreadable link is a value the page renders, not a crash. */
export async function decodeState(value: string): Promise<DecodeResult> {
  if (!shareSupported()) return { error: 'this browser cannot read a shared link' }
  let text: string
  try {
    text = await inflate(base64urlDecode(value))
  } catch {
    return { error: 'the link is damaged' }
  }
  let payload: unknown
  try {
    payload = JSON.parse(text)
  } catch {
    return { error: 'the link is damaged' }
  }
  return validate(payload)
}

/** Parse what a stranger sent. The model must never be handed a half-shaped object and
 *  discover it three components deep, so the shape is checked here, in full. */
function validate(payload: unknown): DecodeResult {
  if (!isObject(payload)) return { error: 'the link is damaged' }
  if (payload.v !== VERSION) return { error: `the link uses an unknown format (v${String(payload.v)})` }

  const { policy, fleet, env, horizon } = payload
  if (typeof policy !== 'string') return { error: 'the link is damaged' }
  if (!isObject(fleet) || typeof fleet.expireAfter !== 'string' || !Array.isArray(fleet.nodes)) {
    return { error: 'the link is damaged' }
  }
  if (!fleet.nodes.every(n => isObject(n) && typeof n.name === 'string' && typeof n.createdAt === 'string')) {
    return { error: 'the link is damaged' }
  }
  if (!isObject(env) || !isObject(horizon)) return { error: 'the link is damaged' }
  if (typeof horizon.start !== 'string' || typeof horizon.end !== 'string') {
    return { error: 'the link is damaged' }
  }

  return {
    state: {
      policy,
      fleet: fleet as unknown as Fleet,
      env: env as unknown as Env,
      horizon: horizon as unknown as Horizon,
    },
  }
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

async function deflate(text: string): Promise<Uint8Array> {
  const stream = new Blob([text]).stream().pipeThrough(new CompressionStream('deflate-raw'))
  return new Uint8Array(await new Response(stream).arrayBuffer())
}

async function inflate(bytes: Uint8Array): Promise<string> {
  const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream('deflate-raw'))
  return new Response(stream).text()
}

/** base64url: no padding, and no character that needs percent-encoding, so the value
 *  survives being pasted through anything that re-linkifies text. */
function base64urlEncode(bytes: Uint8Array): string {
  let binary = ''
  for (const b of bytes) binary += String.fromCharCode(b)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function base64urlDecode(value: string): Uint8Array {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) throw new Error('not base64url')
  const binary = atob(value.replace(/-/g, '+').replace(/_/g, '/'))
  return Uint8Array.from(binary, c => c.charCodeAt(0))
}
