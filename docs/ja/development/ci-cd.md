# CI/CD 設計

本リポジトリは 3 つの GitHub Actions ワークフローと、ドキュメントデプロイ用の
ワークフローを運用する。このページでは、必須ステータスチェックを高速に保ちつつ、
チェックを `pending` のまま固まらせないための変更検知ゲーティングの仕組みを
解説する。

## ワークフロー一覧

| ワークフロー | トリガー | 目的 |
|---|---|---|
| `ci.yaml` | `main` への push、全 PR | 必須チェック: `lint`、`test`、`build`、`chart`（および後述の `changes`） |
| `e2e.yaml` | `main` への push、全 PR | surge メカニズム向けの KWOK ベース Karpenter e2e、単一の `e2e` ジョブ |
| `release.yaml` | `v*` タグの push | マルチアーキのコントローライメージと Helm chart（OCI）を `ghcr.io` にビルド・push した後、GitHub Release を作成 |
| `pages.yaml` | `main` への push（ドキュメント関連パス）、手動実行 | 本 VitePress サイトをビルドし GitHub Pages にデプロイ |

## `pending` の罠

`main` のブランチ保護は必須ステータスチェックを名前で指定している。あるワークフロー
やジョブが、たとえばワークフロー直下の `paths-ignore` や *ジョブ* 単位の `if:`
条件によって特定の push / PR で完全にスキップされると、GitHub はそのチェックの
結論を一切報告しない。結論のない必須チェックは「green」ではなく `pending` に
固まったままであり、必須チェックが 1 つでもその状態である限り PR は絶対に
マージできない。これが本リポジトリでドキュメントのみの変更や無関係な変更で CI を
スキップするために `paths-ignore`（やジョブ単位の `if:`）を**使わない**理由である。

正しい解決策は、*ジョブ* は常に実行しつつ、その中の高コストな *ステップ* だけを
関係する変更がない場合にスキップすることである。重要な各ステップにステップ単位の
`if:` を付けることで、ジョブ自体は入力が変わっていなければ数秒で実際の結論
（成功）に到達し、変わっていれば処理をすべて実行する。

## `ci.yaml`: `changes` ジョブ

`ci.yaml` はゲーティング用フラグを専用の `changes` ジョブで一度だけ計算し、
`lint`・`test`・`build`・`chart` の各ジョブが `needs: changes` を宣言してその
出力を読む。分類ロジックを（4 つのジョブそれぞれに重複させるのでなく）1 つの
ジョブに集約することで DRY を保っている。`changes` 自体には `if:` を付けず常に
実行する — 分類スクリプトの出力を信頼する前にそのセルフテストまで走らせる —
ため、`changes` 自身も `pending` に固まった状態を作り得ない。

分類器は `.github/scripts/detect-ci-changes.sh` であり、変更されたパスの
改行区切りリストを標準入力から読み、4 つの真偽値を出力する、小さく純粋な
シェルスクリプト（`git` も GitHub Actions のコンテキストも使わない）である。
`ci.yaml` は pull request では `git diff --name-only "$BASE_SHA" HEAD` の
出力をこれに渡し、`main` への直接 push では全フラグを `true` として扱う（PR
のベースとの diff が存在せず、`main` への push では常にすべて（全ステップ）を実行すべき
であるため）。`.github/scripts/detect-ci-changes.test.sh` はサンプルとなる
パス集合の表に対して分類器をユニットテストし、CI が実行されるたびに走る
ため、ゲーティングロジック自体が気づかれないうちに壊れることはない。

### パス → フラグ → ジョブ

| パスパターン | フラグ | ゲートされるジョブ／ステップ |
|---|---|---|
| `*.go`、`go.mod`、`go.sum`、`api/`、`config/`、`.golangci.yml` | `go` | `lint`、`test`、`build` |
| `charts/**` | `chart` | `chart` |
| `Dockerfile`、`.dockerignore` | `docker` | `build` |
| `Makefile`、`aqua.yaml`、`aqua-policy.yaml`、`aqua/**`、`.github/workflows/ci.yaml`、`.github/scripts/**` | `infra` | `lint`・`test`・`build`・`chart` のすべて |

結果として各ジョブのステップゲートは次のようになる。

- **`lint`**、**`test`** は `go || infra` のとき実ステップを実行する。
- **`build`** は `go || docker || infra` のとき実ステップを実行する。
- **`chart`** は `chart || infra` のとき実ステップを実行する。

`infra` は意図的に広く取ってある。CI ワークフロー自体、共有 Makefile、aqua の
ツールチェーン固定バージョンへの変更は 4 ジョブすべての挙動に影響しうるため、
どれが実際に影響を受けるかを推測するのではなく全 4 ジョブへ波及させている。

## `e2e.yaml`: 単一ジョブ内での変更検知

`e2e.yaml` は意図的に `ci.yaml` と異なる形をとる: ジョブは `e2e` の 1 つだけで
あり、`e2e` という名前の単一の必須チェックに対応する。変更検知は別ジョブで
なくジョブの*最初のステップ*として実行される。なぜなら、`changes` 方式の別
ジョブを導入すると、他に消費者のいない単一ジョブのワークフローにジョブ間
`needs` 依存という余計な間接化を持ち込むだけで、ここでは DRY の恩恵がないからである（`ci.yaml` は分類結果を
4 ジョブで共有するが、`e2e.yaml` のジョブは 1 つしかない）。検知ステップは diff
を e2e に関係するパス（`internal/`、`cmd/`、`charts/`、`test/e2e/`、
`go.mod`/`go.sum`、`Makefile`、`Dockerfile`、`aqua.yaml`、
`.github/workflows/e2e.yaml`）について調べ、純粋な Markdown の変更を除外する。
以降のすべてのステップ — 約 45 分の kind / Karpenter KWOK ブートストラップと
`go test` の実行を含む — はその結果でゲートされる。これらのパスに触れない PR は
数秒で `e2e: success` を報告し、`main` への push では常にフルスイートが実行
される。

## `release.yaml`: タグ駆動の OCI 公開

`release.yaml` は `v*` タグの push のみでトリガーされ、通常の push では動かない
ため `pending` 必須チェックのリスクがない — release 系ワークフローはブランチ
保護の対象外である。逐次実行される 4 つのジョブから成る: `guard` は push された
タグが `Chart.yaml` のバージョンと食い違っていれば即座に失敗する; `image` は
マルチアーキ（`linux/amd64`、`linux/arm64`）のコントローライメージを
`ghcr.io/akashisn/node-rotation-controller` へビルド・push し、ハイフン付きの
プレリリースタグ（例: `v0.4.0-rc.1`）でない限り `latest` タグも付与する;
`chart` は Helm chart をパッケージし `oci://ghcr.io/akashisn/charts` へ OCI
アーティファクトとして push する; `release` はパッケージ済み chart を
ダウンロードし、それを添付した GitHub Release を作成し、`image` ジョブが `latest` を
スキップするのと同じハイフン付きタグに対してはプレリリースとしてマークする。

## `pages.yaml`: ドキュメントデプロイ

4 つ目のワークフロー `pages.yaml` は、本 VitePress サイトを `npm run docs:build`
でビルドし GitHub Pages にデプロイする。トリガーは `docs/**`、ルートの
`README.md`/`README.ja.md`（ビルド時に Getting Started ページへコピーされる）、
`package.json`、`package-lock.json`、またはワークフロー自身のファイルに触れる
`main` への push、および手動実行（`workflow_dispatch`）である。上述の必須チェック集合には
含まれない（ドキュメントを公開するだけで、マージをゲートしないため）。したがって、この
ページで説明した変更検知ゲーティングは不要である。
