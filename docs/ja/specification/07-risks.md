# 7. リスクと状況

## 7.1 リスク

| # | リスク | 緩和策 |
|---|------|------------|
| R1 | コントローラーのクラッシュ / リーダー喪失 | `replicas=2` + リーダー選出; backstop 保持; 失敗アラート |
| R2 | ウィンドウがすべての候補に不足 | §3.2 スループットチェックが警告; 候補持続でアラート |
| R3 | surge キャパシティ不可（AZ 不足 / limits） | ヘッドルーム事前チェック（§5.2）; `readyTimeout` ロールバック; マルチ AZ / マルチインスタンスタイプ |
| R4 | ドレインが設定ミスの PDB でブロック | `terminationGracePeriod` が強制ドレイン; PDB レビューはアプリオーナーの責務 |
| R5 | クリティカル期間中の freeze 忘れ | 宣言的に管理（GitOps）、アドホックではなく |
| R6 | テストクラスターが定常的にターンオーバー | `ageThreshold` を超える soak 期間中はシャットダウンを無効化 |

- **R3 ゾーンの注意:** surge は候補の AZ に固定（§3.7）。同一 AZ の不足は他のゾーンにフォールバックできない — ゾーン PV NodePool では AZ ごとのヘッドルームを確保すべき。

## 7.2 検証済み前提

::: tip 検証サマリー
**検証済み（20+ シナリオ）:** コア surge、同一 AZ ゾーン PV リバインド、ロールバック、limits ゲーティング、マルチプールの閉じ込め、PDB ドレイン、do-not-disrupt マーカー、force-expiry 検出、キャパシティ吸収、placeholder プリエンプション、ウィンドウ境界、リーダー変更再開、forceful fallback、earliest-deadline ソート、オペレーターオプトアウト、12 時間 tight-race soak。

**未決:** 真の同一 AZ キャパシティ不足（ICE）によるリアルクラウドでのロールバック（issue #109）。
:::

### コアメカニズム

| 前提 | ステータス | 日付 |
|------------|--------|------|
| スタンドアロン `NodeClaim` が Auto Mode でプロビジョニング可能 | 検証済み | 2026-05-29 |
| placeholder Pod surge が make-before-break を完了 | 検証済み | 2026-06-22 |
| 同一 AZ surge で EBS 再アタッチ（ゾーン PV リバインド） | 検証済み | 2026-06-22 |
| `readyTimeout` ミスがクリーンにロールバック | 検証済み | 2026-06-22 |
| NodePool `limits` 消費が surge をゲート | 検証済み | 2026-06-22 |
| required `karpenter.sh/nodepool` が surge をプールに閉じ込め | 検証済み | 2026-06-22 |
| 明示的 `NodeClaim` 削除が voluntary パスでドレイン（PDB） | 検証済み | 2026-06-22 |
| `do-not-disrupt` を両ノードに適用、完了時に削除 | 検証済み | 2026-06-22 |
| pending 中の force-expiry が `expired` を記録（success/failure ではない） | 検証済み | 2026-06-22 |

### 応用シナリオ

| 前提 | ステータス | 日付 |
|------------|--------|------|
| キャパシティ吸収パス（空きにビンパック、新ノードなし） | 検証済み | 2026-06-23 |
| リーダー変更がアノテーションのみから再開 | 検証済み | 2026-06-23 |
| 進行中のローテーションがウィンドウ境界を超えて完了 | 検証済み | 2026-06-23 |
| placeholder がプリエンプション犠牲者; 敵対的プリエンプション → ロールバック | 検証済み | 2026-06-23 |
| `do-not-disrupt` が Drift に対して有効 | 検証済み | 2026-06-23 |

### v0.4 以降の追加

| 前提 | ステータス | 日付 |
|------------|--------|------|
| Forceful fallback（12 ノードバッチ、graceful + surge なしミックス） | 検証済み | 2026-07-04 |
| earliest-deadline 候補ソート | 検証済み | 2026-07-04 |
| オペレーター `do-not-disrupt` が選定から除外 | 検証済み | 2026-07-04 |

### soak テスト

| 前提 | ステータス | 日付 |
|------------|--------|------|
| 12h tight-race soak: 71/71 graceful、0 expired、0 failure | 検証済み | 2026-07-15 |
| Forceful fallback が制限された claim に対して決定的に発動 | 検証済み | 2026-07-15 |

::: details 完全な検証エビデンス — クリックで展開

#### スタンドアロン NodeClaim（能力確認、surge メカニズムではない）

`nodeClassRef` + `requirements` のみの NodeClaim が `Ready` に到達（~30s、実 EC2）; admission が `--dry-run=server` を受け入れ; graceful な finalizer ドリブン削除を確認。K8s 1.35、`karpenter.sh/v1`（2026-05-29）。

#### placeholder Pod surge（2026-06-22）

低優先度 placeholder が NodePool 所有の surge `NodeClaim` を誘導し `Ready` に到達（~30s）。旧 NodeClaim 削除前に完了; ドレインは voluntary パスに従い; ワークロードが surge ノードに再スケジュール; `noderotation_completed_total{outcome="success"}` がインクリメント。

#### 同一 AZ ゾーン PV リバインド（2026-06-22）

StatefulSet gp3 PVC が `us-west-2a` でバインド; ローテーションを跨いで `matchNodeRequirements` のゾーンパリティがすべての surge ノードを `us-west-2a` に維持; **同一** PV が再アタッチ（再プロビジョニングではない）; センチネルデータ生存。

#### タイムアウトロールバック（2026-06-22）

