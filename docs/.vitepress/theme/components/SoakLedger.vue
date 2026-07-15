<script setup lang="ts">
import { computed } from 'vue'
import { useData } from 'vitepress'
import { ROTATIONS, EXPIRE_AFTER } from './scenario-p-data'

const { lang } = useData()
const ja = computed(() => lang.value.startsWith('ja'))

// "07-14 13:18:50" from "2026-07-14T13:18:50Z" — the year is constant and the
// full instant is in the data module for anyone re-running the scenario.
const fmt = (iso: string) => `${iso.slice(5, 10)} ${iso.slice(11, 19)}`

const T = computed(() =>
  ja.value
    ? {
        summary: `全回転台帳 — 72 行（本編 71 + epi 1）を開く`,
        sub: `誕生・完了は UTC。margin = 誕生 + ${EXPIRE_AFTER} − 完了。surge 先はすべて新規 provision の EC2 インスタンス。epi 行の「—」（placeholder/surge なし）が surge-less パスの証拠そのもの。`,
        cols: { n: '#', claim: 'claim', mode: 'mode', birth: '誕生', done: '完了', sw: 'surgeWait', dr: 'drain', tot: 'total', m: 'margin(分)', node: 'surge 先ノード' },
      }
    : {
        summary: `Open the full per-rotation ledger — 72 rows (71 main + 1 epilogue)`,
        sub: `Birth/completion are UTC. margin = birth + ${EXPIRE_AFTER} − completion. Every surge target is a freshly provisioned EC2 instance. The epilogue row's "—" (no placeholder, no surge node) is itself the evidence of the surge-less path.`,
        cols: { n: '#', claim: 'claim', mode: 'mode', birth: 'birth', done: 'done', sw: 'surgeWait', dr: 'drain', tot: 'total', m: 'margin (min)', node: 'surge node' },
      },
)
</script>

<template>
  <details class="soak-ledger">
    <summary>{{ T.summary }}</summary>
    <p class="sub">{{ T.sub }}</p>
    <div class="tblwrap">
      <table>
        <thead>
          <tr>
            <th class="num">{{ T.cols.n }}</th>
            <th>{{ T.cols.claim }}</th>
            <th>{{ T.cols.mode }}</th>
            <th>{{ T.cols.birth }}</th>
            <th>{{ T.cols.done }}</th>
            <th class="num">{{ T.cols.sw }}</th>
            <th class="num">{{ T.cols.dr }}</th>
            <th class="num">{{ T.cols.tot }}</th>
            <th class="num">{{ T.cols.m }}</th>
            <th>{{ T.cols.node }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(d, i) in ROTATIONS" :key="d.c" :class="{ epi: d.p === 'epi' }">
            <td class="num">{{ i + 1 }}</td>
            <td class="mono">{{ d.c }}</td>
            <td><span :class="['mode', d.p === 'epi' ? 'f' : 'g']">{{ d.p === 'epi' ? 'forceful-fallback' : 'surge' }}</span></td>
            <td class="mono">{{ fmt(d.birth) }}</td>
            <td class="mono">{{ fmt(d.done) }}</td>
            <td class="num">{{ d.sw !== null ? d.sw + 's' : '—' }}</td>
            <td class="num">{{ d.dr }}s</td>
            <td class="num">{{ d.tot !== null ? d.tot + 's' : '—' }}</td>
            <td class="num margin">{{ d.m.toFixed(1) }}</td>
            <td class="mono">{{ d.node ?? '—' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </details>
</template>

<style scoped>
.soak-ledger {
  margin: 16px 0;
  padding: 12px 16px;
  border: 1px solid var(--fig-line);
  border-radius: 12px;
  background: var(--fig-surface);
}
summary { cursor: pointer; font-weight: 600; color: var(--fig-text); }
summary:hover { color: var(--vp-c-brand-1); }
.sub { margin: 10px 0 6px; font-size: 12.5px; line-height: 1.6; color: var(--fig-muted); }
/* The table scrolls in BOTH axes inside its own wrapper — 72 rows must never
   stretch the page, and the min-content width must never widen it (#250). */
.tblwrap { overflow: auto; max-height: 460px; border: 1px solid var(--fig-line); border-radius: 8px; }
table { width: 100%; min-width: 760px; margin: 0; border-collapse: collapse; font-size: 12.5px; }
th {
  position: sticky; top: 0; z-index: 1;
  padding: 7px 10px; text-align: left;
  font-size: 11px; font-weight: 600; letter-spacing: 0.06em; text-transform: uppercase;
  color: var(--fig-muted); background: var(--fig-surface-2);
}
td { padding: 5px 10px; border-bottom: 1px solid var(--fig-line); color: var(--fig-text); vertical-align: top; }
tr:last-child td { border-bottom: none; }
.mono, td.num, th.num { font-family: var(--vp-font-family-mono); font-variant-numeric: tabular-nums; }
td.num, th.num { text-align: right; white-space: nowrap; }
td.mono { white-space: nowrap; }
.mode { font-family: var(--vp-font-family-mono); font-size: 10.5px; font-weight: 700; letter-spacing: 0.03em; text-transform: uppercase; padding: 1px 6px; border-radius: 5px; white-space: nowrap; }
.mode.g { color: var(--fig-graceful); background: var(--fig-graceful-fill); }
.mode.f { color: var(--fig-forceful); background: var(--fig-forceful-fill); }
tr.epi td.margin { color: var(--fig-forceful); font-weight: 700; }
</style>
