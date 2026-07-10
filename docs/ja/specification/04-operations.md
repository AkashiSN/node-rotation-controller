# 4. 運用

## 4.1 キャパシティ / 可用性

| 観点 | 扱い |
|------|------|
| ローテーション時の Pod Pending 時間 | surge により 0 に近づく（Karpenter Graceful セマンティクスと同等）|
| `readyReplicas` が希望数を一時的に下回る | Eviction API 経路での構造的制約。surge しても新 Pod は即時 Ready にはならない。緩和はアプリ層（余剰レプリカ + PDB）の責務でスコープ外 |
| 並列 surge 数 | v1 は `surge.maxUnavailable = 1` を **NodePool ごと** に固定（NodePool 内は直列、異なる NodePool 同士は並行 surge し得る）。代替ノードは placeholder Pod 経由で誘発される **NodePool-owned** ノード（§3.3）。`maxUnavailable > 1` は将来バージョン用の予約で、その場合ノード `m` 台分の余裕が要る。単一ノード分の surge 予算がどう強制されるかは下の Note を参照 |

> **Note — 単一ノード分の surge 予算がどう強制されるか。** `spec.limits` は **リソース予算**（`{cpu, memory, …}`）であって**ノード台数ではない** — 実際の前提条件は、placeholder の requests（surge ノードのリソースフットプリント、§3.3）が NodePool の*残り*予算（`limits − 既プロビジョニング分`）に収まることであり、加えて外部の EC2 vCPU クォータも要る。直感的には「ベースライン +1 ノード」だが、これは台数ではなく**リソース**チェックとして強制される。コントローラは **ローテーション開始前にこのヘッドルームを事前確認** し（§5.2 ステップ 3 — placeholder の requests は選定された候補が定義するため、候補選定の後）、残り予算がノード 1 台分のリソースを収められなければ警告してスキップする。

## 4.2 観測性

`/metrics` で Prometheus メトリクスを公開。

| メトリクス | 種別 | ラベル | 用途 |
|-----------|------|--------|------|
| `noderotation_candidates` | Gauge | `nodepool` | 候補 NodeClaim 数 |
| `noderotation_in_progress` | Gauge | `nodepool` | 進行中ローテーション数 |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` | 累積完了数。outcome ∈ {success, failure, expired} — `expired` は旧 NodeClaim が graceful なローテーション完了前に強制失効したケース（`pending` または `failed` のまま `deletionTimestamp` が現れた時点で捕捉するか、`draining` ミラー無しでの消滅により捕捉する — §5.2。ローテーションごとに 1 回だけ計上し、決して success には計上しない）|
| `noderotation_forceful_fallback_total` | Counter | `nodepool` | 開始された surge-less ウィンドウ有界 forceful fallback ローテーションの累積数（§3.3）。完了時ではなく surge-less 開始時に増分する（完了は引き続き `noderotation_completed_total` を増分する）|
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` | phase ∈ {surge_wait, drain} ごとの所要時間。`surge_wait` = `started-at → surge_ready`、`drain` = `draining-at → 旧 NodeClaim の finalize`。`drain` は `pending → draining` 遷移時にスタンプされる NodePool の `draining-at` annotation をアンカーとする — 旧 NodeClaim の `deletionTimestamp` は、ヒストグラムを一度だけ観測する唯一の完了地点までに finalize されて失われているため、このプール側アンカーが必要（§5.3）|
| `noderotation_window_active` | Gauge | `nodepool` | NodePool が統治ポリシーのウィンドウ内にあるかの 0/1 指標 |
| `noderotation_policy_conflict` | Gauge | `nodepool` | 0/1: NodePool が RotationPolicy の競合（同一 specificity のセレクタ衝突、またはランタイム不正な統治ポリシー、§5.4）でローテーションをブロックされている。ブロック中は 1、単一の有効なポリシーが統治すると 0 |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` | 凍結期限 Unix 時刻（0 = 凍結なし）|
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` | 導出された `ageThreshold`（§3.2）|
| `noderotation_rotation_chances` | Gauge | `nodepool` | 導出閾値での保証ローテーション回数 `G` |
| `noderotation_window_period_seconds` | Gauge | `nodepool` | スケジュール和集合の最悪ウィンドウ周期 `P` |
| `noderotation_short_lead_nodes` | Gauge | `nodepool` | **自身の** `spec.expireAfter` ではもはや `K` 回を保証できない NodeClaim 数（ノード単位の `A ≤ 0`、§3.2 レイヤ 3）|
| `noderotation_drain_stuck` | Gauge | `nodepool` | 0/1: 進行中ローテーションの drain が `tGP + buffer` を超過（§5.2）|
| `noderotation_retry_count` | Gauge | `nodepool` | その NodePool の NodeClaim 群における `noderotation.io/retry-count`（§5.3）の最大値（無ければ 0）— 系統的失敗のシグナル。annotation だけでは Prometheus アラートの入力にならない |

