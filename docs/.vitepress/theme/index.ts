import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import './custom.css'
import TimelineForcefulFallback from './components/TimelineForcefulFallback.vue'
import CoverageMatrix from './components/CoverageMatrix.vue'
import PolicySimulator from './components/PolicySimulator.vue'
import SoakMarginChart from './components/SoakMarginChart.vue'
import SoakAnatomyChart from './components/SoakAnatomyChart.vue'
import SoakLedger from './components/SoakLedger.vue'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('TimelineForcefulFallback', TimelineForcefulFallback)
    app.component('CoverageMatrix', CoverageMatrix)
    app.component('PolicySimulator', PolicySimulator)
    app.component('SoakMarginChart', SoakMarginChart)
    app.component('SoakAnatomyChart', SoakAnatomyChart)
    app.component('SoakLedger', SoakLedger)
  },
} satisfies Theme
