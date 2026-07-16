<script setup lang="ts">
import { computed } from 'vue'
import { useData } from 'vitepress'
import { ROTATIONS } from './scenario-p-data'

const { lang } = useData()
const ja = computed(() => lang.value.startsWith('ja'))

// Geometry in SVG user units; the svg scales with the container width and
// scrolls inside its own wrapper below min-width, so no resize handling and no
// pointer-coordinate math (tooltips are native <title> elements).
const W = 880
const H = 300
const M = { l: 46, r: 16, t: 18, b: 34 }
const X_MAX_H = 14.5 // hours of run time on the x axis (12h main + epilogue)
const Y_MAX_MIN = 80 // minutes of margin on the y axis
const x = (t: number) => M.l + (t / X_MAX_H) * (W - M.l - M.r)
const y = (m: number) => M.t + (1 - m / Y_MAX_MIN) * (H - M.t - M.b)

const mains = ROTATIONS.filter((d) => d.p === 'main')
const epi = ROTATIONS.find((d) => d.p === 'epi')!
const bandTop = Math.max(...mains.map((d) => d.m)) // 71.2
const bandBot = Math.min(...mains.map((d) => d.m)) // 68.3
const yGrid = [0, 20, 40, 60, 80]
const xGrid = [0, 2, 4, 6, 8, 10, 12, 14]
const tEndH = 12 // T_end = T0 + 12h

const T = computed(() =>
  ja.value
    ? {
        cap: '図 1 — 全 72 回転の deadline margin',
        sub: '横軸: T0 からの経過時間。縦軸: 完了時点で deadline までに残っていた時間（分）。T_end（12h 観測窓の終端、窓内 60 回転）より右の点は有人エピローグ期間中の完了で、刻みも margin も変わっていない。オレンジの 1 点がエピローグの意図的な fallback 発火（margin 10.1 分）— 本編の帯との落差がこの検証の要約になっている。',
        aria: '全72回転のdeadline margin。本編71回転は68.3〜71.2分の帯に収まり、エピローグの1点のみ10.1分。',
        yUnit: '分',
        band: `本編 71 回転: ${bandBot}〜${bandTop} 分`,
        epiLabel: `epi: ${epi.m} 分（意図的発火）`,
        tEnd: 'T_end',
        legendMain: 'graceful surge（本編 71）',
        legendEpi: 'forceful fallback（epi 1）',
        tipMain: 'graceful surge',
        tipEpi: 'forceful fallback',
        tipDone: '完了',
        tipMargin: 'margin',
      }
    : {
        cap: 'Figure 1 — deadline margin of all 72 rotations',
        sub: 'x: elapsed time since T0. y: time still left to the deadline at completion (minutes). Dots right of the T_end rule (end of the 12h observation window; 60 rotations inside it) completed while the attended epilogue ran — cadence and margin unchanged. The single orange dot is the epilogue’s deliberate fallback firing (margin 10.1 min) — the drop from the main-pool band is the whole run in one picture.',
        aria: 'Deadline margin of all 72 rotations. The 71 main-pool rotations sit in a 68.3–71.2 minute band; the single epilogue point is at 10.1 minutes.',
        yUnit: 'min',
        band: `main pool, 71 rotations: ${bandBot}–${bandTop} min`,
        epiLabel: `epi: ${epi.m} min (deliberate firing)`,
        tEnd: 'T_end',
        legendMain: 'graceful surge (main pool, 71)',
        legendEpi: 'forceful fallback (epilogue, 1)',
        tipMain: 'graceful surge',
        tipEpi: 'forceful fallback',
        tipDone: 'done',
        tipMargin: 'margin',
      },
)

function tip(d: (typeof ROTATIONS)[number]): string {
  const mode = d.p === 'epi' ? T.value.tipEpi : T.value.tipMain
  return `${mode}\nclaim ${d.c}\n${T.value.tipDone} ${d.done}  ${T.value.tipMargin} ${d.m}${ja.value ? ' 分' : ' min'}`
}
</script>