> **ラベルに関する注記。** per-NodePool のメンテナンスウィンドウ（各 NodePool が自身の統治 `RotationPolicy` を解決する、§5.4）により、`noderotation_window_period_seconds` と `noderotation_window_active` はいずれも**意味を持つ** `nodepool` ラベルを持つ — ポリシーが異なる `maintenanceWindows` を持てば `P` とウィンドウ*への所属* はプール間で異なりうる。`noderotation_age_threshold_seconds` と `noderotation_rotation_chances` も同様に NodePool ごとに値が変わる（各プールの代表 `expireAfter`/`terminationGracePeriod` *と* そのポリシーの `K` を畳み込むため）。これは、単一のクラスタ共通ウィンドウがこれらの系列をプール間で同一にしていた以前の草案の v1 簡略化を解消する。

> **ライフサイクルに関する注記。** `nodepool` 単位の系列は **NodePool が削除された時点で消去する** — コントローラが削除 reconcile で破棄する。ゲージは reconcile ごとに再計算されるため、削除されて reconcile が止まったプールはそのままだと最後の値を永遠にラッチしてしまう（消えたはずの `noderotation_drain_stuck = 1` が無限にアラートし続ける）。統治ポリシーを失った NodePool（もはやどの `RotationPolicy` にもマッチしない）も、もはやローテーションされないため同様にその系列が破棄される（§5.4）。そのようなプールにアンカーされた進行中ローテーションは、placeholder と `do-not-disrupt` マーカーが孤児化しないよう先にロールバックされる（§5.4）。

**Kubernetes Events。** 毎 reconcile で計算される警告レベルの条件は Kubernetes
の `Warning` Event としても発行され、運用者はメトリクスを読まずとも
`kubectl describe` で確認できる:

| サーフェス | オブジェクト | reason | 発火タイミング |
|-----------|------------|--------|--------------|
| 非致命的 schedule 所見（§3.2 レイヤ 1–2） | NodePool | 所見コード（例: `KBelowTwo`, `AVeryAggressive`, `TGPUnset`, `HardCapExceeded`, `RetryBackoffShort`, `DrainEstimateAboveTGP`, `ThroughputBelowArrival`, `ThroughputBurstShortfall`, `RotationSpansNextWindow`, `OverrideGBelowK`） | その所見が NodePool で有効になったとき |
| short-lead NodeClaim（§3.2 レイヤ 3） | NodeClaim | `ShortLead` | claim 自身の `expireAfter` が `K` 回のローテーションを保証できなくなったとき |
| surge-less forceful fallback（§3.3） | NodePool | `ForcefulFallback` | 候補の deadline 前に graceful な surge を完了できないため surge-less なウィンドウ有界ローテーションを開始するとき（opt-in の `surge.forcefulFallback`）|
| ローテーション開始（§5.2 ステップ 3） | NodePool | `RotationStarted` | 候補が選ばれ、プールの直列ゲートがアンカーされたとき（`Normal`）|
| ローテーション完了（§5.2） | NodePool | `RotationCompleted` | 旧 NodeClaim が `draining` から finalize されて消えたとき（`Normal`）|
| ローテーション試行の失敗（§5.2） | NodeClaim | `RotationFailed` | surge ノードが `readyTimeout` 内に `Ready` にならず、試行がロールバックされたとき |
| プレースホルダがスケジュール不能（§3.3） | NodeClaim | `SurgeUnschedulable` | プレースホルダが `PodScheduled=False` を持つとき。スケジューラの reason と message が Event に転記される |
| surge プレースホルダがクランプされた（§3.3） | NodeClaim | `SurgeClamped` | ほぼ満杯のノードをローテーション可能に保つため、プレースホルダを `NodeClaim.status.allocatable − DaemonSet オーバーヘッド` にクランプしたとき（`Normal`）。不足分は AZ ごとの帯で有界であり、preemption + Karpenter フォローアップで吸収される |
| クランプの不足分が帯を超過（§3.3） | NodeClaim | `SurgeClampBandExceeded` | 発火したクランプが測定された AZ 帯で説明できる以上を手放したとき — requests 会計が scheduler と乖離している（`Warning`）。ローテーションは続行する |
| surge クランプを拒否（§3.3） | NodeClaim | `SurgeClampRefused` | DaemonSet オーバーヘッドが `NodeClaim.status.allocatable` を使い切り、どのクランプもノードを誘発できないとき。プレースホルダは完全な drain を保ち、スケジュール不能のまま留まり、ローテーションはロールバックする（`Warning`）|

