// node --test docs/.vitepress/theme/components/simulator/share.test.ts
//
// The codec behind ?s=. It is fed by strangers — a link pasted through a chat client that
// ate its tail, a link from an older payload version — so every case here is about what it
// does with input it did not produce. It must never throw: the page has to survive a bad
// link, not crash on it.
import assert from 'node:assert/strict'
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
  // A link that a chat client wraps or truncates is not a link. This is the guard against a
  // future payload change quietly making them unpasteable.
  const value = await encodeState(DEFAULT_STATE)
  assert.ok(value.length < 1000, `encoded to ${value.length} chars`)
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
  }
})

test('an unknown payload version is refused, not guessed at', async () => {
  const got = await decodeState(await deflated(JSON.stringify({ ...DEFAULT_STATE, v: 99 })))
  assert.ok('error' in got)
})

/** Encode a raw string the way the codec does, so the tests can forge payloads. */
async function deflated(text: string): Promise<string> {
  const stream = new Blob([text]).stream().pipeThrough(new CompressionStream('deflate-raw'))
  const bytes = new Uint8Array(await new Response(stream).arrayBuffer())
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}
