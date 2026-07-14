// node --test docs/.vitepress/theme/components/simulator/share.test.ts
//
// The codec behind ?s=. It is fed by strangers — a link pasted through a chat client that
// ate its tail, a link from an older payload version — so every case here is about what it
// does with input it did not produce. It must never throw: the page has to survive a bad
// link, not crash on it.
import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { test } from 'node:test'

import { DEFAULT_ENV, DEFAULT_FLEET, DEFAULT_POLICY_YAML, horizonForCoverage } from './model.ts'
import { decodeState, encodeState, shareSupported, type ShareState } from './share.ts'

const DEFAULT_STATE: ShareState = {
  policy: DEFAULT_POLICY_YAML,
  fleet: structuredClone(DEFAULT_FLEET),
  env: { ...DEFAULT_ENV },
  horizon: horizonForCoverage(DEFAULT_FLEET, 2),
}

const decoded = async (state: ShareState) => decodeState(await encodeState(state))

test('the Compression Streams API is what the codec needs, and this runtime has it', () => {
  assert.equal(shareSupported(), true)
})

test('a round trip returns the state that went in', async () => {
  const got = await decoded(DEFAULT_STATE)
  assert.deepEqual(got, { state: DEFAULT_STATE })
})

test('the YAML survives VERBATIM — comments, blank lines, non-ASCII, trailing newline', async () => {
  // The YAML is the authoritative artifact on the page. A codec that "normalised" it would
  // hand the receiver a different document than the sharer was looking at.
  const policy = 'apiVersion: noderotation.io/v1alpha1\n# コメント\n\nkind: RotationPolicy\n'
  const got = await decoded({ ...DEFAULT_STATE, policy })
  assert.deepEqual(got, { state: { ...DEFAULT_STATE, policy } })
})

test('a large fleet round trips', async () => {
  const nodes = Array.from({ length: 60 }, (_, i) => ({
    name: `node-${i + 1}`,
    createdAt: new Date(Date.UTC(2026, 0, 1) + i * 3_600_000).toISOString(),
  }))
  const state = { ...DEFAULT_STATE, fleet: { ...DEFAULT_STATE.fleet, nodes } }
  const got = await decoded(state)
  assert.deepEqual(got, { state })
})

test('the default state encodes to a link short enough to paste', async () => {
  // Measured in a real browser, the default state currently encodes to 966 chars. The full
  // URL is origin + path (~60 chars) + this value, and the practical floor for tools that
  // still wrap or truncate long links (chat clients, GitHub comments) is ~2000 chars. The
  // threshold below is not "a bit above today's number" — pinning it that tight would fail
  // the test on any innocent tweak to the default policy YAML or fleet. It exists to catch a
  // payload change that makes links unpasteable, with real headroom for the payload to grow.
  const value = await encodeState(DEFAULT_STATE)
  assert.ok(value.length < 1500, `encoded to ${value.length} chars`)
  assert.match(value, /^[A-Za-z0-9_-]+$/) // base64url: no padding, nothing to percent-encode
})

test('an unreadable payload is a VALUE, not an exception', async () => {
  for (const bad of [
    '',                       // empty
    '!!!not base64!!!',       // not base64url
    'aGVsbG8',                // valid base64url, not deflate
    await deflated('not json at all'),
    await deflated('42'),                       // JSON, not an object
    await deflated(JSON.stringify({ v: 1 })),   // object, no fields
    await deflated(JSON.stringify({ v: 1, policy: 'x', fleet: {}, env: {}, horizon: {} })), // fleet.nodes missing
    await deflated(JSON.stringify({ ...DEFAULT_STATE, v: 1, fleet: { expireAfter: '336h', nodes: 'no' } })),
  ]) {
    const got = await decodeState(bad)
    assert.ok('error' in got, `expected an error for ${JSON.stringify(bad).slice(0, 40)}`)
    assert.equal((got as { error: { code: string } }).error.code, 'damaged')
  }
})

test('an unknown payload version is refused, not guessed at, and carries its own code', async () => {
  const got = await decodeState(await deflated(JSON.stringify({ ...DEFAULT_STATE, v: 99 })))
  assert.ok('error' in got)
  assert.equal((got as { error: { code: string } }).error.code, 'version')
})

test('a payload with no v at all is damaged, not mistaken for "a newer version"', async () => {
  // Only a v that is a NUMBER STRICTLY GREATER than VERSION describes a genuinely newer link.
  // A missing v is ordinary corruption (an empty object decodes fine as JSON, so it reaches
  // validate() at all) — it must not be told "this comes from a newer version of the
  // simulator", which used to happen because `payload.v !== VERSION` was true for undefined too.
  const got = await decodeState(await deflated(JSON.stringify({})))
  assert.ok('error' in got)
  assert.equal((got as { error: { code: string } }).error.code, 'damaged')
})

test('a value past the character ceiling is refused before it is ever decompressed', async () => {
  // The default link is 966 chars and a 50-node fleet (the UI's own generator cap) sits far
  // below this; anything past it is already unpasteable, so it is rejected on sight rather
  // than handed to the decompressor.
  //
  // 'A'.repeat(16385) is valid base64url but NOT valid deflate, so this alone does not prove
  // the ceiling fired: with MAX_VALUE_CHARS deleted, inflate() would still throw on this
  // garbage and decodeState() would still land on 'damaged' — just via the generic message,
  // not the ceiling's own. Asserting the exact message is what tells the two apart.
  const got = await decodeState('A'.repeat(16385))
  assert.ok('error' in got)
  assert.deepEqual((got as { error: { code: string; message: string } }).error, {
    code: 'damaged',
    message: 'the link is too long to be a real one',
  })
})

