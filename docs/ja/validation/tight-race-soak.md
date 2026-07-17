# Tight-race `expireAfter` Soak — シナリオ P

::: tip この検証の対象
`leadTime` が `expireAfter` と本気で競争する状態を 12 時間持続させた実 EKS ソーク。実証: (1) graceful surge が常に先着（`expired` は 0 のまま）、(2) graceful が組めなくなった瞬間に forceful fallback が決定的に発火。
:::

プールは固定 `expireAfter: 2h12m` — 導出 `leadTime` 1h12m のすぐ上 — で、サブデイリーウィンドウ（30 分周期 × 48 本/日）の下で実行。[§3.2](/ja/specification/03-design#32-候補選定) の保証をライブで実証した。前提の validated 切り替えは [§7.2](/ja/specification/07-risks#72-検証済み前提)、先行検証は [シナリオ O](/ja/validation/forceful-fallback)、メトリクス定義は [ランブック](/ja/runbook#3-noderotation_-メトリクスの読み方) を参照。

**実施**: 2026-07-14T14:20:29Z（T0）→ +12h、EKS Auto Mode、K8s 1.36、`us-west-2`。
正本記録: `test/e2e/eks-automode/VALIDATION.md`（§ "Run: 2026-07-15 — Scenario P"）。

**判定: CLEAN PASS**（12 PASS; 基準 13 は N/A — 未行使）。

| 71/71 | 0 | 68.3 分 | 56 秒 |
|:---|:---|:---|:---|
| graceful 回転（12h 内 60 = 5.0/h） | expired・failure・fallback・short_lead・restart | deadline margin 最小値 | エピローグ: 解凍 → fallback |

## 導出スケジュール

過去の PoC は日次ウィンドウで巨大な `expireAfter`（E=336h）を使い、レースは常に大差勝ちだった。本検証は E を **2h12m** に置き、12 時間の本気レースを持続。`surge.forcefulFallback` は **装備したまま**（静穏性を同時実証）。

| 量 | 値 |
|---|---|
| `t_rot`（上界） | `5m + 5m + 2m = 12m` |
| `t_rot_est`（予測） | `5m + 5m = 10m` |
| `leadTime` | `2·30m + 12m = 1h12m` |
| `ageThreshold`（auto） | `2h12m − 1h12m = 1h` |
| `C`（ウィンドウあたり） | `ceil(28m / 12m) = 3`（6/h） |
| 定常負荷 | N=5、5/h → 予測の 83%（タイト） |
| 期待 findings | warn `RotationSpansNextWindow` が 1 件のみ |

導出値は [`internal/schedule/schedule_test.go`](https://github.com/AkashiSN/node-rotation-controller/blob/main/internal/schedule/schedule_test.go) の `TestDeriveScenarioPSoak` にピン留め。

## margin の全体像

T0 から記録終了（T0+14.1h）まで: **71 回転すべてが新規 provision ノードへの graceful make-before-break surge**。[T0, T_end] 内 60（= 5.0/h）、エピローグ中に 11 回転追加（~12 分間隔維持）。

margin（deadline − 完了）: **68.3–71.2 分**（全 71 回転、分散 < 3 分）。劣化の蓄積なし。

<SoakMarginChart />

## 13 基準

| # | 基準 | 判定 |
|---|---|---|
| 1 | `expired` == 0 | PASS |
| 2 | `success` ≈5/h、計 ≥ 40 | PASS（12h 内 60） |
| 3 | 本編 `forceful_fallback` == 0 | PASS（静穏性） |
| 4 | `short_lead_nodes` == 0 | PASS（909 スクレイプ） |
| 5 | restart 0、seq 連続 | PASS |
| 6 | 負荷継続（全スナップショット） | PASS（desired=available=ready=5） |
| 7 | 検証対象構成の一致 | PASS（ゲージ 6 本 × 909） |
| 8 | 全回転 margin > 0 | PASS（min 68.3m） |
| 9 | 終了 census クリーン | PASS（全 5 claim < A） |
| 10 | Karpenter の予期しない disruption なし | PASS |
| 11 | epi: 凍結中 = 候補のみ | PASS（~2h 保持） |
| 12 | epi: 解凍で fallback 発火 | PASS（56 秒） |
| 13 | アボート則（解凍失期） | N/A |

::: details 基準の詳細観測 — クリックで展開

| # | 観測詳細 |
|---|---|
| 1 | 本編 + tail-follow + エピローグ通して 0。Karpenter `Expiration` イベントも 0 |
| 2 | [T0, T_end] 内 60（5.0/h）; 記録終了までに 71（epi 込み 72）; ~12m 間隔 |
| 3 | 全期間 0 — 静穏性の初実証 |
| 4 | 909 スクレイプで max 0 |
| 5 | controller `restartCount=0`（12h）; gap 0、restart 0、`SCRAPE_ERROR` 0 |
| 6 | desired=available=ready=5 維持; Pending 滞留なし |
| 7 | 導出ゲージ 6 本 × 909 スクレイプで不一致 0; 両 policy `Accepted`; 期待 warn 1 件のみ |
| 8 | min 68.3 / 中央値 70.3 / max 71.2 分（n=71） |
| 9 | T_end の 5 claim は齢 11–59m（right-censored）; 滞留 0、`failed` 0 |
| 10 | `DisruptionBlocked`/`Unconsolidatable` のみ（budgets + `do-not-disrupt` が抑止） |
| 11 | 凍結中 ~2h、`candidates=1` / `in_progress=0` 維持 |
| 12 | 下記エピローグ参照（56 秒、placeholder ゼロ、本編無擾乱） |
| 13 | 正常リリースにより未行使; fail-closed は offline 検証のみ |

:::

## ノード入れ替えの解剖

71 回転はすべて同型のシーケンス:

1. claim が齢 **1h**（= A）に達し候補化 → placeholder Pod 作成
2. Karpenter が新ノードを provision → placeholder bind → surge ノード Ready（**surgeWait 中央値 34 秒**、23–54 秒）
3. 両ノードに `do-not-disrupt` → 旧 NodeClaim 削除 → voluntary drain（**drain 中央値 44 秒**、18–83 秒）
4. 後始末 → **total 中央値 81 秒**（45–131 秒）。旧ノード残骸は `WhenEmpty`/60s で回収

<SoakAnatomyChart />

<SoakLedger />

## エピローグ — fallback の決定的発火

::: tip 別プールを使う理由
本編の「発動しない」だけでは半分。凍結した単一ノードプールで境界越えを人為的・決定的に作成。プール規模 1 の理由: 完了に最大 `tGP + Buffer = 7m` かかり得るため、複数台では 12 分帯内の完了を保証できない（複数台の実績は [シナリオ O](/ja/validation/forceful-fallback)）。
:::

| 時刻 | イベント |
|---|---|
| 02:22:50Z | epi claim `gtx42` 誕生（freeze 付きで作成）→ deadline **d = 04:34:50Z** |
| 03:22:50Z | 齢 1h で候補化; 凍結中 → `candidates=1`, `in_progress=0` |
| 04:23:50Z | 区間探索で R を決定（d−12m < R < d−8m）; freeze 解除。残余 11m < `t_rot` 12m |
| 04:24:46Z | **解凍から 56 秒後**: claim 削除（drain 48 秒 < 7m 上界）。`mode=forceful-fallback`、`surgeNode` なし |

**証拠:**
- `forceful_fallback_total{nodepool-soak-epi}` 0→1
- spec 文言どおりの `ForcefulFallback` Warning イベント
- placeholder 台帳: epi claim の記録 **ゼロ**
- `expired` 終始 0
- 本編無擾乱（解凍→発火ウィンドウで 70→71、完了間隔 ≤ `P + t_rot` = 42m）

アボート則（d−8m までに解凍不可なら凍結のまま撤去）は未行使。fail-closed は kubectl スタブ offline 検証で 3 系統 exit 3 確認済。

## 再試験するには

ランブック: [`test/e2e/eks-automode/SCENARIOS.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/test/e2e/eks-automode/SCENARIOS.md) § Scenario P。

事前チェック:

```sh
go test ./internal/schedule/ -run TestDeriveScenarioPSoak -v  # 導出ピン: A=1h / C=3 / G=2 / warn 1 件
test/e2e/eks-automode/scenarios/soak-analyze-fixture.sh       # 解析系の自己検査
```

### 運用ノート

- **JSON ロギング必須**（`logging.development: false`）: 解析器は zap JSON を読む。console 形式では台帳が空になる
- **認証トークンの寿命を事前確認** — 途中で切れると二次記録が盲目化（一次のクラスタ内 scraper は無欠損）
- **`expireAfter` の live patch 禁止** — プールを作り直す（patch は Karpenter drift を誘発）
- **コスト**: 約 $11（コントロールプレーン + NAT ~20h、5 × 2 vCPU ノード × 14h + エピローグ）。`terraform destroy` を忘れないこと

## 本検証が確定させたこと

仕様書 §7.2 に validated 行が 2 つ追加:
- サブデイリーウィンドウ下で `leadTime` は genuine に競う `expireAfter` に 12h 勝ち続け、fallback は装備されたまま静穏
- claim が graceful surge の組めない点を越えた瞬間、forceful fallback が決定的に発火

**残る未検証項目:** 同一 AZ の実キャパシティ不足（ICE）のみ — [ロードマップ（§6.2）](/ja/specification/06-release#62-ロードマップ) と [§7.2](/ja/specification/07-risks#72-検証済み前提) を参照。
