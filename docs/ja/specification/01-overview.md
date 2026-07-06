# 1. 概要

## 1.1 背景

Karpenter（および Karpenter ベースの EKS Auto Mode）では Node の disruption を 2 種類に分類している。

| 分類 | 例 | NodePool Disruption Budgets | 代替 Node の事前起動 | PDB |
|------|-----|------------------------------|-----------------------|-----|
| Graceful | Drift, Consolidation | 適用される | する（make-before-break）| 厳密に尊重 |
| **Forceful** | **Expiration**, Spot Interruption | **適用されない** | **しない** | `terminationGracePeriod` でキャップ |

Expiration が意図的に Forceful とされているのは、AMI パッチやセキュリティ更新を Budgets / PDB の誤設定で無期限延期させない設計思想に基づく。これは Karpenter 公式 design [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) に明文化されており、同 design は「**運用者が独自に graceful rotation を実装する**」ことも妥当な解の一つとして列挙している。EKS Auto Mode はさらに **21 日のノード最大寿命**を、ユーザが*短縮*はできても*除去*はできない制約として追加している — ノードは「最大 21 日の寿命を持ち、その後自動的に置き換えられる」（[EKS Auto Mode ユーザーガイド](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)）。ノードの真の寿命は `expireAfter` の失効に加え最大 `terminationGracePeriod` の drain を**含む**ため、この上限は両者の**合計**に対する制約として課される: `expireAfter + terminationGracePeriod ≤ 21d`（AWS は「expireAfter と NodePool の terminationGracePeriod の合計値は 21 日を超えることはできません」と明記 — [AWS builders.flash, 2025-04](https://aws.amazon.com/jp/builders-flash/202504/dive-deep-eks-node-automated-update/)）。参考までに Auto Mode のデフォルトは `expireAfter` 336h（≈14d）、`terminationGracePeriod` 24h（[Create a Node Pool](https://docs.aws.amazon.com/eks/latest/userguide/create-node-pool.html)）。

現実的な帰結として、運用中のクラスタでは **PDB を厳しくしても Node は必ず Force drain される瞬間が来る**。Karpenter は drain 開始の **後から** 代替 Node を立ち上げるため、`request==limit` のような厳しい capacity 要件のワークロードではピーク時間と衝突した瞬間に Pod Pending が発生する。

## 1.2 ゴール

| # | ゴール |
|---|--------|
| G1 | age 閾値（メンテナンススケジュールと目標ローテーション回数から NodePool ごとに導出 — §3.2）に達した `NodeClaim` をメンテナンスウィンドウ内で voluntary 経路で先回り置換し、**Forceful Expiration を実質発火させない** |
| G2 | 代替の NodePool-owned ノードを先に追加して `Ready` を待ってから旧 `NodeClaim` を delete する（**ノードレベルの surge / make-before-break**。Pod レベルの順序付けは PDB に委譲 — §3.3）。Pod Pending の窓を 0 に近づける |
| G3 | 業務影響の少ない時間帯に置換を **閉じ込める**（曜日 / 時刻 / タイムゾーン設定） |
| G4 | 既存の保護機構（PDB、`topologySpreadConstraints`、preStop、Pod Readiness Gate、ALB Slow Start）と **共存して成立** する。置き換えない |

## 1.3 非ゴール

| # | 非ゴール | 理由 |
|---|----------|------|
| N1 | Karpenter Consolidation / Drift の置き換え | Karpenter の自発的最適化は引き続き有効。本コントローラは Expiration 経路のみ肩代わり |
| N2 | Spot Interruption への対応 | 2 分の hard limit がある AWS インフラ側イベント。[AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) を使う |
| N3 | アプリケーションの warm-up 責務 | JVM 起動、コネクションプール初期化等は `readinessProbe` / `readinessGate` / ALB slow start の領分。本コントローラは **Node** の orchestration を提供し、アプリは自分の readiness を自分で表現する |
| N4 | `expireAfter == 0` 化、21 日 hard cap の解除 | Auto Mode の hard cap は回避不能。`expireAfter` はコントローラ停止時の **バックストップ** として意図的に残す |
| N5 | OS パッチ起因の Node 再起動 | スコープ外。[kured](https://github.com/kubereboot/kured) を使う |

## 1.4 用語

| 用語 | 定義 |
|------|------|
| **NodeClaim** | Karpenter v1 CRD。実インスタンス（EC2 等）に 1:1 対応 |
| **surge** | 旧 Node を抜く前に新 Node を立ち上げて `Ready` 化する make-before-break 戦略 |
| **メンテナンスウィンドウ** | コントローラが置換を **開始してよい** 曜日・時間帯の **和集合**（1 つ以上）。窓終端を跨いだ進行中の置換は完遂させる |
| **age 閾値** | `creationTimestamp` からの経過時間がこの値を超えた `NodeClaim` を候補とする値。スケジュールと目標ローテーション回数（`minRotationChances`）から NodePool ごとに **導出**、直接指定しない（§3.2）。実際のノード単位トリガは各 NodeClaim 自身の `spec.expireAfter` デッドラインに基づき、`ageThreshold` はその age 換算の代表値（§3.2）|
| **バックストップ** | コントローラが停止しても Karpenter 標準の `expireAfter`（Forceful Expiration）が最終的に Node を置換する安全装置。意図的に残す |

**記号** — §3〜§5 で頻出。完全な導出と「ノード単位か NodePool テンプレートか」の権威ある区別（下表の **取得元** 列）は §3.2 を参照。

| 記号 | 意味 |
|------|------|
| `E` | `expireAfter` — Forceful Expiration までの NodeClaim の寿命（ノード単位・権威: `NodeClaim.spec.expireAfter`） |
| `tGP` | `terminationGracePeriod` — Karpenter が drain を保持できる上限 |
| `P` | 最悪ウィンドウ周期 — 連続するメンテナンスウィンドウ機会の最大ギャップ（§3.1） |
| `t_rot` | 1 ノードのローテーション所要時間の上限 = `readyTimeout + tGP + buffer` |
| `K` | `minRotationChances` — 失効前に保証したいローテーション回数（下限 1） |
| `leadTime` | deadline のどれだけ前に選定するか = `K·P + t_rot` |
| `A` | `ageThreshold` — ノードが候補になる age。導出は `A = E − (K·P + t_rot)` |
| `G` | スケジュールが実際に保証するローテーション回数。auto 導出では `G = K`、明示的な `ageThreshold` override 時は再計算 |

## 1.5 Karpenter エコシステムでの位置付け

本コントローラは Karpenter 公式の設計方針と整合している。Karpenter 本体の挙動を変えるのではなく、その **上のレイヤ** で動作する。

### Karpenter 本体に同等機能が組み込まれない理由

[`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) は Expiration を Forceful に保つ判断を記録しており、graceful な expiration を求めるユーザに対し以下 3 オプションを提示している。

1. （推奨）Expiration は Forceful のまま
2. NodePool ごとに `expirationPolicy: Forceful | Graceful` を追加
3. **「運用者が独自に graceful rotation を実装する」**

本コントローラは Option 3 に該当する。Karpenter 本体に "graceful surge rotation for Expiration" が組み込まれる可能性は当面低く、ユーザ側実装が公式に妥当解として位置付けられている。

### Disruption Budgets では不十分な理由

`NodePool.spec.disruption.budgets` は `schedule + duration` をサポートし、表面的にはメンテナンスウィンドウに見える。実際には 2 つの構造的制約がある。

| 要件 | Karpenter 単体で実現可能か |
|------|----------------------------|
| 窓内のみ disruption を許可、窓外は拒否 | △ ブラックリスト方式のみ可能（複数 budget の **最小値が採用される** 仕様による）— [Discussion #1079](https://github.com/kubernetes-sigs/karpenter/discussions/1079) 参照 |
| 上記を **Expiration** にも適用 | ✗ Budgets は Drift / Consolidation のみが対象、**Expiration には適用されない** |
| Expiration 時に surge 置換 | ✗ Expiration は Forceful で代替 Node の事前起動なし |

本コントローラは下 2 行を埋め、1 行目も大幅に簡潔化する。

### 隣接プロジェクト

| プロジェクト | スコープ | 重複度 |
|------------|---------|--------|
| Karpenter NodePool Disruption Budgets | Drift / Consolidation のレート制御 | 補完関係、Expiration には適用外 |
| [kured](https://github.com/kubereboot/kured) | OS パッチ起因の Node 再起動 | 無、NodeClaim にタッチしない |
| [AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) | Spot 中断 / Scheduled Event | 無、トリガが異なる |
| [descheduler](https://github.com/kubernetes-sigs/descheduler) | Pod 再配置 | 無、Node にタッチしない |
| EKS Node Auto Repair (AWS マネージド) | 故障 Node 置換 | 無、期限切れ駆動ではない |

---

