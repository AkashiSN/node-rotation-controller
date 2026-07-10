# 1. 概要

## 1.1 背景

Karpenter（および Karpenter ベースの EKS Auto Mode）ではノードの disruption を 2 種類に分類している。

| 分類 | 例 | NodePool Disruption Budgets | 代替ノードの事前起動 | PDB |
|------|-----|------------------------------|-----------------------|-----|
| Graceful | Drift, Consolidation | 適用される | する（make-before-break）| 厳密に尊重 |
| **Forceful** | **Expiration**, Spot Interruption | **適用されない** | **しない** | 尊重されるが `terminationGracePeriod` でキャップ |

Expiration が意図的に Forceful とされているのは、AMI パッチやセキュリティ更新を Budgets / PDB の誤設定で無期限延期させない設計思想に基づく。これは Karpenter 公式 design [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) に明文化されており、同 design は「**運用者が独自に graceful rotation を実装する**」ことも妥当な解の一つとして列挙している。

EKS Auto Mode はさらに **21 日のノード最大寿命**を、ユーザが*短縮*はできても*除去*はできない制約として追加している — ノードは「最大 21 日の寿命を持ち、その後自動的に置き換えられる」（[EKS Auto Mode ユーザーガイド](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)）。ノードの真の終端は `expireAfter` の失効時点に最大 `terminationGracePeriod` の drain を**加えた**時点であるため、この上限は両者の**合計**に対する制約として課される: `expireAfter + terminationGracePeriod ≤ 21d`（AWS は「expireAfter と NodePool の terminationGracePeriod の合計値は 21 日を超えることはできません」と明記 — [AWS builders.flash, 2025-04](https://aws.amazon.com/jp/builders-flash/202504/dive-deep-eks-node-automated-update/)）。参考までに Auto Mode のデフォルトは `expireAfter` 336h（≈14d）、`terminationGracePeriod` 24h（[Create a Node Pool](https://docs.aws.amazon.com/eks/latest/userguide/create-node-pool.html)）。

現実的な帰結として、運用中のクラスタでは **PDB を厳しくしてもノードは必ず予測不能なタイミングで Force drain される**。Karpenter は drain 開始の **後から** 代替ノードを立ち上げるため、`request == limit` のような厳しい capacity 要件を持つレイテンシ敏感なワークロードでは、強制的な Pod Pending のウィンドウが生じ、ピーク業務時間帯と衝突しうる。

## 1.2 ゴール

| # | ゴール |
|---|--------|
| G1 | age 閾値（メンテナンススケジュールと目標ローテーション回数から NodePool ごとに導出 — §3.2）に達した `NodeClaim` をメンテナンスウィンドウ内で voluntary 経路で先回りローテーションし、**Forceful Expiration を実質発火させない** |
| G2 | 代替の NodePool-owned ノードを先に追加して `Ready` を待ってから旧 `NodeClaim` を delete することで（ノードレベルの surge / make-before-break。Pod レベルの順序付けは PDB に委譲 — §3.3）、**Pod Pending のウィンドウをなくす** |
| G3 | 業務影響の少ない時間帯にローテーションを **閉じ込める**（曜日 / 時刻 / タイムゾーン設定） |
| G4 | 既存の保護機構（PDB、`topologySpreadConstraints`、preStop、Pod Readiness Gate、ALB slow start）と **共存して成立** する。置き換えない |

## 1.3 非ゴール

| # | 非ゴール | 理由 |
|---|----------|------|
| N1 | Karpenter Consolidation / Drift の置き換え | Karpenter の自律最適化は引き続き有効。本コントローラは Expiration 経路のみ肩代わり |
| N2 | Spot Interruption への対応 | 2 分の hard limit がある AWS インフラ側イベント。[AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) を使う |
| N3 | アプリケーションの warm-up 責務 | JVM のウォームアップ、コネクションプール初期化等は `readinessProbe` / `readinessGate` / ALB slow start の領分。本コントローラは **ノード** の orchestration を提供し、アプリは自分の readiness を自分で表現する |
| N4 | `expireAfter == 0` 化、21 日 hard cap の解除 | Auto Mode の hard cap は回避不能。`expireAfter` はコントローラ停止時の **バックストップ** として意図的に残す |
| N5 | OS パッチ起因のノード再起動 | スコープ外。[kured](https://github.com/kubereboot/kured) を使う |

## 1.4 用語

| 用語 | 定義 |
|------|------|
| **NodeClaim** | Karpenter v1 CRD。実インスタンス（EC2 等）に 1:1 対応 |
| **surge** | 旧ノードを抜く前に新ノードを立ち上げて `Ready` 化する make-before-break 戦略 |
| **placeholder（Pod）** | NodePool 所有の代替キャパシティを誘発するためにコントローラが作成する低優先度の "pause" Pod — Karpenter が新ノードを起動する（または scheduler が既存の空きキャパへ bin-pack する）ことでこれを収容する。単独の `NodeClaim` は作らない（§3.3）|
| **メンテナンスウィンドウ** | コントローラがローテーションを **開始してよい** 曜日・時間帯の **和集合**（1 つ以上）。ウィンドウ終端を跨いだ進行中のローテーションは完遂させる |
| **freeze（凍結）** | `noderotation.io/freeze` annotation で設定する NodePool 単位のローテーション保留。開始 *のみ* をゲートするウィンドウとは異なり、進行中の `pending` ローテーションも期限切れまで保留する（§3.1）|
| **age 閾値** | `creationTimestamp` からの経過時間がこの値を超えた `NodeClaim` を候補とする値。スケジュールと目標ローテーション回数（`minRotationChances`）から NodePool ごとに **導出**、直接指定しない（§3.2）。実際のノード単位トリガは各 NodeClaim 自身の `spec.expireAfter` デッドラインに基づき、`ageThreshold` はその age 換算の代表値（§3.2）|
| **candidate（候補）** | 選定条件（§3.2）をすべて満たし、ローテーション対象として適格な `NodeClaim` |
| **governing policy（統治ポリシー）** | ある NodePool に対しセレクタの specificity で勝ち、そのスケジュール・`minRotationChances`・`surge` 設定を供給する `RotationPolicy`（§5.4）|
| **バックストップ** | コントローラが停止しても Karpenter 標準の `expireAfter`（Forceful Expiration）が最終的にノードを置換する安全装置。意図的に残す |
| **voluntary / forceful 経路** | Karpenter の 2 つの disruption 分類（§1.1）。**voluntary 経路**（Consolidation、Drift、およびコントローラ自身の `NodeClaim` delete）は PDB を尊重する。**forceful 経路**（`expireAfter`、Spot Interruption）は PDB を `terminationGracePeriod` までしか尊重しない。本コントローラは常に voluntary 経路を通す |
| **forceful fallback** | opt-in のウィンドウ有界モード（`surge.forcefulFallback`、既定 off。ADR-0001）。surge **なしで** 失効の迫った `NodeClaim` をウィンドウ内に削除する — voluntary 経路のままなので PDB は適用される（§3.3）|

**記号** — §3〜§5 で頻出。完全な導出と「ノード単位か NodePool テンプレートか」の権威ある区別（§3.2 の表の **取得元** 列）は §3.2 を参照。

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
| `D` | メンテナンスウィンドウ長 — 単一のウィンドウ機会の長さ（§3.2 レイヤ 2）|
| `gap` | 連続するウィンドウ機会の間でウィンドウ和集合が**閉じている**最短の区間（§3.2 レイヤ 2）|
| `m` | `surge.maxUnavailable` — NodePool ごとの同時ローテーション数。v1 は `1` 固定（§3.2 レイヤ 2）|
| `C` | ウィンドウ機会あたりの処理容量 — 1 回のウィンドウ機会で開始できるローテーション数。`C = m · ceil(D / (t_rot + cooldownAfter))`（§3.2 レイヤ 2）|
| `N` | NodePool のノード台数 — レイヤ 2 のスループット検証でのみ使い、ノード単位の導出には用いない（§3.2）|

> `buffer`・`cooldownAfter`・`readyTimeout` は導出記号ではなく設定フィールド（§5.4）であり、上記の `t_rot` および `C` の式に効く。

## 1.5 Karpenter エコシステムでの位置付け

本コントローラは Karpenter 公式の設計方針と整合している。Karpenter 本体の挙動を変えるのではなく、その **上のレイヤ** で動作する。

### Karpenter 本体に同等機能が組み込まれない理由

[`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) は Expiration を Forceful に保つ判断を記録しており、graceful な expiration を求めるユーザに対し以下 3 オプションを提示している。

1. （upstream 推奨）Expiration は Forceful のまま
2. NodePool ごとに `expirationPolicy: Forceful | Graceful` を追加
3. **「運用者が独自に graceful rotation を実装する」**

本コントローラは Option 3 に該当する。upstream がユーザ側実装を妥当な解として明示的に位置付けている以上、同等機能が Karpenter 本体に取り込まれて本プロジェクトが不要になるリスクは低い。

### Disruption Budgets では不十分な理由

`NodePool.spec.disruption.budgets` は `schedule + duration` をサポートし、表面的にはメンテナンスウィンドウに見える。実際には 2 つの構造的制約 — 下表の 2 つの ✗ 行 — がある（1 行目の要件（△）は不格好な方法でしか実現できない）。

| 要件 | Karpenter 単体で実現可能か |
|------|----------------------------|
| ウィンドウ内のみ disruption を許可、ウィンドウ外は拒否 | △ ブラックリスト方式のみ可能（ウィンドウ外を複数 budget で `nodes: "0"` に設定する。重複する budget は **最小値が採用される** 仕様のため）— [Discussion #1079](https://github.com/kubernetes-sigs/karpenter/discussions/1079) 参照 |
| 上記を **Expiration** にも適用 | ✗ Budgets は Drift / Consolidation のみが対象、**Expiration には適用されない** |
| Expiration 時に surge 置換 | ✗ Expiration は Forceful で代替ノードの事前起動なし |

本コントローラは下 2 行を埋め、1 行目の実現も大幅に簡単にする。

### 隣接プロジェクト

| プロジェクト | スコープ | 重複度 |
|------------|---------|--------|
| Karpenter NodePool Disruption Budgets | Drift / Consolidation のレート制御 | 補完関係、Expiration には適用外 |
| [kured](https://github.com/kubereboot/kured) | OS パッチ起因のノード再起動 | なし。NodeClaim を操作しない |
| [AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) | Spot 中断 / Scheduled Event | なし。トリガが異なる |
| [descheduler](https://github.com/kubernetes-sigs/descheduler) | Pod 再配置 | なし。ノードを操作しない |
| EKS Node Auto Repair (AWS マネージド) | 故障 Node 置換 | なし。期限切れ駆動ではない |
