# node-rotation-controller

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-v0.6.1_released_(pre--1.0)-blue.svg)](docs/ja/specification/)

Karpenter 配下のノードを、設定可能なメンテナンスウィンドウ内で **make-before-break（surge）** 型に先回り置換し、Karpenter の Forceful な `expireAfter` 発火を実質起こさないようにする Kubernetes コントローラ。

EKS Auto Mode をはじめ、ノードの Expiration が Forceful で Disruption Budgets が効かない Karpenter v1+ 環境向け。

## ステータス

**v0.6.1 — v1 surge MVP、リリース済み（pre-1.0）。** v1 の make-before-break ローテーションステートマシン（仕様 §5.2）、`ageThreshold` / 候補導出（§3.2）、surge placeholder（§3.3）、メトリクスと Warning Events（§4.2）、Helm chart、Karpenter v1 起動時プリフライト（§5.1）が実装済みで、ユニットテストと envtest スモークテストが CI で動いている。**opt-in のウィンドウ有界 surge-less forceful fallback**（`surge.forcefulFallback`、既定 off。ADR-0001）も同様である: 候補が自身の `expireAfter` 期限までに graceful な surge を完了できないとき、コントローラは制御できない時刻に Forceful な expiration が起きるのを待たず、その `NodeClaim` をウィンドウ内で削除する（経路は Karpenter の voluntary な、PDB を尊重するものと同じ）。候補は **deadline の早い順** に並べられ、運用者が付けた `karpenter.sh/do-not-disrupt` アノテーションを持つノードは候補選定から除外される。

