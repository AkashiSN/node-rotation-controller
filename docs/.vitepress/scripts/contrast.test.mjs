// node --test docs/.vitepress/scripts/contrast.test.mjs
//
// The simulator's regions are legible because their STROKES carry the contrast (#260). That
// is a property of the palette, and a palette is edited by eye — so it is asserted here
// rather than trusted. A future tweak that drops a boundary below the 3:1 WCAG 1.4.11 asks
// of a meaningful non-text element fails this test, not the reader.
//
// The fills are deliberately NOT checked: a background fill cannot clear 3:1 at any alpha it
// could honestly use (the teal reaches 2.0:1 at a=0.35 and is by then loud enough to fight
// the bars over it). That is the whole reason the strokes exist.
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const CSS = fileURLToPath(new URL('../theme/custom.css', import.meta.url))

/** VitePress's own page background, which is what these strokes are drawn on. */
const PAGE_BG = { light: [255, 255, 255], dark: [27, 27, 31] }

const srgb = (c) => {
  const x = c / 255
  return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4
}
const luminance = ([r, g, b]) => 0.2126 * srgb(r) + 0.7152 * srgb(g) + 0.0722 * srgb(b)

function contrast(fg, bg) {
  const [hi, lo] = [luminance(fg), luminance(bg)].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}

/** #rrggbb, or rgba(r, g, b, a) composited over the background it is painted on. */
function parseColour(value, bg) {
  const hex = value.match(/^#([0-9a-f]{6})$/i)
  if (hex) return [0, 2, 4].map(i => parseInt(hex[1].slice(i, i + 2), 16))

  const rgba = value.match(/^rgba?\(([^)]+)\)$/i)
  assert.ok(rgba, `unrecognised colour: ${value}`)
  const parts = rgba[1].split(',').map(s => Number(s.trim()))
  const [r, g, b] = parts
  const a = parts.length > 3 ? parts[3] : 1
  return [r, g, b].map((c, i) => a * c + (1 - a) * bg[i])
}

/** Every --sim-* declaration, per theme block. `:root` is light; `.dark` is dark. */
async function tokens() {
  const css = await readFile(CSS, 'utf8')
  const out = { light: {}, dark: {} }
  for (const [theme, selector] of [['light', ':root'], ['dark', '\\.dark']]) {
    const block = css.match(new RegExp(`^${selector}\\s*\\{([\\s\\S]*?)^\\}`, 'm'))
    assert.ok(block, `no ${selector} block in custom.css`)
    for (const m of block[1].matchAll(/(--sim-[\w-]+)\s*:\s*([^;]+);/g)) {
      out[theme][m[1]] = m[2].trim()
    }
  }
  return out
}

test('every simulator boundary stroke clears 3:1 against the page background', async () => {
  const t = await tokens()
  const strokes = ['--sim-window-line', '--sim-eligible-line', '--sim-brush-line']

  for (const theme of ['light', 'dark']) {
    const bg = PAGE_BG[theme]
    for (const name of strokes) {
      const raw = t[theme][name]
      assert.ok(raw, `${name} is not defined for the ${theme} theme`)
      const ratio = contrast(parseColour(raw, bg), bg)
      assert.ok(ratio >= 3, `${theme} ${name} (${raw}) is ${ratio.toFixed(2)}:1 — below the 3:1 floor`)
    }
  }
})

test('the curtain actually dims: it moves the strip toward the page background', async () => {
  const t = await tokens()
  // The brush is found because everything around it recedes. A curtain too weak to change
  // the luminance beneath it would leave the brush exactly as lost as the old fill was.
  for (const theme of ['light', 'dark']) {
    const bg = PAGE_BG[theme]
    const band = theme === 'dark' ? [32, 51, 52] : [222, 236, 234] // a window band, composited
    const veiled = parseColour(t[theme]['--sim-curtain'], band)
    const before = Math.abs(luminance(band) - luminance(bg))
    const after = Math.abs(luminance(veiled) - luminance(bg))
    assert.ok(after < before,
      `${theme}: the curtain must pull a window band toward the page background (${after} !< ${before})`)
  }
})

test('the palette is defined for BOTH themes — a token that exists in only one is a bug', async () => {
  const t = await tokens()
  assert.deepEqual(Object.keys(t.light).sort(), Object.keys(t.dark).sort())
  assert.ok(Object.keys(t.light).length >= 6)
})
