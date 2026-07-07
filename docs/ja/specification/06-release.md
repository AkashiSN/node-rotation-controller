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
| v0.3（MVP, v1 surge）| Reconcile + surge + drain + metrics + Helm chart |
| v0.4 | pre-pull（v2 機能）|
| v1.0 | `RotationPolicy` CRD（`v1`）安定、production runbook の文書化、実 EKS Auto Mode クラスタでのsoak テスト済み |