Event は**条件に入った遷移時に発行することでデデュープ**される: 一度解消され
後で再発生した所見/claim は再発火する。デデュープ状態はインメモリのため、
コントローラ再起動時には有効な各警告が一度だけ再発行される。致命的所見は
Event 化されない — §5.2 の feasibility ゲートがローテーション開始をブロック
しログに記録する。

**状態機械の遷移。** ローテーション状態機械（§5.2）の各遷移は `INFO` ログ行を
1 行ずつ出力する。これにより、ローテーションの経過を `NodeClaim` のタイムスタンプ
や Karpenter の Event からではなく、ログ単体から再構成できる。量はローテーション
1 回あたり数行なので、`V(1)` の背後には**置かない**。

各行は、その遷移を確定させる永続的な annotation 書き込み（§5.3）の **後** に出力
され、前には出力されない。書き込みに失敗した reconcile は同じフェーズから再試行
される — 状態機械の書き込みは設計上べき等である — ため、書き込みの前に置かれた行は
遷移を 1 度記すのではなく再試行のたびに繰り返されてしまう。その代償は逆向きになる:
書き込みとログの間でコントローラが死ぬと、その行は**失われる**。ローテーション自体は
影響を受けず、オブジェクト上の永続状態が引き続き権威である。ログはそれを最善努力で
語るものであって、台帳ではない。

| ログ行 | フィールド |
|-------|----------|
| `rotation candidate selected` | `nodeclaim`, `age`, `deadline`, `eligible`, `surgeless` |
| `no rotation candidate` | `reason` — ブロックしている開始ゲート（`outOfWindow`, `frozen`, `cooldownAfterSuccess`, `cooldownAfterFailure`）、または `noEligibleClaim` と内訳（`claims`, `notTriggered`, `inBackoff`, `inFlight`, `optedOut`, `deleting`, `notReady`, `terminal`）|
| `surge placeholder created` | `placeholder`, `requests`, `reschedulablePods`, `daemonSetPods`, `mirrorPods`, `completedPods`, `nodePinnedPods`。クランプ発火時（§3.3）は加えて `clamped`, `unclamped`, `limit`, `shortfall`、不足分が帯を超えれば `bandExceeded`。拒否時は `clampRefused` がリソース名を示す |
| `surge placeholder is not schedulable` | `placeholder`, `reason`, `detail` — プレースホルダの `PodScheduled=False` 条件 |
| `surge node ready` | `surgeNode`, `surgeWait` |
| `drain started` | `node`, `mode` ∈ {`surge`, `forceful-fallback`} |
| `rotation attempt failed` | `reason`, `readyTimeout`, `retryCount`, `backoffUntil` |
| `rotation complete` | `mode`, `drain` |

