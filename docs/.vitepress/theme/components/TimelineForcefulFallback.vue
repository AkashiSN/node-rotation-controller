<script setup lang="ts">
import { computed } from 'vue'
import { useData } from 'vitepress'
const { lang } = useData()
const ja = computed(() => lang.value.startsWith('ja'))

// 12-node event data (from the source table). Suffixes are the NodeClaim tail.
// `note` carries the per-row "behavior / evidence" cell, bilingual since it varies per row.
const nodes = [
  { n: 1, t: '09:26:24', age: '48m', mode: 'graceful', nc: '52t8h', note: {
    ja: 'placeholder surge → make-before-break（total 1m2s）',
    en: 'placeholder surge → make-before-break (total 1m2s)',
  } },
  { n: 2, t: '09:33:41', age: '55m', mode: 'graceful', nc: '5rp47', note: {
    ja: 'placeholder <code>Pending→Running</code>、<code>mode</code> 空',
    en: 'placeholder <code>Pending→Running</code>, <code>mode</code> empty',
  } },
  { n: 3, t: '09:41:29', age: '1h03m', mode: 'graceful', nc: '94f5n', note: {
    ja: '同上（cooldown 6m で間隔 ~7m）',
    en: 'same as above (cooldown 6m → interval ~7m)',
  } },
  { n: 4, t: '09:48:45', age: '1h10m', mode: 'graceful', nc: 'fvd7p', note: {
    ja: '同上',
    en: 'same as above',
  } },
  { n: 5, t: '09:56:34', age: '1h18m', mode: 'graceful', nc: 'gjgg8', note: {
    ja: '同上',
    en: 'same as above',
  } },
  { n: 6, t: '10:04:50', age: '1h26m', mode: 'graceful', nc: 'n8zqt', note: {
    ja: '同上',
    en: 'same as above',
  } },
  { n: 7, t: '10:12:40', age: '1h34m', mode: 'graceful', nc: 'p2vdk', note: {
    ja: '同上',
    en: 'same as above',
  } },
  { n: 8, t: '10:19:54', age: '1h41m', mode: 'graceful', nc: 'phmms', note: {
    ja: '同上',
    en: 'same as above',
  } },
  { n: 9, t: '10:24:21', age: '1h46m', mode: 'graceful', nc: 'rnwb7', note: {
    ja: '<b>最後の graceful</b>（age 1h46m、境界 1h48m の直前）',
    en: '<b>last graceful</b> (age 1h46m, just inside the 1h48m boundary)',
  } },
  { n: 10, t: '10:26:55', age: '1h48m', mode: 'forceful', nc: 'sgh57', note: {
    ja: '<b>最初の forceful</b>（age 1h48m35s）、<code>mode=forceful-fallback</code>、placeholder <code>NotFound</code>、<b>counter 0→1</b>',
    en: '<b>first forceful</b> (age 1h48m35s), <code>mode=forceful-fallback</code>, placeholder <code>NotFound</code>, <b>counter 0→1</b>',
  } },
  { n: 11, t: '10:30:59', age: '1h52m', mode: 'forceful', nc: 't7sb5', note: {
    ja: 'surge-less、Warning event、counter →2',
    en: 'surge-less, Warning event, counter →2',
  } },
  { n: 12, t: '10:33:16', age: '1h54m', mode: 'forceful', nc: 'tkkf7', note: {
    ja: '<b>counter →3 完成</b>、全12本 rotate 完了（deadline 10:38 の ~5m 前）',
    en: '<b>counter →3 — complete</b>, all 12 rotations done (~5m before the 10:38 deadline)',
  } },
]

