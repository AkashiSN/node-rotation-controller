# 運用ランブック

`node-rotation-controller` を実クラスタで運用するための手引き。設計の *なぜ* は
[仕様書](specification.md)が source of truth であり、本ランブックは運用者向けの
*どうやって* を扱う。各節は対応する仕様セクションへリンクしている。

英語原文: [docs/runbook.md](../runbook.md)。

> 本コントローラは pre-1.0 であり、実際の EKS Auto Mode クラスタでの soak テストは
> 未実施（[§6.2 ロードマップ](specification.md#62-ロードマップ)を参照）。本ランブックは
> production 展開の出発点であって、保証ではない。

## 目次

1. [ゾーン制約 PV ワークロード向けの AZ ごとの surge ヘッドルーム](#1-ゾーン制約-pv-ワークロード向けの-az-ごとの-surge-ヘッドルーム)
2. [Auto Mode の `terminationGracePeriod` を下げる](#2-auto-mode-の-terminationgraceperiod-を下げる)
3. [`noderotation_*` メトリクスの読み方](#3-noderotation_-メトリクスの読み方)
4. [freeze ワークフロー](#4-freeze-ワークフロー)
5. [drain が詰まったときの対処](#5-drain-が詰まったときの対処)
6. [アラート（PrometheusRule）](#6-アラートprometheusrule)

---

## 1. ゾーン制約 PV ワークロード向けの AZ ごとの surge ヘッドルーム

**対象:** **ゾーン制約** PersistentVolume（EBS `gp3`/`io2`、または
`nodeAffinity` に `topology.kubernetes.io/zone` 制約を持つ PV）に紐づくワークロードを
前段に置く NodePool。

**なぜ重要か。** surge は make-before-break であり、コントローラは古いノードを drain する
*前に* 置換ノードを追加する。ゾーン制約 PV ワークロードでは、既存ボリュームが再アタッチ
できるよう、置換ノードは **候補の AZ に固定される** —
`topology.kubernetes.io/zone` は `surge.matchNodeRequirements` の既定 `required`
集合に含まれる
（[§3.3 *ステートフル／ゾーン制約のワークロード*](specification.md#33-surge-シーケンスv1)）。
コントローラはゾーンストレージを AZ 間で移動できないし、するべきでもない。

その帰結は **ハードな制約** である: 同一 AZ の容量不足は **別ゾーンへフォールバック
できない**。候補の AZ に同一ゾーン置換用のスケジュール可能な容量がなければ surge は完了
できない。placeholder Pod が `Running` にならず `readyTimeout` が発火し、ローテーションは
`expireAfter` ベースラインへロールバックする
（[§3.3 *ロールバック挙動*](specification.md#33-surge-シーケンスv1)）。同一 AZ 不足が
繰り返されると `noderotation_retry_count` の増大として現れる
（リスク [R3](specification.md#71-リスク)）。

**指針。** ゾーン制約 PV ワークロードを前段に置く各 NodePool では、**使用中の AZ ごとに
ノード 1 台分の surge ヘッドルームを確保する**:

- NodePool の `requirements` が **使用中のすべての AZ** を許可していることを確認する
  （ボリュームが複数ゾーンにまたがるなら `topology.kubernetes.io/zone` を 1 ゾーンに
  絞らない）。
- NodePool の `spec.limits` リソース予算を、*各* AZ で定常状態のフットプリント＋ノード
  1 台分の余地が残るよう設定する。コントローラはローテーション開始前に pool 全体の
  `limits` ヘッドルームを事前チェックするが（§5.2 step 3）、`limits` は **pool 全体の
  リソース予算であり AZ ごとの台数ではない** — 「`us-east-1a` に予備ノード 1 台」を表現
  できない。AZ ごとのヘッドルームは運用者の責任である。
- 使用する **各** AZ で（集計値だけでなく）基盤プロバイダに容量があり、EC2 vCPU クォータ
  にも余地があることを確認する。
- クラウドプロバイダが対応していれば、使用中の各 AZ で surge ノードのインスタンス形状に
  対するキャパシティ予約を検討する。

**不足の検知方法。** 同一 AZ 不足は `readyTimeout` 起因のロールバック
（`noderotation_completed_total` の `failure` 結果）と `noderotation_retry_count`
の増大（アラート: `NodeRotationRetryCountHigh`、[§6](#6-アラートprometheusrule)）として
現れる。このアラートがゾーン制約 PV の NodePool で発火したら、まず AZ ごとの容量不足を
疑うこと。

---

## 2. Auto Mode の `terminationGracePeriod` を下げる

**対象:** EKS Auto Mode の NodePool（既定の `terminationGracePeriod`（`tGP`）が
`24h`）。

**なぜ重要か。** コントローラのスループットモデル
（[§3.2 layer 2](specification.md#32-候補選定)）は、Karpenter が drain を束縛できる上限が
それであるため、各ローテーションについて `tGP` 全体を drain に要しうる時間として見込まねば
ならない。単一ノードのローテーション上限は `t_rot = readyTimeout + tGP + buffer` であり、
長さ `D` のウィンドウは直列で `C = floor(D / (t_rot + cooldownAfter))` 台を回せる。

既定の `tGP = 24h` では `t_rot ≈ 24.5h` となり、典型的な 4 時間ウィンドウは
`C = floor(4h / (24.5h + 10m)) = 0` を計算する — PDB を尊重する drain は通常数分で
終わるにもかかわらず、コントローラは **毎ウィンドウで警告する**（`ThroughputZero`）。
モデルは正しい: drain が速いと仮定できないだけである。

**指針。** NodePool の `spec.template.spec.terminationGracePeriod` を **現実的な
ノードあたりの drain 上限** に下げる — [§3.2](specification.md#32-候補選定) の worked
example は `1h` を用いる。`tGP = 1h` なら `t_rot ≈ 1.5h` となり、同じ 4 時間ウィンドウは
`C = floor(4h / (1.5h + 10m)) = 2` 回/occurrence を与える。

`tGP` を下げると、さらに以下の効果がある（いずれもここでは有益）:

- **Auto Mode の 21 日ハードキャップが緩む**（`E + tGP ≤ 21d`、
  [§1.1](specification.md#11-背景)）。`tGP = 1h` は `expireAfter` を最大 ~`20d` まで
  許容し、これは疎な（例: 週次）ウィンドウで lead-time 導出を満たすのに必要なヘッドルーム
  そのものである。
- **stuck-drain 判定が厳しくなる**。`noderotation_drain_stuck` は `tGP + buffer` で
  発火するため、`tGP` が小さいほど詰まった drain を早く表面化できる
  （[§5](#5-drain-が詰まったときの対処)）。

**トレードオフ。** 本当に遅い drain は `24h` ではなく **`tGP` で強制完了される**。
`tGP` はワークロードの実際の PDB 尊重 drain 時間から選ぶこと — 健全な drain が自発的に
終わる程度には長く、スループットモデルが通る程度には短く。例の `1h` を盲目的にコピーしない
こと。

> `tGP` が未設定（self-managed Karpenter は nil を許容）の場合、drain は Karpenter に
> 束縛されない。コントローラはスループットモデルと stuck-drain アラートの両方に固定の
> フォールバック上限（例: `1h`）を代入する
> （[§3.2 layer-1 `TGPUnset` 警告](specification.md#32-候補選定)）。

---

## 3. `noderotation_*` メトリクスの読み方

`/metrics` で公開される（[§4.2](specification.md#42-観測性)）。以下の名前とラベルは
コントローラが出力する **正確な** 文字列である。NodePool 単位の系列は **NodePool 削除時に
クリアされる** ため、削除された pool が最後の値を保持し続けることはない。クラスタ全体の
`noderotation_window_active` は影響を受けない。

| メトリクス | 型 | ラベル | 読み方 |
|--------|------|--------|------------|
| `noderotation_candidates` | Gauge | `nodepool` | ローテーション待ちの適格 NodeClaim 数。各ウィンドウ内/後で **0 に向かうべき**。2 ウィンドウにわたり > 0 のままなら遅延（[R2](specification.md#71-リスク)）。 |
| `noderotation_in_progress` | Gauge | `nodepool` | 進行中のローテーション数（v1 は直列なので 0 か 1）。 |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` | 累積完了数。`outcome ∈ {success, failure, expired}`。`expired` = 優雅なローテーション完了 **前に** 古いノードが Forceful Expiration された（lead-time レースに負けた — [§3.5](specification.md#35-バックストップ挙動)）。`success` には数えない。 |
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` | フェーズ別レイテンシ。`phase ∈ {surge_wait, drain}`。`surge_wait` 増大 ≈ プロビジョニングが遅い/失敗、`drain` 増大 ≈ eviction が遅い。 |
| `noderotation_window_active` | Gauge | — | `0/1` のクラスタ全体ウィンドウ所属。**設計上ラベルなし**（v1 のウィンドウは単一 union）。 |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` | 有効な freeze の Unix タイムスタンプ（`0` = freeze なし）。非ゼロ → ローテーションが **意図的に抑止** されている（[§4](#4-freeze-ワークフロー)）。 |
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` | 導出された `ageThreshold` `A`（[§3.2](specification.md#32-候補選定)）。pool ごとに異なる。 |
| `noderotation_rotation_chances` | Gauge | `nodepool` | 導出しきい値での保証ローテーション回数 `G`。auto 導出では `G = K`。override は下げうる（かつ検証される）。 |
| `noderotation_window_period_seconds` | Gauge | `nodepool` | 最悪ケースのウィンドウ周期 `P`。v1 では全 pool で同一（ウィンドウはクラスタ全体）。`nodepool` ラベルは将来を見越したもの。 |
| `noderotation_short_lead_nodes` | Gauge | `nodepool` | **自身の** 刻印済み `expireAfter` で `K` 回を保証できなくなった NodeClaim（[§3.2 layer 3](specification.md#32-候補選定)）。best-effort でローテーションされるが Forceful Expiration に至りうる。 |
| `noderotation_drain_stuck` | Gauge | `nodepool` | `0/1`: 進行中の drain が `tGP + buffer` を超過。`1` → 運用者の対処が必要（[§5](#5-drain-が詰まったときの対処)）。 |
| `noderotation_retry_count` | Gauge | `nodepool` | pool の NodeClaim 全体での最大 `noderotation.io/retry-count`（なければ `0`）。`≥ 3` → **systematic** な失敗（継続的な preemption か同一 AZ 不足、[R3](specification.md#71-リスク)）。 |

**Warning Events。** 非致命的な finding は NodePool / NodeClaim 上の Kubernetes
`Warning` Event としても表面化されるため
（[§4.2](specification.md#42-観測性)）、`kubectl describe nodepool <name>` で
メトリクススタックなしに確認できる。Reason には `KBelowTwo`、`AVeryAggressive`、
`TGPUnset`、`HardCapExceeded`、`ThroughputZero`、`ThroughputBelowArrival`、
`OverrideGBelowK`、`ShortLead` などがある。

---

## 4. freeze ワークフロー

**目的。** 単一の NodePool のローテーションを指定時刻まで抑止する — 例: 業務クリティカルな
期間 — コントローラをアンインストールせず、`expireAfter` バックストップも失わずに。

**仕組み。** **NodePool** に freeze アノテーションを RFC3339 タイムスタンプ値で設定する
（[§3.1](specification.md#31-メンテナンスウィンドウ)）:

```sh
kubectl annotate nodepool <name> \
  noderotation.io/freeze='2026-12-31T23:59:59Z' --overwrite
```

freeze が有効な間、コントローラは:

- その pool で新規ローテーションを **開始しない**;
- まだ `pending` の進行中ローテーションを **保留する** — drain は未開始なので一時停止は
  安全。placeholder の（再）作成と `pending → draining` 遷移は中断される;
- **受動的な記帳は継続する**（保護用 `do-not-disrupt`/cordon マーカーの再表明、
  surge-claim 識別の永続化）ため、freeze はクラッシュ復旧保証を弱めない;
- すでに `draining` のローテーションは **完走させる** — drain は途中で安全に中断できない。

freeze が `readyTimeout` を超えて続くと、保留中の `pending` 試行は通常の失敗パスで
ロールバックする。`noderotation_freeze_until_timestamp` が有効な freeze を報告する
（`0` = なし）。

**アドホックではなく GitOps で管理する**（リスク [R5](specification.md#71-リスク)）。
忘れられたアドホック freeze はそのタイムスタンプが過ぎるまで pool のローテーションを
密かに無効化する。バックストップが `expireAfter` でノード齢を束縛し続けるが、優雅なパスは
止まる。freeze は GitOps リポジトリで宣言してレビューで可視化し、**ソースから削除した
ときに失効** させること（誰かが解除を思い出したときではなく）。早期に解除するには:

```sh
kubectl annotate nodepool <name> noderotation.io/freeze- # アノテーション削除
```

---

## 5. drain が詰まったときの対処

**症状。** ある NodePool で `noderotation_drain_stuck == 1`（アラート:
`NodeRotationDrainStuck`）。

**意味。** 進行中ローテーションが `draining` に入り（コントローラはすでに古い NodeClaim を
削除済みで、Karpenter が voluntary な PDB 尊重パスでノードを drain している）、その drain が
`tGP + buffer` を超過した（[§5.2](specification.md#52-reconcile-ループ)）。この gauge は
毎 reconcile でライブ状態から再計算されるため、drain が終わった瞬間に **自動的にクリア
される**。

**重要 — シリアルゲートは意図的に保持される。** `draining` のローテーションは
**ロールバックできず**（古い NodeClaim にはすでに `deletionTimestamp` がある）、
コントローラはこれが詰まっている間 **2 本目のローテーションを開始しない** —
NodePool 単位のゲートを解放すると 1 本目が半端に drain された状態で 2 ノード目を disrupt
することになり、`surge.maxUnavailable = 1` に違反する。よって詰まった drain は **その
NodePool の全ローテーションをブロックする** までクリアされない。他の NodePool は影響を
受けない。

**対処は運用者側。** ブロッカーはほぼ常に **満たせない PDB** か Pod の **詰まった
finalizer** である。

1. drain 中のノードと NodeClaim を見つける:

   ```sh
   kubectl get nodeclaim -l karpenter.sh/nodepool=<name> \
     -o wide | grep -i terminating
   kubectl get node <node> -o yaml | grep -A3 deletionTimestamp
   ```

2. eviction をブロックしているものを見つける:

   ```sh
   # ノード上に残る Pod
   kubectl get pods --all-namespaces \
     --field-selector spec.nodeName=<node> -o wide
   # PDB と許容される disruption
   kubectl get pdb --all-namespaces \
     -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,ALLOWED:.status.disruptionsAllowed,CURRENT:.status.currentHealthy,DESIRED:.status.desiredHealthy
   ```

   `ALLOWED = 0` の PDB が典型的な原因 — ワークロードの健全レプリカが少なすぎて 1 つも
   譲れない。コントローラではなく **PDB かワークロードを直す**（PDB が disruption を
   許すようスケールアップする、または満たし得ない `minAvailable`/`maxUnavailable` を
   修正する）。

3. **詰まった finalizer** の場合、`metadata.finalizers` が消えない Pod を特定し、その
   finalizer を所有するコントローラ側で解消する。

4. **`tGP` が設定されている場合**、Karpenter は最終的に `tGP` で **drain を強制完了** する
   — よって詰まった drain は `tGP` 以内に自己解決し、stuck-drain アラートは *優雅な*
   drain が終わっていないという予告であって、ノードが永久に詰まっているわけではない。
   これが `tGP` を束縛しておく実務上の理由である
   （[§2](#2-auto-mode-の-terminationgraceperiod-を下げる)）。**`tGP` が未設定**
   （self-managed Karpenter）の場合、Karpenter 側の強制が **ない** — ブロックする PDB や
   詰まった finalizer が drain を無期限に保持しうるため、運用者の対処だけが解決手段である。

コントローラのアノテーションや placeholder を削除してローテーションを「こじ開けよう」と
**しないこと** — ローテーションは NodePool アンカーから再開され
（[§5.2](specification.md#52-reconcile-ループ)）、ハンドラは冪等である。根本の
PDB/finalizer を直し、drain を完了させること。

---

## 6. アラート（PrometheusRule）

Helm chart は [§4.2](specification.md#42-観測性) の 6 アラートを含む **任意の**
`PrometheusRule` を同梱する。既定では **オフ**（既存インストールや Prometheus Operator の
ないクラスタに影響しないように）。有効化:

```sh
helm upgrade --install rot charts/node-rotation-controller \
  --set prometheusRule.enabled=true
```

| アラート | 式 | 意味 |
|-------|------------|-------|
| `NodeRotationCompletedFailureOrExpired` | `increase(noderotation_completed_total{outcome=~"failure\|expired"}[1h]) > 0` | 直近 1 時間でローテーションが失敗、またはノードが force-expire された。 |
| `NodeRotationCandidatesNotDraining` | `min_over_time(noderotation_candidates[<2·P>]) > 0` | 2 ウィンドウ連続で候補がはけていない（[R2](specification.md#71-リスク)）。 |
| `NodeRotationStalledInWindow` | window active **かつ** candidates `> 0` **かつ** 完了ゼロ | メンテナンスウィンドウ内でローテーションが詰まっている。 |
| `NodeRotationDrainStuck` | `noderotation_drain_stuck == 1` | drain が `tGP + buffer` を超えてブロック — [§5](#5-drain-が詰まったときの対処)。 |
| `NodeRotationShortLeadNodes` | `noderotation_short_lead_nodes > 0` | 刻印済み `expireAfter` で `K` 回を保証できなくなった NodeClaim。 |
| `NodeRotationRetryCountHigh` | `noderotation_retry_count >= 3` | 同一ローテーションが失敗し続ける — systematic な原因（[R3](specification.md#71-リスク)）。 |

**スケジュール依存のレンジを調整する。** 2 つのアラートはウィンドウ周期 `P` とウィンドウ
長 `D` に依存する。これらのレンジはハードコードではなく **chart の値** である:

- `prometheusRule.candidatesNotDraining.windowRange` — おおよそ **2 ウィンドウ周期**
  （`2·P`）に設定する。既定の `8d` は `{Wed, Sat}` スケジュール（`P = 4d`）に合う。
  週次ウィンドウ（`P = 7d`）なら `14d` に上げる。
- `prometheusRule.stalledInWindow.completionRange` — おおよそ **ウィンドウ長**（`D`）に
  設定する。既定の `4h` は `02:00–06:00` ウィンドウに合う。

各アラートの `for` と `severity` も設定可能
（[`values.yaml`](../../charts/node-rotation-controller/values.yaml) を参照）。
`min_over_time`/`increase` のレンジは Prometheus subquery を意図的に避けてルールを軽く
保っている。スケジュールを大きく変える場合は subquery をネストするのではなく記録ウィンドウ
を広げること。