このうち 2 行はエッジではなく**レベルトリガ**な条件を表す — reconcile が毎パス
再評価する — ため、上記の Warning Event と同じ遷移デデュープを持つ:
`no rotation candidate` は reason または内訳が変化したときにのみ、
`surge placeholder is not schedulable` はスケジューラのメッセージが変化した
ときにのみ再発火する。このデデュープがなければ、アイドルな NodePool は
`longRequeue` ごとに、`readyTimeout` 長のストールは `shortRequeue` ごとに
ログを出すことになる。

> **`surge placeholder created` が除外内訳を報告する理由。** Karpenter 自身の
> `FailedScheduling` メッセージは、見つけるべき総容量 — プレースホルダの requests
> **＋** 新規プロビジョニングするノードに必ず加わる DaemonSet オーバーヘッド — を
> 報告する。これは、コントローラが `ReschedulableRequests`（§3.3）で DaemonSet Pod
> を二重計上しているように読み違えやすい。この行は計算された requests と、そこから
> 除外された Pod の両方を示すため、稼働中のプレースホルダを調べずに両者を突き合わ
> せられる。

> **生存性（liveness）は警告ログではなくメトリクスから判断する。** 同じ遷移
> デデュープが `INFO` レベルの警告**ログ**行にも適用されるため、定常状態 —
> 所見が安定し、遷移が起きず、ローテーションが進行中でない状態 — では、健全な
> reconcile ループが多数のパスにわたって**ゼロ行**のログしか出さないことがある。
> （進行中のローテーションはログを出す。上記「状態機械の遷移」を参照。沈黙は
> 「何も変化していない」を意味し、「何も動いていない」を意味しない。）
> したがってデデュープされた
> 警告ログは liveness シグナルでは**ない**: reconcile の生存性は、所見の変化に
> 関係なく毎パス進む `controller_runtime_reconcile_total` /
> `controller_runtime_reconcile_time_seconds_*` カウンタと `workqueue_*`
> メトリクス（depth・adds・work duration）から読み取らねばならない。デバッグ時
> にログで毎パスの活動を*見たい*場合は、コントローラのログ詳細度を上げる
> （`--zap-devel` / より高い `-v`）。デバッグ詳細度（`V(1)`）では、コントローラは
> 追加で、**毎パス・デデュープなしに**現在の所見と、軽量な reconcile ごとの
> `reconcile` ハートビート（phase・候補数・claim 数・in-window・所見数）を出力
> する。このデバッグ出力は純粋に付加的であり — `INFO` ログや Warning Event の
> デデュープも、いかなるメトリクスも変更しない — 人間が読むための補助にすぎ
> ない。上記のメトリクスが権威ある liveness シグナルである。

推奨アラート:

- `increase(noderotation_completed_total{outcome=~"failure|expired"}[1h]) > 0`
- `noderotation_candidates > 0` が 2 ウィンドウ連続で成立（コントローラが追いついていない）
- `noderotation_window_active == 1` のウィンドウ全期間で完了 0 件かつ候補 > 0
- `noderotation_drain_stuck == 1`（drain が `tGP + buffer` を超えてブロック — PDB かスタックした finalizer、§5.2）
- `noderotation_short_lead_nodes > 0`（焼き込まれた `expireAfter` ではもはや `K` 回を保証できない NodeClaim あり、§3.2 レイヤ 3）
- `noderotation_retry_count >= 3`（同じローテーションが失敗し続けている — 執拗な placeholder preempt や同一 AZ 容量不足といった系統的原因、§5.3）
- `increase(noderotation_forceful_fallback_total[1h]) > 0`（ウィンドウ境界内の forceful fallback が発火した — graceful surge が期限との競争に敗れた、§3.3・ADR-0001。1 回の fallback は設計どおりの挙動なので `severity: info` で同梱する。環境に合わせて持続的なレートへと調整する）

