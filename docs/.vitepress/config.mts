// docs/.vitepress/config.mts
import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

// GitHub-compatible heading slugify. VitePress's default (@mdit-vue/shared)
// renders a dotted-number heading like "1.1 Background" as `_1-1-background`,
// which does NOT match the GitHub anchor (`11-background`) that the canonical
// docs use in their cross-references — so those deep links loaded the right
// page on the site but never scrolled to the section. The canonical docs are
// browsed BOTH on GitHub and on this site, and their fragments are GitHub-form,
// so we make VitePress emit GitHub-form ids too (one algorithm, both surfaces).
// Mirrors github-slugger: lower-case, strip punctuation except letters,
// numbers, connector `_` and hyphen, and map each space to a hyphen WITHOUT
// collapsing runs (so "Capacity / Availability" → `capacity--availability`).
function githubSlugify(str: string): string {
  return str
    .normalize('NFKC')
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\p{Pc}\- ]/gu, '')
    .replace(/ /g, '-')
}

export default withMermaid(defineConfig({
  title: 'node-rotation-controller',
  description: 'Proactive make-before-break rotation for Karpenter-managed nodes',
  base: '/node-rotation-controller/',
  cleanUrls: true,
  markdown: {
    anchor: { slugify: githubSlugify },
  },
  srcExclude: ['superpowers/**'],
  // The canonical docs (specification/, ja/specification/, reference/adr/ and
  // their ja/ translations) are the source of truth and are never
  // forked/rewritten for the site — they legitimately link to repo files that
  // live outside the docs root (CLAUDE.md, go.mod, the Helm chart's
  // values.yaml/README.md) and therefore cannot resolve as site pages. Ignore
  // ONLY those specific outside-root targets; every other link (intra-docs
  // pages, the generated Getting Started pages, the spec's cross-locale
  // anchors, figure/nav links) stays checked so real dead links still fail
  // the build.
  ignoreDeadLinks: [
    /CLAUDE(\.md)?$/,
    /go\.mod$/,
    /\.\.\/charts\//,
  ],
  themeConfig: {
    search: { provider: 'local' },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/AkashiSN/node-rotation-controller' },
    ],
  },
  locales: {
    root: {
      label: 'English',
      lang: 'en',
      themeConfig: {
        nav: [
          { text: 'Getting Started', link: '/getting-started' },
          { text: 'Specification', link: '/specification/' },
          { text: 'Runbook', link: '/runbook' },
          { text: 'Validation', link: '/validation/forceful-fallback' },
          { text: 'Development', link: '/development/ci-cd' },
        ],
        sidebar: [
          { text: 'Overview', collapsed: false, items: [
            { text: 'Getting Started', link: '/getting-started' },
            { text: 'Runbook', link: '/runbook' },
          ]},
          { text: 'Specification', collapsed: false, items: [
            { text: 'Contents', link: '/specification/' },
            { text: '1. Overview', link: '/specification/01-overview' },
            { text: '2. Scope', link: '/specification/02-scope' },
            { text: '3. Design', link: '/specification/03-design' },
            { text: '4. Operations', link: '/specification/04-operations' },
            { text: '5. Implementation', link: '/specification/05-implementation' },
            { text: '6. Release', link: '/specification/06-release' },
            { text: '7. Risks & Status', link: '/specification/07-risks' },
          ]},
          { text: 'Validation', collapsed: false, items: [
            { text: 'Forceful fallback (Scenario O)', link: '/validation/forceful-fallback' },
          ]},
          { text: 'Development', collapsed: false, items: [
            { text: 'CI/CD design', link: '/development/ci-cd' },
          ]},
          { text: 'Reference', collapsed: false, items: [
            { text: 'ADR index', link: '/reference/adr/' },
            { text: 'ADR-0001 forceful fallback', link: '/reference/adr/0001-window-bounded-forceful-fallback' },
            { text: 'Perf: pod cache scalability', link: '/reference/perf/pod-cache-scalability' },
          ]},
        ],
      },
    },
    ja: {
      label: '日本語',
      lang: 'ja',
      link: '/ja/',
      themeConfig: {
        nav: [
          { text: 'はじめに', link: '/ja/getting-started' },
          { text: '仕様書', link: '/ja/specification/' },
          { text: 'ランブック', link: '/ja/runbook' },
          { text: '検証', link: '/ja/validation/forceful-fallback' },
          { text: '開発者向け', link: '/ja/development/ci-cd' },
        ],
        sidebar: [
          { text: '概要', collapsed: false, items: [
            { text: 'はじめに', link: '/ja/getting-started' },
            { text: 'ランブック', link: '/ja/runbook' },
          ]},
          { text: '仕様書', collapsed: false, items: [
            { text: '目次', link: '/ja/specification/' },
            { text: '1. 概要', link: '/ja/specification/01-overview' },
            { text: '2. スコープ', link: '/ja/specification/02-scope' },
            { text: '3. 設計', link: '/ja/specification/03-design' },
            { text: '4. 運用', link: '/ja/specification/04-operations' },
            { text: '5. 実装', link: '/ja/specification/05-implementation' },
            { text: '6. リリース', link: '/ja/specification/06-release' },
            { text: '7. リスクと状況', link: '/ja/specification/07-risks' },
          ]},
          { text: '検証', collapsed: false, items: [
            { text: 'Forceful fallback（シナリオ O）', link: '/ja/validation/forceful-fallback' },
          ]},
          { text: '開発者向け', collapsed: false, items: [
            { text: 'CI/CD 設計', link: '/ja/development/ci-cd' },
          ]},
          // ADR/perf are EN-only; link out to the English reference pages.
          { text: 'リファレンス（英語）', collapsed: false, items: [
            { text: 'ADR インデックス', link: '/reference/adr/' },
            { text: 'Perf: pod cache scalability', link: '/reference/perf/pod-cache-scalability' },
          ]},
        ],
      },
    },
  },
}))
