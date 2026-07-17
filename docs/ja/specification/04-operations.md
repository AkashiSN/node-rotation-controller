# 4. 運用

## 4.1 キャパシティ / 可用性

::: tip このセクションの内容
surge がローテーション中の Pod 可用性にどう影響するか、および 1 ノード surge バジェットの適用方法。
:::

| 懸念事項 | 対処 |
|---------|-----------|
| Pod pending 時間 | surge によりほぼゼロ |
| `readyReplicas` のディップ | アプリケーション層での緩和 |
| 同時 surge ノード | NodePool 内シリアル（v1） |

- **Pod pending 時間:** surge により Karpenter Graceful セマンティクス（make-before-break）と同等。
- **`readyReplicas` のディップ:** Kubernetes の構造的制限 — surge があっても eviction 後に新しい Pod が即座に `Ready` にならない。緩和: レプリカのオーバープロビジョニング + PDB。スコープ外。
- **同時 surge:** v1 は NodePool あたり `surge.maxUnavailable = 1`（内部シリアル; 異なるプールは同時に surge 可能）。代替ノードは NodePool 所有（placeholder 経由で誘導、§3.3）。

### 1 ノード surge バジェットの適用方法

`spec.limits` は **リソースバジェット**（`{cpu, memory, …}`）であり、ノード数ではない。前提条件は placeholder のリクエストが NodePool の残りバジェット（`limits − 現在プロビジョニング済み`）に収まること。

コントローラーは **開始前にヘッドルームを事前チェック**（§5.2 ステップ 3、候補選定後 — placeholder のリクエストは選定された候補に依存するため）。バジェットが 1 ノード分を収容できない場合は警告付きでスキップ。

## 4.2 オブザーバビリティ

### Prometheus メトリクス

`/metrics` で公開:

| メトリクス | タイプ | ラベル |
|--------|------|--------|
| `noderotation_candidates` | Gauge | `nodepool` |
| `noderotation_in_progress` | Gauge | `nodepool` |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` |
| `noderotation_forceful_fallback_total` | Counter | `nodepool` |
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` |
| `noderotation_window_active` | Gauge | `nodepool` |
| `noderotation_policy_conflict` | Gauge | `nodepool` |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` |
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` |
| `noderotation_rotation_chances` | Gauge | `nodepool` |
| `noderotation_throughput_capacity` | Gauge | `nodepool` |
| `noderotation_t_rot_estimate_seconds` | Gauge | `nodepool` |
| `noderotation_t_rot_bound_seconds` | Gauge | `nodepool` |
| `noderotation_window_period_seconds` | Gauge | `nodepool` |
| `noderotation_short_lead_nodes` | Gauge | `nodepool` |
| `noderotation_drain_stuck` | Gauge | `nodepool` |
| `noderotation_retry_count` | Gauge | `nodepool` |

::: details メトリクス詳細 — クリックで展開

- **`noderotation_candidates`:** プールあたりの適格 NodeClaim 数
- **`noderotation_in_progress`:** プールあたりのアクティブローテーション数
- **`noderotation_completed_total`:** 累積完了数; `outcome` ∈ {`success`, `failure`, `expired`}。`expired` = graceful ローテーション完了前に force-expire（1回のみ発行、success としてカウントしない）
- **`noderotation_forceful_fallback_total`:** surge なし forceful fallback ローテーション開始数（§3.6）; 開始時にインクリメント
- **`noderotation_duration_seconds`:** フェーズごと; `phase` ∈ {`surge_wait`, `drain`}。`surge_wait` = `started-at → surge_ready`; `drain` = `draining-at → 旧 NodeClaim ファイナライズ`。成功遷移ごとに最大 1 回観測（リトライ書き込みでの二重カウントなし）
- **`noderotation_window_active`:** 0/1 ウィンドウメンバーシップ表示
- **`noderotation_policy_conflict`:** 0/1 セレクタータイまたは無効ポリシーによりブロック（§5.4）
- **`noderotation_freeze_until_timestamp`:** アクティブ freeze の Unix タイムスタンプ（0 = なし）
- **`noderotation_age_threshold_seconds`:** 導出された `ageThreshold`（§3.2）
- **`noderotation_rotation_chances`:** 保証回数 `G`
- **`noderotation_throughput_capacity`:** layer-2 予測 `C` — 発生あたりの開始数（§3.2）
- **`noderotation_t_rot_estimate_seconds`:** 予測サービス時間 `t_rot_est = provisioningEstimate + drainEstimate`
- **`noderotation_t_rot_bound_seconds`:** 期限側上限 `t_rot = readyTimeout + tGP + buffer`
- **`noderotation_window_period_seconds`:** 最悪ケース周期 `P`
- **`noderotation_short_lead_nodes`:** 自身の `spec.expireAfter` で `K` 回を保証できない NodeClaim 数（§3.2 layer 3）
- **`noderotation_drain_stuck`:** 0/1 ドレインが `tGP + buffer` を超過（§5.2）
- **`noderotation_retry_count`:** プール内 NodeClaim の最大 `retry-count`（0 = なし）