v0.6.0 のテーマは **スループット予測** — ノードが 1 台も動き出す前に「そのウィンドウは fleet を回しきれる幅があるか」を答えるチェックである。予測はいま、健全なローテーションが実際に要する時間からモデル化される: `surge.provisioningEstimate`（候補の作成 → 新ノードが Ready）＋ `surge.drainEstimate`（PDB を尊重する drain）＋ `cooldownAfter` であり、強制 kill の期限から導出するのをやめた（ADR-0003）。その入力はメトリクスとして export されるので、予測は起動時ログ 1 回きりではなく観測可能になった。`surge.failurePause` は **失敗した** 試行のあとのポーズを `cooldownAfter` から分離する（ADR-0004）: 成功したローテーション間の settle ポーズを `0` まで下げて drain の直列化を PDB に委ねても、systematic な失敗のもとでの候補の巡回コストは有界のままにできる。さらにドキュメントサイトが [**ブラウザ上のポリシーシミュレーター**](https://akashisn.info/node-rotation-controller/ja/simulator) をホストするようになった: コントローラ自身の schedule / selection コードを WebAssembly にコンパイルし（CI が乖離を防いでいる）、ページ上で編集したポリシーに対して実行する。

v0.6.1 は chart のみの追補である: コントローラの Deployment に optional なトップレベル `priorityClassName` を設定できるようになり、共有プールでプールを回すコンポーネント自身を既定優先度で動かさずに済む。これはコントローラ自身の優先度であり、surge placeholder の負優先度クラス（仕様 §3.3）とは無関係である。コントローラの挙動・CRD スキーマ・アノテーション・メトリクスの変更はない。

コアの surge 経路は EKS Auto Mode 上でフルローテーション回帰スイートを通して検証済みであり、同期したバッチに対するトリックなしの forceful fallback 実行と、sub-daily ウィンドウ下の 12 時間 tight-race soak（Scenario P）も含まれる。v1.0 に向けて残る項目は、実際の同一 AZ 容量不足（ICE）によるロールバックである（[ロードマップ](docs/ja/specification/06-release.md#62-ロードマップ)を参照）。

なお依然として **pre-1.0** であり、設定スキーマは minor リリース間で変わりうる。設計の一次情報（source of truth）は [docs/specification/](docs/specification/) であり、[docs/ja/specification/](docs/ja/specification/) は同期された日本語訳である。Karpenter の契約は[互換性](#互換性)を参照。

English: [README.md](README.md) / [docs/specification/](docs/specification/)

## なぜ必要か

Karpenter はノードの disruption を 2 種類に分類している。

| 分類 | 例 | NodePool Disruption Budgets | 代替ノードの事前起動 |
|------|-----|------------------------------|-----------------------|
| Graceful | Drift, Consolidation | 適用される | する（make-before-break）|
| **Forceful** | **Expiration**, Spot Interruption | **適用されない** | **しない** |

Expiration が意図的に Forceful とされているのは、AMI パッチやセキュリティ更新が **誤設定された** Budgets によって無期限に延期されるのを防ぐためである（参照: 公式 [forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md)）。同 design は「運用者が独自に graceful rotation を実装する」ことも妥当解として明示している。EKS Auto Mode はさらに、解除できない **21 日のノード寿命 hard cap** を強制する。

帰結: ノードは 21 日以内のどこかで **必ず Force drain される** — その drain が PDB を尊重するのは `terminationGracePeriod` までで、無期限ではない — Karpenter が代替を立ち上げるのは drain 開始の **後** であり、これがピーク営業時間帯に当たりうる。

本コントローラはこのギャップを以下で埋める。

1. `expireAfter` 接近の `NodeClaim` を watch
2. 設定可能な **メンテナンスウィンドウ**（例: 土曜 02:00–06:00）に置換を閉じ込める
3. 低優先度の **placeholder Pod** で NodePool 所有の代替ノードを先に誘発し（standalone `NodeClaim` は作成しない — 仕様 §3.3）、予約容量の準備完了を待ってから旧 `NodeClaim` を delete（**surge**）
4. 旧ノードの drain は Karpenter 標準の termination controller に委ねる（**PDB が効く voluntary 経路**）

## スコープ外

- Karpenter Consolidation / Drift / Disruption Budgets の置き換え（共存）
- Spot 中断（[AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) を使う）
- OS パッチ起因のノード再起動（[kured](https://github.com/kubereboot/kured) を使う）
- Pod 再配置（[descheduler](https://github.com/kubernetes-sigs/descheduler) を使う）
- アプリケーション側 warm-up（`readinessProbe` / `readinessGate` / `slow_start` の領分）— surge が配置するのはノードであり、アプリ自身の配置はアプリの責務

## 互換性

互換性の契約は **安定版 `karpenter.sh/v1` CRD サーフェスであり、特定の Karpenter コントローラのマイナーバージョンではない。** これは **EKS Auto Mode** において重要である: Auto Mode は管理対象の Karpenter バージョンをユーザに公開しないが、本コントローラは互換性のある `karpenter.sh/v1` `NodePool`/`NodeClaim` API を提供する任意のクラスタで動作する。

- **ランタイム対象:** EKS Auto Mode、および `karpenter.sh/v1` 互換の `NodePool`/`NodeClaim` API を提供する任意の Karpenter v1+ クラスタ。
- **ビルド/テスト基準:** 本リポジトリは [`go.mod`](go.mod) に固定された `sigs.k8s.io/karpenter` のバージョンに対してコンパイル・テストする。これは typed な Go API の固定であって、クラスタがそのマイナーを動かすことを要求するものでは **ない**。
- **内部・クラウド API 不使用:** 本コントローラは Kubernetes API オブジェクト（`NodeClaim`/`NodePool` CRD、core の `Node`/`Pod`）のみを介して動作する。Karpenter コントローラの内部やクラウドプロバイダ API は一切呼ばない。公開 `karpenter.sh/v1` サーフェスが互換である限り、未知の Auto Mode 内部は問題にならない。
- 起動時プリフライトが、`karpenter.sh/v1`（`nodeclaims`/`nodepools`）が提供されない・読み取れない場合に fail fast する。

必須の CRD フィールド・ラベル・アノテーションの一覧は[互換性ポリシー](docs/ja/specification/02-scope.md#21-スコープと互換性)を参照。

## プロジェクト構成

```
.
├── docs/
│   ├── specification/         仕様書（英語、章ごと）
│   ├── runbook.md             運用ランブック（英語）
│   ├── ja/specification/      日本語訳
│   ├── ja/runbook.md          運用ランブック（日本語）
│   └── reference/             ADR とパフォーマンスノート
├── charts/                    Helm chart（node-rotation-controller）
├── examples/                  すぐ流用できる RotationPolicy マニフェスト
├── cmd/                       Controller エントリポイント（manager bootstrap + 起動時プリフライト）
└── internal/                  Reconciler と関連パッケージ: ローテーションステートマシン
                               （controller）、schedule/selection、surge placeholder、
                               window、policy、metrics、preflight
```

## インストール

> Karpenter v1+ が既にインストールされていることが前提。本 chart は Karpenter
> やその CRD をインストールしない — Karpenter が所有する `NodeClaim`/`NodePool`
> リソースを操作するだけである。

GitHub Container Registry（OCI）から公開済みの chart をインストールする:

```sh
helm install node-rotation-controller \
  oci://ghcr.io/akashisn/charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  --set-json 'rotationPolicies=[{"spec":{"nodePoolSelector":{"matchLabels":{"workload":"api"}},"maintenanceWindows":[{"timezone":"Asia/Tokyo","days":["Wed","Sat"],"start":"02:00","end":"06:00"}]}}]'
```

またはこのリポジトリのローカルチェックアウトからインストールする:

```sh
helm install node-rotation-controller charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  --set-json 'rotationPolicies=[{"spec":{"nodePoolSelector":{"matchLabels":{"workload":"api"}},"maintenanceWindows":[{"timezone":"Asia/Tokyo","days":["Wed","Sat"],"start":"02:00","end":"06:00"}]}}]'
```

- **chart が入れるもの。** コントローラ（leader election 付き `replicas=2`）、
  その RBAC、クラスタスコープの `RotationPolicy` CRD（chart の `crds/`
  ディレクトリから）とサンプルの `RotationPolicy` オブジェクト、surge
  placeholder Pod 用の専用の負優先度 `PriorityClass`（仕様 §3.3・§4.3・§5.1）。
- **置換の設定。** `rotationPolicies` 配下にポリシーを列挙する（仕様 §5.4 の
  スキーマ）— chart はエントリごとに 1 つの `RotationPolicy` をレンダリングする
  ため、NodePool ごとに異なるウィンドウ / `ageThreshold` / surge を与えられる。
  デフォルト値は 1 エントリのサンプルを同梱しており、上のクイックスタートは
  それを自分の NodePool に向けている。
  [`charts/node-rotation-controller/values.yaml`](charts/node-rotation-controller/values.yaml)
  を参照。
- **自前のポリシーを使う。** `rotationPolicies: []` を設定すれば、自前の
  `RotationPolicy` オブジェクト（分岐するポリシーごとに 1 つ）を別管理できる。
  どのポリシーにもマッチしない NodePool は単に置換されない。すぐ流用できる
  ポリシー — 単一の catch-all、NodePool ごとに分岐するポリシー、specificity
  解決、メンテナンスウィンドウの合成 — は [`examples/`](examples/) を参照。

> **メンテナー向けメモ（初回リリース時のみ）:** ghcr.io のイメージと chart の
> パッケージは初回公開時に **private** で作成されることがある。未認証の
> `helm install` / イメージ pull が動くよう、GitHub の *Packages* 設定で
> `node-rotation-controller` と `charts/node-rotation-controller` を public に
> してから、ログアウトしたクライアントで **検証** する — 例:
> `helm pull oci://ghcr.io/akashisn/charts/node-rotation-controller --version <X.Y.Z>`
> （chart バージョンには先頭の `v` を **付けない** — release guard が除去する）、
> またはイメージ manifest を匿名で取得して HTTP 200 を期待する。（GitHub API 経由
> でパッケージ可視性を照会・変更するには `read:packages` / `write:packages` 権限の
> トークンが必要だが、*Packages* 設定 UI はトークン不要。）リリースは `vX.Y.Z`
> タグを push して切る（Release workflow を参照）。

## 参加するには

本プロジェクトは pre-1.0 で活発に開発中であり、v1 のスコープは仕様書に記載の surge MVP である。設計へのフィードバックも実装の貢献も、GitHub の Issue と PR の両方で歓迎する。

開発ワークフローは [CONTRIBUTING.md](CONTRIBUTING.md)、コミュニティ規範は [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) を参照。

## 開発

[aqua](https://aquaproj.github.io) と `make` が必要。Docker は
`make docker-build` のときのみ必要。**Go ツールチェーン**と全 CLI ツール
（golangci-lint・gopls・setup-envtest・kind・ko・kustomize・helm・kubectl・
terraform・awscli）は [`aqua.yaml`](aqua.yaml) でバージョン固定される。aqua を
インストールすれば、`make` の各ターゲットが固定バージョンを自動的に用意・利用
する（aqua が初回利用時に遅延インストールし、`make` 実行時に `$PATH` へリンク
する）。aqua.yaml の Go バージョンは `go.mod` の `go` ディレクティブと同期する。

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
