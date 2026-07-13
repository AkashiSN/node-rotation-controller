// docs/.vitepress/theme/components/simulator/useWasm.ts
//
// Lazy-load the Go wasm module and expose the one function it registers.
//
// The module is 3.4 MB gzipped and must never enter the initial docs bundle: it
// lives in public/ and is fetched at runtime, from onMounted of the simulator
// component only — i.e. on this route and no other.
import { ref, shallowRef } from 'vue'
import { withBase } from 'vitepress'
import type { SimRequest, SimResponse } from './model.ts'

declare global {
  interface Window {
    Go: new () => { importObject: WebAssembly.Imports; run(i: WebAssembly.Instance): void }
    simulate?: (policyYAML: string, requestJSON: string) => string
  }
}

// Module-level singleton: VitePress is an SPA, so the simulator component can
// mount, unmount (navigate away), and remount (navigate back, either locale)
// many times in one page session. go.run(instance) never returns — the Go
// program parks in select{} to keep serving simulate() — so a fresh Go()
// + WebAssembly.Instance per mount would never be torn down and would leak
// the previous runtime's linear memory and closures. Hoisting the state to
// module scope means every useWasm() call shares one instance, and the 3.4 MB
// module is fetched and instantiated at most once per page session.
const loading = ref(false)
const ready = ref(false)
const error = ref('')
const fn = shallowRef<((p: string, r: string) => string) | null>(null)

// The load() attempt currently in flight, if any. Concurrent callers (e.g. a
// Retry click while the initial onMounted load is still pending) must await
// this SAME promise rather than each racing their own — otherwise a caller
// that returns early sees ready still false and the function ref still null.
// Cleared once the attempt settles (success or failure) so a Retry after a
// failure starts a genuinely new attempt instead of being pinned forever.
let inFlight: Promise<void> | null = null

export function useWasm() {
  return { loading, ready, error, load, simulate }
}

async function load(): Promise<void> {
  if (ready.value) return
  if (inFlight) return inFlight
  inFlight = doLoad().finally(() => {
    inFlight = null
  })
  return inFlight
}

async function doLoad(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    // withBase, NOT a bare relative URL: the site is served under
    // base:'/node-rotation-controller/' and public/ assets sit at the base root,
    // so "simulator.wasm" would resolve against the PAGE — working on /simulator
    // and 404ing on /ja/simulator.
    await loadScript(withBase('/wasm_exec.js'))
    const go = new window.Go()
    const instance = await instantiate(withBase('/simulator.wasm'), go.importObject)
    go.run(instance)                       // registers globalThis.simulate
    if (!window.simulate) throw new Error('the wasm module did not register simulate()')
    fn.value = window.simulate
    ready.value = true
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

/** Call the module. It never throws: un-runnable input comes back as {error}. */
function simulate(policyYAML: string, request: SimRequest): SimResponse {
  if (!fn.value) return { error: 'the simulator is not loaded' }
  return JSON.parse(fn.value(policyYAML, JSON.stringify(request))) as SimResponse
}

function loadScript(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    if (window.Go) return resolve()
    const el = document.createElement('script')
    el.src = src
    el.onload = () => resolve()
    el.onerror = () => reject(new Error(`failed to load ${src}`))
    document.head.appendChild(el)
  })
}

// instantiateStreaming requires the response to carry a Content-Type of
// application/wasm; some static hosts (this project has hit GitHub-Pages /
// Cloudflare content-type quirks before) serve .wasm with the wrong MIME
// type, which makes instantiateStreaming reject before the module body is
// ever read. Fall back to buffering the response and using the non-streaming
// WebAssembly.instantiate, which doesn't care about Content-Type. Surface the
// streaming error (not the fallback's) if both paths fail — it's usually the
// one that pinpoints the MIME mismatch.
async function instantiate(
  url: string,
  importObject: WebAssembly.Imports,
): Promise<WebAssembly.Instance> {
  try {
    const { instance } = await WebAssembly.instantiateStreaming(fetch(url), importObject)
    return instance
  } catch (streamingError) {
    try {
      const response = await fetch(url)
      const { instance } = await WebAssembly.instantiate(await response.arrayBuffer(), importObject)
      return instance
    } catch {
      throw streamingError
    }
  }
}
