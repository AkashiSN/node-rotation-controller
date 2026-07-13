// Component (DOM) tests for the simulator page.
//
// `node --test` cannot import or compile a .vue SFC, so the pure modules stay on it
// (timeline / zoom / calendar / ruler / model / policyYaml) and the COMPONENTS run here:
// @vitejs/plugin-vue is what compiles the SFC, @vue/test-utils mounts it, happy-dom gives
// it a DOM.
//
// Scope, stated honestly: happy-dom has NO LAYOUT ENGINE. getBoundingClientRect and
// ResizeObserver are stubs, so these are STATE-TRANSITION tests — given this pointer or key
// sequence and this stubbed geometry, zoom.ts reaches this view and the SVG renders these
// elements. Real layout, real pinch and real overflow are NOT covered here and the cases
// below do not pretend otherwise; Playwright is the escalation if that ever proves to
// matter.
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [vue()],
  test: {
    environment: 'happy-dom',
    include: ['docs/.vitepress/theme/components/**/*.spec.ts'],
  },
})