// Bilingual strings. EN values are translations of the source JA labels.
const T = computed(() => ja.value ? {
  eyebrow: 'Scenario O · EKS Auto Mode · 2026-07-12 UTC',
  h1: '共有デッドライン上の 12 ノード rotation — graceful → forceful fallback の分岐',
  lede: '12 ノードは同時生成で<b>同一 creationTimestamp（デッドライン）</b>を共有。serial（<span class="mono">maxUnavailable=1</span>）で 1 本ずつ処理され、<b>その番が来た時刻がデッドラインまで <span class="mono">t_rot</span>（12m）以上残っているか</b>だけで graceful か forceful かが決まる。<span class="mono">expireAfter</span> は終始 <b>2h 固定</b>（trick-free）。',
  metrics: {
    fallback: { k: 'forceful_fallback_total', from: '0', to: '3' },
    completed: { k: 'completed{success}', v: '12', detail: '9 graceful + 3 forceful' },
    backstop: { k: 'expireAfter backstop', v: '0', detail: 'expired' },
    restarts: { k: 'controller restarts', v: '0', detail: 'counter 無リセット' },
  },
  sectionTimeline: 'タイムライン（2h 比例軸）',
  sectionLogic: '判定ロジック（pick 時点の残り時間で分岐）',
  sectionLog: '時系列（実測ログ）',
  zones: { pre: 'pre-threshold', graceful: 'graceful window', forceful: 'forceful window' },
  svg: {
    ariaLabel: '12ノード rotation の比例タイムライン',
    thresholds: {
      eligible: { time: '09:26:20Z', sub: 'age 48m · eligible' },
      forceful: { time: '10:26:20Z', sub: 'age 1h48m · forceful 境界' },
      backstop: { time: '10:38:20Z', sub: 'age 2h · backstop（未到達）' },
    },
    batch: 'batch',
  },
  legend: {
    graceful: 'graceful surge（placeholder あり）× 9',
    forceful: 'forceful fallback（surge-less）× 3',
    backstop: 'expireAfter backstop（今回 0）',
  },
  caseA: {
    title: 'graceful', rule: '残り ≥ t_rot（12m）',
    body: 'デッドラインまで surge を完了する余裕がある。<b>placeholder Pod → 新ノード Ready → 旧ノードを voluntary drain</b>（PDB 適用）。<span class="mono">mode</span> は空。番号 <b>1–9</b>。',
  },
  caseB: {
    title: 'forceful', rule: '残り < t_rot（12m）',
    body: 'graceful surge が間に合わない。in-window かつ opt-in なので <b>placeholder なしで NodeClaim を削除</b>（voluntary・PDB 適用）。<span class="mono">mode=forceful-fallback</span>、Warning event、counter +1。番号 <b>10–12</b>。',
  },
  caseC: {
    title: 'backstop', rule: 'age = 2h に到達',
    body: 'forceful すら間に合わず期限到達 → Karpenter が強制失効（<span class="mono">outcome=expired</span>）。「fallback の fallback」。<b>今回は forceful が 10:33 までに全て処理し 0 件。</b>',
  },
  cols: { time: '時刻 UTC', age: 'age', phase: 'フェーズ', nc: 'NodeClaim', note: '挙動・証拠' },
  markers: {
    batch: {
      mk: '12ノード同期バッチ生成',
      rest: '共有デッドライン 10:38:20Z。schedule が意図的 warn（<code class="mono">ThroughputBurstShortfall</code> N=12&gt;K·C=6 ／ <code class="mono">ThroughputBelowArrival</code>）',
    },
    eligible: {
      mk: 'eligible 境界（ageThreshold A=48m）',
      rest: 'ここから候補が rotation 対象に',
    },
    forcefulBoundary: {
      mk: 'forceful 境界通過（deadline − t_rot）',
      rest: '直前 10:24:00 に cooldown 6m→1m。以降の pick は全て Case B',
    },
    backstop: {
      mk: 'expireAfter backstop — 未到達（Case C 0件）',
    },
  },
  footer: [
    '<b>#157（earliest-deadline order）</b>: 12本は creationTimestamp を共有するため順序は <code>Name</code> tiebreak に縮退し、正確に昇順で消費 — <span class="mono">52t8h &lt; 5rp47 &lt; 94f5n &lt; fvd7p &lt; gjgg8 &lt; n8zqt &lt; p2vdk &lt; phmms &lt; rnwb7 &lt; sgh57 &lt; t7sb5 &lt; tkkf7</span>。',
    '<b>mix は cooldown 調整で成形</b>: 実 graceful は ~1 分（46s–1m51s）で終わるため放置すると serial surge が全部さばき forceful が出ない。前半 <code>cooldownAfter=6m</code> で surplus をデッドラインまで残し、境界直前 10:24 に <code>1m</code> に落として backstop 前に forceful を出し切った。<code>expireAfter</code> は不変なのでトリックではない。',
    '<b>t_rot = readyTimeout 5m + tGP 5m + Buffer 2m = 12m</b>（Buffer は #215 で 15m→2m）。分岐点は純粋に「pick 時刻が 10:26:20Z より前か後か」だけで、共有デッドラインゆえ境界をまたいだ瞬間に残り全部が forceful へ一斉に切り替わった。',
  ],
} : {
  eyebrow: 'Scenario O · EKS Auto Mode · 2026-07-12 UTC',
  h1: '12-node rotation on a shared deadline — graceful → forceful fallback split',
  lede: 'All 12 nodes are created at once and share <b>the same creationTimestamp (deadline)</b>. They are processed one at a time, serially (<span class="mono">maxUnavailable=1</span>), and graceful vs. forceful is decided purely by <b>whether, at the moment a node\'s turn comes up, <span class="mono">t_rot</span> (12m) or more remains until the deadline</b>. <span class="mono">expireAfter</span> stays <b>fixed at 2h</b> throughout (trick-free).',
  metrics: {
    fallback: { k: 'forceful_fallback_total', from: '0', to: '3' },
    completed: { k: 'completed{success}', v: '12', detail: '9 graceful + 3 forceful' },
    backstop: { k: 'expireAfter backstop', v: '0', detail: 'expired' },
    restarts: { k: 'controller restarts', v: '0', detail: 'counter never reset' },
  },
  sectionTimeline: 'Timeline (2h proportional axis)',
  sectionLogic: 'Decision logic (branches on remaining time at pick)',
  sectionLog: 'Chronology (observed log)',
  zones: { pre: 'pre-threshold', graceful: 'graceful window', forceful: 'forceful window' },
  svg: {
    ariaLabel: 'Proportional timeline of the 12-node rotation',
    thresholds: {
      eligible: { time: '09:26:20Z', sub: 'age 48m · eligible' },
      forceful: { time: '10:26:20Z', sub: 'age 1h48m · forceful boundary' },
      backstop: { time: '10:38:20Z', sub: 'age 2h · backstop (not reached)' },
    },
    batch: 'batch',
  },
  legend: {
    graceful: 'graceful surge (with placeholder) × 9',
    forceful: 'forceful fallback (surge-less) × 3',
    backstop: 'expireAfter backstop (0 this run)',
  },
  caseA: {
    title: 'graceful', rule: 'remaining ≥ t_rot (12m)',
    body: 'There is enough time before the deadline to complete the surge. <b>placeholder Pod → new node Ready → voluntary drain of the old node</b> (PDB applies). <span class="mono">mode</span> is empty. Nodes <b>1–9</b>.',
  },
  caseB: {
    title: 'forceful', rule: 'remaining < t_rot (12m)',
    body: 'The graceful surge would not finish in time. Since it is in-window and opt-in, <b>the NodeClaim is deleted without a placeholder</b> (voluntary path, PDB applies). <span class="mono">mode=forceful-fallback</span>, a Warning event, counter +1. Nodes <b>10–12</b>.',
  },
  caseC: {
    title: 'backstop', rule: 'age reaches 2h',
    body: 'Even forceful fallback would not finish in time and the deadline is reached → Karpenter force-expires the node (<span class="mono">outcome=expired</span>). The "fallback of the fallback." <b>This run, forceful fallback handled all remaining nodes by 10:33, so this case is 0.</b>',
  },
  cols: { time: 'Time UTC', age: 'age', phase: 'phase', nc: 'NodeClaim', note: 'behavior / evidence' },
  markers: {
    batch: {
      mk: '12-node synchronized batch creation',
      rest: 'shared deadline 10:38:20Z. schedule intentionally warns (<code class="mono">ThroughputBurstShortfall</code> N=12&gt;K·C=6 / <code class="mono">ThroughputBelowArrival</code>)',
    },
    eligible: {
      mk: 'eligible boundary (ageThreshold A=48m)',
      rest: 'from here, candidates become rotation targets',
    },
    forcefulBoundary: {
      mk: 'forceful boundary crossed (deadline − t_rot)',
      rest: 'cooldown dropped 6m→1m just before, at 10:24:00. Every pick from here on is Case B',
    },
    backstop: {
      mk: 'expireAfter backstop — not reached (Case C: 0)',
    },
  },
  footer: [
    '<b>#157 (earliest-deadline order)</b>: since all 12 share the same creationTimestamp, ordering degenerates to the <code>Name</code> tiebreak, and they are consumed in exact ascending order — <span class="mono">52t8h &lt; 5rp47 &lt; 94f5n &lt; fvd7p &lt; gjgg8 &lt; n8zqt &lt; p2vdk &lt; phmms &lt; rnwb7 &lt; sgh57 &lt; t7sb5 &lt; tkkf7</span>.',
    '<b>The graceful/forceful mix was shaped via cooldown tuning</b>: a real graceful rotation finishes in ~1 minute (46s–1m51s), so left alone the serial surge would handle all 12 and no forceful fallback would occur. <code>cooldownAfter=6m</code> in the first half held the surplus back until the deadline, then dropping it to <code>1m</code> at 10:24 (just before the boundary) let forceful fallback clear the rest before the backstop. <code>expireAfter</code> never changes, so this is not a trick.',
    '<b>t_rot = readyTimeout 5m + tGP 5m + buffer 2m = 12m</b> (buffer shrank 15m→2m in #215). The split is decided purely by whether the pick time falls before or after 10:26:20Z; because the deadline is shared, the instant that boundary was crossed, every remaining node flipped to forceful at once.',
  ],
})
</script>

