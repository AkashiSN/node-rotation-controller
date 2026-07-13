// docs/.vitepress/scripts/build-wasm.mjs
// Prebuild step for every docs script: produce the policy simulator's wasm module
// and Go's own loader into docs/public/ (issue #240).
//
// This HARD-FAILS without a Go toolchain, by design. A docs build that silently
// omitted the module would deploy a simulator page whose only content is a load
// error — and it would deploy it looking green.
import { spawnSync } from 'node:child_process'
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const r = spawnSync('make', ['docs-wasm'], { cwd: root, stdio: 'inherit' })

if (r.status !== 0) {
  console.error(`
The docs site embeds the policy simulator, whose wasm module is built from Go
(cmd/wasm). \`make docs-wasm\` failed${r.error ? `: ${r.error.message}` : ''}.

You need the repo's toolchain: install aqua (https://aquaproj.github.io) and put
its bin dir on PATH — \`export PATH="$(aqua root-dir)/bin:$PATH"\` — then aqua
lazily installs the pinned Go (aqua.yaml) on first use.
`)
  process.exit(1)
}
