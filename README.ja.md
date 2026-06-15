# node-rotation-controller

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-specification-orange.svg)](docs/ja/specification.md)

Karpenter 配下の Node を、設定可能なメンテナンスウィンドウ内で **make-before-break（surge）** 型に先回り置換し、Karpenter の Forceful な `expireAfter` 発火を実質起こさないようにする Kubernetes コントローラ。

EKS Auto Mode をはじめ、Node Expiration が Forceful で Disruption Budgets が効かない Karpenter v1+ 環境向け。

## Status

**初期開発（v0.2 skeleton）。** 設計は確定済みで、controller-runtime のスケルトン（manager・leader election・CI）から実装を開始した。置換ロジック（仕様 §5.2）は v0.3 で実装する。設計の source of truth は引き続き [docs/ja/specification.md](docs/ja/specification.md)。

English: [README.md](README.md) / [docs/specification.md](docs/specification.md)

## なぜ必要か

Karpenter は Node の disruption を 2 種類に分類している。

| 分類 | 例 | NodePool Disruption Budgets | 代替 Node の事前起動 |
|------|-----|------------------------------|-----------------------|
| Graceful | Drift, Consolidation | 適用される | する（make-before-break）|
| **Forceful** | **Expiration**, Spot Interruption | **適用されない** | **しない** |

Expiration は意図的に Forceful とされている（参照: 公式 [forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md)）。AMI パッチやセキュリティ更新を Budgets で無期限延期させないため。同 design は「運用者が独自に graceful rotation を実装する」ことも妥当解として明示している。EKS Auto Mode はさらに **21 日 hard cap** を強制する。

帰結: Node は **必ず Force drain される瞬間が来る**（PDB 無視可、`terminationGracePeriod` でキャップ）。Karpenter は drain 開始の **後から** 代替を立ち上げるため、ピーク時間と衝突した瞬間に Pod Pending が発生する。

本コントローラはこのギャップを以下で埋める。

1. `expireAfter` 接近の `NodeClaim` を watch
2. 設定可能な **メンテナンスウィンドウ**（例: 土曜 02:00–06:00）に置換を閉じ込める
3. 低優先度の **placeholder Pod** で NodePool 所有の代替ノードを先に誘発し（standalone `NodeClaim` は作成しない — 仕様 §3.3）、予約容量の準備完了を待ってから旧 `NodeClaim` を delete（**surge**）
4. 旧 Node の drain は Karpenter 標準の termination controller に委ねる（**PDB が効く voluntary 経路**）

## スコープ外

- Karpenter Consolidation / Drift / Disruption Budgets の置き換え（共存）
- Spot 中断（[AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) を使う）
- OS パッチ起因の Node 再起動（[kured](https://github.com/kubereboot/kured) を使う）
- Pod 再配置（[descheduler](https://github.com/kubernetes-sigs/descheduler) を使う）
- アプリケーション側 warm-up（`readinessProbe` / `readinessGate` / `slow_start` の領分）

## プロジェクト構成

```
.
├── docs/
│   ├── specification.md       仕様書（英語）
│   └── ja/specification.md    日本語訳
├── charts/                    Helm chart（予定）
├── cmd/                       Controller エントリポイント（manager bootstrap）
├── api/                       CRD types（必要なら）（予定）
└── internal/                  Reconciler 実装（skeleton；本実装は予定）
```

## 開発

Go 1.26 以上と `make` が必要。Docker は `make docker-build` のときのみ必要。

| コマンド | 用途 |
|----------|------|
| `make build` | マネージャーバイナリを `bin/manager` にビルド |
| `make test` | ユニットテストと envtest ベースのスモークテストを実行 |
| `make lint` | golangci-lint を実行 |
| `make docker-build` | コンテナイメージをビルド |

`make test` は初回実行時に envtest のコントロールプレーンバイナリをダウンロードする。

ワークフロー（Issue・ブランチ・PR）は [CONTRIBUTING.md](CONTRIBUTING.md) を参照。

## ライセンス

Apache 2.0 — [LICENSE](LICENSE) を参照。