<template>
  <div class="ff-wrap">
    <header class="top">
      <p class="eyebrow">{{ T.eyebrow }}</p>
      <h1>{{ T.h1 }}</h1>
      <p class="lede" v-html="T.lede"></p>
    </header>

    <div class="metrics">
      <div class="tile good">
        <p class="k">{{ T.metrics.fallback.k }}</p>
        <p class="v">{{ T.metrics.fallback.from }} <span class="arrow">→</span> {{ T.metrics.fallback.to }}</p>
      </div>
      <div class="tile good">
        <p class="k">{{ T.metrics.completed.k }}</p>
        <p class="v">{{ T.metrics.completed.v }} <small>{{ T.metrics.completed.detail }}</small></p>
      </div>
      <div class="tile good">
        <p class="k">{{ T.metrics.backstop.k }}</p>
        <p class="v">{{ T.metrics.backstop.v }} <small>{{ T.metrics.backstop.detail }}</small></p>
      </div>
      <div class="tile good">
        <p class="k">{{ T.metrics.restarts.k }}</p>
        <p class="v">{{ T.metrics.restarts.v }} <small>{{ T.metrics.restarts.detail }}</small></p>
      </div>
    </div>

    <p class="section-label">{{ T.sectionTimeline }}</p>
    <div class="panel">
      <div class="tl-scroll">
        <svg class="tl" viewBox="0 0 1000 172" role="img" :aria-label="T.svg.ariaLabel">
          <!-- zone bands -->
          <rect class="zband" x="70"    y="44" width="348" height="70" fill="var(--fig-idle-fill)"/>
          <rect class="zband" x="418"   y="44" width="435" height="70" fill="var(--fig-graceful-fill)"/>
          <rect class="zband" x="853"   y="44" width="87"  height="70" fill="var(--fig-forceful-fill)"/>

          <!-- zone labels -->
          <text class="zone-lab" x="244"   y="38" text-anchor="middle" fill="var(--fig-faint)">{{ T.zones.pre }}</text>
          <text class="zone-lab" x="635.5" y="38" text-anchor="middle" fill="var(--fig-graceful)">{{ T.zones.graceful }}</text>
          <text class="zone-lab" x="896.5" y="38" text-anchor="middle" fill="var(--fig-forceful)">{{ T.zones.forceful }}</text>

          <!-- threshold lines -->
          <line class="thr"      x1="418" y1="40" x2="418" y2="120"/>
          <line class="thr hard" x1="853" y1="40" x2="853" y2="120" stroke="var(--fig-forceful)"/>
          <line class="thr hard" x1="940" y1="40" x2="940" y2="120" stroke="var(--fig-backstop)"/>

          <!-- threshold captions -->
          <text class="thr-name" x="418" y="132" text-anchor="middle">{{ T.svg.thresholds.eligible.time }}</text>
          <text class="thr-sub"  x="418" y="144" text-anchor="middle">{{ T.svg.thresholds.eligible.sub }}</text>
          <text class="thr-name" x="853" y="132" text-anchor="middle" fill="var(--fig-forceful)">{{ T.svg.thresholds.forceful.time }}</text>
          <text class="thr-sub"  x="853" y="144" text-anchor="middle">{{ T.svg.thresholds.forceful.sub }}</text>
          <text class="thr-name" x="940" y="132" text-anchor="end" fill="var(--fig-backstop)">{{ T.svg.thresholds.backstop.time }}</text>
          <text class="thr-sub"  x="940" y="144" text-anchor="end">{{ T.svg.thresholds.backstop.sub }}</text>

          <!-- baseline axis -->
          <line class="axis" x1="70" y1="100" x2="940" y2="100"/>
          <!-- ticks every 30m -->
          <line class="tick" x1="70"  y1="100" x2="70"  y2="106"/>
          <line class="tick" x1="287.5" y1="100" x2="287.5" y2="106"/>
          <line class="tick" x1="505" y1="100" x2="505" y2="106"/>
          <line class="tick" x1="722.5" y1="100" x2="722.5" y2="106"/>
          <line class="tick" x1="940" y1="100" x2="940" y2="106"/>
          <text class="t-lab" x="70"  y="162" text-anchor="middle">08:38</text>
          <text class="t-lab" x="287.5" y="162" text-anchor="middle">09:08</text>
          <text class="t-lab" x="505" y="162" text-anchor="middle">09:38</text>
          <text class="t-lab" x="722.5" y="162" text-anchor="middle">10:08</text>
          <text class="t-lab" x="940" y="162" text-anchor="middle">10:38</text>

          <!-- batch flag -->
          <circle cx="70" cy="100" r="4" class="flag"/>
          <text class="thr-sub" x="70" y="90" text-anchor="middle">{{ T.svg.batch }}</text>

          <!-- graceful pins 1..9 (x = 70 + age_min/120*870) -->
          <g class="pin-grp" style="animation-delay:.02s"><circle class="pin g" cx="418.6" cy="100" r="11"/><text class="pin-n" x="418.6" y="103.5" text-anchor="middle">1</text></g>
          <g class="pin-grp" style="animation-delay:.06s"><circle class="pin g" cx="471.3" cy="100" r="11"/><text class="pin-n" x="471.3" y="103.5" text-anchor="middle">2</text></g>
          <g class="pin-grp" style="animation-delay:.10s"><circle class="pin g" cx="527.9" cy="100" r="11"/><text class="pin-n" x="527.9" y="103.5" text-anchor="middle">3</text></g>
          <g class="pin-grp" style="animation-delay:.14s"><circle class="pin g" cx="580.6" cy="100" r="11"/><text class="pin-n" x="580.6" y="103.5" text-anchor="middle">4</text></g>
          <g class="pin-grp" style="animation-delay:.18s"><circle class="pin g" cx="637.2" cy="100" r="11"/><text class="pin-n" x="637.2" y="103.5" text-anchor="middle">5</text></g>
          <g class="pin-grp" style="animation-delay:.22s"><circle class="pin g" cx="697.1" cy="100" r="11"/><text class="pin-n" x="697.1" y="103.5" text-anchor="middle">6</text></g>
          <g class="pin-grp" style="animation-delay:.26s"><circle class="pin g" cx="754.0" cy="100" r="11"/><text class="pin-n" x="754.0" y="103.5" text-anchor="middle">7</text></g>
          <g class="pin-grp" style="animation-delay:.30s"><circle class="pin g" cx="806.5" cy="100" r="11"/><text class="pin-n" x="806.5" y="103.5" text-anchor="middle">8</text></g>
          <g class="pin-grp" style="animation-delay:.34s"><circle class="pin g" cx="838.6" cy="100" r="11"/><text class="pin-n" x="838.6" y="103.5" text-anchor="middle">9</text></g>
          <!-- forceful pins 10..12 -->
          <g class="pin-grp" style="animation-delay:.40s"><circle class="pin f" cx="857.2" cy="78"  r="11"/><text class="pin-n" x="857.2" y="81.5"  text-anchor="middle">10</text><line class="tick" x1="857.2" y1="89" x2="857.2" y2="100"/></g>
          <g class="pin-grp" style="animation-delay:.44s"><circle class="pin f" cx="886.7" cy="100" r="11"/><text class="pin-n" x="886.7" y="103.5" text-anchor="middle">11</text></g>
          <g class="pin-grp" style="animation-delay:.48s"><circle class="pin f" cx="903.3" cy="78"  r="11"/><text class="pin-n" x="903.3" y="81.5"  text-anchor="middle">12</text><line class="tick" x1="903.3" y1="89" x2="903.3" y2="100"/></g>
        </svg>
      </div>
      <div class="tl-legend">
        <span class="sw"><span class="dot g"></span> {{ T.legend.graceful }}</span>
        <span class="sw"><span class="dot f"></span> {{ T.legend.forceful }}</span>
        <span class="sw"><span class="dot b"></span> {{ T.legend.backstop }}</span>
      </div>
    </div>

    <p class="section-label">{{ T.sectionLogic }}</p>
    <div class="cases">
      <div class="case g">
        <span class="count">6</span>
        <h3><span class="badge g">Case A</span> {{ T.caseA.title }}</h3>
        <p class="rule">{{ T.caseA.rule }}</p>
        <p v-html="T.caseA.body"></p>
      </div>
      <div class="case f">
        <span class="count">6</span>
        <h3><span class="badge f">Case B</span> {{ T.caseB.title }}</h3>
        <p class="rule">{{ T.caseB.rule }}</p>
        <p v-html="T.caseB.body"></p>
      </div>
      <div class="case b">
        <span class="count">0</span>
        <h3><span class="badge b">Case C</span> {{ T.caseC.title }}</h3>
        <p class="rule">{{ T.caseC.rule }}</p>
        <p v-html="T.caseC.body"></p>
      </div>
    </div>

    <p class="section-label">{{ T.sectionLog }}</p>
    <div class="tbl-scroll">
      <table>
        <thead>
          <tr><th>{{ T.cols.time }}</th><th>{{ T.cols.age }}</th><th>{{ T.cols.phase }}</th><th>{{ T.cols.nc }}</th><th>{{ T.cols.note }}</th></tr>
        </thead>
        <tbody>
          <tr class="marker"><td class="time">08:38:20</td><td class="age">0</td><td colspan="3"><span class="mk">▸ {{ T.markers.batch.mk }}</span> — <span v-html="T.markers.batch.rest"></span></td></tr>
          <tr class="marker"><td class="time">09:26:20</td><td class="age">48m</td><td colspan="3"><span class="mk">▸ {{ T.markers.eligible.mk }}</span> — <span v-html="T.markers.eligible.rest"></span></td></tr>

          <template v-for="row in nodes" :key="row.n">
            <tr class="marker hard" v-if="row.n === 10">
              <td class="time">10:26:20</td><td class="age">1h48m</td>
              <td colspan="3"><span class="mk">▸ {{ T.markers.forcefulBoundary.mk }}</span> — <span v-html="T.markers.forcefulBoundary.rest"></span></td>
            </tr>
            <tr>
              <td class="time">{{ row.t }}</td>
              <td class="age">{{ row.age }}</td>
              <td><span :class="['phase', row.mode === 'graceful' ? 'g' : 'f']">{{ row.mode }}</span></td>
              <td class="node"><span :class="['num', row.mode === 'graceful' ? 'g' : 'f']">{{ row.n }}</span>{{ row.nc }}</td>
              <td class="ev" v-html="ja ? row.note.ja : row.note.en"></td>
            </tr>
          </template>

          <tr class="marker bs"><td class="time">10:38:20</td><td class="age">2h</td><td colspan="3"><span class="mk">▸ {{ T.markers.backstop.mk }}</span></td></tr>
        </tbody>
      </table>
    </div>

    <footer class="notes">
      <p v-for="(note, i) in T.footer" :key="i" v-html="note"></p>
    </footer>
  </div>
