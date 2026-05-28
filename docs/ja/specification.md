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

Expiration が意図的に Forceful とされているのは、AMI パッチやセキュリティ更新を Budgets / PDB の誤設定で無期限延期させない設計思想に基づく。これは Karpenter 公式 design [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) に明文化されており、同 design は「**運用者が独自に graceful rotation を実装する**」ことも妥当な解の一つとして列挙している。EKS Auto Mode はさらに **21 日 hard cap**（`expireAfter + terminationGracePeriod ≤ 21d`）をユーザが解除できない制約として追加している。

現実的な帰結として、運用中のクラスタでは **PDB を厳しくしても Node は必ず Force drain される瞬間が来る**。Karpenter は drain 開始の **後から** 代替 Node を立ち上げるため、`request==limit` のような厳しい capacity 要件のワークロードではピーク時間と衝突した瞬間に Pod Pending が発生する。

## 1.2 ゴール

| # | ゴール |
|---|--------|
| G1 | age 閾値（既定: `expireAfter - 4d`）に達した `NodeClaim` をメンテナンスウィンドウ内で voluntary 経路で先回り置換し、**Forceful Expiration を実質発火させない** |
| G2 | 代替 `NodeClaim` を先に作成して `Ready=True` を待ってから旧 `NodeClaim` を delete する（**surge / make-before-break**）。Pod Pending の窓を 0 に近づける |
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
| **メンテナンスウィンドウ** | コントローラが置換を **開始してよい** 曜日・時間帯。窓終端を跨いだ進行中の置換は完遂させる |
| **age 閾値** | `creationTimestamp` からの経過時間がこの値を超えた `NodeClaim` を候補とする値。既定 `expireAfter - 4d`、設定可能 |
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
maintenanceWindow:
  timezone: Asia/Tokyo   # IANA tz データベース名
  days: [Sat]            # ISO 曜日名: Mon/Tue/Wed/Thu/Fri/Sat/Sun
  start: "02:00"
  end:   "06:00"
```

**セマンティクス**:

- Reconciler は常時稼働。窓判定は 1 分間隔の Ticker で評価
- 窓外は reconcile loop が no-op
- 窓は **置換開始** のみを制御。窓終端を跨いだ進行中の置換は完遂させる（中断のほうが危険）
- 個別 `NodePool` に annotation（例: `noderotation.io/freeze=<RFC3339>`）を付けると、その時刻まで置換を **凍結** できる（業務クリティカル期間用途）

## 3.2 候補選定

`NodeClaim` 単位で以下を **すべて** 満たしたものを候補とする。

| 条件 | 既定値 | 備考 |
|------|--------|------|
| `now() - metadata.creationTimestamp > ageThreshold` | `ageThreshold = expireAfter - 4d`（例: `expireAfter=14d` なら 10d）| 設定可能。窓頻度との整合に注意（下記 Note 参照） |
| 設定された selector に合致する `NodePool` 配下 | 必須 | `nodepoolSelectors` でマッチした `NodePool` が対象 |
| `status.conditions[Ready] == True` | 必須 | NotReady な NodeClaim はスキップ |
| `metadata.annotations["noderotation.io/state"]` が空または `pending` | 必須 | 進行中・完了済はスキップ |

複数該当時は age 降順（古い順）に並べる。

> **`ageThreshold` 調整に関する Note**
>
> 全 Node が `expireAfter` 到達前に最低 1 回はメンテナンスウィンドウを経過するように閾値を詰める必要がある。週次窓の場合、安全側下限は `expireAfter - ageThreshold ≥ 7d`。例: `expireAfter=14d` で週次窓なら `ageThreshold=10d` で 4 日のマージン。

## 3.3 surge シーケンス（v1）

1 reconcile で 1 `NodeClaim` を処理。v1 は **直列 1 並列固定** で blast radius を最小化。

```
[Reconcile]
  │
  ├─ 窓外 / 凍結中 / 既に active な置換あり: requeue
  │
  ├─ candidate := pick_oldest()
  │     なければ requeue
  │
  ├─ surge_nc := create_replacement_nodeclaim(
  │     spec_from = candidate.spec,
  │     labels    = {"noderotation.io/surge-for": candidate.name},
  │   )
  │
  ├─ wait_until_ready(surge_nc, timeout = 15m)
  │     timeout: delete(surge_nc); annotate(candidate, failed); alert
  │
  ├─ annotate(candidate, "noderotation.io/state=draining")
  │
  ├─ delete(candidate)
  │     // Karpenter termination controller が PDB を尊重して graceful drain
  │     // 上限は terminationGracePeriod
  │
  ├─ wait_until_gone(candidate, timeout = terminationGracePeriod + buffer)
  │
  └─ cooldown(10m); requeue
