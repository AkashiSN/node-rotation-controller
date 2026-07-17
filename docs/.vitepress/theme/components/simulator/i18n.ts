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
  fleetHint: string
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
  everyDay: string
  windowStart: string
  windowEnd: string
  minRotationChances: string
  ageThreshold: string
  cooldownAfter: string
  readyTimeout: string
  provisioningEstimate: string
  drainEstimate: string
  forcefulFallback: string
  /** Short descriptions for each field, shown below the input. */
  fieldHelp: {
    timezone: string
    days: string
    windowStart: string
    windowEnd: string
    minRotationChances: string
    ageThreshold: string
    readyTimeout: string
    cooldownAfter: string
    provisioningEstimate: string
    drainEstimate: string
    forcefulFallback: string
    expireAfter: string
    tgp: string
    nodeCount: string
    spread: string
    firstCreatedAt: string
  }
  /** The policy form's sections, mirroring the policy's own structure. */
  policyGroups: { window: string; derivation: string; surge: string }
  /** A finding's severity, as an accessible name for the glyph that carries it. The
   *  MESSAGE stays the controller's own, verbatim — only the severity word is ours. */
  severity: Record<'warn' | 'fatal', string>
  /** The symbol reference: what A, t_rot, t_rot_est, G and C mean. The formulas are code and
   *  are the same in every locale; only the prose is translated, and the specification stays
   *  the single source of truth for both. */
  symbols: {
    title: string
    hint: string
    defs: Record<'a' | 'tRot' | 'tRotEst' | 'g' | 'c', string>
    specSymbols: string
    specDerivation: string
  }
  /** The derivation rows. The formulas, symbols and values are CODE and identical in every
   *  locale; only these two strings are prose. */
  derivation: {
    /** The A row when spec.ageThreshold was given rather than derived. */
    overrideNote: string
    /** Marks a tGP that is the controller's fixed fallback, not the operator's value. */
    fallbackMark: string
  }
  timeline: string
  horizonInvalid: string
  diagnostics: string
  noDiagnostics: string
  partial: string
  loading: string
  loadFailed: string
  retry: string
  share: {
    copy: string
    copied: string
    copyFailed: string
    buildFailed: string
    tooBig: string
    unsupported: string
    badLink: string
    badLinkVersion: string
  }
  legend: {
    life: string; rotation: string; surgeless: string; ready: string; done: string
    deadline: string; breach: string; window: string; blocked: string
  }
  /** The redesigned chart: axis, segments, zoom controls, calendar, ruler. */
  chart: {
    provisioning: string
    running: string
    drain: string
    drainCap: string
    drainCapFallback: string
    eligibleAfter: string
    deadline: string
    breach: string
    overlap: string
    inFlight: string
    malformed: string
    continues: string
    surgeless: string
    /** The view controls. */
    rotationHint: string
    firstRotation: string
    prevRotation: string
    nextRotation: string
    fitWindow: string
    reset: string
    zoomIn: string
    zoomOut: string
    viewHint: string
    /** The minimap: what the strip is, and what its gestures do. */
    minimap: string
    minimapHint: string
    view: string
    simulatedThrough: string
    /** The horizon control. */
    coverage: string
    coverageHint: string
    coverageOption: (n: number) => string
    pinned: string
    advanced: string
    /** The window calendar. */
    calendar: string
    calendarHint: (weeks: number, tz: string) => string
    calendarPartial: (through: string) => string
    calendarNoWeeks: string
    calendarCell: (weekday: string, time: string, pct: number, open: number, observed: number) => string
    calendarUnknown: string
    weekdays: string[]
    /** The scale ruler. */
    ruler: string
    rulerLifecycle: string
    rulerRotation: string
    rulerRatio: (rotation: string, lifetime: string, pct: string) => string
    rulerRatioForecast: (rotation: string, lifetime: string, pct: string) => string
    quantity: Record<'lifecycle' | 'bound' | 'cap' | 'actual' | 'forecast' | 'policy', string>
    /** The hidden restatement of the run. */
    table: string
    generation: string
    slot: string
    anomalies: string
  }
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
  fleetHint: 'The nodes to simulate. The generator creates a batch; you can also edit individual rows.',
  nodeCount: 'Nodes',
  firstCreatedAt: 'First created at',
  spread: 'Spread (e.g. 168h, 30m, 2h30m)',
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
  everyDay: 'Every day',
  windowStart: 'Window start',
  windowEnd: 'Window end',
  minRotationChances: 'minRotationChances (K)',
  ageThreshold: 'ageThreshold',
  cooldownAfter: 'cooldownAfter',
  readyTimeout: 'readyTimeout',
  provisioningEstimate: 'provisioningEstimate',
  drainEstimate: 'drainEstimate',
  forcefulFallback: 'Forceful fallback',
  /** Short descriptions for each field, shown below the input. */
  fieldHelp: {
    timezone: 'IANA timezone for the window (e.g. Asia/Tokyo)',
    days: 'Which weekdays the window recurs on',
    windowStart: 'Time the window opens (24h)',
    windowEnd: 'Time the window closes (24h)',
    minRotationChances: 'How many windows a node gets before expiry (K)',
    ageThreshold: '"auto" or a Go duration string using h/m/s units (e.g. "120h", "1h30m")',
    readyTimeout: 'Max wait for the surge node to become Ready',
    cooldownAfter: 'Pause between successive successful rotations',
    provisioningEstimate: 'Expected provisioning time (throughput forecast)',
    drainEstimate: 'Expected drain time (throughput forecast)',
    forcefulFallback: 'Delete surge-less if graceful can\'t finish in time',
    expireAfter: 'Node lifetime before Karpenter force-expires it (e.g. 336h)',
    tgp: 'How long Karpenter waits before force-killing pods (e.g. 24h)',
    nodeCount: 'Number of nodes to generate (1–50)',
    spread: 'Time between first and last node creation (e.g. 168h = 1 week)',
    firstCreatedAt: 'Creation timestamp of the first node (ISO 8601)',
  },
  policyGroups: {
    window: 'Maintenance window — when rotation may START',
    derivation: 'Derivation — how early a node is picked',
    surge: 'Surge — how one rotation runs',
  },
  severity: { warn: 'Warning', fatal: 'Fatal' },
  symbols: {
    title: 'What A, t_rot, t_rot_est, G and C mean',
    hint: 'What each symbol above means. E is the node\'s expireAfter, P the worst-case period between window occurrences, D one occurrence\'s length, tGP its terminationGracePeriod, and buffer the fixed slack the controller reserves for its own detection lag — see its value in the t_rot row above.',
    defs: {
      a: 'The age at which a node becomes a candidate. Derived to sit below expireAfter by construction, so the backstop stays a backstop.',
      tRot: 'The upper bound on ONE node\'s rotation — the surge wait plus a force-completed drain. A deadline bound, not what a healthy rotation takes.',
      tRotEst: 'The expected service time of a healthy rotation. No deadline term and no buffer: it is the throughput denominator, not a bound.',
      g: 'The rotation chances the schedule actually guarantees before expireAfter fires. G = K under auto-derivation; an explicit ageThreshold override recomputes it.',
      c: 'How many rotations ONE window occurrence can start (m = surge.maxUnavailable, fixed at 1 in v1). Rotations are serial and separated by cooldownAfter.',
    },
    specSymbols: 'Specification §1.4 (the symbols)',
    specDerivation: '§3.2 (the derivation)',
  },
  derivation: {
    overrideNote: 'the value given in spec.ageThreshold — auto derives E − (K·P + t_rot)',
    fallbackMark: 'fallback',
  },
  timeline: 'Timeline',
  horizonInvalid: 'The horizon is empty or invalid — start must be strictly before end.',
  diagnostics: 'Diagnostics',
  noDiagnostics: 'No diagnostics.',
  partial: 'This simulation is PARTIAL — it did not run the whole horizon. See the diagnostics.',
  loading: 'Loading the simulator (3.4 MB)…',
  loadFailed: 'The simulator failed to load.',
  retry: 'Retry',
  share: {
    copy: 'Copy share link',
    copied: 'Copied',
    copyFailed: 'Could not reach the clipboard — the link is in the address bar.',
    buildFailed: 'Could not build the share link.',
    tooBig: 'This is too large to fit in a share link — try a smaller fleet or policy YAML.',
    unsupported: 'This browser cannot build a share link (it lacks the Compression Streams API).',
    badLink: 'Could not read the shared link, so this is the default policy and fleet.',
    badLinkVersion: 'This shared link comes from a newer version of the simulator, so this is the default policy and fleet.',
  },
  legend: {
    life: 'node lifetime', rotation: 'rotation start',
    surgeless: 'surge-less (forceful fallback)', ready: 'replacement ready',
    done: 'rotation done', deadline: 'expireAfter deadline',
    breach: 'expireAfter breach', window: 'maintenance window', blocked: 'blocked',
  },
  chart: {
    provisioning: 'provisioning (the replacement is coming up)',
    running: 'running',
    drain: 'draining (the old node)',
    drainCap: 'terminationGracePeriod — the cap Karpenter force-completes the drain at',
    drainCapFallback: 'drain cap (terminationGracePeriod unset — the controller\'s fallback bound)',
    eligibleAfter: 'eligible after (the trigger is strict: this instant itself is not yet eligible)',
    deadline: 'expireAfter deadline',
    breach: 'expireAfter breach — Karpenter\'s forceful expiration takes the node here',
    overlap: 'make-before-break: the replacement is up before the old node drains',
    inFlight: 'still in flight when the simulation ended',
    malformed: 'the response is missing this boundary (a bug)',
    continues: 'still alive when the simulation ended — the bar continues',
    surgeless: 'surge-less (forceful fallback): no replacement is staged first',
    rotationHint: 'The rotation buttons step by OCCASION — one maintenance-window occurrence in which rotation happened — and fit it. Rotations are serial inside an occurrence (one node every t_rot_est + cooldownAfter), so they are all on screen together and there is nothing left to step to inside it.',
    firstRotation: 'First rotation',
    prevRotation: 'Previous rotation',
    nextRotation: 'Next rotation',
    fitWindow: 'Fit a maintenance window',
    reset: 'Whole horizon',
    zoomIn: 'Zoom in',
    zoomOut: 'Zoom out',
    viewHint: 'Zoom changes the VIEW only — the simulated horizon does not move. On the chart: wheel to zoom, drag to pan; arrow keys pan, +/− zoom, 0 resets.',
    minimap: 'The whole run; the bright band is what the chart is showing',
    minimapHint: 'Drag the band to move the view, its edges to resize it; the wheel scrolls without changing the zoom.',
    view: 'View',
    simulatedThrough: 'simulated through',
    coverage: 'Lifetime coverage',
    coverageHint: 'A multiple of the LONGEST node lifetime (expireAfter). Not "generations": staggered createdAt, per-node overrides, window waits and cooldown all break that equivalence.',
    coverageOption: (n) => `${n}x`,
    pinned: 'The horizon is pinned to the instants below; the coverage buttons will unpin it.',
    advanced: 'Exact horizon (ISO 8601)',
    calendar: 'When the maintenance window was OPEN (observed)',
    calendarHint: (weeks, tz) =>
      `Folded from the ${weeks} whole week${weeks === 1 ? '' : 's'} this run observed, in ${tz} — the union of every maintenanceWindows entry. Each cell is open minutes ÷ observed minutes, not a week count: any yes/no threshold would either turn a one-minute clip into a full cell or erase it.`,
    calendarPartial: (through) => `The run stopped at ${through}, so only the whole weeks before it are counted.`,
    calendarNoWeeks: 'The horizon does not contain a whole calendar week, so there is nothing to fold. Lengthen it.',
    calendarCell: (weekday, time, pct, open, observed) =>
      `${weekday} ${time}: open ${pct}% of the time observed (${open} of ${observed} minutes)`,
    calendarUnknown: 'never observed (a wall-clock hour the DST change skipped)',
    weekdays: ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'],
    ruler: 'How long is each duration, really?',
    rulerLifecycle: 'Lifecycle (weeks)',
    rulerRotation: 'Rotation mechanics (minutes)',
    rulerRatio: (rotation, lifetime, pct) =>
      `The worst rotation OBSERVED in this run took ${rotation} — ${pct} of the longest node lifetime (${lifetime}). That is why the timeline needs zoom: the two live on different scales.`,
    rulerRatioForecast: (rotation, lifetime, pct) =>
      `No rotation completed in this run, so this is the policy's FORECAST: ${rotation} — ${pct} of the longest node lifetime (${lifetime}). A forecast, not a measurement.`,
    quantity: {
      lifecycle: 'lifecycle offset',
      bound: 'deadline-side bound the controller reserves',
      cap: 'cap (a ceiling, not a duration anything took)',
      actual: 'simulated actual',
      forecast: 'policy forecast',
      policy: 'policy setting',
    },
    table: 'The run, as a table',
    generation: 'Generation',
    slot: 'Slot',
    anomalies: 'The response contradicts itself:',
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
  extraWindows: (n) => `YAML にはメンテナンスウィンドウがあと ${n} 件あります（実効ウィンドウはそれらの和集合）。フォームが編集するのは 1 件目だけです。`,
  fleet: 'ノード群',
  fleetHint: 'シミュレートするノード。ジェネレーターでバッチ生成し、個別行を編集することもできる。',
  nodeCount: 'ノード数',
  firstCreatedAt: '最初のノードの作成時刻',
  spread: '分散幅（例: 168h, 30m, 2h30m）',
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
  everyDay: '毎日',
  windowStart: 'ウィンドウの開始',
  windowEnd: 'ウィンドウの終了',
  minRotationChances: 'minRotationChances (K)',
  ageThreshold: 'ageThreshold',
  cooldownAfter: 'cooldownAfter',
  readyTimeout: 'readyTimeout',
  provisioningEstimate: 'provisioningEstimate',
  drainEstimate: 'drainEstimate',
  forcefulFallback: 'Forceful fallback',
  /** 各フィールドの短い説明。入力の下に表示される。 */
  fieldHelp: {
    timezone: 'ウィンドウの IANA タイムゾーン（例: Asia/Tokyo）',
    days: 'ウィンドウが繰り返される曜日',
    windowStart: 'ウィンドウの開始時刻（24h）',
    windowEnd: 'ウィンドウの終了時刻（24h）',
    minRotationChances: 'expiry 前にノードが得るウィンドウ回数 (K)',
    ageThreshold: '"auto" または h/m/s 単位の Go duration 形式の文字列（例: "120h", "1h30m"）',
    readyTimeout: 'surge ノードが Ready になるまでの最大待ち時間',
    cooldownAfter: '成功したローテーション間の休止',
    provisioningEstimate: '期待プロビジョニング時間（スループット予測用）',
    drainEstimate: '期待ドレイン時間（スループット予測用）',
    forcefulFallback: 'graceful が間に合わなければ surge なしで回す',
    expireAfter: 'Karpenter が強制失効させるまでのノード寿命（例: 336h）',
    tgp: 'Karpenter が Pod を強制 kill するまでの猶予（例: 24h）',
    nodeCount: '生成するノード数（1〜50）',
    spread: '最初と最後のノード作成の間隔（例: 168h = 1 週間）',
    firstCreatedAt: '最初のノードの作成タイムスタンプ（ISO 8601）',
  },
  policyGroups: {
    window: 'メンテナンスウィンドウ — ローテーションを「開始してよい」時間帯',
    derivation: '導出 — ノードをどれだけ早く選ぶか',
    surge: 'surge — 1 回のローテーションの動き方',
  },
  severity: { warn: '警告', fatal: '致命的' },
  symbols: {
    title: 'A・t_rot・t_rot_est・G・C とは',
    hint: '上の各記号の意味です。E はノードの expireAfter、P はウィンドウの出現間隔の最悪値、D は 1 回のウィンドウの長さ、tGP は terminationGracePeriod、buffer はコントローラー自身の検知遅れのために確保する固定の猶予時間です。実際の値は上の t_rot の行で確認できます。',
    defs: {
      a: 'ノードが候補になる年齢。構成上つねに expireAfter より手前に来るように導出されるため、バックストップはバックストップのままでいられます。',
      tRot: 'ノード 1 台のローテーション時間の上界 — surge の待ちと、強制完了されるドレインの合計。期限側の「上界」であって、健全なローテーションの所要時間ではありません。',
      tRotEst: '健全なローテーション 1 回の想定所要時間。期限の項も buffer も含みません（上界ではなく、スループットの分母だからです）。',
      g: 'expireAfter が発火する前にスケジュールが実際に保証するローテーション機会の回数。auto 導出では G = K、ageThreshold を明示指定すると再計算されます。',
      c: '1 回のウィンドウで開始できるローテーション数（m = surge.maxUnavailable、v1 では 1 固定）。ローテーションは直列で、間隔は cooldownAfter です。',
    },
    specSymbols: '仕様 §1.4（記号一覧）',
    specDerivation: '§3.2（導出）',
  },
  derivation: {
    overrideNote: 'spec.ageThreshold の指定値です（auto なら E − (K·P + t_rot) から導出されます）',
    fallbackMark: 'フォールバック',
  },
  timeline: 'タイムライン',
  horizonInvalid: 'シミュレート期間が空か不正です — 開始は終了より厳密に前である必要があります。',
  diagnostics: '診断',
  noDiagnostics: '診断はありません。',
  partial: 'このシミュレーションは部分的です（期間の最後まで実行されていません）。診断を確認してください。',
  loading: 'シミュレーターを読み込み中（3.4 MB）…',
  loadFailed: 'シミュレーターの読み込みに失敗しました。',
  retry: '再試行',
  share: {
    copy: '共有リンクをコピー',
    copied: 'コピーしました',
    copyFailed: 'クリップボードに書き込めませんでした。アドレスバーのリンクをコピーしてください。',
    buildFailed: '共有リンクを作れませんでした。',
    tooBig: 'この内容は共有リンクに収まらないほど大きすぎます。ノード群やポリシー YAML を小さくしてください。',
    unsupported: 'このブラウザーでは共有リンクを作れません（Compression Streams API 非対応）。',
    badLink: '共有リンクを読み取れませんでした。既定のポリシーとノード群を表示しています。',
    badLinkVersion: 'この共有リンクは新しいバージョンのシミュレーターのものです。既定のポリシーとノード群を表示しています。',
  },
  legend: {
    life: 'ノードの寿命', rotation: 'ローテーション開始',
    surgeless: 'surge なし（forceful fallback）', ready: '代替ノード Ready',
    done: 'ローテーション完了', deadline: 'expireAfter 期限',
    breach: 'expireAfter 超過', window: 'メンテナンスウィンドウ', blocked: 'ブロック',
  },
  chart: {
    provisioning: 'プロビジョニング中（代替ノードを立ち上げている）',
    running: '稼働中',
    drain: 'ドレイン中（旧ノード）',
    drainCap: 'terminationGracePeriod — Karpenter がドレインを強制完了する上限',
    drainCapFallback: 'ドレインの上限（terminationGracePeriod 未設定 — コントローラーの既定の上限値）',
    eligibleAfter: 'この時刻より後に対象化（トリガーは厳密不等号: この瞬間自体はまだ対象外）',
    deadline: 'expireAfter 期限',
    breach: 'expireAfter 超過 — ここで Karpenter の強制失効がノードを落とす',
    overlap: 'make-before-break: 旧ノードのドレイン前に代替ノードが立ち上がっている',
    inFlight: 'シミュレーション終了時点でまだ進行中',
    malformed: 'この境界がレスポンスにない（バグ）',
    continues: 'シミュレーション終了時点でまだ生存 — バーはこの先も続く',
    surgeless: 'surge なし（forceful fallback）: 代替ノードを先に立てない',
    rotationHint: 'ローテーションのボタンは「回」単位で移動します（ローテーションが起きたメンテナンスウィンドウ 1 回分）。そのウィンドウに合わせて表示するので、ウィンドウの境界も、そこで起きたローテーションもすべて一度に画面に入ります（ウィンドウの中でのローテーションは直列で、間隔は t_rot_est + cooldownAfter です）。',
    firstRotation: '最初のローテーション',
    prevRotation: '前のローテーション',
    nextRotation: '次のローテーション',
    fitWindow: 'メンテナンスウィンドウに合わせる',
    reset: '期間全体',
    zoomIn: '拡大',
    zoomOut: '縮小',
    viewHint: 'ズームは「表示範囲」だけを変えます（シミュレート期間は動きません）。チャート上ではホイールで拡大縮小、ドラッグで移動。矢印キーで移動、+/− で拡大縮小、0 でリセット。',
    minimap: '実行全体。明るい帯がチャートに表示されている範囲です',
    minimapHint: '帯をドラッグすると表示位置が動き、端をドラッグすると表示幅が変わります。ホイールは拡大率を変えずにスクロールします。',
    view: '表示範囲',
    simulatedThrough: 'シミュレート済み',
    coverage: '寿命カバレッジ',
    coverageHint: '最長のノード寿命（expireAfter）の倍数です。「世代数」ではありません: createdAt のばらつき、ノードごとの上書き、ウィンドウ待ち、cooldown のいずれもその等価性を壊します。',
    coverageOption: (n) => `${n}倍`,
    pinned: 'シミュレート期間は下の時刻に固定されています。カバレッジのボタンを押すと固定が解除されます。',
    advanced: '正確なシミュレート期間（ISO 8601）',
    calendar: 'メンテナンスウィンドウが実際に開いていた時間帯（観測値）',
    calendarHint: (weeks, tz) =>
      `この実行が観測した完全な ${weeks} 週間を、${tz} で畳み込んだものです（全 maintenanceWindows エントリの和集合）。各セルは「開いていた分数 ÷ 観測した分数」で、週数のカウントではありません: 開閉を二値化する閾値は、1 分だけの重なりをセル全体に膨らませるか、逆に消してしまうかのどちらかになります。`,
    calendarPartial: (through) => `実行は ${through} で停止したため、それ以前の完全な週だけを数えています。`,
    calendarNoWeeks: 'シミュレート期間に完全な暦週が 1 つも含まれていないため、畳み込むものがありません。期間を延ばしてください。',
    calendarCell: (weekday, time, pct, open, observed) =>
      `${weekday} ${time}: 観測時間のうち ${pct}% が開いていた（${observed} 分中 ${open} 分）`,
    calendarUnknown: '観測なし（DST の切り替えで飛ばされた壁時計上の時刻）',
    weekdays: ['月', '火', '水', '木', '金', '土', '日'],
    ruler: '各 duration は実際どれくらいの長さか',
    rulerLifecycle: 'ライフサイクル（週の尺度）',
    rulerRotation: 'ローテーションの機構（分の尺度）',
    rulerRatio: (rotation, lifetime, pct) =>
      `この実行で観測された最悪のローテーションは ${rotation}（最長のノード寿命 ${lifetime} の ${pct}）。タイムラインにズームが要るのはこのためです — 両者は別の尺度に住んでいます。`,
    rulerRatioForecast: (rotation, lifetime, pct) =>
      `この実行では 1 件もローテーションが完了していないため、これはポリシーの予測値です: ${rotation}（最長のノード寿命 ${lifetime} の ${pct}）。実測値ではありません。`,
    quantity: {
      lifecycle: 'ノード誕生からのオフセット',
      bound: 'コントローラーが期限側に確保する上界',
      cap: '上限（実際にかかった時間ではなく天井）',
      actual: 'シミュレート上の実測',
      forecast: 'ポリシーの予測',
      policy: 'ポリシー設定値',
    },
    table: '実行結果（表）',
    generation: '世代',
    slot: 'スロット',
    anomalies: 'レスポンスに矛盾があります:',
  },
}

export function useLabels() {
  const { lang } = useData()
  return computed(() => (lang.value.startsWith('ja') ? ja : en))
}