</template>

<style scoped>
:deep(.mono){font-family:ui-monospace,"SF Mono","Cascadia Code",Menlo,Consolas,monospace}
.ff-wrap{
  background:var(--fig-bg); color:var(--fig-text);
  font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
  line-height:1.55; -webkit-font-smoothing:antialiased;
  max-width:1060px; margin:0 auto; padding:44px 24px 72px;
}

header.top{margin-bottom:34px}
.eyebrow{
  font-family:ui-monospace,monospace; font-size:12px; letter-spacing:.14em;
  text-transform:uppercase; color:var(--fig-accent); margin:0 0 10px;
}
h1{
  font-size:clamp(24px,3.4vw,34px); line-height:1.15; margin:0 0 12px;
  letter-spacing:-.01em; text-wrap:balance; font-weight:680;
}
.lede{margin:0; max-width:66ch; color:var(--fig-muted); font-size:16px}
.lede :deep(b){color:var(--fig-text); font-weight:620}

/* metrics strip */
.metrics{
  display:grid; grid-template-columns:repeat(4,1fr); gap:12px; margin:28px 0 40px;
}
@media(max-width:620px){.metrics{grid-template-columns:repeat(2,1fr)}}
.tile{
  background:var(--fig-surface); border:1px solid var(--fig-line); border-radius:12px;
  padding:16px 16px 14px; box-shadow:var(--fig-shadow);
}
.tile .k{font-family:ui-monospace,monospace; font-size:11.5px; letter-spacing:.03em;
  text-transform:uppercase; color:var(--fig-muted); margin:0 0 8px; line-height:1.3}