:::

#### ラベルに関する注記

NodePool ごとのウィンドウ（各プールが自身の `RotationPolicy` を解決、§5.4）により、`noderotation_window_period_seconds` と `noderotation_window_active` は **ロードベアリング** な `nodepool` ラベルを持つ — `P` とメンバーシップはプール間で異なりうる。

- **`expireAfter: Never`:** すべての導出ゲージが `0`（導出スキップ）
- **ウィンドウ発生なし（`P ≤ 0`）:** bound/estimate は非ゼロ; `throughput_capacity` のみ `0`

#### ライフサイクル

- シリーズは **NodePool 削除時にクリア** — ゲージは各 reconcile で再計算
- governing policy を失ったプールも同様にシリーズがドロップ（§5.4）

### Kubernetes Events

Warning レベルの状態が `kubectl describe` で確認可能:

| オブジェクト | Reason | タイミング |
|--------|--------|------|
| NodePool | `KBelowTwo`, `AVeryAggressive`, `TGPUnset`, `HardCapExceeded`, `RetryBackoffShort`, `DrainEstimateAboveTGP`, `ThroughputBelowArrival`, `ThroughputBurstShortfall`, `RotationSpansNextWindow`, `OverrideGBelowK` | スケジュール finding がアクティブ |
| NodeClaim | `ShortLead` | claim が `K` 回を保証できない |
| NodePool | `ForcefulFallback` | surge なしローテーション開始 |
| NodePool | `RotationStarted` | 候補選定（`Normal`） |
| NodePool | `RotationCompleted` | 旧 NodeClaim ファイナライズ（`Normal`） |
| NodeClaim | `RotationFailed` | `readyTimeout` 超過; ロールバック |
| NodeClaim | `SurgeUnschedulable` | placeholder `PodScheduled=False` |
| NodeClaim | `SurgeClamped` | placeholder クランプ済み（`Normal`） |
| NodeClaim | `SurgeClampBandExceeded` | クランプ shortfall > バンド（`Warning`） |
| NodeClaim | `SurgeClampRefused` | DaemonSet が allocatable を消費（`Warning`） |

- **重複排除:** 状態への遷移時に発行; クリアして再発時に再発行
- **Fatal finding** は Event ではない — ローテーション開始をブロックし §5.2 feasibility gate でログ

### 状態マシンのログ行

すべての状態遷移が 1 つの `INFO` ログ行を発行（永続アノテーション書き込み後）:

| 行 | 主要フィールド |
|------|------------|
| `rotation candidate selected` | `nodeclaim`, `age`, `deadline`, `surgeless` |
| `no rotation candidate` | `reason`、census カウント |
| `surge placeholder created` | `placeholder`, `requests`、除外カウント、クランプ情報 |
| `surge placeholder is not schedulable` | `placeholder`, `reason`, `detail` |
| `surge node ready` | `surgeNode`, `surgeWait` |
| `drain started` | `node`, `mode` ∈ {`surge`, `forceful-fallback`} |
| `rotation attempt failed` | `reason`, `readyTimeout`, `retryCount`, `backoffUntil` |
| `rotation complete` | `mode`, `drain`, `surgeNode`, `surgeWait`, `total` |

