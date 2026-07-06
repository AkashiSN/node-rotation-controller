// docs/.vitepress/config.mts
import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

export default withMermaid(defineConfig({
  title: 'node-rotation-controller',
  description: 'Proactive make-before-break rotation for Karpenter-managed nodes',
  base: '/node-rotation-controller/',
  cleanUrls: true,
  srcExclude: ['superpowers/**'],
  // The canonical docs (specification.md, runbook.md, adr/README.md and their
  // ja/ translations) are the source of truth and are never forked/rewritten
  // for the site — they legitimately link to repo files that live outside the
  // docs root (CLAUDE.md, go.mod, the Helm chart's values.yaml/README.md) and
  // therefore cannot resolve as site pages. Ignore ONLY those specific
  // outside-root targets; every other link (intra-docs pages, the generated
  // Getting Started pages, the spec's cross-locale anchors, figure/nav links)
  // stays checked so real dead links still fail the build.
  ignoreDeadLinks: [
    /CLAUDE(\.md)?$/,
    /go\.mod$/,
    /\/charts\//,
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
          { text: 'Specification', link: '/specification' },
          { text: 'Runbook', link: '/runbook' },
          { text: 'Validation', link: '/validation/forceful-fallback' },
          { text: 'Development', link: '/development/ci-cd' },
        ],
        sidebar: [
          { text: 'Overview', items: [
            { text: 'Getting Started', link: '/getting-started' },
            { text: 'Specification', link: '/specification' },
            { text: 'Runbook', link: '/runbook' },
          ]},
          { text: 'Validation', items: [
            { text: 'Forceful fallback (Scenario O)', link: '/validation/forceful-fallback' },
          ]},
          { text: 'Development', items: [
            { text: 'CI/CD design', link: '/development/ci-cd' },
          ]},
          { text: 'Reference (English)', items: [
            { text: 'ADR index', link: '/adr/' },
            { text: 'ADR-0001 forceful fallback', link: '/adr/0001-window-bounded-forceful-fallback' },
            { text: 'Perf: pod cache scalability', link: '/perf/pod-cache-scalability' },
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
          { text: '仕様書', link: '/ja/specification' },
          { text: 'ランブック', link: '/ja/runbook' },
          { text: '検証', link: '/ja/validation/forceful-fallback' },
          { text: '開発者向け', link: '/ja/development/ci-cd' },
        ],
        sidebar: [
          { text: '概要', items: [
            { text: 'はじめに', link: '/ja/getting-started' },
            { text: '仕様書', link: '/ja/specification' },
            { text: 'ランブック', link: '/ja/runbook' },
          ]},
          { text: '検証', items: [
            { text: 'Forceful fallback（シナリオ O）', link: '/ja/validation/forceful-fallback' },
          ]},
          { text: '開発者向け', items: [
            { text: 'CI/CD 設計', link: '/ja/development/ci-cd' },
          ]},
          // ADR/perf are EN-only; link out to the English pages.
          { text: 'リファレンス（英語）', items: [
            { text: 'ADR インデックス', link: '/adr/' },
            { text: 'Perf: pod cache scalability', link: '/perf/pod-cache-scalability' },
          ]},
        ],
      },
    },
  },
}))