.tile .v{font-family:ui-monospace,monospace; font-size:26px; font-weight:600;
  letter-spacing:-.01em; font-variant-numeric:tabular-nums; display:flex; align-items:baseline; gap:6px}
.tile .v small{font-size:13px; font-weight:500; color:var(--fig-faint)}
.tile.good .v{color:var(--fig-graceful)}
.tile .arrow{color:var(--fig-forceful)}

.section-label{
  font-family:ui-monospace,monospace; font-size:12px; letter-spacing:.12em;
  text-transform:uppercase; color:var(--fig-faint); margin:0 0 14px;
  display:flex; align-items:center; gap:10px;
}
.section-label::after{content:""; flex:1; height:1px; background:var(--fig-line)}

/* timeline card */
.panel{
  background:var(--fig-surface); border:1px solid var(--fig-line); border-radius:14px;
  box-shadow:var(--fig-shadow); padding:22px 8px 10px; margin-bottom:40px;
}
.tl-scroll{overflow-x:auto; padding:0 14px}
svg.tl{display:block; width:100%; min-width:760px; height:auto}
.zband{stroke:none}
.thr{stroke:var(--fig-line-strong); stroke-width:1.4; stroke-dasharray:4 4}
.thr.hard{stroke-dasharray:none; stroke-width:1.6}
.axis{stroke:var(--fig-line-strong); stroke-width:1.4}
.tick{stroke:var(--fig-line); stroke-width:1}
.t-lab{fill:var(--fig-muted); font-family:ui-monospace,monospace; font-size:11px}
.thr-name{fill:var(--fig-text); font-family:ui-monospace,monospace; font-size:11px; font-weight:600; letter-spacing:.02em}
.thr-sub{fill:var(--fig-faint); font-family:ui-monospace,monospace; font-size:10px}
.zone-lab{font-family:ui-monospace,monospace; font-size:11px; letter-spacing:.14em; text-transform:uppercase; font-weight:600}
.pin{stroke:var(--fig-surface); stroke-width:2}
.pin.g{fill:var(--fig-graceful)} .pin.f{fill:var(--fig-forceful)}
.pin-n{font-family:ui-monospace,monospace; font-size:10px; font-weight:700; fill:var(--fig-pin-num)}
.flag{fill:var(--fig-muted)}
@media (prefers-reduced-motion:no-preference){
  .pin-grp{opacity:0; animation:pop .4s ease forwards}
  @keyframes pop{from{opacity:0; transform:translateY(4px)}to{opacity:1; transform:none}}
}
.tl-legend{display:flex; flex-wrap:wrap; gap:18px; padding:10px 20px 6px; font-size:13px; color:var(--fig-muted)}
.tl-legend .sw{display:inline-flex; align-items:center; gap:7px}
.dot{width:11px; height:11px; border-radius:50%; display:inline-block}
.dot.g{background:var(--fig-graceful)} .dot.f{background:var(--fig-forceful)}
.dot.b{background:var(--fig-backstop)}

