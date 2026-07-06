<script setup lang="ts">
import { computed } from 'vue'
import { useData } from 'vitepress'
const { lang } = useData()
const ja = computed(() => lang.value.startsWith('ja'))

// 17 scenario columns, in display order (source: coverage-matrix.html thead).
const columns = ['0', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M-A', 'M-B', 'N', 'O']

// Group display order (source: the 6 `tr.grp` subheader rows).
const groupOrder = ['surgePlacement', 'gates', 'backstop', 'dnd', 'robustness', 'forcefulFallback']

// 17 capability rows, transcribed cell-by-cell from the source `<table class="mx">`.
// `cells` is aligned 1:1 with `columns` above. 'p' = primary (●), 's' = secondary (◯), 'n' = none.
// `ovl` marks the two overlapping rows (source `tr.ovl`): backstop (D⊂I) and readyTimeout rollback (C≈M-B).
// `ext` marks the two external-guarantee rows (source `tr.ext`): Karpenter honor (N) and zonal-EBS (A).
const rows = [
  {
    id: 'surgeNew', group: 'surgePlacement',
    cells: ['p', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 's', 'n', 'n', 'n', 'n', 'n', 'n', 's'],
  },
  {
    id: 'surgeAbsorb', group: 'surgePlacement',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'n', 'n'],
  },
  {
    id: 'placementConfinement', group: 'surgePlacement',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 's', 'n', 'n', 'n', 'n', 'n', 'n'],
  },
  {
    id: 'gateLimits', group: 'gates',
    cells: ['n', 'n', 'p', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 's', 'n', 'n'],
  },
  {
    id: 'gateWindow', group: 'gates',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n'],
  },
  {
    id: 'backstop', group: 'backstop', ovl: true,
    cells: ['n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'n', 'n', 's'],
  },
  {
    id: 'rollback', group: 'backstop', ovl: true,
    cells: ['n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n'],
  },
  {
    id: 'expiredOutcome', group: 'backstop',
    cells: ['n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 's'],
  },
  {
    id: 'pdbDrain', group: 'backstop',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 's'],
  },
  {
    id: 'dndWrite', group: 'dnd',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n'],
  },
  {
    id: 'dndExclude', group: 'dnd', spec: '#170',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p'],
  },
  {
    id: 'dndHonor', group: 'dnd', ext: true,
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n'],
  },
  {
    id: 'leaderResume', group: 'robustness',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n', 'n', 'n'],
  },
  {
    id: 'preemption', group: 'robustness',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p', 'n', 'n', 'n'],
  },
  {
    id: 'zonalEbs', group: 'robustness', ext: true,
    cells: ['n', 'p', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n'],
  },
  {
    id: 'forcefulFallback', group: 'forcefulFallback', spec: '#156',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p'],
  },
  {
    id: 'earliestDeadline', group: 'forcefulFallback', spec: '#157',
    cells: ['n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'n', 'p'],
  },
]

const groupedRows = computed(() => groupOrder.map((gid) => ({ id: gid, rows: rows.filter((r) => r.group === gid) })))

function tagClass(row: (typeof rows)[number]) {
  return row.ovl ? 'warn' : row.ext ? 'ext' : null
}

// Bilingual strings. EN values are translations of the source JA labels; column
// tooltips and inline code/spec tokens are language-invariant and reused as-is.
const T = computed(() => ja.value ? {
  eyebrow: 'EKS Auto Mode PoC · SCENARIOS.md × VALIDATION.md',
  h1: 'シナリオ・カバレッジ・マトリクス',
  lede: '縦 = 検証される<b>能力（挙動レイヤ）</b>、横 = <b>シナリオ 0〜O</b>。<span class="mono">●</span> = そのシナリオの<b>主眼</b>、<span class="mono">◯</span> = 副次的に通過。同じ能力を <span class="mono">●</span> が2つ持つ行が<b>重複</b>、<span class="chip ext">☁</span> 行は<b>Karpenter/AWS 自体の保証</b>（自コントローラのロジックではない）。',
  metrics: {
    scenarios: { k: 'シナリオ', v: '17', small: '0, A–O' },
    capabilities: { k: '検証される能力', v: '17', small: 'レイヤ' },
    overlap: { k: '重複している能力行', v: '2', small: 'backstop · rollback' },
    external: { k: 'Karpenter/AWS 寄り', v: '2', small: 'N · A(一部)' },
  },
  legend: {
    primary: '主眼（primary）',
    secondary: '副次的に通過（secondary）',
    none: '対象外',
    overlapChip: '重複',
    overlapText: '同一能力を2シナリオが主眼に',
    extChip: '外部',
    extText: 'Karpenter/AWS の保証',
  },
  rowheadLabel: '能力 / シナリオ →',
  colTitles: {
    '0': 'core make-before-break surge',
    A: 'zonal-EBS rebind',
    B: 'limits gates surge',
    C: 'readyTimeout rollback',
    D: 'expireAfter backstop (R6)',
    E: 'expired outcome',
    F: 'multi-NodePool confinement',
    G: 'PDB-respected drain',
    H: 'do-not-disrupt applied on surge',
    I: 'scaled R6 soak',
    J: 'capacity-absorb',
    K: 'leader-change resume',
    L: 'window boundary',
    'M-A': 'placeholder preemption victim/Never',
    'M-B': 'readyTimeout-bounded rollback',
    N: 'do-not-disrupt honored vs Drift',
    O: 'trick-free forceful fallback + #157 + #170',
  },
  groups: {
    surgePlacement: 'Surge & 配置',
    gates: 'ゲート（開始しない / 境界）',
    backstop: 'Backstop・失敗・outcome',
    dnd: 'do-not-disrupt（同一キー・3面）',
    robustness: '堅牢性・その他',
    forcefulFallback: 'Forceful fallback（v0.4+ / #156・#157・#170）',
  },
  rows: {
    surgeNew: { label: 'Surge — 新規プロビジョニング', sub: 'make-before-break' },
    surgeAbsorb: { label: 'Surge — capacity-absorb', sub: '既存 spare へ bin-pack（新ノード無し）' },
    placementConfinement: { label: '配置 — NodePool 閉じ込め', sub: '別プールの spare へ漏れない' },
    gateLimits: { label: 'ゲート — NodePool <code class="mono">limits</code>', sub: '余地無しなら surge しない' },
    gateWindow: { label: 'ゲート — メンテナンス窓 / 境界', sub: 'in-flight 完了・境界後は開始せず' },
    backstop: { label: '<code class="mono">expireAfter</code> backstop (R6)', tag: '⚠ D⊂I', sub: 'lead time が勝ち切る' },
    rollback: { label: '<code class="mono">readyTimeout</code> rollback + cleanup', tag: '⚠ C≈M-B', sub: 'state=failed / retry++ / uncordon' },
    expiredOutcome: { label: '強制失効の <code class="mono">expired</code> outcome', sub: 'pending 中に捕捉' },
    pdbDrain: { label: 'PDB 尊重の voluntary drain', sub: 'minAvailable が実際にブロック' },
    dndWrite: { label: '① コントローラが<b>書く</b>', sub: 'surge ペア保護 + owned marker' },
    dndExclude: { label: '② 候補選択から<b>除外</b>（読む）' },
    dndHonor: { label: '③ Karpenter が <b>honor</b>（Drift）', tag: '☁ 外部' },
    leaderResume: { label: 'leader 交代の再開', sub: 'annotation のみから継続' },
    preemption: { label: 'placeholder の preemption', sub: '被害者 / preemptionPolicy=Never' },
    zonalEbs: { label: 'zonal-EBS(PV) 再アタッチ', tag: '☁ 一部', sub: 'ステートフル・CSI/AWS' },
    forcefulFallback: { label: 'surge-less forceful fallback' },
    earliestDeadline: { label: 'earliest-deadline 順序' },
  },
  columnLegend: '<b>列凡例</b> — <b>0</b> core surge · <b>A</b> zonal-EBS · <b>B</b> limits · <b>C</b> readyTimeout · <b>D</b> backstop · <b>E</b> expired · <b>F</b> confinement · <b>G</b> PDB · <b>H</b> dnd-write · <b>I</b> R6 soak · <b>J</b> absorb · <b>K</b> leader · <b>L</b> window · <b>M-A</b> preemption · <b>M-B</b> rollback · <b>N</b> dnd-honor · <b>O</b> forceful-fallback(+#157/#170)',
  summaryLabel: '重複・包含サマリ',
  cards: {
    backstop: {
      rel: 'D ⊂ I', title: 'backstop',
      body: '<b>I が D を包含。</b>D は backstop メトリクスの<b>静的スナップショット</b>、I は<b>連続 rotation で動的に</b>同じ性質を実証。R6 の実証は I 単体で足りる。',
      rec: '再検証は <b>I を残し D を畳む</b>（静的値は I の 1 行に併記）。',
    },
    rollback: {
      rel: 'C ≈ M-B', title: 'rollback',
      body: '両者とも同じ <b>rollback クリーンアップ</b>（<code class="mono">state=failed</code>・retry++・uncordon・surge 破棄）を検証。違いは<b>引き金だけ</b>（C=ノード未 Ready／M-B=placeholder が Pending）。',
      rec: 'rollback アサーションは<b>どちらか一方</b>で足りる（M-A の preemption は固有なので残す）。',
    },
    external: {
      rel: 'N · A(一部)', title: '外部保証',
      body: '<b>N</b>（Karpenter が do-not-disrupt を honor）と <b>A の EBS 再アタッチ</b>は、自コントローラのロジックというより <b>Karpenter/AWS の保証</b>の確認（de-risking）。',
      rec: 'コスト削減時は<b>優先度を下げられる</b>（ドキュメント信頼が前提）。',
    },
  },
  footer: [
    '<b>do-not-disrupt は「重複」ではなく 3 面</b>: ①書く(H) / ②読んで除外(#170, O) / ③Karpenter が honor(N) は別レイヤなので冗長ではない。ただし同一キーに集中するので、まとめて把握しておくと変更時の影響範囲が読みやすい。',
    '<b>最小カバー集合の目安</b>: 固有 = <code>0, A, B, E, F, G, J, K, L, M-A, O</code>。重複を畳むなら <code>I</code>(D 内包) + <code>C か M-B</code> のどちらか、<code>N</code> はオプション。<b>O は多能力</b>（#156 本体 + #157 + #170、backstop/expired/PDB/surge も副次的に通過）。',
    '出典: <code>test/e2e/eks-automode/SCENARIOS.md</code>（手順）と <code>VALIDATION.md</code>（各シナリオの観測証拠）、spec <code>§7.2</code>。<span class="mono">●/◯</span> は「主眼か副次か」の判断であり、副次通過を網羅列挙したものではない。',
  ],
} : {
  eyebrow: 'EKS Auto Mode PoC · SCENARIOS.md × VALIDATION.md',
  h1: 'Scenario coverage matrix',
  lede: 'Rows = the <b>capability (behavior layer)</b> under test, columns = <b>scenarios 0–O</b>. <span class="mono">●</span> = that scenario\'s <b>primary focus</b>, <span class="mono">◯</span> = passes through as a secondary effect. A row where two scenarios both have <span class="mono">●</span> is <b>overlapping</b>; rows marked <span class="chip ext">☁</span> are <b>guarantees from Karpenter/AWS itself</b> (not this controller\'s logic).',
  metrics: {
    scenarios: { k: 'Scenarios', v: '17', small: '0, A–O' },
    capabilities: { k: 'Capabilities under test', v: '17', small: 'layers' },
    overlap: { k: 'Overlapping capability rows', v: '2', small: 'backstop · rollback' },
    external: { k: 'Karpenter/AWS-leaning', v: '2', small: 'N · A (partial)' },
  },
  legend: {
    primary: 'primary focus',
    secondary: 'passes through (secondary)',
    none: 'not covered',
    overlapChip: 'overlap',
    overlapText: 'the same capability is the primary focus of 2 scenarios',
    extChip: 'external',
    extText: 'a Karpenter/AWS guarantee',
  },
  rowheadLabel: 'Capability / scenario →',
  colTitles: {
    '0': 'core make-before-break surge',
    A: 'zonal-EBS rebind',
    B: 'limits gates surge',
    C: 'readyTimeout rollback',
    D: 'expireAfter backstop (R6)',
    E: 'expired outcome',
    F: 'multi-NodePool confinement',
    G: 'PDB-respected drain',
    H: 'do-not-disrupt applied on surge',
    I: 'scaled R6 soak',
    J: 'capacity-absorb',
    K: 'leader-change resume',
    L: 'window boundary',
    'M-A': 'placeholder preemption victim/Never',
    'M-B': 'readyTimeout-bounded rollback',
    N: 'do-not-disrupt honored vs Drift',
    O: 'trick-free forceful fallback + #157 + #170',
  },
  groups: {
    surgePlacement: 'Surge & placement',
    gates: "Gates (don't start / boundary)",
    backstop: 'Backstop, failure & outcome',
    dnd: 'do-not-disrupt (same key, 3 facets)',
    robustness: 'Robustness & other',
    forcefulFallback: 'Forceful fallback (v0.4+ / #156, #157, #170)',
  },
  rows: {
    surgeNew: { label: 'Surge — new provisioning', sub: 'make-before-break' },
    surgeAbsorb: { label: 'Surge — capacity-absorb', sub: 'bin-packs onto existing spare (no new node)' },
    placementConfinement: { label: 'Placement — NodePool confinement', sub: "doesn't leak onto another pool's spare" },
    gateLimits: { label: 'Gate — NodePool <code class="mono">limits</code>', sub: 'no surge when there is no headroom' },
    gateWindow: { label: 'Gate — maintenance window / boundary', sub: 'in-flight finishes; nothing new starts past the boundary' },
    backstop: { label: '<code class="mono">expireAfter</code> backstop (R6)', tag: '⚠ D⊂I', sub: 'lead time wins out' },
    rollback: { label: '<code class="mono">readyTimeout</code> rollback + cleanup', tag: '⚠ C≈M-B', sub: 'state=failed / retry++ / uncordon' },
    expiredOutcome: { label: 'Forced-expiry <code class="mono">expired</code> outcome', sub: 'caught while pending' },
    pdbDrain: { label: 'PDB-respecting voluntary drain', sub: 'minAvailable actually blocks' },
    dndWrite: { label: '① the controller <b>writes</b>', sub: 'surge-pair protection + owned marker' },
    dndExclude: { label: '② <b>excluded</b> from candidate selection (reads)' },
    dndHonor: { label: '③ Karpenter <b>honors</b> it (Drift)', tag: '☁ external' },
    leaderResume: { label: 'Resume across leader change', sub: 'continues from annotations alone' },
    preemption: { label: 'placeholder preemption', sub: 'victim / preemptionPolicy=Never' },
    zonalEbs: { label: 'zonal-EBS (PV) reattach', tag: '☁ partial', sub: 'stateful workloads · CSI/AWS' },
    forcefulFallback: { label: 'surge-less forceful fallback' },
    earliestDeadline: { label: 'earliest-deadline order' },
  },
  columnLegend: '<b>Column legend</b> — <b>0</b> core surge · <b>A</b> zonal-EBS · <b>B</b> limits · <b>C</b> readyTimeout · <b>D</b> backstop · <b>E</b> expired · <b>F</b> confinement · <b>G</b> PDB · <b>H</b> dnd-write · <b>I</b> R6 soak · <b>J</b> absorb · <b>K</b> leader · <b>L</b> window · <b>M-A</b> preemption · <b>M-B</b> rollback · <b>N</b> dnd-honor · <b>O</b> forceful-fallback(+#157/#170)',
  summaryLabel: 'Overlap & containment summary',
  cards: {
    backstop: {
      rel: 'D ⊂ I', title: 'backstop',
      body: '<b>I subsumes D.</b> D is a <b>static snapshot</b> of the backstop metric, while I demonstrates the same property <b>dynamically across a continuous rotation</b>. Proving R6 via I alone is sufficient.',
      rec: 'For re-validation, <b>keep I and fold D away</b> (note the static value alongside I\'s row).',
    },
    rollback: {
      rel: 'C ≈ M-B', title: 'rollback',
      body: 'Both verify the same <b>rollback cleanup</b> (<code class="mono">state=failed</code>, retry++, uncordon, surge teardown). The only difference is the <b>trigger</b> (C = node not Ready / M-B = placeholder stuck Pending).',
      rec: 'Either one alone is enough for the rollback assertions (M-A\'s preemption is distinct, so keep it).',
    },
    external: {
      rel: 'N · A (partial)', title: 'external guarantees',
      body: '<b>N</b> (Karpenter honoring do-not-disrupt) and <b>A\'s EBS reattach</b> are more a check of <b>Karpenter/AWS\'s own guarantees</b> (de-risking) than of this controller\'s logic.',
      rec: 'Can be <b>deprioritized under cost pressure</b> (on the assumption that the docs can be trusted).',
    },
  },
  footer: [
    '<b>do-not-disrupt is not a "duplicate" but 3 facets</b>: ① writes it (H) / ② reads and excludes it (#170, O) / ③ Karpenter honors it (N) are separate layers, so this is not redundancy. But since they all revolve around the same key, understanding them together makes the blast radius of a change easier to read.',
    '<b>Rough minimal-cover set</b>: unique = <code>0, A, B, E, F, G, J, K, L, M-A, O</code>. To fold overlaps: <code>I</code> (subsumes D) plus either <code>C</code> or <code>M-B</code>, with <code>N</code> optional. <b>O is multi-capability</b> (the #156 core plus #157 and #170; it also passes through backstop/expired/PDB/surge secondarily).',
    'Source: <code>test/e2e/eks-automode/SCENARIOS.md</code> (procedure) and <code>VALIDATION.md</code> (observed evidence per scenario), spec <code>§7.2</code>. <span class="mono">●/◯</span> reflect a judgment call about primary vs. secondary focus, not an exhaustive enumeration of every secondary pass-through.',
  ],
})
</script>

<template>
  <div class="cm-wrap">
    <p class="eyebrow">{{ T.eyebrow }}</p>
    <h1>{{ T.h1 }}</h1>
    <p class="lede" v-html="T.lede"></p>

    <div class="metrics">
      <div class="tile">
        <p class="k">{{ T.metrics.scenarios.k }}</p>
        <p class="v">{{ T.metrics.scenarios.v }} <small>{{ T.metrics.scenarios.small }}</small></p>
      </div>
      <div class="tile">
        <p class="k">{{ T.metrics.capabilities.k }}</p>
        <p class="v">{{ T.metrics.capabilities.v }} <small>{{ T.metrics.capabilities.small }}</small></p>
      </div>
      <div class="tile warn">
        <p class="k">{{ T.metrics.overlap.k }}</p>
        <p class="v">{{ T.metrics.overlap.v }} <small>{{ T.metrics.overlap.small }}</small></p>
      </div>
      <div class="tile ext">
        <p class="k">{{ T.metrics.external.k }}</p>
        <p class="v">{{ T.metrics.external.v }} <small>{{ T.metrics.external.small }}</small></p>
      </div>
    </div>

    <div class="legend">
      <span class="item"><span class="dot p"></span> {{ T.legend.primary }}</span>
      <span class="item"><span class="dot s"></span> {{ T.legend.secondary }}</span>
      <span class="item"><span class="dot n"></span> {{ T.legend.none }}</span>
      <span class="item"><span class="chip warn">⚠ {{ T.legend.overlapChip }}</span> {{ T.legend.overlapText }}</span>
      <span class="item"><span class="chip ext">☁ {{ T.legend.extChip }}</span> {{ T.legend.extText }}</span>
    </div>

    <div class="mx-scroll">
      <table class="cm-mx">
        <thead>
          <tr>
            <th class="rowhead">{{ T.rowheadLabel }}</th>
            <th v-for="col in columns" :key="col" :title="T.colTitles[col]">{{ col }}</th>
          </tr>
        </thead>
        <tbody>
          <template v-for="g in groupedRows" :key="g.id">
            <tr class="grp"><td colspan="18">{{ T.groups[g.id] }}</td></tr>
            <tr v-for="row in g.rows" :key="row.id" :class="{ ovl: row.ovl, ext: row.ext }">
              <td class="rl">
                <span v-html="T.rows[row.id].label"></span><span
                  v-if="T.rows[row.id].tag"
                  class="tag"
                  :class="tagClass(row)"
                >{{ T.rows[row.id].tag }}</span><span
                  v-if="row.spec"
                  class="spec"
                >{{ row.spec }}</span><span
                  v-if="T.rows[row.id].sub"
                  class="sub"
                >{{ T.rows[row.id].sub }}</span>
              </td>
              <td class="c" v-for="(state, i) in row.cells" :key="i"><span class="dot" :class="state"></span></td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <p class="codes" v-html="T.columnLegend"></p>

    <p class="section-label">{{ T.summaryLabel }}</p>
    <div class="cards">
      <div class="card">
        <h3><span class="rel">{{ T.cards.backstop.rel }}</span> {{ T.cards.backstop.title }}</h3>
        <p v-html="T.cards.backstop.body"></p>
        <p class="rec" v-html="T.cards.backstop.rec"></p>
      </div>
      <div class="card">
        <h3><span class="rel">{{ T.cards.rollback.rel }}</span> {{ T.cards.rollback.title }}</h3>
        <p v-html="T.cards.rollback.body"></p>
        <p class="rec" v-html="T.cards.rollback.rec"></p>
      </div>
      <div class="card ext">
        <h3><span class="rel">{{ T.cards.external.rel }}</span> {{ T.cards.external.title }}</h3>
        <p v-html="T.cards.external.body"></p>
        <p class="rec" v-html="T.cards.external.rec"></p>
      </div>
    </div>

    <footer class="notes">
      <p v-for="(note, i) in T.footer" :key="i" v-html="note"></p>
    </footer>
  </div>
</template>

<style scoped>
:deep(.mono){font-family:ui-monospace,"SF Mono","Cascadia Code",Menlo,Consolas,monospace}
.cm-wrap{
  background:var(--fig-bg); color:var(--fig-text);
  font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
  line-height:1.55; -webkit-font-smoothing:antialiased;
  max-width:1120px; margin:0 auto; padding:44px 24px 72px;
}

.eyebrow{font-family:ui-monospace,monospace;font-size:12px;letter-spacing:.14em;text-transform:uppercase;color:var(--fig-graceful);margin:0 0 10px}
h1{font-size:clamp(23px,3.2vw,32px);line-height:1.15;margin:0 0 12px;letter-spacing:-.01em;text-wrap:balance;font-weight:680}
.lede{margin:0;max-width:70ch;color:var(--fig-muted);font-size:15.5px}
.lede :deep(b){color:var(--fig-text);font-weight:620}

.metrics{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin:28px 0 30px}
@media(max-width:620px){.metrics{grid-template-columns:repeat(2,1fr)}}
.tile{background:var(--fig-surface);border:1px solid var(--fig-line);border-radius:12px;padding:15px 16px 13px;box-shadow:var(--fig-shadow)}
.tile .k{font-family:ui-monospace,monospace;font-size:11px;letter-spacing:.03em;text-transform:uppercase;color:var(--fig-muted);margin:0 0 7px;line-height:1.3}
.tile .v{font-family:ui-monospace,monospace;font-size:25px;font-weight:600;letter-spacing:-.01em;font-variant-numeric:tabular-nums;display:flex;align-items:baseline;gap:6px}
.tile .v small{font-size:12px;font-weight:500;color:var(--fig-faint)}
.tile.warn .v{color:var(--fig-forceful)} .tile.ext .v{color:var(--fig-ext)}

.legend{display:flex;flex-wrap:wrap;gap:16px 22px;margin:0 0 18px;font-size:13px;color:var(--fig-muted);align-items:center}
.legend .item{display:inline-flex;align-items:center;gap:8px}
.dot{display:inline-block;border-radius:50%}
.dot.p{width:13px;height:13px;background:var(--fig-graceful)}
.dot.s{width:12px;height:12px;background:transparent;border:2px solid var(--fig-graceful)}
.dot.n{width:6px;height:6px;background:var(--fig-line-strong);opacity:.55}
.chip{font-family:ui-monospace,monospace;font-size:10.5px;font-weight:700;letter-spacing:.03em;padding:2px 7px;border-radius:5px}
.chip.warn{color:var(--fig-forceful);background:var(--fig-forceful-fill)}
.chip.ext{color:var(--fig-ext);background:var(--fig-ext-soft)}

.section-label{font-family:ui-monospace,monospace;font-size:12px;letter-spacing:.12em;text-transform:uppercase;color:var(--fig-faint);margin:32px 0 14px;display:flex;align-items:center;gap:10px}
.section-label::after{content:"";flex:1;height:1px;background:var(--fig-line)}

.mx-scroll{overflow-x:auto;border:1px solid var(--fig-line);border-radius:12px;box-shadow:var(--fig-shadow)}
table.cm-mx{border-collapse:separate;border-spacing:0;background:var(--fig-surface);font-size:13px;min-width:900px}
table.cm-mx th,table.cm-mx td{padding:0;text-align:center}
/* column headers */
table.cm-mx thead th{position:sticky;top:0;z-index:3;background:var(--fig-surface-2);
  font-family:ui-monospace,monospace;font-size:11.5px;font-weight:700;color:var(--fig-text);
  height:38px;width:40px;border-bottom:1px solid var(--fig-line-strong);border-left:1px solid var(--fig-line)}
table.cm-mx thead th.rowhead{z-index:5;left:0;text-align:left;width:262px;min-width:262px;padding-left:14px;
  font-size:11px;letter-spacing:.07em;text-transform:uppercase;color:var(--fig-muted)}
/* row label cells */
td.rl{position:sticky;left:0;z-index:2;background:var(--fig-surface);text-align:left;
  width:262px;min-width:262px;padding:8px 12px;border-bottom:1px solid var(--fig-line);border-right:1px solid var(--fig-line-strong);
  font-size:13px;line-height:1.35}
td.rl .sub{display:block;color:var(--fig-faint);font-size:11.5px;margin-top:1px}
td.rl .spec{font-family:ui-monospace,monospace;font-size:10.5px;color:var(--fig-graceful)}
/* data cells */
td.c{width:40px;height:38px;border-bottom:1px solid var(--fig-line);border-left:1px solid var(--fig-line)}
/* group subheader row */
tr.grp td{position:sticky;left:0;background:var(--fig-surface-2);color:var(--fig-muted);
  font-family:ui-monospace,monospace;font-size:10.5px;letter-spacing:.1em;text-transform:uppercase;font-weight:700;
  padding:7px 12px;border-bottom:1px solid var(--fig-line);border-top:1px solid var(--fig-line-strong)}
/* overlap + external row accents */
tr.ovl td.rl{background:var(--fig-forceful-fill);border-right-color:var(--fig-forceful)}
tr.ovl td.c{background:linear-gradient(0deg,var(--fig-forceful-fill),var(--fig-forceful-fill))}
tr.ext td.rl{background:var(--fig-ext-soft);border-right-color:var(--fig-ext)}
tr.ext td.c{background:linear-gradient(0deg,var(--fig-ext-soft),var(--fig-ext-soft))}
.tag{font-family:ui-monospace,monospace;font-size:9.5px;font-weight:700;letter-spacing:.03em;padding:1px 5px;border-radius:4px;margin-left:6px;vertical-align:middle}
.tag.warn{color:var(--fig-forceful);background:var(--fig-forceful-fill);border:1px solid var(--fig-forceful)}
.tag.ext{color:var(--fig-ext);background:var(--fig-ext-soft);border:1px solid var(--fig-ext)}
/* dots inside cells */
td.c .dot{vertical-align:middle}

.codes{margin:14px 2px 0;font-size:12px;color:var(--fig-muted);line-height:1.9}
.codes :deep(b){font-family:ui-monospace,monospace;color:var(--fig-text);font-weight:700}

.cards{display:grid;grid-template-columns:repeat(3,1fr);gap:14px;margin-top:4px}
@media(max-width:820px){.cards{grid-template-columns:1fr}}
.card{background:var(--fig-surface);border:1px solid var(--fig-line);border-radius:12px;padding:17px;box-shadow:var(--fig-shadow);position:relative;overflow:hidden}
.card::before{content:"";position:absolute;left:0;top:0;bottom:0;width:4px;background:var(--fig-forceful)}
.card.ext::before{background:var(--fig-ext)}
.card h3{margin:0 0 6px;font-size:14.5px;display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.card .rel{font-family:ui-monospace,monospace;font-size:12px;color:var(--fig-forceful);font-weight:700}
.card.ext .rel{color:var(--fig-ext)}
.card p{margin:0;font-size:13px;color:var(--fig-muted)}
.card p :deep(b){color:var(--fig-text);font-weight:600}
.card .rec{margin-top:9px;font-size:12.5px;color:var(--fig-text);background:var(--fig-surface-2);border-radius:7px;padding:8px 10px}

footer.notes{margin-top:34px;padding-top:18px;border-top:1px solid var(--fig-line);color:var(--fig-muted);font-size:12.5px;max-width:80ch}
footer.notes :deep(code){font-family:ui-monospace,monospace;font-size:11.5px;color:var(--fig-text)}
footer.notes :deep(b){color:var(--fig-text)}
</style>
