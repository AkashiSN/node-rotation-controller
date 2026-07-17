# 1. 概要

## 1.1 背景

::: tip このセクションの説明
Karpenter の Forceful Expiration は予測不能なタイミングで発動し、Disruption Budgets やプリプロビジョニングを無視する。本コントローラーは、そのローテーションをより早い段階で制御されたメンテナンスウィンドウ内に移動させ、voluntary パスを使用する。
:::

Karpenter（および EKS Auto Mode）はノードの disruption を2つに分類する：

| カテゴリ | 例 | Budgets 適用? | プリプロビジョニング? | PDB |
|----------|------|------------------|------------------|-----|
| Graceful | Drift, Consolidation | あり | あり | 厳格に尊重 |
| **Forceful** | **Expiration**, Spot | **なし** | **なし** | `tGP` で制限 |

- **Expiration が Forceful である理由:** AMI パッチやセキュリティ更新が設定ミスの budgets や PDB によって無期限に遅延しないようにするため。上流の [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) に記載。「オペレーターが独自の graceful ローテーションを実装する」が許容されるソリューションパスとして明示されている。
- **EKS Auto Mode の制約:** **21日間の最大ノード寿命**（短縮は可能だが削除は不可）。`expireAfter + terminationGracePeriod ≤ 21d` が強制される（[AWS builders.flash, 2025-04](https://aws.amazon.com/jp/builders-flash/202504/dive-deep-eks-node-automated-update/)）。デフォルト値: `expireAfter` 336h（≈14日）、`terminationGracePeriod` 24h（[Create a Node Pool](https://docs.aws.amazon.com/eks/latest/userguide/create-node-pool.html)）。

**実際の影響:** 非自明なクラスターでは、PDB 設定に関係なくノードが予測不能なタイミングで強制ドレインされる。Karpenter はドレイン開始 *後に* のみ代替ノードのプロビジョニングを開始する。レイテンシに敏感なワークロード（`request == limit`）では、ビジネスのピーク時間に衝突しうる Pod Pending ウィンドウが生じる。

## 1.2 目標

| # | 目標 |
|---|------|
| G1 | Forceful Expiration の発動を防止 |
| G2 | Pod Pending ウィンドウの排除 |
| G3 | ローテーションをビジネスセーフな時間帯に限定 |
| G4 | 既存の保護機構との共存 |

- **G1:** メンテナンスウィンドウ内に age threshold（NodePool ごとに導出、§3.2）に近づいた `NodeClaim` を voluntary disruption パスで置換する。
- **G2:** NodePool 所有の代替ノードを先に追加し、`Ready` になるのを待ってから古いノードを削除（ノードレベルの surge / make-before-break）。Pod レベルの順序制御は PDB に委譲（§3.5）。
- **G3:** 設定可能なメンテナンスウィンドウ（曜日 / 時刻 / タイムゾーン）。
- **G4:** PDB、`topologySpreadConstraints`、preStop hooks、Pod Readiness Gates、ALB slow start — すべて有効なまま維持し、置換しない。

## 1.3 非目標

| # | 非目標 | 根拠 |
|---|----------|-----------|
| N1 | Consolidation / Drift の置換 | Expiration パスのみを引き継ぐ |
| N2 | Spot 中断の処理 | 2分のハードリミット; [AWS NTH](https://github.com/aws/aws-node-termination-handler) を使用 |
| N3 | アプリケーション側のウォームアップ | `readinessProbe` / `readinessGate` / ALB slow start の責務 |
| N4 | `expireAfter == 0` や 21日制限のバイパス | バイパス不可; backstop として保持 |
| N5 | OS パッチリブートのオーケストレーション | スコープ外; [kured](https://github.com/kubereboot/kured) を参照 |

## 1.4 用語

### 基本概念

- **NodeClaim:** Karpenter v1 CRD; 基盤インスタンス（例: EC2）との 1:1 表現
- **surge:** 代替ノードを作成し `Ready` になるのを待ってから古いノードをドレイン（make-before-break）
- **placeholder (Pod):** コントローラーが NodePool 所有の代替キャパシティを誘導するために作成する低優先度の "pause" Pod — Karpenter が新しいノードをプロビジョニング（またはスケジューラーが既存の空きにビンパック）。スタンドアロン `NodeClaim` は使用しない（§3.3）
- **メンテナンスウィンドウ:** コントローラーがローテーションを *開始* できる曜日/時刻の範囲の **和集合**。進行中のローテーションはウィンドウの境界を超えて完了する
- **freeze:** NodePool ごとの保留（`noderotation.io/freeze` アノテーション）。進行中の `pending` ローテーションさえ一時停止（§3.1）— ウィンドウ（*開始* のみゲート）とは異なる
- **age threshold:** `NodeClaim` がローテーション候補になる `creationTimestamp` からの経過時間。スケジュールと目標ローテーション回数（`minRotationChances`）から NodePool ごとに **導出**（§3.2）
- **candidate:** すべての選定条件（§3.2）を満たし、ローテーション対象となる `NodeClaim`
- **governing policy:** 指定の NodePool に対してセレクタの specificity で勝利する `RotationPolicy`（§5.4）
- **backstop:** Karpenter のネイティブな `expireAfter`（Forceful Expiration）。安全ネットとして保持

### Disruption パス

- **voluntary パス:** Consolidation、Drift、および本コントローラーの `NodeClaim` 削除 — PDB を尊重
- **forceful パス:** `expireAfter`、Spot Interruption — `terminationGracePeriod` までのみ PDB を尊重
- **forceful fallback:** オプトイン、ウィンドウ内限定モード（`surge.forcefulFallback`、デフォルト off; ADR-0001）。リスクのある `NodeClaim` を surge **なし** でウィンドウ内に削除 — voluntary パス経由（PDB 適用、§3.3）

### シンボル

§3–§5 で使用。完全な導出は §3.2 を参照。

| シンボル | 意味 |
|--------|---------|
| `E` | `expireAfter` — Forceful Expiration までの NodeClaim 寿命 |
| `tGP` | `terminationGracePeriod` — ドレイン上限 |
| `P` | 最悪ケースのウィンドウ周期（連続する発生間の最大ギャップ、§3.1） |
| `t_rot` | ローテーション所要時間上限 = `readyTimeout + tGP + buffer` |
| `t_rot_est` | 期待ローテーションサービス時間 = `provisioningEstimate + drainEstimate`（layer-2 のみ） |
| `K` | `minRotationChances` — 保証するローテーション機会（下限 1） |
| `leadTime` | ノードを選定する先行時間 = `K·P + t_rot` |
| `A` | `ageThreshold` — 導出式 `A = E − (K·P + t_rot)` |
| `G` | スケジュールが実際に保証するローテーション回数 |
| `D` | メンテナンスウィンドウの長さ（単一発生） |
| `gap` | 連続する発生間の最短閉鎖区間 |
| `m` | `surge.maxUnavailable` — NodePool あたりの同時ローテーション数（v1 では `1` 固定） |
| `C` | 発生あたりのウィンドウ容量 = `m · ceil(D / (t_rot_est + cooldownAfter))` |
| `N` | NodePool ノード数（layer-2 スループットチェックのみ） |

::: details シンボル補足 — クリックで展開

- **`drainEstimate`:** 期待される正常な PDB 準拠ドレイン時間（`surge.drainEstimate`）; 未設定 ⇒ `min(tGP, 10m)`。Layer-2 予測のみ
- **`provisioningEstimate`:** 期待される surge プロビジョニング時間（`surge.provisioningEstimate`）; 未設定 ⇒ `min(readyTimeout, 5m)`。Layer-2 予測のみ（ADR-0003）
- **`cooldownAfter`:** 成功後の安定化待機（gate A）。`C` に影響するが `t_rot` には含まれない
- **`failurePause`:** 失敗後の試行間一時停止（gate B、§4.4、ADR-0004）。導出シンボルには影響しない
- **`buffer`:** 固定コントローラー定数（`4·shortRequeue = 2m`）。検出遅延をカバー。`t_rot` のみに影響 — `t_rot_est` やオペレーター設定には **含まれない**
- **`readyTimeout`:** `t_rot` に影響する設定フィールド

:::

## 1.5 Karpenter エコシステムにおける位置づけ

本コントローラーは Karpenter の **上位レイヤー** で動作する。Karpenter の動作を変更しない。

### 上流が吸収しない理由

[`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) は Expiration を forceful に保つ意図的な決定を記録し、3つの選択肢を提示している：

1. （上流推奨）Expiration を forceful のまま維持
2. NodePool ごとの `expirationPolicy: Forceful | Graceful` フィールドを追加
3. **「オペレーターが独自の graceful ローテーションを実装する」**

本コントローラーは選択肢 3。上流による吸収のリスクは低い。

### Disruption Budgets が不十分な理由

| 要件 | 達成可能? |
|-------------|-------------|
| ウィンドウ内のみ disruption を許可 | △ 煩雑 |
| ウィンドウを Expiration に適用 | ✗ |
| Expiration 中の surge | ✗ |

- **△ 煩雑:** ブラックリスト方式のみ（ウィンドウ外に複数 budgets で `nodes: "0"` を設定）。アルゴリズムは重複する budgets の *最小値* を取る — [Discussion #1079](https://github.com/kubernetes-sigs/karpenter/discussions/1079) 参照
- **✗ Budgets は Expiration に適用されない** — Consolidation/Drift のみ
- **✗ プリプロビジョニングなし** — Expiration は forceful

本コントローラーは第2・第3のギャップを埋め、第1を大幅に簡素化する。

### 隣接プロジェクト

| プロジェクト | スコープ | 重複 |
|---------|-------|---------|
| Karpenter Disruption Budgets | Drift/Consolidation のレート制限 | 補完的 |
| [kured](https://github.com/kubereboot/kured) | OS パッチ用リブート | なし |
| [AWS NTH](https://github.com/aws/aws-node-termination-handler) | Spot / スケジュールイベント | なし |
| [descheduler](https://github.com/kubernetes-sigs/descheduler) | Pod リバランス | なし |
| EKS Node Auto Repair | 異常ノードの置換 | なし |
