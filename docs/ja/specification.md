# node-rotation-controller — 仕様書

Karpenter 配下の Node を、設定可能なメンテナンスウィンドウ内で make-before-break（surge）型に先回り置換し、Karpenter の Forceful な `expireAfter` 発火を実質的に起こさないようにする Kubernetes コントローラの機能仕様。

英語原文: [docs/specification.md](../specification.md)

---

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
| N3 | アプリケーションの warm-up 責務 | JVM 起動、コネクションプール初期化等は `readinessProbe` / `readinessGate` / ALB slow start の領分。本コントローラは **Node** の orchestration を提供し、アプリは自分の readiness を自分で表現する。v3 で合成リクエスト Job を差し込む余地は残す |
| N4 | `expireAfter == 0` 化、21 日 hard cap の解除 | Auto Mode の hard cap は回避不能。`expireAfter` はコントローラ停止時の **バックストップ** として意図的に残す |
| N5 | OS パッチ起因の Node 再起動 | スコープ外。[kured](https://github.com/kubereboot/kured) を使う |

## 1.4 用語

| 用語 | 定義 |
|------|------|
| **NodeClaim** | Karpenter v1 CRD。実インスタンス（EC2 等）に 1:1 対応 |
| **surge** | 旧 Node を抜く前に新 Node を立ち上げて `Ready` 化する make-before-break 戦略 |
| **メンテナンスウィンドウ** | コントローラが置換を **開始してよい** 曜日・時間帯の **和集合**（1 つ以上）。窓終端を跨いだ進行中の置換は完遂させる |
| **age 閾値** | `creationTimestamp` からの経過時間がこの値を超えた `NodeClaim` を候補とする値。スケジュールと目標ローテーション回数（`minRotationChances`）から NodePool ごとに **導出**、直接指定しない（§3.2）|
| **バックストップ** | コントローラが停止しても Karpenter 標準の `expireAfter`（Forceful Expiration）が最終的に Node を置換する安全装置。意図的に残す |

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

## 2.1 スコープと互換性

### サポート対象環境

| 環境 | 状態 |
|------|------|
| EKS Auto Mode | 主対象（21 日 hard cap が最大の動機） |
| EKS 上の self-managed Karpenter v1+ | サポート |
| その他の CNCF 系（AKS NAP 等） | best-effort。CRD API は同じだが運用セマンティクスは差異あり |

### Karpenter API バージョン

`karpenter.sh/v1` 必須。`v1beta1`、`v1alpha5` は非サポート。

## 2.2 既存メカニズムとの関係

| メカニズム | 関係 |
|-----------|------|
| Karpenter Consolidation / Drift | **共存**。本コントローラは Expiration 経路のみを肩代わり。Consolidation / Drift の voluntary 置換は Karpenter にそのまま委ねる |
| NodePool `expireAfter` | **共存**（バックストップ）。`expireAfter > age 閾値` で運用することを推奨 |
| NodePool `terminationGracePeriod` | **依存**。NodeClaim delete 後の drain は Karpenter の termination controller に委ね、PDB を尊重しつつ `terminationGracePeriod` でキャップされる経路を共用 |
| PodDisruptionBudget | **依存**。NodeClaim delete 後の drain は voluntary 経路で PDB が厳密に効く |
| `topologySpreadConstraints` | **依存**。surge しても旧 Node 上の全 Pod は drain 時に同時に消える。引き続き分散は必須 |

---

## 3.1 メンテナンスウィンドウ

```yaml
maintenanceWindows:        # リスト。実効ウィンドウは全エントリの和集合
  - timezone: Asia/Tokyo   # IANA tz データベース名
    days: [Wed, Sat]       # ISO 曜日名: Mon/Tue/Wed/Thu/Fri/Sat/Sun
    start: "02:00"
    end:   "06:00"
```

**セマンティクス**:

- Reconciler は常時稼働。窓判定は 1 分間隔の Ticker で評価
- `maintenanceWindows` は **リスト**。実効メンテナンスウィンドウは全エントリの **和集合**。平日枠＋週末枠のように組み合わせて置換頻度を上げられる
- （和集合）窓外は reconcile loop が no-op
- 窓は **置換開始** のみを制御。窓終端を跨いだ進行中の置換は完遂させる（中断のほうが危険）
- 個別 `NodePool` に annotation（例: `noderotation.io/freeze=<RFC3339>`）を付けると、その時刻まで置換を **凍結** できる（業務クリティカル期間用途）

この和集合から **最悪ウィンドウ周期 `P`**（連続するウィンドウ開始間隔の最大値）が定まり、§3.2 の `ageThreshold` 導出に渡される。例: 和集合 `{Wed 02:00, Sat 02:00}` のギャップは `Wed→Sat = 3d`, `Sat→Wed = 4d` なので `P = 4d`。

> **注（DST）。** `P` は繰り返す **壁時計** サイクル上で計算する。夏時間切替で個々のギャップが ±1h ずれ得るが、v1 はこれを既知の近似として特別扱いしない。

## 3.2 候補選定

`NodeClaim` 単位で以下を **すべて** 満たしたものを候補とする。

| 条件 | 既定値 | 備考 |
|------|--------|------|
| `now() > deadline − leadTime`（`deadline = NodeClaim.metadata.creationTimestamp + NodeClaim.spec.expireAfter`、`leadTime = K·P + t_rot`）| `leadTime` は **導出値**（下記）。直接指定しない | 各 NodeClaim **自身** の `spec.expireAfter`（権威ある期限）を起点とし、NodePool テンプレートは見ない。導出される `ageThreshold` はこのトリガの age 等価。既定 `auto`、明示上書きも可だが検証は走る |
| 設定された selector に合致する `NodePool` 配下 | 必須 | `nodepoolSelectors` でマッチした `NodePool` が対象 |
| `status.conditions[Ready] == True` | 必須 | NotReady な NodeClaim はスキップ — 既に不健全なノードは EKS Node Auto Repair と `expireAfter` バックストップに委ね、本コントローラでは置換しない（コントローラが面倒を見るのは surge で自ら作成したノードの健全性のみ）|
| `metadata.annotations["noderotation.io/state"]` が空、または `retryBackoff` 経過後の `failed` | 必須 | `pending`/`draining` は進行中で §5.2 ステップ 1 が駆動し再選定しない。`failed` は backoff 後に再試行（§5.3）|

複数該当時は age 降順（古い順）に並べる。

### 欲しいローテーション回数から `ageThreshold` を導出する

`ageThreshold` を手で調整するのは誤りやすく（緩すぎると窓到来前に Forceful Expiration が発火する）、コントローラはスケジュールと目標ローテーション回数から **NodePool ごとに導出** する。

> **これが中心的なレースである。** Forceful Expiration はメンテナンスウィンドウや PDB に関係なく各ノードの `deadline` で発火するため、コントローラは毎サイクル、その時刻 **より前** に graceful な surge ローテーションを *完了* させなければならない。候補選定はまさにこの先読みである — ノードは `deadline` が now から `leadTime = K·P + t_rot` 以内に入った時点で選定される（= `age > ageThreshold`）。`leadTime` を左から読むと、窓を *捕まえる* ための `K` 回の最悪ウィンドウ周期（`K·P`）＋ その窓内で *完了する* ための 1 ノードの所要時間（`t_rot`）であり、`expireAfter` 発火前に少なくとも `K` 回、完了余裕のあるウィンドウを保証する。`K ≥ 2` なら窓を逃す/遅れてもリトライが手元に残る。下の導出はこれを満たす *最大の* 閾値を選び、安全な範囲で可能な限り遅くローテーションする。

**記号**

| 記号 | 意味 | 取得元 |
|------|------|--------|
| `E` | `expireAfter` | ノード単位: **`NodeClaim.spec.expireAfter`**（権威ある値。NodeClaim の `creationTimestamp` 起点）。NodePool `spec.template.spec.expireAfter` は per-NodePool の検証/ログ用 **代表値** に限定 — 既存 NodeClaim には伝播しない（下の注参照）|
| `tGP` | `terminationGracePeriod` | ノード単位: `NodeClaim.spec.terminationGracePeriod`。NodePool `spec.template.spec.terminationGracePeriod` は代表値 |
| `P` | 最悪ウィンドウ周期（連続するウィンドウ機会の最大ギャップ） | `maintenanceWindows` の和集合から導出（§3.1） |
| `t_rot` | 1 ノードのローテーション所要上限 = `readyTimeout + tGP + buffer`（**`cooldownAfter` は含めない** — cooldown 前にノードは drain 済み。下のマージン注を参照）| 設定 + NodePool から導出 |
| `K` | 欲しい保証ローテーション回数（`minRotationChances`） | ユーザ指定。下限 **1** |

**導出** — `[ageThreshold, E)` の中に `K` 回の完了可能なチャンスを保証する *最大の* 閾値を採用し、可能な限り遅くローテーションする（churn と surge コストを最小化）:

```
ageThreshold (A) = E − (K·P + t_rot)
```

これは、利用可能区間 `[A, E − t_rot]` が最悪位相でも `K` 回のウィンドウ機会を含み（`floor(((E − t_rot) − A) / P) ≥ K`）、各回が `E` 前に完了する `t_rot` の余裕を持つため成立する。

> **マージン。** この下界は **タイト** で、最悪位相での保証はちょうど `K`（`floor(K·P / P) = K`）であり、組み込みの余裕はない。したがって安全マージンは `K` 自体で取るしかなく、1 回窓を逃す/遅れてもリトライが残るよう `K ≥ 2` を推奨する。`cooldownAfter` はウィンドウ内で連続するローテーション *間* の整定休止であり、1 ノードの完了時間（`t_rot`、上で除外したのはこのため）には **含まれない** が、スループット（下のレイヤ2）には **効く**。

> **権威ある期限の取得元。** *ノード単位* のトリガを駆動する期限は、各 **`NodeClaim.spec.expireAfter`**（その NodeClaim の `creationTimestamp` 起点）から読む — NodePool の `spec.template.spec.expireAfter` ではない。Karpenter は生成時に `expireAfter` を NodeClaim へ焼き込み、Forceful Expiration は `creationTimestamp + NodeClaim.spec.expireAfter` で発火する。NodePool テンプレートを後から編集しても既存 NodeClaim には **伝播せず**、drift による置換を誘発するだけである。よってコントローラは `leadTime` を各ノード自身の `deadline` を起点に当て、テンプレート `E` は per-NodePool の起動時検証およびログ/導出 `ageThreshold`（§4.2）の **代表値** としてのみ用いる。あるノード自身の `spec.expireAfter` がテンプレートと異なる場合（drift 進行中やテンプレート変更後など）、そのトリガは自身の値に従う — 恒等式 `now() > deadline − leadTime ⟺ age > ageThreshold` が厳密に成立するのは両者が一致するときのみである。

**検証**（レイヤ1 — スケジュール充足性）

| 条件 | 判定 |
|------|------|
| `K < 1` | **fatal** — 不正な設定 |
| `K < 2`（= `K = 1`） | **warn** — 1 回でも窓を逃す/失敗すると Forceful Expiration までリトライ余地なし |
| `A ≤ 0`（= `E ≤ K·P + t_rot`。その構成では `K` 回すら保証不能） | **fatal** — `E` を上げる（Auto Mode は `21d − tGP` まで）、ウィンドウ機会を増やして `P` を縮める、または `K` を下げる |
| `0 < A < P`（ノードが 1 ウィンドウ周期分も生きないうちに候補化する） | **warn** — 過度に積極的: ノードが非常に若くして置換され churn / surge コストが最大化する。`E` を上げるか `K` を下げる |
| Auto Mode かつ `E + tGP > 21d` | **warn** — ハードキャップ違反 |
| NodePool `spec.limits` のリソース予算（`{cpu, memory, …}`）に surge ノードの requests を収める余地がない（ノード 1 台分の余裕が枯渇） | **warn** — 空き予算がないと surge は着地できない。ノード 1 台分のリソースの余裕を残すよう `limits` を上げる。コントローラはローテーション開始時にも再確認する（§5.2）|

**検証**（レイヤ2 — スループット） — 導出とは独立で、**警告のみ**・`A` は変えない。ウィンドウ内のローテーションは直列で `cooldownAfter` を挟むため（NodePool ごとの start ゲートとして §5.2 ステップ2 で enforce）、尺 `D` の各ウィンドウ機会で `C = m · floor(D / (t_rot + cooldownAfter))` 台を捌ける（`m = surge.maxUnavailable`、v1 は `1` 固定）。候補到来率が容量を超える（`C < N · P / A`、`N` は NodePool 台数）と候補が滞留し一部が Forceful Expiration し得る:

- **warn**: ウィンドウ拡張（`D` 増）、機会追加（`P` 縮小）、または `maxUnavailable` 引き上げ（将来バージョン用）。

> **計算例。** Auto Mode, `E = 14d`, `tGP = 1h`, 和集合 `{Wed, Sat} 02:00–06:00` → `P = 4d`, `t_rot ≈ 1.5h`（`readyTimeout 15m + tGP 1h + buffer`）, `K = 2`。すると `A = 14d − (2·4d + 1.5h) ≈ 5.9d`: ノードは約 5.9d で候補化し、14d 前に 2 回の窓が保証される。スループット `C = floor(4h / (1.5h + 10m)) = 2`/機会。
>
> **週次単独**窓 `{Sat}` は `P = 7d` なので `A = 14d − (2·7d) = 0` → **fatal**: 週次窓では `E = 14d` で 2 回を保証できない。これがまさに固定 `expireAfter − 4d` 既定が安全でなかった理由であり、導出がそれを表面化し、`E` を上げる（~`20d` で `A ≈ 6d`）か曜日を増やすよう運用者に促す。

導出された `A`、保証回数 `G`、`P` は NodePool ごとに起動時ログとメトリクス（§4.2）で露出する。auto 導出では構成上 `G = K` となる。`G` を別途持つのは、明示的な `ageThreshold` 上書き時に `G` を **その上書き値から再計算** し、要求した `K` から乖離していることを観測できるようにするためである。

## 3.3 surge シーケンス（v1）

1 reconcile で 1 ノードを処理。v1 は **NodePool ごとに直列固定（`surge.maxUnavailable = 1`）** で blast radius を最小化。異なる NodePool 同士は並行してローテーションし得る。

### standalone ノードではなく *同一 NodePool* に surge する

代替ノードは、置換対象ノードと **同じ NodePool** に属さなければならない。したがって本コントローラは standalone な `NodeClaim` を作成して置換することは **しない**。（standalone NodeClaim は実際にプロビジョニング *可能* だが（§7.2 参照）、できたノードは NodePool owner を持たず、その Pod は NodePool 会計・expiry・drift・disruption budget の外にある「管理されないノード」に載り続ける。`api` / `batch` のように NodePool を意図的に分離している環境では受け入れられない。）

代わりに、一時的な **placeholder Pod** — コントローラが **直接作成・管理する**（あえて Deployment/ReplicaSet/Job を使わない）単一の低優先度 "pause" Pod — を作成し、Karpenter にその NodePool 内へ新ノードを誘発させる。そのスケジューリング要件は **置換対象ノード** から複製する — 最重要なのは AZ（`topology.kubernetes.io/zone`）で、加えて再スケジュールされる Pod が依存する arch / instance-type / capacity-type 制約も継承する（下の *ステートフル／ゾーン制約のワークロード* 参照）。resource requests は **置換対象ノードに現在スケジュールされている*再スケジュール対象*の Pod 群の requests 合計**（drain 後に再着地すべきワークロード）に設定する。この合計からは、Karpenter が新キャパに再収容する必要のない Pod を**除外**する: **DaemonSet** Pod（kube-proxy, CNI, CSI, ログ収集等）— Karpenter は*どの*新ノードにも DaemonSet オーバーヘッドを既に加算するため、ここで数えると**二重計上**になり過大プロビジョンになる — に加え、mirror/static Pod、完了済み（`Succeeded`/`Failed`）Pod、そして他所へ再着地できない当該ノード固定の Pod（例: hostname affinity）を除く。これにより Karpenter は既存キャパへ bin-pack せず、同一ゾーンに、そのワークロードを収容できる大きさの新ノードを起動せざるを得なくなる。Karpenter はその NodePool 内に新ノードをプロビジョニングする。旧ノードの drain 後に placeholder を削除し、新ノードは NodePool の通常メンバーとして残る。

placeholder は **bare Pod**（どのコントローラにも管理されない）かつ低優先度のため、再スケジュールされたワークロード Pod がその領域を必要とすると scheduler が **preempt** し、placeholder は **再作成なしで単に削除** される。（Deployment/Job 配下の Pod なら再生成されて再 pending し、余計なノード churn を生む — bare な、コントローラ管理の Pod を使う理由はまさにこれ。）その唯一の役割は、drain が実 Pod を着地させるまで 1 ノード分の capacity を確保しておくことである。

**placeholder の優先度。** placeholder は **専用の `PriorityClass`**（`globalDefault: false`、通常ワークロードの `0` より低く、システム重要クラスよりはるかに低い**負値**）かつ `preemptionPolicy: Never` で動かす。これにより placeholder を意図的な preempt *被害者*にする: 再着地するワークロード（優先度 `≥ 0`）が上記のとおり placeholder を preempt する一方、placeholder 自身は実ワークロードやシステム重要 Pod を **決して preempt せず**、pending 中も既存 Pod を退去させて bin-pack するのではなく Karpenter が新ノードを足すのを待つだけ — *新ノードのみ* の意図を補強する。**注意 — preempt は再着地ワークロードの専有ではない。** 負の優先度は placeholder を*最大限* preempt されやすくするため、優先度値だけでは **無関係な高優先度の pending Pod** が surge の途中（placeholder が空間を確保している当のワークロードが drain でまだ生まれてもいない時点）で placeholder を preempt するのを止められない。そうなった場合、状態機械は placeholder の喪失を検知して再作成する（上の §3.3 の *placeholder 不在 → 再作成* 分岐）。このループは **無限ではなく有界** である: `pending` フェーズ全体が `readyTimeout` で打ち切られ、その後ローテーションは **ロールバック** して `expireAfter` ベースラインへ縮退する（§3.3 *ロールバック*）— よって執拗な敵対的 preempt のシナリオでさえ、永遠に churn せずクリーンな失敗へと自己終端する。

### surge 中の Consolidation レース対策

新旧ノードが共存する間、Karpenter の Consolidation / Drift がコントローラとレースし得る:

- **新** ノードは一時的に低利用率のため「empty/underutilized」と判定され即座に consolidate されうる
- **旧** ノードはコントローラの orchestration 完了前に consolidate / drift されたり、意図した順序より先に削除対象に選ばれうる

両方を防ぐため、surge の間 **新旧両ノード** に `karpenter.sh/do-not-disrupt` を付与する。Karpenter の文書化された挙動上、この annotation がブロックするのは **voluntary disruption（Consolidation, Drift, Emptiness）のみ** であり、*forceful* な手法 — **Forceful Expiration（`expireAfter`）**、Interruption、Node Repair — からはノードを除外**しない**。（Karpenter の `nodeclaim/expiration` コントローラで確認済み: 期限切れ NodeClaim を `creationTimestamp + expireAfter` 到達と同時に annotation を一切参照せず削除する。ノードレベルの `do-not-disrupt` 判定は voluntary な候補選定経路にのみ存在する。）したがって Forceful Expiration とのレースに勝つのはこの annotation の役割では **ない** — それは §3.2 の `leadTime` サイジングが構造的に担保し、各ノードを `deadline` **より前** に graceful な surge を完了できるだけ早く選定する。ここでの annotation の役割はより限定的だが依然不可欠である: Karpenter 自身のオプティマイザが、組み立て途中の surge ペアをコントローラの背後で consolidate / drift してしまうのを止める。コントローラ自身の明示的な旧 NodeClaim `delete` は、annotation とは無関係に voluntary（termination controller）経路で drain を進める。annotation は最後に除去し、新ノードを通常管理へ戻す。（**残存リスク:** annotation は旧ノードの寿命を延ばさないため、surge が置換ノードの `Ready` 化を待っている最中に旧ノードの `deadline` が到来すると、Karpenter は予定どおり旧ノードを force-expire し、再スケジュール対象の Pod をまだ存在しないキャパシティへ着地させることになる。これは `leadTime` が tight なケース／最終ウィンドウの縁ケースであり、防止されるのではなくネイティブのベースラインへ縮退する — §3.5 参照。）

以下の図は 1 回のローテーションの **論理** シーケンスである。単一のブロッキング呼び出しとしては **実行されない**。コントローラはこれを **非ブロッキングな requeue 駆動の状態機械**（§5.2）として実装し、進捗を旧 NodeClaim の `noderotation.io/state` annotation に保持する（§5.3）。各 `wait_*` ステップは *後続の reconcile で再評価される状態* であって、worker をブロックする goroutine ではない。`[state: …]` タグが各ステップをその annotation 値に対応づける。

```
ROTATION（論理シーケンス。各ステップは別々の reconcile）
  │
  ├─ candidate を選定（退役させる旧ノード）              [state: (none) → pending]
  │     annotate(candidate, state=pending, started-at=now)
  │     annotate(candidate.node, do-not-disrupt=true)   // 旧ノードを voluntary disruption から凍結
  │     placeholder := create_placeholder_workload(
  │         nodepool     = candidate.nodepool,          // 同一 NodePool
  │         requirements = match(candidate.node, surge.matchNodeRequirements), // 同一 zone/arch/...（zonal PV 再バインド）
  │         requests     = sum(candidate 上の再スケジュール対象 Pod の requests), // DaemonSet / mirror / 完了済 / ノード固定は除外
  │         annotations  = {do-not-disrupt: true},
  │         priority     = placeholderPriorityClass,        // 専用・負値; preemptionPolicy=Never
  │         labels       = {surge-for: candidate.name},
  │     )                                               // Karpenter が同一 AZ に NodePool-owned な新ノードを誘発
  │
  ├─ surge_ready?（placeholder が started-at 以降に作成された NEW ノードへスケジュール → そのノードが Ready）  [state: pending]
  │     yes → annotate(new_node, do-not-disrupt=true)
  │           annotate(candidate, state=draining)
  │           delete(candidate)                         // 明示削除。do-not-disrupt にブロックされない
  │     no かつ placeholder 不在（喪失 / state 書込後にクラッシュ）→
  │           recreate_placeholder(candidate); requeue(30s)
  │     no かつ elapsed(started-at) > readyTimeout(15m) → FAIL:
  │           delete(placeholder); unfreeze(candidate.node)
  │           annotate(candidate, state=failed, failed-at=now); alert
  │     else → requeue(30s)                             // まだ待機。非ブロッキング
  │
  ├─ candidate_gone?（旧 NodeClaim が finalize 消滅）              [state: draining]
  │     // termination controller が PDB を尊重して graceful drain（上限 terminationGracePeriod）
  │     yes → delete(placeholder)                       // pause Pod を解放。
  │           unfreeze(new_node)                        //   そのノードは NodePool capacity として残る
  │           annotate(nodepool, last-rotation-at=now)  // cooldown のアンカー。旧 NodeClaim（ローテーション
  │           emit_metrics(success)                     //   ごとの state の担い手）はここで消えるため NodePool に置く
  │     else → requeue(30s)                             // terminationGracePeriod + buffer で上限
  │
  └─ cooldown はここの requeue ではなく START ゲート（§5.2 ステップ2）で enforce する:
        この NodePool の次ローテーションは
        now − nodepool/last-rotation-at ≥ cooldownAfter まで待つ。              [state: (削除により消滅)]
```

> placeholder の唯一の役割は、drain の前に NodePool へちょうど 1 ノード分の capacity を先出しすること（make-before-break）。requests は **置換対象ノードの*再スケジュール対象* Pod requests 合計**（DaemonSet・mirror・完了済・ノード固定の Pod を除外 — 上の §3.3 参照）にサイズするので、Karpenter は収容のため *新* ノードを起動せざるを得ない。第 2 のガードとして、`surge_ready` 判定は placeholder が実際に `creationTimestamp` が `started-at` *より後* のノード（= 既存キャパへの bin-pack ではなく真に新規）に着地したことも要求する — これにより、偽って満たされた surge が「実容量を足さないまま旧ノードを削除する」ことは決して起きない。これら requests は surge ノードのリソースフットプリントを定義するので、`surge_headroom` 事前チェック（§5.2）が NodePool の残り `spec.limits` リソース予算と突き合わせる対象でもある。requests の正確な余白付け**と除外フィルタの厳密化**（DaemonSet / mirror / 完了済 / ノード固定）は PoC で確定する。

### Pod レベルの挙動 — make-before-break はノードレベルのみ

本設計の make-before-break は **ノード** レベルであり、Pod レベルではない。コントローラは Pod の rolling update を **行わない**。すなわち、旧 Pod を終了させる前に surge ノードへ新 Pod を先に立てることはしない。surge ノードは **空の capacity** として追加される。

旧 `NodeClaim` を削除すると、Karpenter の termination controller が **Eviction API** 経由で旧ノードを drain する（PDB 尊重）。evict された各 Pod は削除され、その所有ワークロードのコントローラ（Deployment/ReplicaSet/StatefulSet）が **置き換え Pod** を生成し、scheduler が空きキャパ（典型的には surge ノード）へ配置する。これは本質的に **evict → 再スケジュール** であり、置き換え Pod が旧 Pod の終了前に `Ready` になる保証は *ない*（§4.1 参照）。

したがって surge ノードの役割は、Pod の順序付けではなく、**着地先を事前に用意して** PDB ゲートされた eviction が長い pending を伴わずに進めるようにすることである。Pod レベルの安全性はワークロードの **PodDisruptionBudget** と余剰レプリカに委譲される:

- 厳格な PDB（例: `minAvailable` を希望レプリカ数と等しく設定）の場合、Eviction API は置き換え Pod が `Ready` になるまで次の eviction をブロックする。surge ノードが置き換え Pod のスケジュール・`Ready` 化のための capacity を供給するため、drain は実質的に Pod レベルの make-before-break となる。
- PDB が緩い／無い場合、eviction は一括で進み `readyReplicas` が下がる（§4.1）。

要するに、コントローラが保証するのはノードレベルの surge であり、**Pod レベルの make-before-break は PDB + 余剰レプリカ（surge ノードの capacity がそれを可能にする）によって達成されるのであって、コントローラ自身が行うものではない**（G4 と整合）。

### ステートフル／ゾーン制約のワークロード — 置換ノードの要件一致

surge は容量を **足すだけ** で Pod を新ノードに固定しない（上記）ため、再スケジュールされた Pod は scheduler が配置できる場所に着地する。**zonal** な PersistentVolume にバインドした Pod — EBS `gp3`/`io2`、あるいは PV が `topology.kubernetes.io/zone` の `nodeAffinity` を持つ任意のボリューム — は、そのボリュームと **同じ AZ** のノードにしか再スケジュールできない。surge ノードが *別* の AZ にプロビジョニングされると、その Pod は着地先を失い、旧ノードの drain 後に `Pending` のまま残る — make-before-break を最も必要とするステートフルワークロードでこそ崩れる。

そのため placeholder は、単なる NodePool のラベルではなく **置換対象ノードのスケジューリング要件** を複製する。**どの** 要件を複製するかは `surge.matchNodeRequirements`（§6）で **設定可能** である。列挙した各キーを置換対象ノードからコピーし、placeholder へ **`required`**（ハード `nodeAffinity` / `nodeSelector`、値は候補ノードのもの）または **`preferred`**（ソフト `nodeAffinity`、容量逼迫時は緩和）の制約として付与する。

- 既定の `required` 集合は **`topology.kubernetes.io/zone`**（surge ノードを候補の AZ に固定し既存 EBS ボリュームを再アタッチ可能にする）に加え、arch/capacity の一致のための **`kubernetes.io/arch`** と **`karpenter.sh/capacity-type`**。これは正確な instance-type を固定せずに zonal-PV 再バインドを成立させる — instance-type まで固定するとスケジュール可能プールを不必要に狭め、同一 AZ の容量確保を難しくする。
- より厳密な一致が必要なら運用者がキーを追加する — 例: 正確なタイプ一致のための `node.kubernetes.io/instance-type`（またはファミリ）、あるいはワークロードの `nodeAffinity` / `nodeSelector` / `topologySpreadConstraints` が依存する任意のカスタムノードラベル。逆に厳密さとスケジュール可能性を引き換えにキーを `preferred` へ移すこともできる。

設定したキーは置換対象 `NodeClaim` の `spec.requirements` と置換対象ノードのラベルから読み取り、**NodePool の許容 requirements と交差** させる — この交差により、NodePool テンプレートが許容集合を後から狭めていても placeholder は NodePool 内でスケジュール可能なまま保たれる（さもなければ、今や非許容となった候補ラベルにより placeholder が永久に unschedulable となり、`readyTimeout` でロールバックに陥る）。設定にあって候補ノードに無いキーはスキップする。**検証:** `required` から `topology.kubernetes.io/zone` を外すと **警告** する — surge ノードが別 AZ に着地すると zonal-PV な Pod が宙吊りになりうるため。

これは **同一 AZ の着地先** を再生成するだけで、ストレージを **移動しない**。置き換え Pod がそこにスケジュールされると、CSI ドライバが既存の zonal ボリュームを同一 AZ の新ノードへ再アタッチする。zonal ストレージのクロス AZ 移行はスコープ外であり、surge が行えるものでも行うべきものでもない。（含意: 置換対象ノードの AZ に同一ゾーン置換のためのスケジュール可能なキャパシティが無い場合、surge は完了できず `readyTimeout` 経由でロールバックする（§3.3 *ロールバック*）。旧ノードはそのまま残り、`expireAfter` バックストップが引き続き効く。zonal-PV ワークロードを抱える NodePool は、使用中の各 AZ に surge の余裕を残すべきである — R3 参照。）

### ロールバック挙動

| 失敗 | 動作 |
|------|------|
| 新ノードが timeout 内に `Ready` 化しない | placeholder を削除（Karpenter が不要ノードを reap）、旧ノードの `do-not-disrupt` を除去、旧はそのまま、アラート発火 |
| 新ノードが旧 delete 後に `NotReady` 化 | 旧の drain は止められないため、再スケジュールされた Pod の capacity は Karpenter に修復を委ねる |
| Karpenter API 不達 | スキップ、次の reconcile で再評価 |
| surge 中にコントローラが死亡 | `do-not-disrupt` や placeholder が残り得る。起動時の reconcile sweep で stale な `noderotation.io/*` マーカーと孤児 placeholder を掃除 |

> v1 は 1 サイクル 1 件処理。窓内に全候補を捌けない場合は次の窓へ持ち越し。`expireAfter` バックストップが効くため最悪でも最終的には（Forceful 経路で）置換される。

## 3.4 将来バージョン（v2 / v3）

v1 は意図的にアプリケーション層に踏み込まない。以下は拡張余地として確保する。

| バージョン | 追加 | 投入トリガー |
|-----------|------|-------------|
| v1 | surge + 順次 delete | 初版 |
| v2 | 代替 Node に pin したイメージ pre-pull Job を delete 前に実行 | 代替 Node 上での新 Pod 起動に image pull 遅延が観測された場合 |
| v3 | 合成リクエストで JVM warmup する Job を delete 前に実行 | 置換後の 5xx スパイクが `readinessGate` で吸収できない場合 |

§5.4 の設定スキーマには v2 / v3 用フィールドを v1 時点で開けてある。

## 3.5 バックストップ挙動

コントローラ停止時は以下が順に効く。

1. Karpenter Consolidation / Drift が一部 Node を voluntary 置換し得る（AMI drift 等）
2. NodePool `expireAfter` が期限超過 Node に対し Forceful drain を開始
3. NodePool `terminationGracePeriod` が drain を上限で打ち切る
4. Auto Mode の 21 日 hard cap が最終的な天井

> **重要**: バックストップ 2–4 は Forceful 経路で、PDB は `terminationGracePeriod` までしか守られない。コントローラの長期停止は元のリスクプロファイルを復活させる。surge 中にクラッシュしたコントローラがノードに残した **stale な `karpenter.sh/do-not-disrupt`** はこれを変えない: ノードレベルの `do-not-disrupt` が抑止するのは voluntary disruption（経路 1）のみで、`expireAfter`（経路 2）は抑止**しない**。よって経路 2 は予定どおり発火し、ノードが `deadline` を超えて生き延びることはない。起動時 sweep は stale なマーカーを除去するが、そもそもこのマーカーはノードの寿命を延ばしてはいなかった。

> **graceful な縮退 — 現状より悪くならない。** あらゆる失敗モードは Karpenter 標準の Forceful Expiration（経路 2）へ縮退する。ローテーションが失敗しても、メンテナンスウィンドウを逃しても、コントローラが完全に不在でも、ノードは **本コントローラが無い場合とまったく同じように** `expireAfter` で期限切れ・drain される — §3.3 の残存リスク、すなわち surge 中に `deadline` が到来して置換ノードの `Ready` 化前に旧ノードが force-expire されるケースを含め（forceful だが、コントローラが無い場合と同一）。コントローラはローテーションを *より早く*・*graceful に* するだけであり、設計上、安全網を取り除くことはなく、また — ノードレベルの `do-not-disrupt` が `expireAfter` に何ら影響しない以上 — ノードの寿命を `expireAfter` を超えて延ばすこともできない。したがって **最悪ケースは現状のベースラインと同一** — forceful だが上限あり — であり、これこそが本設計を段階的に安全導入できる理由であり、§3.2 のリードタイムが *失敗時* の安全のためではなく *通常時* にレースを勝つために設計されている理由でもある。

---

## 4.1 Capacity / 可用性

| 観点 | 扱い |
|------|------|
| 置換時の Pod Pending 時間 | surge により 0 に近づく（Karpenter Graceful セマンティクスと同等）|
| `readyReplicas` が希望数を一時的に下回る | Eviction API 経路での構造的制約。surge しても新 Pod は即時 Ready にはならない。緩和はアプリ層（余剰レプリカ + PDB）の責務でスコープ外 |
| 並列 surge 数 | v1 は `surge.maxUnavailable = 1` を **NodePool ごと** に固定（NodePool 内は直列、異なる NodePool 同士は並行 surge し得る）。代替ノードは placeholder Pod 経由で誘発される **NodePool-owned** ノード（§3.3）。ここで `spec.limits` は **リソース予算**（`{cpu, memory, …}`）であって**ノード台数ではない**点に注意 — 実際の前提条件は、placeholder の requests（surge ノードのリソースフットプリント、§3.3）が NodePool の*残り*予算（`limits − 既プロビジョニング分`）に収まることであり、加えて外部の EC2 vCPU クォータも要る。直感的には「ベースライン +1 ノード」だが、これは台数ではなく**リソース**チェックとして enforce される。コントローラは **ローテーション開始前にこの余地を事前確認** し（§5.2）、残り予算がノード 1 台分のリソースを収められなければ警告してスキップする。`maxUnavailable > 1` は将来バージョン用の予約で、その場合ノード `m` 台分の余裕が要る |

## 4.2 観測性

`/metrics` で Prometheus メトリクスを公開。

| メトリクス | 種別 | ラベル | 用途 |
|-----------|------|--------|------|
| `noderotation_candidates` | Gauge | `nodepool` | 候補 NodeClaim 数 |
| `noderotation_in_progress` | Gauge | `nodepool` | 進行中置換数 |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` | 累積完了数。outcome ∈ {success, failure} |
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` | phase ∈ {surge_wait, drain} ごとの所要時間 |
| `noderotation_window_active` | Gauge | — | 窓内か（0/1）|
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` | 凍結期限 Unix 時刻（0 = 凍結なし）|
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` | 導出された `ageThreshold`（§3.2）|
| `noderotation_rotation_chances` | Gauge | `nodepool` | 導出閾値での保証ローテーション回数 `G` |
| `noderotation_window_period_seconds` | Gauge | `nodepool` | スケジュール和集合の最悪ウィンドウ周期 `P` |

> **ラベルに関する注記。** `noderotation_window_period_seconds` は `nodepool` ラベルを持つが、v1 ではメンテナンスウィンドウは**クラスタ共通**（`maintenanceWindows` は単一の和集合、§3.1）— よって `P` は全 NodePool で同一であり、本メトリクスはどの `nodepool` でも同じ値を報告する。このラベルは**将来を見据えたもの**で、per-NodePool 窓が入った際（§7.3 Open Question 2）に系列の形が変わらないよう保持している。対照的に `noderotation_age_threshold_seconds` と `noderotation_rotation_chances` は v1 でも*既に* NodePool ごとに値が変わる — 各 NodePool の代表 `expireAfter`/`terminationGracePeriod` を畳み込むため — ので、その `nodepool` ラベルは現時点で意味を持つ。また `noderotation_window_active` は、ウィンドウ*membership* が v1 ではクラスタ共通の単一の真理であるため、意図的にラベル無しとしている。

推奨アラート:

- `increase(noderotation_completed_total{outcome="failure"}[1h]) > 0`
- `noderotation_candidates > 0` が 2 窓連続で減らない（コントローラが追いついていない）
- `noderotation_window_active == 1` の窓全期間で完了 0 件かつ候補 > 0

## 4.3 RBAC と クラウド権限

### Kubernetes RBAC

```yaml
- apiGroups: ["karpenter.sh"]
  resources: ["nodeclaims"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["karpenter.sh"]
  resources: ["nodepools"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

### クラウド（例: AWS）IAM

v1 ではクラウド API を直接叩かない。すべて Karpenter の `NodeClaim` CRD 経由。

v2（pre-pull）/ v3（warmup）の Job は新 Node 上の Pod として動き、その Node のロールを継承するため、コントローラ自体への追加権限は不要。

## 4.4 コスト

各置換で旧・新 Node が短時間並行課金される。1 置換あたり概算: オンデマンド 1 台 × 10〜20 分。週次で N 件置換なら月額追加は `≈ N × 4 × 時間単価 × 0.25` で、ベース Node コストに対して小さい。ローテーションは *NodePool ごと* には直列だが NodePool *間* では並行する（§3.3）ため、ピークの重なり（= 瞬間的な追加コストのピーク）は同時にローテーション中の NodePool 数に比例する。

---

## 5.1 アーキテクチャ

```
┌─ Cluster (Karpenter v1+) ─────────────────────────────────────┐
│                                                               │
│  ┌─ Namespace: node-rotation-system（設定可能）──────────────┐│
│  │                                                           ││
│  │  Deployment: node-rotation-controller                     ││
│  │    - controller-runtime manager                           ││
│  │    - replicas=2、leader election（active 1）              ││
│  │    - NodeClaim watcher + 1 分 Ticker                      ││
│  │    - /metrics                                             ││
│  │                                                           ││
│  │  ConfigMap: node-rotation-config                          ││
│  │    - maintenanceWindows / minRotationChances / selectors  ││
│  └───────────────────────────────────────────────────────────┘│
│                          │ watch / create / delete            │
│                          ↓                                    │
│  ┌─ NodeClaims (karpenter.sh/v1) ────────────────────────────┐│
│  │   nc-aaa (15d) ← 旧、置換対象                              ││
│  │   nc-bbb (14d) ← 旧                                        ││
│  │   nc-ccc (08d) ← 新（surge）                               ││
│  └───────────────────────────────────────────────────────────┘│
└───────────────────────────────────────────────────────────────┘
```

## 5.2 Reconcile ループ

[controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) で実装。Reconciler は `NodeClaim` を watch しつつ、窓端と凍結解除検知のために定期 Ticker も併用する。

各 `Reconcile` 呼び出しは **非ブロッキングな 1 ステップだけ**を実行して `Requeue` を返す。**ブロッキング待機は存在しない**（15 分の surge 待ちや drain 待ちは `started-at`/削除タイムスタンプに対する *経過時間チェック* で、後続の reconcile で再評価される）。したがって worker は占有されず、進捗はコントローラ再起動を跨いで残る — 全状態は annotation から読み戻す（§5.3）。直列処理は、新規開始の **前に**進行中の置換を処理することで担保する。

```text
Reconcile(req):                              # req は NodeClaim イベントまたは定期 Tick
  if req is Tick:                            # Tick は単一オブジェクトに紐づかない
      for np in in_scope_nodepools():        #   → 選定された全 NodePool に fan out
          reconcile_nodepool(np)
      return Requeue(1m)
  return reconcile_nodepool(nodepool(req.obj))

reconcile_nodepool(np):
  # ── 1. まず進行中の置換を駆動（直列: NodePool ごと高々 1 件）──
  active := claim_in_state(np, {pending, draining})
  if active != nil:
      return advance(active)

  # ── 2. 進行中なし → 窓 / 凍結 / cooldown / surge 余地でゲート ──
  if not in_window(now): return Requeue(1m)
  if frozen(np):         return Requeue(1m)
  cool := cooldownAfter − since_last_rotation(np)        # since_last_rotation = now − np[last-rotation-at]; 未設定なら +∞
  if cool > 0:                               # 連続するローテーション間の整定休止（§3.2 スループットモデル）
      return Requeue(cool)
  if not surge_headroom(np):                 # placeholder requests が (spec.limits − プロビジョニング済) に収まらない: 台数ではなくリソース予算
      warn("insufficient limits headroom; cannot surge"); return Requeue(1m)

  # ── 3. 新規置換を開始（state を書くだけ。ブロックしない）──
  cand := pick_oldest_eligible(np)           # state 空、または retryBackoff 経過後の failed
  if cand == nil: return Requeue(1m)
  annotate(cand, state=pending, started-at=now)
  annotate(cand.node, do-not-disrupt=true)
  create_placeholder(np, cand)               # bare 低優先度 Pod; requests = Σ 置換対象ノードの再スケジュール対象（非 DaemonSet）Pod requests
  return Requeue(30s)

# advance() は進行中 candidate を state に応じて 1 ステップ進める:
advance(cand):
  switch cand.state:
  case pending:                              # surge ノードの Ready 待ち
      if surge_ready(cand):                  # placeholder が NEW ノード（created > started-at）に載り Ready
          annotate(new_node(cand), do-not-disrupt=true)
          annotate(cand, state=draining)
          delete(cand)                       # 明示削除。do-not-disrupt にブロックされない
          return Requeue(30s)
      if placeholder(cand) is missing:       # state 書込後のクラッシュ/リーダー交代、または placeholder 喪失
          create_placeholder(nodepool(cand), cand)
          return Requeue(30s)
      if elapsed(cand.started-at) > readyTimeout:        # 既定 15m
          delete(placeholder(cand)); unfreeze(cand.node)
          annotate(cand, state=failed, failed-at=now); emit_metrics(failure); alert
          return Requeue(1m)
      return Requeue(30s)
  case draining:                             # 旧 NodeClaim の finalize 消滅待ち
      if gone(cand):
          delete(placeholder(cand)); unfreeze(new_node(cand))
          annotate(nodepool(cand), last-rotation-at=now)  # cooldown アンカーを NodePool に — 旧 NodeClaim は消えている
          emit_metrics(success)
          return Requeue(1m)                 # cooldown はステップ2の start ゲートで enforce。削除済み claim への requeue では効かない
      # terminationGracePeriod + buffer で上限。Karpenter が drain を強制
      return Requeue(30s)
```

`pick_oldest_eligible` は `state` が空（新規）か、`failed` かつ `now − failed-at > retryBackoff` の claim を選ぶ。`pending`/`draining` は新規候補として再選定されない（ステップ 1 が駆動）。Leader election は `coordination.k8s.io/Lease` 標準。リーダー交代時、新リーダーは annotation のみから再開する。

ステップ2の `cooldownAfter` ゲートは、成功完了ごとに **NodePool** へ書く `noderotation.io/last-rotation-at` をアンカーにする。旧 NodeClaim には載せない: ローテーションごとの state を担うその object はローテーション完了時に削除されるため、それをキーにした requeue は no-op になる（旧来の「削除済み claim への `Requeue(cooldown=…)`」が実際には休止を enforce せず、次の Tick で即ローテーションを開始し得たのはこのため）。生存する NodePool にアンカーすることで、完了境界とリーダー交代をまたいで休止が持続する。ゲートは NodePool ごとに評価され、NodePool ごと直列のモデルと整合する（別 NodePool は引き続き並行ローテーション可）。

## 5.3 状態モデル

進行状態は Kubernetes オブジェクト（旧 `NodeClaim`、新旧 2 ノード、NodePool、一時的な placeholder Pod）にのみ持つ — **外部データストア不要**。placeholder Pod は短命なランタイムオブジェクトであり永続状態ではない。失われても、起動時 sweep が旧 NodeClaim の `noderotation.io/state` annotation（ローテーション位置の唯一の真実）から状況を再構成する。

| キー | 付与先 | 値 | 用途 |
|------|-------|-----|------|
| `noderotation.io/state` | 旧 NodeClaim | `pending` / `draining` / `failed` | 進行ステート（source of truth）|
| `noderotation.io/started-at` | 旧 NodeClaim | RFC3339 | `readyTimeout` の期限 + observability |
| `noderotation.io/failed-at` | 旧 NodeClaim | RFC3339 | 失敗後の再選定 `retryBackoff` の基点 |
| `noderotation.io/surge-for` | placeholder ワークロード | 旧 NodeClaim の `metadata.name` | 対応関係。placeholder とそのノードの発見・クリーンアップに使用 |
| `karpenter.sh/do-not-disrupt` | 旧ノード + 新ノード | `true` | surge 中の Karpenter **voluntary disruption のみ**（Consolidation/Drift/Emptiness）をブロック — `expireAfter`・Interruption・Node Repair は**ブロックしない**（§3.3）。最後に除去。残存値はノードの寿命を延ばさない（§3.5 参照）|
| `noderotation.io/freeze` | NodePool | RFC3339（凍結期限）| その時刻まで置換抑制 |
| `noderotation.io/last-rotation-at` | NodePool | RFC3339 タイムスタンプ | その NodePool の最後のローテーション完了時刻。`cooldownAfter` の start ゲート（§5.2 ステップ2）のアンカー。ローテーションごとの state を担う旧 NodeClaim が成功時に削除されるため、休止がその削除を越えて持続するよう **NodePool** に置く |

### 状態遷移

旧 NodeClaim の `noderotation.io/state` が §5.2 の状態機械を駆動する。annotation は各副作用の **前に** 書くため、クラッシュ／リーダー交代から復帰可能。

| From | イベント | To | 副作用 |
|------|---------|----|--------|
| *(なし)* | 窓内で選定 | `pending` | 旧ノードに `do-not-disrupt`、placeholder 作成 |
| `pending` | surge ノード `Ready` | `draining` | 新ノードに `do-not-disrupt`、旧 NodeClaim を `delete` |
| `pending` | `readyTimeout` 経過 | `failed` | placeholder 削除、旧ノード unfreeze、アラート |
| `draining` | 旧 NodeClaim 消滅 | *(削除)* | placeholder 削除、新ノード unfreeze、success 計上 |
| `failed` | `retryBackoff` 経過かつ窓内 | `pending` | 再入（annotation リセット）。連続失敗は `expireAfter` backstop が担保 |

`pending`/`draining` は §5.2 ステップ 1 が駆動し、新規候補として再選定されない。これが直列（parallelism=1）処理も担保する。完了したローテーションは annotation の担体である旧 NodeClaim ごと削除されるため状態を残さない。起動時にコントローラは stale な `noderotation.io/*` マーカーと孤児 placeholder を sweep する（§3.3 ロールバック表）。

## 5.4 設定スキーマ

v1 は ConfigMap 単一。複数ポリシー対応が必要になれば CRD 化を検討。

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: node-rotation-config
  namespace: node-rotation-system
data:
  policy.yaml: |
    nodepoolSelectors:
      - matchLabels:
          # NodePool のラベルに合わせて調整
          workload: api

    ageThreshold: auto         # NodePool ごとに導出（§3.2）。明示的な duration 上書きも可だが検証は走る
    minRotationChances: 2      # K。下限 1、2 未満は警告のみ

    maintenanceWindows:        # リスト。実効ウィンドウは全エントリの和集合（§3.1）
      - timezone: Asia/Tokyo
        days: [Wed, Sat]
        start: "02:00"
        end:   "06:00"

    surge:
      maxUnavailable: 1        # v1 は 1 固定（直列）。> 1 は将来バージョン用の予約
      readyTimeout: 15m        # surge ノードがこの時間内に Ready にならなければ state=failed
      cooldownAfter: 10m       # ウィンドウ内で連続するローテーション間の整定休止（t_rot には含めない。スループットに影響、§3.2）
      retryBackoff: 30m        # failed な NodeClaim を再選定するまでの待機（§5.3）
      matchNodeRequirements:   # placeholder が複製する候補ノードの requirement（§3.3「ステートフル／ゾーン制約のワークロード」）
        required:              # ハード nodeAffinity。値は候補ノードからコピー。NodePool の許容 requirements と交差
          - topology.kubernetes.io/zone   # 既定: zonal-PV（EBS）再バインドのため同一 AZ。外すと警告のみ
          - kubernetes.io/arch
          - karpenter.sh/capacity-type
        preferred: []          # ソフト nodeAffinity。容量逼迫時は緩和。例: node.kubernetes.io/instance-type

    prePull:                   # v2（v1 では無効）
      enabled: false
      images: []

    warmup:                    # v3（v1 では無効）
      enabled: false
      jobTemplate: {}
```

---

## 6.1 バージョニングとリリース

- セマンティックバージョニング（`vMAJOR.MINOR.PATCH`）
- v1 スコープと CRD 形が固まるまで pre-1.0（`v0.x.y`）
- API 互換境界: ConfigMap スキーマ（`apiVersion: v1, ConfigMap` で `data.policy.yaml` 文書化）、Prometheus メトリクス名、annotation キー

## 6.2 ロードマップ

| マイルストーン | 内容 |
|---------------|------|
| v0.1（spec）| 本ドキュメント |
| v0.2（skeleton）| プロジェクト構成、controller-runtime bootstrap、leader election、CI |
| v0.3（MVP, v1 surge）| Reconcile + surge + drain + metrics + Helm chart |
| v0.4 | pre-pull（v2 機能）|
| v0.5 | warmup フック（v3 機能）|
| v1.0 | ConfigMap スキーマ安定、production runbook、実 EKS Auto Mode クラスタでのソーク済 |

---

## 7.1 リスク

| # | リスク | 対策 |
|---|--------|------|
| R1 | コントローラ Pod クラッシュ / リーダー喪失 | `replicas=2` + leader election、`expireAfter` バックストップ、failure メトリクスでアラート |
| R2 | 窓内に全候補を捌けない | §3.2 のスループット検証が事前に警告。`noderotation_candidates` が 2 窓連続で減らない場合にアラート。将来バージョンで `maxUnavailable > 1` を検討 |
| R3 | 代替 NodeClaim が立たない（容量不足 / AZ 枯渇 / NodePool の `limits` リソース予算が枯渇）| コントローラは開始前に NodePool `limits` のリソース予算の余地を事前確認し（§5.2）、placeholder の requests が収まらなければ警告。それ以外は Ready タイムアウトでロールバック。NodePool は複数 AZ / 複数インスタンスタイプを許容する設計を維持。**ゾーンの注意点:** surge ノードは zonal-PV 再バインドのため置換対象の AZ に固定される（§3.3 *ステートフル／ゾーン制約のワークロード*）ため、同一 AZ の容量不足を別ゾーンへフォールバックできない — zonal-PV ワークロードを抱える NodePool では各 AZ ごとに surge の余裕を残すこと |
| R4 | 誤設定の PDB で drain がブロックされる | Karpenter の `terminationGracePeriod` で最終的に強制 drain。PDB のレビューはアプリ owner の責務 |
| R5 | 業務クリティカル期間の freeze 付与忘れ | freeze annotation は GitOps 等で宣言的に管理する設計を推奨 |
| R6 | テストクラスタが日次で turnover する場合の検証ギャップ | age 閾値を超えるソーク期間を設けて end-to-end 置換を検証 |

## 7.2 検証済み前提

| 前提 | 状態 | 根拠 |
|------|------|------|
| standalone（NodePool-owned でない）`NodeClaim` が EKS Auto Mode で *プロビジョニング可能* | **検証済** — K8s 1.35 / `karpenter.sh/v1`（2026-05-29）| `nodeClassRef`（マネージド `eks.amazonaws.com/NodeClass`）+ `requirements` のみの NodeClaim が約 30 秒で `Ready`（実 EC2・node 登録）に到達。admission も受理（`--dry-run=server`）、finalizer 駆動の graceful 削除も確認 |

> **なぜ surge 機構ではなく「能力」として記録するか。** standalone NodeClaim の結果は、コントローラが作成した NodeClaim を Karpenter が尊重することを示し、プロジェクトのリスクを下げる。しかし surge 設計（§3.3）はこれを **使わない**: standalone ノードは NodePool に owned されず、Pod が NodePool 会計・expiry・drift・budget の外のノードに載り続け、意図的な NodePool 分離を壊すため。placeholder 方式が成立しない場合の **fallback** として文書化しておく。
>
> **未検証（PoC スコープ）:** *primary* 機構 — 置換対象ノードの*再スケジュール対象*（非 DaemonSet）Pod requests にサイズした placeholder Pod による NodePool-owned ノードの誘発、**voluntary** な Consolidation/Drift を退けるための surge 中の新旧両ノードへの `karpenter.sh/do-not-disrupt` 付与、そして明示的 NodeClaim 削除が旧ノードを voluntary（PDB 尊重）経路で drain すること、加えて preempt された bare placeholder Pod（専用の負値 `PriorityClass`・`preemptionPolicy: Never` で動く）が再 pending せず削除されること、そして surge 途中での無関係な高優先度 preempt が `readyTimeout` → ロールバックで有界であること（§3.3 *placeholder の優先度*）の確認。これらが最初の PoC 項目。（`do-not-disrupt` が `expireAfter` をブロック**しない**ことは未解決の PoC 項目では *なく*、文書化された Karpenter の挙動であり `nodeclaim/expiration` コントローラのソースで確認済みである、§3.3。本設計は expiration レースに勝つために annotation ではなく `leadTime` サイジングに依拠する。）

## 7.3 未決事項

1. 複数 NodePool で別ポリシーが必要になった場合の **CRD ベース移行**
2. **NodePool ごとの窓** vs クラスタ単一窓
3. **祝日対応**（土曜が祝日と重なる場合スキップ）。v1 は意図的に無視
4. v2 の pre-pull イメージ取得方式（Karpenter NodeClass 標準機能 vs 専用 Job）
5. EKS Auto Mode 以外（AKS NAP / GKE）への **マルチクラウド検証**

---

## 参考

- [Karpenter Disruption（公式）](https://karpenter.sh/docs/concepts/disruption/)
- [Karpenter forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) — 「ユーザ側実装」を妥当解として位置付ける根拠
- [Karpenter Discussion #1079 — Schedule for disruption](https://github.com/kubernetes-sigs/karpenter/discussions/1079) — Disruption Budgets の whitelist 限界
- [EKS Auto Mode 公式ドキュメント](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)
- [EKS Auto Mode and maintenance window for "Drifted" nodes (AWS re:Post)](https://repost.aws/articles/ARbff3_8A_R7uiPMpCfjHznw/eks-auto-mode-and-maintenance-window-for-drifted-nodes)