- **レベルトリガー行**（`no rotation candidate`、`surge placeholder is not schedulable`）は遷移 dedup を使用 — reason/census/message が変化した場合のみ再発行
- **デバッグ冗長性**（`V(1)`）で dedup なしの各パス findings とハートビートを追加
- **ライブネスシグナル:** ログの沈黙ではなく `controller_runtime_reconcile_total` / workqueue メトリクスから読み取る

### 推奨アラート

| アラート | 条件 |
|-------|-----------|
| 失敗/expired | `increase(noderotation_completed_total{outcome=~"failure|expired"}[1h]) > 0` |
| 追いつけない | `noderotation_candidates > 0` が 2 回連続ウィンドウ |
| ウィンドウ無駄 | `window_active == 1` 全ウィンドウ、完了ゼロ、候補非ゼロ |
| ドレイン停滞 | `noderotation_drain_stuck == 1` |
| Short lead | `noderotation_short_lead_nodes > 0` |
| 体系的失敗 | `noderotation_retry_count >= 3` |
| Forceful fallback | `increase(noderotation_forceful_fallback_total[1h]) > 0`（severity: info） |

Helm chart がオプションの `PrometheusRule` として提供（`prometheusRule.enabled` でゲート、デフォルト `false`）。チューニングは [プロダクション Runbook](../runbook.md) を参照。

## 4.3 RBAC とクラウド権限

### Kubernetes RBAC

```yaml
- apiGroups: ["karpenter.sh"]
  resources: ["nodeclaims"]
  verbs: ["get", "list", "watch", "update", "patch", "delete"]
- apiGroups: ["karpenter.sh"]
  resources: ["nodepools"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

- **`nodeclaims`:** `create` なし — v1 は `NodeClaim` を作成しない（§3.3）。`update`/`patch` は状態アノテーション; `delete` はローテーションと failure reap を駆動
- **`nodepools`:** `update`/`patch` は active-rotation anchor、状態ミラー、完了アノテーション用
- **`nodes`:** `update`/`patch` は `do-not-disrupt`/マーカー + `spec.unschedulable`（cordon）用
- **`pods`:** placeholder Pod を直接管理
- **`events`:** NodePool/NodeClaim の Warning Events + リーダー選出記録
- **`leases`:** リーダー選出

placeholder の `PriorityClass` は **Helm chart により静的にインストール** — `priorityclasses` 権限不要。

### クラウド（例: AWS）IAM

- **v1:** クラウド API への直接呼び出しなし。すべての操作は `NodeClaim` CRD 経由。
- **v2（pre-pull）:** Job が新ノード上の Pod として実行され、そのロールを継承。コントローラーレベルの追加クラウド権限なし。

## 4.4 コスト

::: tip 要点
各ローテーションは ~10–20 分の重複課金を発生させる。失敗駆動のコストは 2 つのメカニズムで制限: エスカレーティング `retryBackoff` とプールレベルの `failurePause`。
:::

### 通常のローテーションコスト

短時間の重複: surge 中に新旧ノードが同時に課金。

- **ローテーションあたり:** 1 つの追加オンデマンドインスタンスの ~10–20 分
- **月間（週次ローテーション、N ノード）:** `≈ N × 4 × hourly_rate × 0.25`
- **ピーク重複:** 同時にローテーションする NodePool 数に比例

### 失敗した surge のコスト

失敗した試行は `readyTimeout` まで surge ノードを課金する可能性あり（未使用時にその後 reap; 再利用されたノードは通常キャパシティとして残存）。

### コスト制限メカニズム

| メカニズム | 制限対象 |
|-----------|--------|
| エスカレーティング `retryBackoff` | 同一 claim のリトライ |
| プールレベル `failurePause` | 体系的失敗下の候補サイクリング |

- **`failurePause` なし:** 体系的原因が ~1 分以内に次の候補に移行し、候補ごとに `readyTimeout` 分の課金を消費
- **`failurePause` あり:** `readyTimeout + failurePause` あたり最大 1 回の試行（デフォルトで ~25m）
- `failurePause` は `cooldownAfter` と分離 — スループット向上のための settle 短縮がコスト制限を弱めない
- `noderotation_retry_count` がパターンをアラート（§4.2）