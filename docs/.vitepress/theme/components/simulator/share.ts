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

/** A generous ceiling on the `?s=` value itself, shared by BOTH ends of the codec.
 *  decodeState() refuses anything past it before a decompression stream is even opened; the
 *  default link is 966 chars and a 50-node fleet (FleetInput.vue's own generator cap) sits
 *  far below it, so anything past it is already unpasteable. encodeState() enforces the
 *  SAME constant on its own output — a producer that could exceed the consumer's ceiling
 *  would mint links the page that made them refuses to open. */
export const MAX_VALUE_CHARS = 16384

/** deflate-raw reaches ~1000:1 on repetitive input, so an innocuous-looking `?s=` well
 *  under MAX_VALUE_CHARS can still inflate to gigabytes on the visitor's main thread. The
 *  INPUT ceiling above cannot catch this — only the decompressed size can — so inflate()
 *  reads its stream through a byte budget and stops the moment this is exceeded, instead
 *  of buffering the whole result with Response.text(). */
const MAX_INFLATED_BYTES = 1024 * 1024

/** The UI's own fleet generator (FleetInput.vue) clamps at 50 nodes. 200 is a generous
 *  ceiling next to that, not a target: a link is not the place to grow the fleet past what
 *  the page itself can ever produce, and FleetInput.vue renders one row per node. */
const MAX_FLEET_NODES = 200

export interface ShareState {
  policy: string
  fleet: Fleet
  env: Env
  horizon: Horizon
}

/** `code` is what the page acts on (which message to show); `message` is the English
 *  detail, kept for logging/debugging — it is never shown to a Japanese reader as-is. */
export type DecodeError = { code: 'damaged' | 'version'; message: string }

export type DecodeResult = { state: ShareState } | { error: DecodeError }

/** Thrown by encodeState() when the encoded value would exceed MAX_VALUE_CHARS — a distinct
 *  type, not a generic Error, so a caller can show a specific "too big" message rather than
 *  the generic "could not build" one without resorting to matching English error text. */
export class ShareTooLargeError extends Error {}

/** Is the Compression Streams API present? Gates the button. SSR-safe: no window access. */
export function shareSupported(): boolean {
  return typeof CompressionStream === 'function' && typeof DecompressionStream === 'function'
}

/** Encode to the value of `?s=`. `encodeState` is allowed to throw — unlike decodeState, it
 *  is never handed a stranger's input, only the page's own current state — but it must never
 *  succeed at producing a link decodeState itself would refuse. Two failure modes: the codec
 *  is unavailable, or the encoded value would exceed MAX_VALUE_CHARS (a large-but-otherwise-
 *  valid policy YAML or fleet can grow the state past what a URL can carry). */
export async function encodeState(state: ShareState): Promise<string> {
  if (!shareSupported()) throw new Error('this browser cannot compress the link')
  const json = JSON.stringify({ v: VERSION, ...state })
  const value = base64urlEncode(await deflate(json))
  // Same ceiling, same constant, as decodeState's own length check below — a link the
  // producer will hand out must be a link the consumer will accept.
  if (value.length > MAX_VALUE_CHARS) {
    throw new ShareTooLargeError(`encoded state is ${value.length} chars, over the ${MAX_VALUE_CHARS}-char ceiling`)
  }
  return value
}

/** Decode `?s=`. NEVER throws — an unreadable link is a value the page renders, not a crash. */
export async function decodeState(value: string): Promise<DecodeResult> {
  if (!shareSupported()) return damaged('this browser cannot read a shared link')
  // Reject on LENGTH before opening a decompression stream at all: a value this long is
  // already unpasteable, so there is nothing to gain by inflating it first.
  if (value.length > MAX_VALUE_CHARS) return damaged('the link is too long to be a real one')
  let text: string
  try {
    text = await inflate(base64urlDecode(value))
  } catch {
    return damaged()
  }
  let payload: unknown
  try {
    payload = JSON.parse(text)
  } catch {
    return damaged()
  }
  return validate(payload)
}

