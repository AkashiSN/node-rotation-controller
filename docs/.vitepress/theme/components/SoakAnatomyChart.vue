<script setup lang="ts">
import { computed } from 'vue'
import { useData } from 'vitepress'
import { ROTATIONS } from './scenario-p-data'

const { lang } = useData()
const ja = computed(() => lang.value.startsWith('ja'))

// Main-pool rotations only: the epilogue row is surge-less (surgeWait null) and
// belongs to Figure 1's story, not to the anatomy of a graceful rotation.
const rows = ROTATIONS.filter((d) => d.p === 'main')

const W = 880
const H = 260
const M = { l: 46, r: 16, t: 14, b: 34 }
const Y_MAX_S = 140 // seconds; the 720s design bound is stated in the caption,
// not drawn — at scale it would flatten every bar to a sliver.
const iw = (W - M.l - M.r) / rows.length
const bw = Math.max(4, iw - 2)
const y = (s: number) => M.t + (1 - s / Y_MAX_S) * (H - M.t - M.b)
const yGrid = [0, 30, 60, 90, 120]
// x labels on ~every 3h of run time
const xTicks = [0, 17, 35, 53, 70]

const T = computed(() =>
  ja.value
    ? {
        cap: '図 2 — 各回転の所要時間の内訳（surgeWait + drain）',
        sub: '縦軸: 秒。積み上げの下段が placeholder 作成から surge ノード Ready までの待ち、上段が旧ノードの drain。t_rot の設計上界は 720 秒 — 実測 total は 45〜131 秒（中央値 81 秒）で常にその 1/5 以下、この余裕がバックストップに勝ち続ける源泉になっている。',
        aria: '本編71回転それぞれのsurgeWaitとdrainの積み上げ棒グラフ。合計は45〜131秒で、設計上界720秒を大きく下回る。',
        yUnit: '秒',
        legendSw: 'surgeWait（placeholder 作成 → surge ノード Ready）',
        legendDr: 'drain（旧 claim 削除 → 排出完了）',
        tipDone: '完了',
      }
    : {
        cap: 'Figure 2 — time breakdown of each rotation (surgeWait + drain)',
        sub: 'y: seconds. The lower segment is the wait from placeholder creation to the surge node going Ready; the upper segment is the old node’s drain. The design bound on t_rot is 720 s — measured totals are 45–131 s (median 81 s), always under a fifth of it; that headroom is what keeps beating the backstop.',
        aria: 'Stacked bars of surgeWait and drain for each of the 71 main-pool rotations. Totals are 45–131 seconds, far below the 720-second design bound.',
        yUnit: 's',
        legendSw: 'surgeWait (placeholder created → surge node Ready)',
        legendDr: 'drain (old claim deleted → drained)',
        tipDone: 'done',
      },
)

function tip(d: (typeof rows)[number]): string {
  return `claim ${d.c} (${T.value.tipDone} ${d.done})\nsurgeWait ${d.sw}s + drain ${d.dr}s / total ${d.tot}s`
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
        <template v-for="gy in yGrid" :key="`y${gy}`">
          <line class="gl" :x1="M.l" :x2="W - M.r" :y1="y(gy)" :y2="y(gy)" />
          <text :x="M.l - 8" :y="y(gy) + 3.5" text-anchor="end">{{ gy }}</text>
        </template>
        <text :x="M.l - 34" :y="M.t + 2">{{ T.yUnit }}</text>

        <g v-for="(d, i) in rows" :key="d.c" tabindex="0" class="bar">
          <rect class="sw" :x="M.l + i * iw + 1" :width="bw" :y="y(d.sw!)" :height="y(0) - y(d.sw!)" rx="1.5" />
          <rect
            class="dr"
            :x="M.l + i * iw + 1"
            :width="bw"
            :y="y(d.sw! + d.dr)"
            :height="Math.max(0, y(d.sw!) - y(d.sw! + d.dr) - 1.5)"
            rx="1.5"
          />
          <title>{{ tip(d) }}</title>
        </g>

        <text
          v-for="gi in xTicks"
          :key="`x${gi}`"
          :x="M.l + gi * iw + bw / 2"
          :y="H - M.b + 16"
          text-anchor="middle"
        >T0+{{ Math.round(rows[gi].t) }}h</text>
      </svg>
    </div>
    <div class="legend">
      <span><i class="swatch sw" /> {{ T.legendSw }}</span>
      <span><i class="swatch dr" /> {{ T.legendDr }}</span>
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
/* Segment hues: surgeWait is the node's own provisioning, so it keeps the
   rotation teal; drain leaves the teal family for the figure palette's violet
   so the two stacked segments stay apart in both themes. Orange is NOT used
   here — on this page it means forceful fallback (Figure 1), and every bar in
   this figure is a graceful rotation. */
.bar .sw { fill: var(--fig-graceful); }
.bar .dr { fill: var(--fig-ext); }
.bar:focus-visible { outline: 2px solid var(--vp-c-brand-1); }
.legend { display: flex; flex-wrap: wrap; gap: 6px 18px; margin: 6px 2px 2px; font-size: 12px; color: var(--fig-muted); }
.legend .swatch { display: inline-block; width: 10px; height: 10px; margin-right: 6px; border-radius: 3px; vertical-align: -1px; }
.legend .swatch.sw { background: var(--fig-graceful); }
.legend .swatch.dr { background: var(--fig-ext); }
</style>
