# Tight-race `expireAfter` soak — シナリオ P

コントローラの `leadTime` が Karpenter の `expireAfter` バックストップと本気で
競争する状態を 12 時間持続させた実 EKS ソーク検証（issue #118、PR #272）。
プールは固定の `expireAfter: 2h12m` — 導出された `leadTime` 1h12m のすぐ上 —
で、サブデイリーのメンテナンス窓（30 分周期 × 48 本/日）の下で走った。
[仕様書 §3.2](/ja/specification/03-design#32-候補選定) の保証（graceful surge が
必ず先着し `expired` は 0 のまま）をライブで実証し、さらに別プールのエピローグで
「graceful が組めなくなった瞬間、窓内 forceful fallback が決定的に発火する」ことを
確認した。この検証が validated に切り替える前提は
[§7.2](/ja/specification/07-risks#72-検証済み前提)、以下で引用するメトリクスの
運用上の意味は [ランブック](/ja/runbook#3-noderotation_-メトリクスの読み方)、
先行する forceful fallback 検証は [シナリオ O](/ja/validation/forceful-fallback)
を参照。

**実施**: 2026-07-14T14:20:29Z（T0）→ +12h、EKS Auto Mode、Kubernetes 1.36、
`us-west-2`。正本の記録はリポジトリの `test/e2e/eks-automode/VALIDATION.md`
（§ "Run: 2026-07-15 — Scenario P"）。

**判定: 適用可能な全基準で CLEAN PASS**（12 PASS。基準 13 の解凍失期アボート則は
未行使のため N/A）。

| 71<small>/71</small> | 0 | 68.3<small> 分</small> | 56<small> 秒</small> |
|:---|:---|:---|:---|
| 本編の graceful 回転 — 12h 窓内 60（5.0 回/h）、記録終了（T0+14.1h）までに 71 | `expired`・`failure`・本編 fallback・`short_lead`・restart | deadline margin 最小値（中央値 70.3 / 最大 71.2） | エピローグ解凍 → surge-less fallback 完了 |

## 検証の狙いと導出値

過去の PoC は日次窓のため `expireAfter` が巨大（E=336h）になり、Karpenter の
Forceful Expiration との競争は常に大差勝ちだった。本検証は E を **2h12m —
`leadTime` 1h12m のすぐ上** — に置き、バックストップと本気で競争する状態を
12 時間持続させた。`surge.forcefulFallback` は**装備したまま**であり、
「必要のない間は発動しない」（静穏性）も同時に実証している。

| 量 | 導出 |
|---|---|
| `t_rot`（上界） | `readyTimeout 5m + tGP 5m + Buffer 2m = 12m` |
| `t_rot_est`（予測） | `min(readyTimeout, 5m) + min(tGP, 10m) = 5m + 5m = 10m` |
| `leadTime` | `K·P + t_rot = 2·30m + 12m = 1h12m` |
| `ageThreshold`（auto） | `A = E − leadTime = 2h12m − 1h12m = 1h` |
| `C`（窓あたり容量） | `ceil(D / (t_rot_est + cooldownAfter)) = ceil(28m / 12m) = 3`（6 回/h） |
| 定常負荷 | N=5、到着率 N/A = 5 回/h → 予測容量の **83%**（意図的にタイト） |
| 期待 findings | warn の `RotationSpansNextWindow` が**ちょうど 1 件のみ** — 窓間ギャップ 2m < `t_rot_est` + cooldown 12m のため構造的に不可避。`ThroughputBelowArrival` / `ThroughputBurstShortfall` は出ないこと |

導出値は
[`internal/schedule/schedule_test.go`](https://github.com/AkashiSN/node-rotation-controller/blob/main/internal/schedule/schedule_test.go)
の `TestDeriveScenarioPSoak` にピン留めされている — 再試験前にこのテストが通る
ことが「構成が本レポートと同一」の証明になる。

## margin の全体像

T0 から記録終了（T0+14.1h — 正規の 12h 観測窓 + 有人エピローグ期間）までに
本編プールは **71 回転、すべて新規 provision ノードへの graceful
make-before-break surge** を完了した。内訳は `[T0, T_end]` 内が 60 回転 —
予測どおりちょうど 5.0 回/h — で、残り 11 回転はエピローグの準備・発火と
並走しながら約 12 分間隔の同じ刻みで続いた。margin（= 旧 claim の deadline −
回転完了時刻）は 71 回転すべてで 68.3〜71.2 分に収まり、分散は 3 分未満 —
**走らせ続けても劣化の蓄積が皆無**だったことを示す。

<SoakMarginChart />

## 13 基準

| # | 基準 | 判定 | 観測値 |
|---|---|---|---|
| 1 | `outcome="expired"` == 0 | PASS | 本編 + tail-follow + エピローグを通して 0。Karpenter の `Expiration` イベントも 0 |
| 2 | `success` が ≈5/h で単調増加、計 ≥ 40 | PASS | `[T0, T_end]` 内 60（5.0 回/h）、記録終了までに 71（epi 込み 72）、~12m 間隔 |
| 3 | 本編 `forceful_fallback_total` == 0（装備したまま） | PASS | 全期間 0 — 静穏性の初実証 |
| 4 | `short_lead_nodes` == 0（全スクレイプ） | PASS | 909 スクレイプで max 0 |
| 5 | restart 0・scraper `seq` 連続 | PASS | controller `restartCount=0`（12h）、gap 0・restart 0・`SCRAPE_ERROR` 0 |
| 6 | 負荷の継続（全スナップショット） | PASS | desired=available=ready=5 を維持、Pending 滞留なし |
| 7 | 検証対象構成の常時一致 | PASS | 導出ゲージ 6 本 × 909 スクレイプで不一致 0、両 policy `Accepted`、findings は期待した warn 1 件のみ |
| 8 | 全回転 margin > 0 | PASS | min 68.3 / 中央値 70.3 / max 71.2 分（n=71） |
| 9 | 終了 census | PASS | T_end の 5 claim は齢 11〜59 分（right-censored）、滞留 0・`failed` 0 |
| 10 | Karpenter の予期しない disruption なし | PASS | `DisruptionBlocked`/`Unconsolidatable` のみ（budgets と `do-not-disrupt` が抑止した証拠） |
| 11 | epi: 凍結中は候補のみで無回転 | PASS | 凍結中の約 2h、`candidates=1` / `in_progress=0` を維持 |
| 12 | epi: 解凍で fallback が決定的に発火 | PASS | 下のエピローグ節を参照（56 秒、placeholder ゼロ、本編無擾乱） |
| 13 | アボート則（解凍失期） | N/A | 正常リリースにより未行使（fail-closed 挙動は offline 試験でのみ検証） |

## ノード入れ替えの解剖

71 回転はすべて同型の観測シーケンスをたどった:

1. claim が齢 **1h**（= A）に達し候補化 → controller が低優先度の
   **placeholder Pod**（`noderotation-surge-<claim>`、label
   `noderotation.io/surge-for`）を作成。
2. Karpenter が placeholder を bin-pack できず**新ノードを provision**
   （surge claim 誕生）。placeholder が bind → surge ノード Ready
   （**surgeWait: 中央値 34 秒**、23〜54 秒）。
3. 両ノードに `karpenter.sh/do-not-disrupt` を付与 → 旧 `NodeClaim` を削除 →
   Karpenter の voluntary パスで drain（**drain: 中央値 44 秒**、18〜83 秒）。
   workload pod は surge ノードへ再着地。
4. 後始末（注釈剥がし・placeholder 削除）まで含めた **total: 中央値 81 秒**
   （45〜131 秒）。空になった旧ノードの残骸は `WhenEmpty`/60s で回収。

<SoakAnatomyChart />

<SoakLedger />

## エピローグ — fallback の決定的発火

本編の「発動しない」だけでは fallback の検証として片翼なので、**凍結した
単一ノードの別プール**（`nodepool-soak-epi`）で境界越えを人為的に、しかし
決定的に作った。プール規模を 1 にしたのは、forceful はプール内直列で 1 件の
完了に法的最大 `tGP + Buffer = 7m` かかり得るため、複数台では 12 分の
リリース帯内の完了を*保証*できないからである（複数台の混合実績は
[シナリオ O](/ja/validation/forceful-fallback) が保有）。

| | |
|---|---|
| 02:22:50Z | epi claim `gtx42` 誕生（プールは `noderotation.io/freeze` 注釈付きで作成済み）→ deadline **d = 04:34:50Z** |
| 03:22:50Z → | 齢 1h で候補化。凍結中のため `candidates=1` / `in_progress=0` のまま待機（freeze 意味論が有人監視の全時間で持続） |
| 04:23:50Z | `soak-epi-release.sh` が区間探索（d−11m 以降の最初の窓内時刻、d−12m < R < d−8m を証明）した R で凍結解除。残余 11 分 < `t_rot` 12 分 → graceful surge は不成立 |
| 04:24:46Z | **解凍から 56 秒後**、claim 削除完了（drain 実測 48 秒 < 上界 7 分）。ログの `mode=forceful-fallback` と `surgeNode` フィールドの不在が、それ自体 surge-less パスの証明 |
| 証拠 | `forceful_fallback_total{nodepool-soak-epi}` 0→1 ・ spec 文言どおりの `ForcefulFallback` Warning イベント ・ placeholder 台帳（`noderotation.io/surge-for` の `pods -w` 連続監視）に epi claim の記録**ゼロ** ・ `expired` 終始 0 ・ 本編プールはエピローグ中も無擾乱（解凍→発火の窓で 70→71、完了間隔 ≤ `P + t_rot` = 42m） |

解凍失期時のアボート則 — d−8m までに解凍できなければ**凍結したまま**プールを
撤去する（遅れた解凍では失効を防げない）— は今回未行使。リリーススクリプトの
fail-closed 挙動は kubectl スタブによる offline 試験で 3 系統とも exit 3 を
確認済み。

## 再試験するには

ランブックは
[`test/e2e/eks-automode/SCENARIOS.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/test/e2e/eks-automode/SCENARIOS.md)
§ Scenario P。マニフェスト・スクリプトはすべて
`test/e2e/eks-automode/scenarios/` にコミット済み。AWS に触れる前に:

```sh
go test ./internal/schedule/ -run TestDeriveScenarioPSoak -v  # 導出ピン: A=1h / C=3 / G=2 / warn 1 件
test/e2e/eks-automode/scenarios/soak-analyze-fixture.sh       # 解析系の自己検査
```

本検証で得た運用ノート:

- **JSON ロギング（`logging.development: false`）が前提**: 解析器は zap の
  JSON 形式を読む。console 形式では台帳が空になる。
- **認証トークンの寿命を事前確認する。** 操作端末の認証が 12h 走行の途中で
  切れ、ローカル（二次）記録が 15 分盲目化した。一次記録であるクラスタ内
  scraper は無欠損 — 二層記録の設計が想定どおり機能した。
- **`expireAfter` の live patch 禁止** — 変えたければプールを作り直す
  （patch は Karpenter drift を誘発）。
- コスト実績: 約 11 USD（コントロールプレーン + NAT ~20h、2 vCPU ノード
  定常 5 台 × 14h + エピローグ）。**終了後の `terraform destroy` を
  忘れないこと。**

## 本検証が確定させたこと・残るもの

仕様書 §7.2 に validated 行が 2 つ加わった: サブデイリー窓の下で導出された
`leadTime` は genuine に競っている `expireAfter` に 12 時間勝ち続け、fallback は
装備されたまま一度も必要にならない。そして claim が graceful surge の組めない
点を越えた瞬間、窓内 forceful fallback は決定的に発火する。実クラウドに残る
未検証項目は**同一 AZ の実容量枯渇（ICE）のみ**になった —
[ロードマップ（§6.2）](/ja/specification/06-release#62-ロードマップ) と
[§7.2](/ja/specification/07-risks#72-検証済み前提) を参照。