`readyTimeout` をノード ready 時間未満に設定 → タイムアウト → surge claim reap、placeholder 削除、候補保持 + uncordon、`outcome="failure"` インクリメント。

#### Limits ゲーティング（2026-06-22）

`spec.limits.cpu` にヘッドルームなし: 適格候補は surge されず; `insufficient limits headroom; cannot surge` をログ; `in_progress` は 0 のまま。

#### マルチプールの閉じ込め（2026-06-22）

同一 AZ の空きを持つ別プール: surge は候補のプールに留まる（`karpenter.sh/nodepool=nrc-poc`）; 別プール変更なし。

#### PDB 準拠ドレイン（2026-06-22）

ブロッキング PDB（`minAvailable=2`、2 レプリカ）がドレインを停滞; `minAvailable=1` に緩和で 1 つずつマイグレーション完了。

#### `do-not-disrupt` マーカー（2026-06-22）

旧ノードと surge ノードの両方が `do-not-disrupt=true` + `do-not-disrupt-owned` を保持; 完了時の unfreeze で両方を削除。

#### force-expiry 検出（2026-06-22）

プールを freeze + pending 候補の `NodeClaim` を削除 → `state=expired`、anchor クリア、surge 残留なし、`outcome="expired"` インクリメント。

#### キャパシティ吸収パス（2026-06-23）

若い（`ageThreshold` 未満、非候補）同一 AZ スペアノードが ~1970m free で 250m placeholder を吸収（新 NodeClaim 誘導なし）; プールは 2 claim のまま; `outcome="success"` インクリメント。

#### リーダー変更再開（2026-06-23）

ローテーション中（`state=pending`）にリーダー Pod を kill; 新レプリカが Lease を取得し同じローテーションをアノテーションから継続 — 同一 `surge-claim`、同一 `started-at`、リスタートなしで完了。

#### ウィンドウ境界動作（2026-06-23）

ウィンドウ内でローテーション開始; ウィンドウクローズ後も中止せず完了; 第 2 の適格候補は開始しない（`window_active=0`）。

#### placeholder プリエンプション（2026-06-23）

高優先度 Pod が placeholder をプリエンプト; placeholder は何もプリエンプトしない（`preemptionPolicy=Never`）; limits でリプロビジョンブロック下で Pending のまま `readyTimeout` まで → クリーンロールバック。

#### `do-not-disrupt` vs Drift（2026-06-23）

Drifted ノードに `do-not-disrupt=true` → 3 分以上置換されず; アノテーション除去で即座に drift-replace。

#### Forceful fallback — 12 ノードバッチ（2026-07-04）

12 ノード、固定 2h `expireAfter`、`N=12 > K·C=2`: 最初の 6 は graceful、余剰 6 は surge なし。`rotation-mode=forceful-fallback`; `ForcefulFallback` Warning Events; forceful 候補に placeholder なし; `noderotation_forceful_fallback_total` が `0→6` にクリーンに上昇; PDB は維持; expired ゼロ。

#### earliest-deadline ソート（2026-07-04）

12 ノードバッチが 1 つの `creationTimestamp` を共有 → ソートは Name タイブレークに退化: claim は昇順で消費（`2rvd5 < 6ssql < dtkgz < ...`）。

#### オペレーター `do-not-disrupt` 除外（2026-07-04）

候補の Node に `do-not-disrupt=true`（owned マーカーなし）をアノテート → `candidates` ゲージが 4→3 にドロップ; 除去で復元。

#### 12h tight-race soak（2026-07-15）

`E=2h12m`、`leadTime=1h12m`、48 ウィンドウ/日、5 ノードプール。71/71 ローテーション graceful（~12m ケイデンス）。最小マージン 68.3m、中央値 70.3m、最大 71.2m。expired ゼロ、failure ゼロ、`forceful_fallback_total=0`（armed だが不要）。コントローラー `restartCount=0`、909 スクレイプ、連続 `seq`。詳細記録: `test/e2e/eks-automode/VALIDATION.md`。

#### Forceful fallback 境界（2026-07-15）

別の単一ノードミニプール、候補状態になるまで freeze。解放後 graceful surge が期限内に収まらない → surge なしブランチ: `forceful_fallback_total` 0→1; claim が解放 56 秒後に削除（期限の 10m04s 前）; 当該 claim で placeholder は一切なし; `expired` は 0 のまま。

:::

### 未決事項

真の同一 AZ **キャパシティ不足（ICE）** によるロールバック — 短い `readyTimeout` で代替（オンデマンドで決定的に誘発不可）。issue #109 で追跡。

### 注記

- スタンドアロン `NodeClaim` の結果はプロジェクトのリスクを低減するが **surge メカニズムではない**（§3.3 — スタンドアロンノードは NodePool アカウンティングを破壊）
- RBAC の十分性と `karpenter.sh/v1` CRD デコードはすべてのシナリオで暗黙的に検証

## 7.3 未決事項

1. **祝日対応スケジューリング** — ウィンドウ日が祝日に当たる場合にローテーションをスキップ。v1 では意図的に祝日を無視。
2. **pre-pull イメージソース** — Karpenter NodeClass のイメージプル機能を使うか専用 Job か（v2）。
3. **マルチクラウド検証** — EKS Auto Mode を超えた互換性を主張する前に AKS NAP、GKE をテスト。

::: tip 解決済み
*CRD ベースのポリシー移行* と *NodePool ごとのメンテナンスウィンドウ* — `RotationPolicy` CRD で提供（issue #119、§5.4）。
:::