> Helm chart はこの 7 アラートを **任意の** `PrometheusRule` として同梱する（`prometheusRule.enabled` で切替、既定 `false`）。スケジュール依存のレンジ（2 ウィンドウ分・ウィンドウ長分）は `P`/`D` が運用者の `maintenanceWindows` 由来であるため chart の値になっている。各メトリクスの読み方とアラートの調整は[運用ランブック](../runbook.md)を参照。

## 4.3 RBAC とクラウド権限

### Kubernetes RBAC

```yaml
- apiGroups: ["karpenter.sh"]
  resources: ["nodeclaims"]
  # create なし: v1 は NodeClaim を決して作成しない（§3.3）。update/patch は
  # noderotation.io/* の state annotation を担い、delete がローテーションと失敗時の回収を駆動する
  verbs: ["get", "list", "watch", "update", "patch", "delete"]
- apiGroups: ["karpenter.sh"]
  resources: ["nodepools"]
  # update/patch: active-rotation アンカー、active-rotation-state ミラー、
  # last-rotation-at / last-failure-at annotation（§5.2/§5.3）が NodePool 上にあるため
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["nodes"]
  # update/patch: do-not-disrupt / do-not-disrupt-owned / surge-for / cordoned annotation + spec.unschedulable
  # （cordon、§3.3）。Node の書き込みは nodeclaims/nodepools と同じ full-object の
  # update-under-retry パス（§5.3）を使うため、patch に加えて update が必要
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["pods"]
  # placeholder Pod はコントローラが直接作成・管理する（§3.3）
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: [""]
  # core/v1 Events: leader election が Lease イベントをレガシー recorder 経由で記録する
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  # events.k8s.io/v1 Events: §4.2 / §3.2 レイヤ 3 の警告 Event は cluster-scoped な
  # NodePool/NodeClaim オブジェクトに対し新 recorder API で発行され、これらの Event は
  # "default" namespace に作成される（この権限は cluster-wide に付与される）
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

placeholder 専用の `PriorityClass`（§3.3）はランタイムに作成するのではなく、**Helm チャートが静的にインストール** する — したがってコントローラに `priorityclasses` 権限は不要である。

### クラウド（例: AWS）IAM

v1 ではクラウド API を直接呼び出さない。すべて Karpenter の `NodeClaim` CRD 経由。

v2（image pre-pull）の Job は新ノード上の Pod として動き、そのノードのロールを継承するため、コントローラ自体への追加のクラウド権限は不要。

## 4.4 コスト

各ローテーションで旧・新 Node が短時間並行課金される。1 ローテーションあたり概算: オンデマンド 1 台 × 10〜20 分。週次で N 件ローテーションなら月額追加は `≈ N × 4 × 時間単価 × 0.25` で、ベース Node コストに対して小さい。ローテーションは *NodePool ごと* には直列だが NodePool *間* では並行する（§3.3）ため、ピークの重なり（= 瞬間的な追加コストのピーク）は同時にローテーション中の NodePool 数に比例する。

**失敗した** surge 試行も surge ノードを短時間（最大 `readyTimeout`。その後、**まだ無人であれば**明示的に回収 — §3.3 *ロールバック*。その間に無関係な preemptor が着地した surge ノードは意図的に回収*せず*、NodePool の通常キャパシティとしてそのまま残る — リークではなく転用である）課金し得る。

失敗がこのコストを繰り返す速さは 2 つの機構が抑える: エスカレーションする `retryBackoff`（§5.3）が *同一* claim のリトライを抑え、**NodePool レベルの試行間休止**（`last-failure-at` + `cooldownAfter`、§5.2 ステップ 2）が候補の*巡回*を抑える — 後者がなければ、触れる claim をことごとく失敗させる系統的原因（執拗な placeholder preempt、持続的な同一 AZ 容量不足）は 1 分程度で次の候補へ移り、候補ごとに `readyTimeout` 分の失敗 surge 課金を焼き続けることになる。休止があれば、系統的失敗下の NodePool が走らせるのは最大でも **`readyTimeout + cooldownAfter` あたり 1 試行**（既定値で約 25m）であり、そのパターンには `noderotation_retry_count` がアラートする（§4.2）。
