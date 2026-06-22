# node-rotation-controller

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-v0.3_MVP_(pre--1.0)-blue.svg)](docs/ja/specification.md)

Karpenter 配下の Node を、設定可能なメンテナンスウィンドウ内で **make-before-break（surge）** 型に先回り置換し、Karpenter の Forceful な `expireAfter` 発火を実質起こさないようにする Kubernetes コントローラ。

EKS Auto Mode をはじめ、Node Expiration が Forceful で Disruption Budgets が効かない Karpenter v1+ 環境向け。

## Status

**v0.3 — v1 surge MVP 実装済み（pre-1.0）。** v1 の make-before-break 置換ステートマシン（仕様 §5.2）、`ageThreshold` / 候補導出（§3.2）、surge placeholder（§3.3）、メトリクスと Warning Events（§4.2）、Helm chart、Karpenter v1 起動時プリフライト（§5.1）が実装済みで、ユニットテストと envtest スモークテストが CI で動いている。これは **早期検証段階であり、まだ production-ready ではない** — 実際の EKS Auto Mode クラスタでの soak テストは未実施（v1.0 に向けた[ロードマップ](docs/ja/specification.md#62-ロードマップ)を参照）。設計の source of truth は引き続き [docs/ja/specification.md](docs/ja/specification.md)。Karpenter の契約は[互換性](#互換性)を参照。

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

## 互換性

互換性の契約は **安定版 `karpenter.sh/v1` CRD サーフェスであり、特定の Karpenter コントローラのマイナーバージョンではない。** これは **EKS Auto Mode** において重要である: Auto Mode は管理対象の Karpenter バージョンをユーザに公開しないが、本コントローラは互換性のある `karpenter.sh/v1` `NodePool`/`NodeClaim` API を提供する任意のクラスタで動作する。

- **ランタイム対象:** EKS Auto Mode、および `karpenter.sh/v1` 互換の `NodePool`/`NodeClaim` API を提供する任意の Karpenter v1+ クラスタ。
- **ビルド/テスト基準:** 本リポジトリは [`go.mod`](go.mod) に固定された `sigs.k8s.io/karpenter` のバージョンに対してコンパイル・テストする。これは typed な Go API の固定であって、クラスタがそのマイナーを動かすことを要求するものでは **ない**。
- **内部・クラウド API 不使用:** 本コントローラは Kubernetes API オブジェクト（`NodeClaim`/`NodePool` CRD、core の `Node`/`Pod`）のみを介して動作する。Karpenter コントローラの内部やクラウドプロバイダ API は一切呼ばない。公開 `karpenter.sh/v1` サーフェスが互換である限り、未知の Auto Mode 内部は問題にならない。
- 起動時プリフライトが、`karpenter.sh/v1`（`nodeclaims`/`nodepools`）が提供されない・読み取れない場合に fail fast する。

必須の CRD フィールド・ラベル・アノテーションの一覧は[互換性ポリシー](docs/ja/specification.md#21-スコープと互換性)を参照。

## プロジェクト構成

```
.
├── docs/
│   ├── specification.md       仕様書（英語）
│   ├── runbook.md             運用ランブック（英語）
│   ├── ja/specification.md    日本語訳
│   └── ja/runbook.md          運用ランブック（日本語）
├── charts/                    Helm chart（node-rotation-controller）
├── cmd/                       Controller エントリポイント（manager bootstrap + 起動時プリフライト）
└── internal/                  Reconciler と関連パッケージ: 置換ステートマシン
                               （controller）、schedule/selection、surge placeholder、
                               window、policy、metrics、preflight
```

## インストール

> Karpenter v1+ が既にインストールされていることが前提。本 chart は Karpenter
> やその CRD をインストールしない — Karpenter が所有する `NodeClaim`/`NodePool`
> リソースを操作するだけである。

```sh
helm install node-rotation-controller charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  --set-json 'config.policy.nodepoolSelectors=[{"matchLabels":{"workload":"api"}}]'
```

chart はコントローラ（leader election 付き `replicas=2`）、その RBAC、
`node-rotation-config` ConfigMap、surge placeholder Pod 用の専用の負優先度
`PriorityClass`（仕様 §3.3・§4.3・§5.1）をインストールする。置換の設定は
`config.policy`（仕様 §5.4 のスキーマ）を編集する —
[`charts/node-rotation-controller/values.yaml`](charts/node-rotation-controller/values.yaml)
を参照。

## 開発

Go 1.26 以上、`make`、[aqua](https://aquaproj.github.io) が必要。Docker は
`make docker-build` のときのみ必要。CLI ツール（golangci-lint・gopls・
setup-envtest・kind・ko・kustomize・helm・kubectl・terraform）は
[`aqua.yaml`](aqua.yaml) でバージョン固定され、`$PATH` から解決される。aqua を
インストールして `$(aqua root-dir)/bin` を `$PATH` に追加すれば、`make` の各
ターゲットが固定バージョンを自動的に利用する（aqua が初回利用時に遅延インストール）。

| コマンド | 用途 |
|----------|------|
| `make build` | マネージャーバイナリを `bin/manager` にビルド |
| `make test` | ユニットテストと envtest ベースのスモークテストを実行 |
| `make lint` | golangci-lint を実行 |
| `make helm-lint` | Helm chart の lint とレンダリング |
| `make docker-build` | コンテナイメージをビルド |

`make test` は初回実行時に envtest のコントロールプレーンバイナリをダウンロードする。

ワークフロー（Issue・ブランチ・PR）は [CONTRIBUTING.md](CONTRIBUTING.md) を参照。

## ライセンス

Apache 2.0 — [LICENSE](LICENSE) を参照。
