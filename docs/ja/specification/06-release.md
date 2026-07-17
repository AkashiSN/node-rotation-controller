# 6. リリース

## 6.1 バージョニングとリリース

### バージョニング

- **セマンティックバージョニング**（`vMAJOR.MINOR.PATCH`）
- v1 スコープと CRD 形状が安定するまでプレ 1.0 リリース（`v0.x.y`）
- **API 互換性サーフェス:** `RotationPolicy` CRD スキーマ、Prometheus メトリクス名、アノテーションキー

### 配布

| アーティファクト | レジストリ | アーキテクチャ |
|----------|----------|---------------|
| コントローラーイメージ | `ghcr.io/akashisn/node-rotation-controller` | `linux/amd64`, `linux/arm64` |
| Helm chart | `oci://ghcr.io/akashisn/charts/node-rotation-controller` | — |

- `vX.Y.Z` git タグが両 OCI アーティファクトを同一バージョンで公開
- パイプラインがタグと `Chart.yaml` の `version` == `appVersion` の一致をガード
- インストール: `helm install ... oci://ghcr.io/akashisn/charts/node-rotation-controller --version X.Y.Z`

### サプライチェーン証明

- キーレス **cosign 署名** + GitHub build-provenance（**SLSA**）証明をリリースワークフローの OIDC アイデンティティにバインド
- イメージはインレジストリ **SBOM** と SLSA provenance を保持
- 各 GitHub Release にダウンロード可能な **SPDX SBOM** を添付
- 証明と署名はプレリリースタグでも実行
- 検証手順: [`SECURITY.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/SECURITY.md#verifying-releases)

## 6.2 ロードマップ

| マイルストーン | 内容 |
|-----------|---------|
| v0.1（spec） | 本ドキュメント |
| v0.2（skeleton） | プロジェクトレイアウト、controller-runtime ブートストラップ、リーダー選出、CI |
| v0.3（MVP） | Reconcile + surge + drain + メトリクス + Helm chart; `RotationPolicy` CRD（§5.4） |
| v0.4 | chart が各エントリに 1 `RotationPolicy` をレンダー — NodePool ごとのポリシー |
| v0.5 | Forceful fallback（§3.6）; earliest-deadline ソート; オペレーター `do-not-disrupt` オプトアウト; `ThroughputBurstShortfall`; ドキュメントサイト |
| v0.6 | Layer-2 予測に `provisioningEstimate + drainEstimate`（ADR-0003）; `failurePause`（ADR-0004）; ブラウザーポリシーシミュレーター（wasm） |
| v1.0 | 安定 CRD（`v1`）、プロダクション Runbook、EKS Auto Mode で soak テスト済み |

- **v1.0 の未決事項:** 真の同一 AZ キャパシティ不足（ICE）によるロールバック（§7.2）
- **v1.0 検証済み:** マルチ時間 tight-race `expireAfter` soak（§7.2、issue #118）

### スケジュール未定

イメージ **pre-pull** は無効化された設定フラグの背後にある v2 拡張ポイントとして予約。v1 パーサーは `prePull.enabled: false` のみ受け入れる。
