import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import './custom.css'
import TimelineForcefulFallback from './components/TimelineForcefulFallback.vue'
import CoverageMatrix from './components/CoverageMatrix.vue'
import PolicySimulator from './components/PolicySimulator.vue'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('TimelineForcefulFallback', TimelineForcefulFallback)
    app.component('CoverageMatrix', CoverageMatrix)
    app.component('PolicySimulator', PolicySimulator)
  },
} satisfies Theme