test('a genuinely valid, oversized payload is refused by the ceiling alone', async () => {
  // Unlike the case above, this IS a real deflate of a real, fully-valid state — long enough
  // (via a padded policy field) that its encoded value exceeds MAX_VALUE_CHARS. If the ceiling
  // were deleted, this exact payload would decode successfully (a round trip), so a failure
  // here can only be the length check itself, not inflate(), JSON.parse(), or validate().
  const policy = DEFAULT_POLICY_YAML + randomBytes(15000).toString('hex')
  const state = { ...DEFAULT_STATE, policy }
  const value = await encodeState(state)
  assert.ok(value.length > 16384, `expected an oversized value, got ${value.length} chars`)
  const got = await decodeState(value)
  assert.ok('error' in got)
  assert.equal((got as { error: { code: string } }).error.code, 'damaged')
})

test('a decompression bomb is capped by OUTPUT size, not buffered whole', async () => {
  // deflate-raw reaches ~1000:1 on repetitive input, so a few-KB value — comfortably under
  // the character ceiling above — can still inflate to gigabytes. The forged payload here is
  // 5 MB of one repeated character: small enough on the wire to sail past the length check,
  // but the decoder must still refuse to buffer the inflated result whole.
  const bomb = await deflated('x'.repeat(5 * 1024 * 1024))
  assert.ok(bomb.length < 16384, `forged bomb encodes to ${bomb.length} chars, expected under the ceiling`)
  const got = await decodeState(bomb)
  assert.ok('error' in got)
  assert.equal((got as { error: { code: string } }).error.code, 'damaged')
})

test('a fleet past the 200-node cap is refused', async () => {
  // FleetInput.vue's own generator clamps at 50; 200 is a generous ceiling next to that, not
  // a target. A link with 200_000 nodes must not reach FleetInput and render a 200_000-row
  // table on the visitor's page.
  const nodes = Array.from({ length: 201 }, (_, i) => ({
    name: `node-${i + 1}`,
    createdAt: new Date(Date.UTC(2026, 0, 1) + i * 3_600_000).toISOString(),
  }))
  const state = { ...DEFAULT_STATE, fleet: { ...DEFAULT_STATE.fleet, nodes } }
  const got = await decoded(state)
  assert.ok('error' in got)
  assert.equal((got as { error: { code: string } }).error.code, 'damaged')
})

test('exactly 200 nodes is still accepted', async () => {
  const nodes = Array.from({ length: 200 }, (_, i) => ({
    name: `node-${i + 1}`,
    createdAt: new Date(Date.UTC(2026, 0, 1) + i * 3_600_000).toISOString(),
  }))
  const state = { ...DEFAULT_STATE, fleet: { ...DEFAULT_STATE.fleet, nodes } }
  const got = await decoded(state)
  assert.deepEqual(got, { state })
})

test('an optional field that is the wrong TYPE is damaged, not passed through to throw later', async () => {
  // parseGoDuration() (model.ts) does `s.trim()` on whatever it is handed. validate()'s own
  // comment claimed the shape was checked "in full", but five optional fields were never
  // type-checked — a node's expireAfter as a number is the concrete case: it used to sail
  // through validate() and blow up inside parseGoDuration() from the horizon watcher, not
  // from decodeState, so the receiver's coverage buttons went silently dead.
  const state = {
    ...DEFAULT_STATE,
    fleet: {
      ...DEFAULT_STATE.fleet,
      nodes: [{ ...DEFAULT_STATE.fleet.nodes[0], expireAfter: 123 }],
    },
  }
  const got = await decoded(state as unknown as ShareState)
  assert.ok('error' in got)
  assert.equal((got as { error: { code: string } }).error.code, 'damaged')
})

test('the other four optional fields are type-checked too', async () => {
  const bad = [
    { ...DEFAULT_STATE, env: { ...DEFAULT_STATE.env, provisioning: 5 } },
    { ...DEFAULT_STATE, env: { ...DEFAULT_STATE.env, drain: 5 } },
    { ...DEFAULT_STATE, fleet: { ...DEFAULT_STATE.fleet, terminationGracePeriod: 5 } },
    {
      ...DEFAULT_STATE,
      fleet: {
        ...DEFAULT_STATE.fleet,
        nodes: [{ ...DEFAULT_STATE.fleet.nodes[0], terminationGracePeriod: 5 }],
      },
    },
  ]
  for (const state of bad) {
    const got = await decoded(state as unknown as ShareState)
    assert.ok('error' in got, `expected an error for ${JSON.stringify(state).slice(0, 60)}`)
  }
})

/** Encode a raw string the way the codec does, so the tests can forge payloads. */
async function deflated(text: string): Promise<string> {
  const stream = new Blob([text]).stream().pipeThrough(new CompressionStream('deflate-raw'))
  const bytes = new Uint8Array(await new Response(stream).arrayBuffer())
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}
