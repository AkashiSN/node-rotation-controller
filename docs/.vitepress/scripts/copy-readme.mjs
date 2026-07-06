// docs/.vitepress/scripts/copy-readme.mjs
// Copy the repo-root READMEs into the site as gitignored "Getting Started"
// pages, rewriting repo-relative links so VitePress's dead-link checker passes:
//   docs/ja/<f>.md#anchor -> /ja/<f>   (fragment dropped, see below)
//   docs/<f>.md#anchor -> /<f>         (fragment dropped, see below)
//   README.ja.md -> /ja/getting-started          README.md -> /getting-started
//   any other repo-relative path -> absolute GitHub blob URL
//
// The GitHub-style heading anchors used in the READMEs (e.g. "#62-roadmap")
// do not match VitePress's own slugifier for dotted-number headings (which
// renders "6.2 Roadmap" as "_6-2-roadmap"), so a naive rewrite would produce
// a dangling fragment. We cannot reliably reproduce VitePress's slug rules
// here, so for cross-document links into docs/ and docs/ja/ we drop the
// fragment entirely and link to the page instead of a (possibly wrong)
// section — deterministic and never dangling.
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
    if (path === 'README.ja.md') return `](/ja/getting-started${anchor})`
    if (path === 'README.md') return `](/getting-started${anchor})`
    // Cross-document links into the docs site: GitHub-style anchors don't
    // match VitePress's dotted-number slugifier, so drop the fragment
    // rather than link to a possibly-wrong (dangling) section id.
    if (path.startsWith('docs/ja/')) out = '/ja/' + path.slice('docs/ja/'.length).replace(/\.md$/, '')
    else if (path.startsWith('docs/')) out = '/' + path.slice('docs/'.length).replace(/\.md$/, '')
    else return `](${BLOB}/${path}${anchor})`
    return `](${out})`
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
