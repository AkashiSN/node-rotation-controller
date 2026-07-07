# 2. スコープ

## 2.1 スコープと互換性

### サポート対象環境

| 環境 | 状態 |
|------|------|
| EKS Auto Mode | 主対象（21 日 hard cap が最大の動機） |
| EKS 上の self-managed Karpenter v1+ | サポート |
| その他の CNCF 系（AKS NAP 等） | best-effort。CRD API は同じだが、背後で動く Karpenter オペレータ（コントローラ）の挙動には差異がありうる |

### Karpenter 互換性ポリシー

`karpenter.sh/v1` 必須。`v1beta1`、`v1alpha5` は非サポート。

互換性の契約は **安定版 `karpenter.sh/v1` CRD サーフェスであり、特定の Karpenter コントローラのマイナーバージョンではない。** これは主対象である **EKS Auto Mode** において重要である: Auto Mode はマネージドで動かす Karpenter の正確なマイナーバージョンをユーザに公開しないが、本コントローラは互換性のある `karpenter.sh/v1` `NodePool`/`NodeClaim` API を提供する任意のクラスタで動作する — 背後で Auto Mode が動かす Karpenter のバージョンに依存しない。

- **ランタイム対象。** EKS Auto Mode、および `karpenter.sh/v1` 互換の `NodePool`/`NodeClaim` API を提供する任意の Karpenter v1+ クラスタ。
- **ビルド/テスト基準。** 本リポジトリは [`go.mod`](../../../go.mod) に固定された `sigs.k8s.io/karpenter` Go モジュールのバージョン（現在 `v1.13.0`）に対してコンパイル・テストする。これは *typed な Go API* の固定であって、クラスタがそのマイナーを動かすことを要求するものでは **ない**。
- **相互作用の境界。** 本コントローラは Karpenter コントローラの内部やクラウドプロバイダ API を一切呼ばない — Kubernetes API オブジェクト（`NodeClaim`/`NodePool` CRD と core の `Node`/`Pod`）のみを介して相互作用する。したがって、公開された `karpenter.sh/v1` サーフェスが互換である限り、未知の Auto Mode 内部は問題にならない（§4.3 はクラウド IAM を要求しない）。
- **ランタイム強制。** 起動時プリフライト（§5.1）が、クラスタが `karpenter.sh/v1` を `nodeclaims`/`nodepools` リソースとともに提供しない、または RBAC が読み取れない場合に fail fast する — 互換性ギャップを、後続 reconcile での遅延した失敗ではなく、即座に対処可能なエラーとして表面化させる。

**必須の互換性サーフェス。** 本コントローラは以下の公開 `karpenter.sh/v1` フィールド・ラベル・アノテーションのみに依存する。この集合の外（Karpenter コントローラの全内部を含む）は互換性に無関係である:

| Kind / フィールド / キー | 用途 |
|------------------------|------|
| `NodeClaim`, `NodePool`（`karpenter.sh/v1`） | ローテーション単位とその所有プール（§3.2, §3.3） |
| `NodeClaim.spec.expireAfter` | トリガの基準点となるノード単位の deadline（§3.2） |
| `NodeClaim.spec.terminationGracePeriod` | `t_rot` / lead time に効くノード単位の drain 上限（§3.2） |
| `NodeClaim.spec.requirements` | パリティキーがノードラベルとして現れない場合の placeholder requirement 複製のフォールバック源（§3.3） |
| `NodeClaim.status.nodeName` | claim と Node の対応付け（§3.3, §5.2） |
| `NodeClaim.status.conditions[Ready]` | 選定の適格性（§3.2） |
| `NodePool.spec.template.spec.expireAfter` | プール単位検証の代表値 `E`（§3.2） |
| `NodePool.spec.template.spec.terminationGracePeriod` | 代表値 `tGP`（§3.2） |
| `NodePool.spec.template.spec.requirements` | placeholder の requirement 複製（§3.3） |
| `NodePool.spec.template.spec.taints` | placeholder の tolerations（§3.3） |
| `NodePool.spec.limits` | surge ヘッドルームチェック（§3.2, §5.2） |
| `NodePool.status.resources` | ヘッドルームチェック用のプロビジョン済みフットプリント（§5.2） |
| ラベル `karpenter.sh/nodepool` | ノード / placeholder をプールへ対応付け（§3.3） |
| アノテーション `karpenter.sh/do-not-disrupt` | surge ペアを凍結し voluntary disruption から保護（§3.3） |

## 2.2 既存メカニズムとの関係

| メカニズム | 関係 |
|-----------|------|
| Karpenter Consolidation / Drift | **共存**。本コントローラは Expiration 経路のみを肩代わり。Consolidation / Drift の voluntary な disruption は Karpenter にそのまま委ねる |
| NodePool `expireAfter` | **共存**（バックストップ）。導出された `ageThreshold` は導出式の性質上つねに `expireAfter` を下回り（`A = E − (K·P + t_rot)`、§3.2）、設定されたローテーション回数をスケジュールが保証できない場合は検証が **fatal** で失敗する — 両者のギャップは手動チューニングしない |
| NodePool `terminationGracePeriod` | **依存**。コントローラが旧 `NodeClaim` を delete した後、Karpenter の termination controller が PDB を尊重して drain し、`terminationGracePeriod` で上限が課される |
| PodDisruptionBudget | **依存**。NodeClaim delete 後の drain は voluntary 経路で PDB が厳密に効く |
| `topologySpreadConstraints` | **依存**。surge しても旧ノード上の全 Pod は drain 時に同時に消える。引き続き分散は必須 |