<template>
  <figure class="soak-fig">
    <figcaption>
      <p class="cap">{{ T.cap }}</p>
      <p class="capsub">{{ T.sub }}</p>
    </figcaption>
    <div class="fig-scroll">
      <svg :viewBox="`0 0 ${W} ${H}`" role="img" :aria-label="T.aria">
        <!-- y gridlines + labels -->
        <template v-for="gy in yGrid" :key="`y${gy}`">
          <line class="gl" :x1="M.l" :x2="W - M.r" :y1="y(gy)" :y2="y(gy)" />
          <text :x="M.l - 8" :y="y(gy) + 3.5" text-anchor="end">{{ gy }}</text>
        </template>
        <text :x="M.l - 34" :y="M.t - 6">{{ T.yUnit }}</text>
        <!-- x labels -->
        <text v-for="gx in xGrid" :key="`x${gx}`" :x="x(gx)" :y="H - M.b + 16" text-anchor="middle">T0+{{ gx }}h</text>

        <!-- main-pool margin band: soft fill, annotation only (labelled directly) -->
        <rect class="band" :x="M.l" :width="W - M.l - M.r" :y="y(bandTop)" :height="y(bandBot) - y(bandTop)" />
        <text class="band-label" :x="x(7.2)" :y="y(bandTop) - 8" text-anchor="middle">{{ T.band }}</text>

        <!-- T_end rule -->
        <line class="tend" :x1="x(tEndH)" :x2="x(tEndH)" :y1="M.t" :y2="H - M.b" />
        <text :x="x(tEndH) + 5" :y="M.t + 10">{{ T.tEnd }}</text>

        <!-- one dot per rotation; native <title> carries the tooltip -->
        <circle
          v-for="d in ROTATIONS"
          :key="d.c"
          :class="d.p === 'epi' ? 'dot epi' : 'dot'"
          :cx="x(d.t)"
          :cy="y(d.m)"
          :r="d.p === 'epi' ? 6 : 4"
          tabindex="0"
        >
          <title>{{ tip(d) }}</title>
        </circle>
        <text class="epi-label" :x="x(epi.t) - 12" :y="y(epi.m) + 4" text-anchor="end">{{ T.epiLabel }}</text>
      </svg>
    </div>
    <div class="legend">
      <span><i class="sw main" /> {{ T.legendMain }}</span>
      <span><i class="sw epi" /> {{ T.legendEpi }}</span>
    </div>
  </figure>
</template>

<style scoped>
.soak-fig {
  margin: 16px 0;
  padding: 16px 16px 10px;
  border: 1px solid var(--fig-line);
  border-radius: 12px;
  background: var(--fig-surface);
  box-shadow: var(--fig-shadow);
}
.cap { margin: 0; font-size: 14px; font-weight: 700; color: var(--fig-text); }
.capsub { margin: 2px 0 8px; font-size: 12.5px; line-height: 1.6; color: var(--fig-muted); }
.fig-scroll { overflow-x: auto; }
svg { display: block; width: 100%; min-width: 640px; height: auto; }
svg text { font-family: var(--vp-font-family-mono); font-size: 10.5px; fill: var(--fig-muted); }
.gl { stroke: var(--fig-line); stroke-width: 1; }
.band { fill: var(--fig-graceful-fill); }
.band-label { fill: var(--fig-graceful); font-weight: 600; }
.tend { stroke: var(--fig-line-strong); stroke-width: 1; stroke-dasharray: 4 4; }
.dot { fill: var(--fig-graceful); stroke: var(--fig-surface); stroke-width: 2; }
.dot.epi { fill: var(--fig-forceful); }
.dot:focus-visible { outline: none; stroke: var(--vp-c-brand-1); stroke-width: 2.5; }
.epi-label { fill: var(--fig-forceful); font-weight: 600; }
.legend { display: flex; flex-wrap: wrap; gap: 6px 18px; margin: 6px 2px 2px; font-size: 12px; color: var(--fig-muted); }
.legend .sw { display: inline-block; width: 10px; height: 10px; margin-right: 6px; border-radius: 3px; vertical-align: -1px; }
.legend .sw.main { background: var(--fig-graceful); }
.legend .sw.epi { background: var(--fig-forceful); }
</style>