/* case cards */
.cases{display:grid; grid-template-columns:repeat(3,1fr); gap:14px; margin-bottom:42px}
@media(max-width:760px){.cases{grid-template-columns:1fr}}
.case{
  background:var(--fig-surface); border:1px solid var(--fig-line); border-radius:12px;
  padding:18px; box-shadow:var(--fig-shadow); position:relative; overflow:hidden;
}
.case::before{content:""; position:absolute; left:0; top:0; bottom:0; width:4px}
.case.g::before{background:var(--fig-graceful)}
.case.f::before{background:var(--fig-forceful)}
.case.b::before{background:var(--fig-backstop)}
.case h3{margin:0 0 4px; font-size:15.5px; display:flex; align-items:center; gap:8px}
.badge{font-family:ui-monospace,monospace; font-size:10.5px; font-weight:700; letter-spacing:.04em;
  text-transform:uppercase; padding:2px 7px; border-radius:5px}
.badge.g{color:var(--fig-graceful); background:var(--fig-graceful-fill)}
.badge.f{color:var(--fig-forceful); background:var(--fig-forceful-fill)}
.badge.b{color:var(--fig-backstop); background:var(--fig-backstop-fill)}
.case .rule{font-family:ui-monospace,monospace; font-size:12px; color:var(--fig-accent); margin:6px 0 10px}
.case p{margin:0; font-size:13.5px; color:var(--fig-muted)}
.case p :deep(b){color:var(--fig-text); font-weight:600}
.case .count{position:absolute; right:14px; top:14px; font-family:ui-monospace,monospace;
  font-size:22px; font-weight:700; font-variant-numeric:tabular-nums}
