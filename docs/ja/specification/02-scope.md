# 2. スコープ

## 2.1 スコープと互換性

### 対応環境

| 環境 | ステータス |
|-------------|--------|
| EKS Auto Mode | 主要ターゲット |
| Self-managed Karpenter v1+ on EKS | サポート |
| 他の CNCF 上の Karpenter（AKS NAP 等） | ベストエフォート |

- **EKS Auto Mode:** 21日間のハードキャップが最も強い動機付け制約。
- **他の CNCF ディストリビューション:** CRD API は同一だが、基盤オペレーターの動作が異なる可能性がある。

### Karpenter 互換性ポリシー

`karpenter.sh/v1` が必須。以前のバージョン（`v1beta1`、`v1alpha5`）はサポートしない。

互換性の契約は **安定した `karpenter.sh/v1` CRD サーフェス — 特定の Karpenter コントローラーマイナーではない。** これは **EKS Auto Mode** にとって重要であり、管理されている正確な Karpenter マイナーバージョンをユーザーに公開しないため。

- **ランタイムターゲット:** 互換性のある `karpenter.sh/v1` `NodePool`/`NodeClaim` API を提供する任意のクラスター
- **ビルド/テストベースライン:** [`go.mod`](../../go.mod) にバンドルされた `sigs.k8s.io/karpenter` Go モジュールバージョン（現在 `v1.13.0`）。これは型付き Go API を固定するものであり、ランタイム要件 **ではない**
- **インタラクション境界:** Kubernetes API オブジェクト（`NodeClaim`/`NodePool` CRD、およびコア `Node`/`Pod`）のみ。Karpenter 内部やクラウドプロバイダー API は使用しない
- **ランタイム強制:** 起動時の preflight（§5.1）がクラスターが `karpenter.sh/v1` を提供しないか RBAC が読み取れない場合に即座に失敗

### 必要な互換性サーフェス

コントローラーは以下の公開 `karpenter.sh/v1` フィールドのみに依存する：

| Kind / フィールド / キー | 用途 |
|--------------------|----------|
| `NodeClaim`, `NodePool` | ローテーション単位と所有プール |
| `NodeClaim.spec.expireAfter` | ノードごとの期限（§3.2） |
| `NodeClaim.spec.terminationGracePeriod` | `t_rot` のドレイン上限（§3.2） |
| `NodeClaim.spec.requirements` | placeholder 要件のフォールバック（§3.3） |
| `NodeClaim.status.nodeName` | Claim → Node マッピング（§3.3, §5.2） |
| `NodeClaim.status.conditions[Ready]` | 選定の適格性（§3.2） |
| `NodePool.spec.template.spec.expireAfter` | 検証用の代表値 `E` |
| `NodePool.spec.template.spec.terminationGracePeriod` | 代表値 `tGP` |
| `NodePool.spec.template.spec.requirements` | placeholder 要件の複製（§3.3） |
| `NodePool.spec.template.spec.taints` | placeholder の tolerations（§3.3） |
| `NodePool.spec.limits` | surge ヘッドルームチェック（§3.2, §5.2） |
| `NodePool.status.resources` | ヘッドルーム用のプロビジョニング済みフットプリント |
| label `karpenter.sh/nodepool` | ノード/placeholder をプールに紐付け |
| annotation `karpenter.sh/do-not-disrupt` | surge ペアの freeze（§3.3） |

この集合外のすべて — Karpenter コントローラー内部を含む — は互換性に無関係。

## 2.2 既存メカニズムとの連携

| メカニズム | 関係 |
|-----------|--------------|
| Consolidation / Drift | 共存 |
| NodePool `expireAfter` | backstop として共存 |
| `terminationGracePeriod` | 依存 |
| PodDisruptionBudget | 依存 |
| `topologySpreadConstraints` | 依存 |

- **Consolidation / Drift:** コントローラーは Expiration パスのみを引き継ぐ。Consolidation/Drift からの voluntary disruption は Karpenter を通じて引き続き流れる。
- **`expireAfter`:** 導出された `ageThreshold` は構造的に `expireAfter` より低い（`A = E − (K·P + t_rot)`、§3.2）。スケジュールが設定されたローテーション回数を保証できない場合、バリデーションは **fatal** で失敗する — ギャップは手動調整されない。
- **`terminationGracePeriod`:** コントローラーが古い `NodeClaim` を削除した後、Karpenter の termination controller が `terminationGracePeriod` で制限されたドレイン中に PDB を尊重する。
- **PodDisruptionBudget:** ドレインは voluntary パスに従うため、PDB は厳格に尊重される。
- **`topologySpreadConstraints`:** surge があっても、ノードがドレインされるとそのノード上のすべての Pod が同時に消失する。スプレッドは引き続き不可欠。
