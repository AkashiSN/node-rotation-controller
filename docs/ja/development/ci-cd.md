# CI/CD 設計

::: tip このページの内容
必須ステータスチェックを高速に保ちつつ、`pending` で固まらせない仕組み — 集中化された変更検知分類器によるステップレベルのゲーティング。
:::

## ワークフロー一覧

| ワークフロー | トリガー | 目的 |
|---|---|---|
| `ci.yaml` | `main` への push、全 PR | 必須: `changes`, `lint`, `test`, `build`, `docs`, `chart` |
| `e2e.yaml` | `main` への push、全 PR | KWOK ベース Karpenter e2e（単一 `e2e` ジョブ） |
| `release.yaml` | `v*` タグの push | マルチアーキイメージ + Helm chart OCI + attestation + GitHub Release |
| `pages.yaml` | `main` への push（docs パス）、手動 | VitePress サイト → GitHub Pages |

## `pending` の罠

::: warning なぜ重要か
結論のない必須チェックは "green" ではなく `pending` で固まる。必須チェックが 1 つでもその状態なら PR は絶対にマージできない。
:::

`main` のブランチ保護は必須ステータスチェックを名前で指定している。ワークフローやジョブが **完全にスキップ** された場合（`paths-ignore` やジョブ単位の `if:` により）、GitHub は結論を報告しない → `pending` で固まる。

**解決策:** *ジョブ* は常に実行し、関係する変更がない場合に高コストな *ステップ* のみスキップ。

- 重要な各ステップにステップ単位の `if:` を付与
- 入力が変わっていなければジョブは数秒で実際の結論（成功）に到達
- 関連ファイルが変更された場合のみフルワークを実行

これが `paths-ignore` やジョブ単位の `if:` を **使わない** 理由。

## `ci.yaml`: `changes` ジョブ

### 仕組み

1. 専用の `changes` ジョブがゲーティングフラグを計算（常に実行、`if:` なし）
2. `lint`、`test`、`build`、`docs`、`chart` が `needs: changes` を宣言しその出力を読む
3. 分類ロジックは 1 箇所に集約（DRY）

分類器は `.github/scripts/detect-ci-changes.sh`:
- 小さく純粋なシェルスクリプト（`git` も GitHub Actions コンテキストも使わない）
- 標準入力から改行区切りの変更パスを読む
- 5 つの真偽値を出力

### 入力ソース

| コンテキスト | 入力 |
|---------|-------|
| Pull request | `git diff --name-only "$BASE_SHA" HEAD` |
| `main` への push | 全フラグ `true`（diff のベースなし; 常にすべて実行） |

### セルフテスト

`.github/scripts/detect-ci-changes.test.sh` がサンプルパス集合のテーブルに
対して分類器をユニットテスト。CI 実行のたびに走るため、ゲーティングロジック
自体が気づかれずに壊れることはない。

同じ常時実行 job で `.github/scripts/check-release-version-sync.sh` も実行。
chart、README EN/JA、agent/contributor 向けステータス、runbook EN/JA の
current release バージョンが異なる PR は通過できない。

### パス → フラグ → ジョブ

| パスパターン | フラグ | ゲートされるジョブ/ステップ |
|---|---|---|
| `*.go`, `go.mod`, `go.sum`, `api/`, `config/`, `.golangci.yml` | `go` | `lint`, `test`, `build` |
| `charts/**` | `chart` | `chart` |
| `Dockerfile`, `.dockerignore` | `docker` | `build` |
| `docs/**`, `README*.md`, package ファイル、simulator Go source | `docs` | `docs` |
| `Makefile`, `aqua.yaml`, `aqua-policy.yaml`, `aqua/**`, `.github/workflows/ci.yaml`, `.github/scripts/**` | `infra` | `lint`, `test`, `build`, `chart` |

### 結果のステップゲート

| ジョブ | 実ステップ実行条件 |
|-----|---------------------|
| `lint` | `go \|\| infra` |
| `test` | `go \|\| infra` |
| `build` | `go \|\| docker \|\| infra` |
| `docs` | `docs` |
| `chart` | `chart \|\| infra` |