.case.g .count{color:var(--fig-graceful)} .case.f .count{color:var(--fig-forceful)}
.case.b .count{color:var(--fig-backstop); opacity:.7}

/* event table */
.tbl-scroll{overflow-x:auto; border:1px solid var(--fig-line); border-radius:12px; box-shadow:var(--fig-shadow)}
table{border-collapse:collapse; width:100%; min-width:660px; background:var(--fig-surface); font-size:13.5px}
thead th{
  font-family:ui-monospace,monospace; font-size:11px; letter-spacing:.05em; text-transform:uppercase;
  color:var(--fig-muted); text-align:left; padding:11px 14px; border-bottom:1px solid var(--fig-line-strong); font-weight:600;
}
tbody td{padding:9px 14px; border-bottom:1px solid var(--fig-line); vertical-align:top}
tbody tr:last-child td{border-bottom:none}
td.time,td.age,td.node{font-family:ui-monospace,monospace; font-variant-numeric:tabular-nums; white-space:nowrap}
td.age{color:var(--fig-muted)}
.phase{font-family:ui-monospace,monospace; font-size:10.5px; font-weight:700; letter-spacing:.03em;
  text-transform:uppercase; padding:2px 7px; border-radius:5px; white-space:nowrap}
.phase.g{color:var(--fig-graceful); background:var(--fig-graceful-fill)}
.phase.f{color:var(--fig-forceful); background:var(--fig-forceful-fill)}
tr.marker td{background:var(--fig-surface-2)}
tr.marker .mk{font-family:ui-monospace,monospace; font-size:11.5px; font-weight:700; letter-spacing:.04em;
  text-transform:uppercase; color:var(--fig-accent)}
tr.marker.hard .mk{color:var(--fig-forceful)}
tr.marker.bs .mk{color:var(--fig-backstop)}
td .ev{color:var(--fig-muted); font-size:13px}
td .ev :deep(code){font-family:ui-monospace,monospace; font-size:12px; color:var(--fig-text);
  background:var(--fig-surface-2); padding:1px 5px; border-radius:4px}
.num{display:inline-flex; align-items:center; justify-content:center; width:19px; height:19px;
  border-radius:50%; font-family:ui-monospace,monospace; font-size:11px; font-weight:700; color:var(--fig-pin-num); margin-right:2px}
.num.g{background:var(--fig-graceful)} .num.f{background:var(--fig-forceful)}

footer.notes{margin-top:36px; padding-top:20px; border-top:1px solid var(--fig-line);
  color:var(--fig-muted); font-size:13px; max-width:74ch}
footer.notes p{margin:0 0 10px}
footer.notes :deep(code){font-family:ui-monospace,monospace; font-size:12px; color:var(--fig-text)}
footer.notes :deep(b){color:var(--fig-text)}
</style>
