# 6. リリース

## 6.1 バージョニングとリリース

- セマンティックバージョニング（`vMAJOR.MINOR.PATCH`）
- v1 スコープと CRD の形状が固まるまで pre-1.0（`v0.x.y`）
- API 互換性の対象: `RotationPolicy` CRD スキーマ（`v1alpha1` → `v1` へ安定化中）、Prometheus メトリクス名、annotation キー
- **配布。** `vX.Y.Z` の git タグを push すると、コントローライメージ（マルチアーキ
  `ghcr.io/akashisn/node-rotation-controller`、`linux/amd64,linux/arm64`）と Helm
  チャート（`oci://ghcr.io/akashisn/charts/node-rotation-controller`）を、同一
  バージョンで GitHub Container Registry（ghcr.io）に OCI アーティファクトとして
  publish する。リリースパイプラインは publish 前にタグが `Chart.yaml` の
  `version`==`appVersion` と一致することを検証する（タグを正とする）。
- インストール: `helm install ... oci://ghcr.io/akashisn/charts/node-rotation-controller --version X.Y.Z`。

## 6.2 ロードマップ

| マイルストーン | 内容 |
|---------------|------|
| v0.1（spec）| 本ドキュメント |
| v0.2（skeleton）| プロジェクト構成、controller-runtime bootstrap、leader election、CI |
| v0.3（MVP, v1 surge）| Reconcile + surge + drain + metrics + Helm chart。クラスタスコープの `RotationPolicy` CRD（§5.4）が ConfigMap を置き換える |
| v0.4 | chart が `rotationPolicies` のエントリごとに 1 つの `RotationPolicy` をレンダリングする。単一のインストールで NodePool ごとに異なるウィンドウ・`ageThreshold`・surge を与えられる |
| v0.5 | opt-in のウィンドウ有界 surge-less forceful fallback（§3.3、ADR-0001）、deadline の早い順での候補順序付け（§3.2）、運用者が付けた `karpenter.sh/do-not-disrupt` による候補選定からの除外（§3.2）、同期したバッチに対する `ThroughputBurstShortfall` 検出（§3.2）、公開ドキュメントサイト |
| v1.0 | `RotationPolicy` CRD（`v1`）安定、production runbook の文書化、実 EKS Auto Mode クラスタでの soak テスト済み。§7.2 のうち 2 項目が未解決のまま残る: 同一 AZ の実際の容量不足（ICE）によるロールバックと、数時間規模の tight-race な `expireAfter` soak |

イメージの **pre-pull** はどのマイルストーンにも紐づいていない。無効化された設定
フラグの背後にある v2 の拡張ポイントとして予約されたままである（§3.4）。v1 の
パーサが受理するのは `prePull.enabled` のみで、その値は `false` でなければ
ならない。
