// docs/.vitepress/theme/components/simulator/i18n.ts
//
// UI labels only. The controller's own words — result.findings[], diagnostics[],
// and the error string simulate() returns — are rendered VERBATIM in both locales:
// they come from internal/schedule and internal/sim, and re-wording them here would
// fork the message catalogue, which is the drift this whole design exists to prevent.
import { computed } from 'vue'
import { useData } from 'vitepress'

export interface Labels {
  forecast: string
  forecastHint: string
  env: string
  envHint: string
  envBlank: (estimate: string) => string
  provisioning: string
  drain: string
  policy: string
  policyYamlHint: string
  extraWindows: (n: number) => string
  fleet: string
  nodeCount: string
  firstCreatedAt: string
  spread: string
  generate: string
  expireAfter: string
  tgp: string
  nodeName: string
  createdAt: string
  horizon: string
  start: string
  end: string
  timezone: string
  days: string
  windowStart: string
  windowEnd: string
  minRotationChances: string
  ageThreshold: string
  cooldownAfter: string
  readyTimeout: string
  forcefulFallback: string
  timeline: string
  horizonInvalid: string
  diagnostics: string
  noDiagnostics: string
  partial: string
  loading: string
  loadFailed: string
  retry: string
  legend: { rotation: string; surgeless: string; ready: string; breach: string; window: string; blocked: string }
}

const en: Labels = {
  forecast: 'Policy forecast (what the controller derives and exports)',
  forecastHint: 'Derived from the policy alone. It does not follow the simulated durations below.',
  env: 'Simulated actual durations (what the virtual world does)',
  envHint: 'These are NOT the policy estimates: the estimates are the forecast, these are what actually happens. Moving them apart is the interesting case — a policy whose C is optimistic because its estimates are too low.',
  envBlank: (estimate) => `blank = policy estimate: ${estimate}`,
  provisioning: 'Provisioning',
  drain: 'Drain',
  policy: 'Policy',
  policyYamlHint: 'The YAML is authoritative — it is what the simulator decodes, exactly as a cluster would. The form edits it in place.',
  extraWindows: (n) => `+${n} more maintenance window${n === 1 ? '' : 's'} in the YAML (the effective window is their union); the form edits the first one.`,
  fleet: 'Fleet',
  nodeCount: 'Nodes',
  firstCreatedAt: 'First created at',
  spread: 'Spread (Go duration, e.g. 168h)',
  generate: 'Generate',
  expireAfter: 'expireAfter (NodePool template)',
  tgp: 'terminationGracePeriod (NodePool template)',
  nodeName: 'Name',
  createdAt: 'Created at',
  horizon: 'Horizon',
  start: 'Start',
  end: 'End',
  timezone: 'Timezone',
  days: 'Days',
  windowStart: 'Window start',
  windowEnd: 'Window end',
  minRotationChances: 'minRotationChances (K)',
  ageThreshold: 'ageThreshold',
  cooldownAfter: 'cooldownAfter',
  readyTimeout: 'readyTimeout',
  forcefulFallback: 'Forceful fallback',
  timeline: 'Timeline',
  horizonInvalid: 'The horizon is empty or invalid — start must be strictly before end.',
  diagnostics: 'Diagnostics',
  noDiagnostics: 'No diagnostics.',
  partial: 'This simulation is PARTIAL — it did not run the whole horizon. See the diagnostics.',
  loading: 'Loading the simulator (3.4 MB)…',
  loadFailed: 'The simulator failed to load.',
  retry: 'Retry',
  legend: {
    rotation: 'rotation', surgeless: 'surge-less (forceful fallback)', ready: 'replacement ready',
    breach: 'expireAfter breach', window: 'maintenance window', blocked: 'blocked',
  },
}

const ja: Labels = {
  forecast: 'ポリシーの予測値（コントローラーが導出し export する値）',
  forecastHint: 'ポリシーだけから導かれる値です。下の「シミュレート上の実所要時間」には追随しません。',
  env: 'シミュレート上の実所要時間（仮想世界で実際に起きること）',
  envHint: 'これはポリシーの estimate ではありません。estimate は予測、こちらは実際に起きること。両者をずらした状態こそが見どころです（estimate が楽観的すぎて C が過大なポリシー、など）。',
  envBlank: (estimate) => `空欄 = ポリシーの estimate: ${estimate}`,
  provisioning: 'プロビジョニング',
  drain: 'ドレイン',
  policy: 'ポリシー',
  policyYamlHint: 'YAML が正本です（シミュレーターはクラスタと同じ厳密さでこれをデコードします）。フォームはこの YAML をその場で書き換えます。',
  extraWindows: (n) => `YAML にはメンテナンス窓があと ${n} 件あります（実効窓はそれらの和集合）。フォームが編集するのは 1 件目だけです。`,
  fleet: 'ノード群',
  nodeCount: 'ノード数',
  firstCreatedAt: '最初のノードの作成時刻',
  spread: '分散幅（Go duration、例: 168h）',
  generate: '生成',
  expireAfter: 'expireAfter（NodePool テンプレート）',
  tgp: 'terminationGracePeriod（NodePool テンプレート）',
  nodeName: '名前',
  createdAt: '作成時刻',
  horizon: 'シミュレート期間',
  start: '開始',
  end: '終了',
  timezone: 'タイムゾーン',
  days: '曜日',
  windowStart: '窓の開始',
  windowEnd: '窓の終了',
  minRotationChances: 'minRotationChances (K)',
  ageThreshold: 'ageThreshold',
  cooldownAfter: 'cooldownAfter',
  readyTimeout: 'readyTimeout',
  forcefulFallback: 'Forceful fallback',
  timeline: 'タイムライン',
  horizonInvalid: 'シミュレート期間が空か不正です — 開始は終了より厳密に前である必要があります。',
  diagnostics: '診断',
  noDiagnostics: '診断はありません。',
  partial: 'このシミュレーションは部分的です（期間の最後まで実行されていません）。診断を確認してください。',
  loading: 'シミュレーターを読み込み中（3.4 MB）…',
  loadFailed: 'シミュレーターの読み込みに失敗しました。',
  retry: '再試行',
  legend: {
    rotation: 'ローテーション', surgeless: 'surge なし（forceful fallback）', ready: '代替ノード Ready',
    breach: 'expireAfter 超過', window: 'メンテナンス窓', blocked: 'ブロック',
  },
}

export function useLabels() {
  const { lang } = useData()
  return computed(() => (lang.value.startsWith('ja') ? ja : en))
}