/** Parse what a stranger sent. The model must never be handed a half-shaped object and
 *  discover it three components deep, so the shape is checked here, in full — EVERY field,
 *  including the ones that are optional in the model. An optional field left untyped is not
 *  "checked in full": model.ts's parseGoDuration() does `s.trim()` on whatever it is handed,
 *  so a number instead of a string in, say, a node's expireAfter used to sail through this
 *  function and throw three components away, in the horizon watcher — not here. */
function validate(payload: unknown): DecodeResult {
  if (!isObject(payload)) return damaged()
  // 'version' is reserved for a link this page can PROVE is newer — a v that is a number
  // strictly greater than what we understand. A missing v, a non-number v, or an OLDER v is
  // not "newer" at all; calling it that would tell the reader something false, so those fall
  // through to the same 'damaged' every other malformed shape gets.
  if (typeof payload.v === 'number' && payload.v > VERSION) {
    return { error: { code: 'version', message: `the link uses a newer format (v${payload.v})` } }
  }
  if (payload.v !== VERSION) return damaged()

  const { policy, fleet, env, horizon } = payload
  if (typeof policy !== 'string') return damaged()
  if (!isObject(fleet) || typeof fleet.expireAfter !== 'string' || !Array.isArray(fleet.nodes)) {
    return damaged()
  }
  if (!isOptionalString(fleet.terminationGracePeriod)) return damaged()
  // A fleet this large cannot have come from the page's own generator (FleetInput.vue caps
  // it at 50), and letting it through would make FleetInput render one row per node.
  if (fleet.nodes.length > MAX_FLEET_NODES) return damaged()
  if (!fleet.nodes.every(n =>
    isObject(n) && typeof n.name === 'string' && typeof n.createdAt === 'string' &&
    isOptionalString(n.expireAfter) && isOptionalString(n.terminationGracePeriod))) {
    return damaged()
  }
  if (!isObject(env) || !isObject(horizon)) return damaged()
  if (!isOptionalString(env.provisioning) || !isOptionalString(env.drain)) return damaged()
  if (typeof horizon.start !== 'string' || typeof horizon.end !== 'string') {
    return damaged()
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

function damaged(message = 'the link is damaged'): { error: DecodeError } {
  return { error: { code: 'damaged', message } }
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

/** Every optional field in the model (env.provisioning/drain, the fleet- and per-node-level
 *  expireAfter/terminationGracePeriod overrides) means "absent, or a string" — never a
 *  number, bool, or object. `undefined` must pass; anything else that is not a string must
 *  not. */
function isOptionalString(v: unknown): v is string | undefined {
  return v === undefined || typeof v === 'string'
}

async function deflate(text: string): Promise<Uint8Array> {
  const stream = new Blob([text]).stream().pipeThrough(new CompressionStream('deflate-raw'))
  return new Uint8Array(await new Response(stream).arrayBuffer())
}

/** Read the decompressed stream through its own reader with a BYTE BUDGET, rather than
 *  buffering the whole thing with `Response.text()`. deflate-raw can reach ~1000:1 on
 *  repetitive input, so a value that passed the character-length check above can still be a
 *  bomb; the moment the budget is exceeded, the stream is cancelled and reading stops — the
 *  visitor's tab never holds the full inflated payload in memory. */
async function inflate(bytes: Uint8Array): Promise<string> {
  const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream('deflate-raw'))
  const reader = stream.getReader()
  const chunks: Uint8Array[] = []
  let total = 0
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      total += value.byteLength
      if (total > MAX_INFLATED_BYTES) {
        await reader.cancel()
        throw new Error('decompressed payload exceeds the byte budget')
      }
      chunks.push(value)
    }
  } finally {
    reader.releaseLock()
  }
  const out = new Uint8Array(total)
  let offset = 0
  for (const chunk of chunks) {
    out.set(chunk, offset)
    offset += chunk.byteLength
  }
  return new TextDecoder().decode(out)
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
