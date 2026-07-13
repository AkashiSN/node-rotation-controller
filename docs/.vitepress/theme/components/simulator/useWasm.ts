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

export function useWasm() {
  const loading = ref(false)
  const ready = ref(false)
  const error = ref('')
  const fn = shallowRef<((p: string, r: string) => string) | null>(null)

  async function load() {
    if (ready.value || loading.value) return
    loading.value = true
    error.value = ''
    try {
      // withBase, NOT a bare relative URL: the site is served under
      // base:'/node-rotation-controller/' and public/ assets sit at the base root,
      // so "simulator.wasm" would resolve against the PAGE — working on /simulator
      // and 404ing on /ja/simulator.
      await loadScript(withBase('/wasm_exec.js'))
      const go = new window.Go()
      const { instance } = await WebAssembly.instantiateStreaming(
        fetch(withBase('/simulator.wasm')), go.importObject)
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

  return { loading, ready, error, load, simulate }
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
