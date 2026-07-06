import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import './custom.css'
import TimelineForcefulFallback from './components/TimelineForcefulFallback.vue'
import CoverageMatrix from './components/CoverageMatrix.vue'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('TimelineForcefulFallback', TimelineForcefulFallback)
    app.component('CoverageMatrix', CoverageMatrix)
  },
} satisfies Theme
