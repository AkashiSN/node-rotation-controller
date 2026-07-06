// docs/.vitepress/scripts/copy-readme.mjs
// Copy the repo-root READMEs into the site as gitignored "Getting Started"
// pages, rewriting repo-relative links so VitePress's dead-link checker passes:
//   docs/ja/<f>.md#anchor -> /ja/<f>#anchor      docs/<f>.md#anchor -> /<f>#anchor
//   README.ja.md -> /ja/getting-started          README.md -> /getting-started
//   any other repo-relative path -> absolute GitHub blob URL
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const BLOB = 'https://github.com/AkashiSN/node-rotation-controller/blob/main'

function rewrite(md) {
  return md.replace(/\]\((?!https?:\/\/|#)([^)]+)\)/g, (_m, href) => {
    const [path, hash = ''] = href.split('#')
    const anchor = hash ? `#${hash}` : ''
    let out
    if (path === 'README.ja.md') out = '/ja/getting-started'
    else if (path === 'README.md') out = '/getting-started'
    else if (path.startsWith('docs/ja/')) out = '/ja/' + path.slice('docs/ja/'.length).replace(/\.md$/, '')
    else if (path.startsWith('docs/')) out = '/' + path.slice('docs/'.length).replace(/\.md$/, '')
    else out = `${BLOB}/${path}`
    return `](${out}${anchor})`
  })
}

for (const [src, dest] of [
  ['README.md', 'docs/getting-started.md'],
  ['README.ja.md', 'docs/ja/getting-started.md'],
]) {
  const body = rewrite(readFileSync(resolve(root, src), 'utf8'))
  const front = `---\ntitle: Getting Started\neditLink: false\n---\n\n`
  const outPath = resolve(root, dest)
  mkdirSync(dirname(outPath), { recursive: true })
  writeFileSync(outPath, front + body)
  console.log(`copied ${src} -> ${dest}`)
}