- **`infra` は意図的に広い:** CI ワークフロー、共有 Makefile、aqua ツールチェーン固定バージョンの変更は docs 以外の 4 job すべてに影響しうるため、推測せず波及。`docs` flag は docs site と simulator の入力を別に追跡し、wasm モジュールを生成する共有ビルドツールチェーンのファイル（`go.mod`, `go.sum`, `Makefile`, `aqua.yaml`）も含む — これらは `go` や `infra` に加えて `docs` も立てる。

## `e2e.yaml`: 単一ジョブ内での変更検知

`ci.yaml` とは意図的に異なる形:

- **1 ジョブ**（`e2e`）= 1 つの必須チェック
- 変更検知はジョブの **最初のステップ** として実行（別の上流ジョブではない）
- 消費者が 1 つしかないため、別の `changes` ジョブによる DRY の恩恵なし

### 検知スコープ

e2e に関係するパスの diff を検査:
- `internal/`, `cmd/`, `charts/`, `test/e2e/`
- `go.mod`, `go.sum`, `Makefile`, `Dockerfile`, `aqua.yaml`
- `.github/workflows/e2e.yaml`
- 純粋な Markdown 変更を除外

### 動作

| コンテキスト | 結果 |
|---------|--------|
| これらのパスに触れない PR | `e2e: success` を数秒で報告 |
| 関連パスに触れる PR | フル ~45 分スイート |
| `main` への push | 常にフルスイート |

## `release.yaml`: タグ駆動の OCI 公開

`v*` タグの push のみでトリガー — ブランチ保護の対象外のため `pending` チェックのリスクなし。

### 4 つの逐次ジョブ

| ジョブ | 目的 |
|-----|---------|
| `guard` | タグ、chart、current-release marker が不一致なら失敗 |
| `image` | マルチアーキビルド + push + SBOM + SLSA provenance + attest + cosign |
| `chart` | Helm chart パッケージ → OCI push + attest + cosign |
| `release` | GitHub Release 作成（chart + SBOM 添付） |

### image ジョブの詳細

公開前に `guard` が両方のバージョンチェックを実行:

- `check-chart-version.sh`: tag == chart `version` == `appVersion` を要求
- `check-release-version-sync.sh`: README EN/JA、`AGENTS.md`、
  `CONTRIBUTING.md`、runbook EN/JA の current-release marker 一致を要求

同じ sync check は全 PR ですでに実行されるため、release-preparation の更新漏れは
通常 merge 前に検出され、公開前にも再検査される。

- アーキテクチャ: `linux/amd64`, `linux/arm64`
- レジストリ: `ghcr.io/akashisn/node-rotation-controller`
- ハイフン付きプレリリース（例: `v0.4.0-rc.1`）でない限り `latest` タグを付与
- レジストリ内 SBOM + SLSA provenance
- push された index ダイジェストに attestation + キーレス cosign 署名
- Release 用 SPDX SBOM を生成

### chart ジョブの詳細

- Helm chart をパッケージ → `oci://ghcr.io/akashisn/charts`
- push されたマニフェストダイジェストに attestation + キーレス署名

### release ジョブの詳細

- パッケージ済み chart + SPDX SBOM をダウンロード
- 両方を添付した GitHub Release を作成
- ハイフン付きタグの場合はプレリリースとしてマーク

### 権限

ジョブ単位でスコープ（ワークフローレベルではない）:

| ジョブ | 権限 |
|-----|-------------|
| `image`, `chart` | `id-token`, `attestations`, `packages: write` |
| `release` | `contents: write` |

attestation はプレリリースタグに対しても実行。利用者側の検証手順は [`SECURITY.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/SECURITY.md#verifying-releases) を参照。

## `pages.yaml`: ドキュメントデプロイ

本 VitePress サイトを `npm run docs:build` でビルドし GitHub Pages にデプロイ。

### トリガー

- `main` への push で以下に触れた場合: `docs/**`, `README.md`, `README.ja.md`, `package.json`, `package-lock.json`, またはワークフローファイル自身
- `workflow_dispatch` で手動実行

### 必須チェックではない

このワークフローはドキュメントを公開するのみ — マージをゲートしない。変更検知ゲーティングは不要。