```

### ロールバック挙動

| 失敗 | 動作 |
|------|------|
| 代替 `NodeClaim` が timeout 内に `Ready` 化しない | surge NodeClaim を delete（EC2 残置回避）、旧はそのまま、アラート発火 |
| 代替 `NodeClaim` が旧 delete 後に `NotReady` 化 | 旧の drain は止められないため Karpenter に修復を委ねる |
| Karpenter API 不達 | スキップ、次の reconcile で再評価 |

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

> **重要**: バックストップ 2–4 は Forceful 経路で、PDB は `terminationGracePeriod` までしか守られない。コントローラの長期停止は元のリスクプロファイルを復活させる。

---

## 4.1 Capacity / 可用性

| 観点 | 扱い |
|------|------|
| 置換時の Pod Pending 時間 | surge により 0 に近づく（Karpenter Graceful セマンティクスと同等）|
| `readyReplicas` が希望数を一時的に下回る | Eviction API 経路での構造的制約。surge しても新 Pod は即時 Ready にはならない。緩和はアプリ層（余剰レプリカ + PDB）の責務でスコープ外 |
| 並列 surge 数 | v1 は 1 固定。NodePool の `limits.nodes`（あるいは `limits.cpu`）に常時 +1 の余裕が必要 |

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

各置換で旧・新 Node が短時間並行課金される。1 置換あたり概算: オンデマンド 1 台 × 10〜20 分。週次で N 件置換なら月額追加は `≈ N × 4 × 時間単価 × 0.25` で、ベース Node コストに対して小さい。

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
│  │    - maintenanceWindow / ageThreshold / selectors         ││
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

```text
Reconcile(NodeClaim or Tick):
  if not in_window(now): return Requeue(1m)
  if frozen(nodepool):   return Requeue(1m)
  if rotation_active(nodepool): return Requeue(30s)

  candidate := pick_oldest_eligible(nodepool)
  if candidate == nil: return Requeue(1m)

  surge := create_surge_nodeclaim(candidate)
  if !wait_ready(surge, 15m): rollback; alert; return Requeue(10m)

  annotate(candidate, state=draining)
  delete(candidate)
  wait_gone(candidate, terminationGracePeriod + buffer)

  emit_metrics(success)
  return Requeue(cooldown=10m)
```

Leader election は `coordination.k8s.io/Lease` 標準。

## 5.3 状態モデル

全状態を `NodeClaim` の annotation / label に持つ。外部データストア不要。

| キー | 付与先 | 値 | 用途 |
|------|-------|-----|------|
| `noderotation.io/state` | 旧 NodeClaim | `pending` / `draining` / `failed` | 進行ステート |
| `noderotation.io/started-at` | 旧 NodeClaim | RFC3339 | observability 用 |
| `noderotation.io/surge-for` | 新 NodeClaim | 旧 NodeClaim の `metadata.name` | 対応関係。ロールバック時クリーンアップに使用 |
| `noderotation.io/freeze` | NodePool | RFC3339（凍結期限）| その時刻まで置換抑制 |

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

    ageThreshold: 10d

    maintenanceWindow:
      timezone: Asia/Tokyo
      days: [Sat]
      start: "02:00"
      end:   "06:00"

    surge:
      parallelism: 1           # v1 は 1 のみ
      readyTimeout: 15m
      cooldownAfter: 10m

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
| R2 | 窓内に全候補を捌けない | `noderotation_candidates` が 2 窓連続で減らない場合にアラート。将来バージョンで parallelism > 1 を検討 |
| R3 | 代替 NodeClaim が立たない（容量不足 / AZ 枯渇）| Ready タイムアウトでロールバック。NodePool は複数 AZ / 複数インスタンスタイプを許容する設計を維持 |
| R4 | 誤設定の PDB で drain がブロックされる | Karpenter の `terminationGracePeriod` で最終的に強制 drain。PDB のレビューはアプリ owner の責務 |
| R5 | 業務クリティカル期間の freeze 付与忘れ | freeze annotation は GitOps 等で宣言的に管理する設計を推奨 |
| R6 | テストクラスタが日次で turnover する場合の検証ギャップ | age 閾値を超えるソーク期間を設けて end-to-end 置換を検証 |

## 7.2 未決事項

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
