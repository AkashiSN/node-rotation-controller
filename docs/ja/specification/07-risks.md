# 7. リスクと状況

## 7.1 リスク

| # | リスク | 対策 |
|---|--------|------|
| R1 | コントローラ Pod クラッシュ / リーダー喪失 | `replicas=2` + leader election、`expireAfter` バックストップ、failure メトリクスでアラート |
| R2 | 窓内に全候補を捌けない | §3.2 のスループット検証が事前に警告。`noderotation_candidates` が 2 窓連続で減らない場合にアラート。将来バージョンで `maxUnavailable > 1` を検討 |
| R3 | 代替 NodeClaim が立たない（容量不足 / AZ 枯渇 / NodePool の `limits` リソース予算が枯渇）| コントローラは開始前に NodePool `limits` のリソース予算の余地を事前確認し（§5.2）、placeholder の requests が収まらなければ警告。それ以外は Ready タイムアウトでロールバック。NodePool は複数 AZ / 複数インスタンスタイプを許容する設計を維持。**ゾーンの注意点:** surge ノードは zonal-PV 再バインドのため置換対象の AZ に固定される（§3.3 *ステートフル／ゾーン制約のワークロード*）ため、同一 AZ の容量不足を別ゾーンへフォールバックできない — zonal-PV ワークロードを抱える NodePool では各 AZ ごとに surge の余裕を残すこと |
| R4 | 誤設定の PDB で drain がブロックされる | Karpenter の `terminationGracePeriod` で最終的に強制 drain。PDB のレビューはアプリ owner の責務 |
| R5 | 業務クリティカル期間の freeze 付与忘れ | freeze annotation は GitOps 等で宣言的に管理する設計を推奨 |
| R6 | テストクラスタが日次で turnover する場合の検証ギャップ | age 閾値を超えるソーク期間を設けて end-to-end 置換を検証 |

## 7.2 検証済み前提

| 前提 | 状態 | 根拠 |
|------|------|------|
| standalone（NodePool-owned でない）`NodeClaim` が EKS Auto Mode で *プロビジョニング可能* | **検証済** — K8s 1.35 / `karpenter.sh/v1`（2026-05-29）| `nodeClassRef`（マネージド `eks.amazonaws.com/NodeClass`）+ `requirements` のみの NodeClaim が約 30 秒で `Ready`（実 EC2・node 登録）に到達。admission も受理（`--dry-run=server`）、finalizer 駆動の graceful 削除も確認 |
| §3.3 の **placeholder Pod surge** が NodePool-owned な置換を誘発し make-before-break で完了する | **検証済** — EKS Auto Mode / K8s 1.33 / `karpenter.sh/v1`（2026-06-22）| 低優先度の placeholder Pod が新規 NodePool-owned な surge `NodeClaim` を誘発し、それが旧 `NodeClaim` 削除 **より前に** `Ready`（約 30 秒）に到達。削除により旧ノードは voluntary 経路で drain され、ワークロードは surge ノードへ再スケジュール。`noderotation_completed_total{outcome="success"}` が増加 |
| 同一 AZ surge により EBS CSI ドライバが **zonal な EBS ボリューム** を再アタッチできる（zonal-PV 再バインド、§3.3）| **検証済** — EKS Auto Mode（2026-06-22）| StatefulSet の gp3 PVC が `us-west-2a` にバインド。複数回のローテーションを通じて `matchNodeRequirements` の zone パリティが全ての surge ノードを `us-west-2a` に保ち、**同一** PV が（再プロビジョニングでなく）再アタッチされ sentinel データが保持された |
| `readyTimeout` に間に合わなかった surge が **クリーンにロールバック** する（§3.2 / §5.2、R3）| **検証済** — EKS Auto Mode（2026-06-22）| `readyTimeout` をノード Ready 到達時間より短く設定すると attempt がタイムアウト → 誘発した surge claim を reap、placeholder 削除、候補ノードを uncordon/unfreeze、候補は **保持**（ローテーションされず）、`retry-count` 増加、`noderotation_completed_total{outcome="failure"}` 増加 |
| NodePool `limits` 枯渇が surge を開始前に **ゲート** する（§5.2 ステップ3）| **検証済** — EKS Auto Mode（2026-06-22）| `spec.limits.cpu` で余地を残さないと、適格な候補（`noderotation_candidates=1`）でも surge せず — コントローラは `insufficient limits headroom; cannot surge` をログし、`noderotation_in_progress` は 0 のまま、置換ノードは作られなかった |
| required な `karpenter.sh/nodepool` セレクタが surge のバインドとプロビジョニングを候補のプールに **閉じ込める**（§3.3）| **検証済** — EKS Auto Mode（2026-06-22）| 2 つ目の非 in-scope NodePool が同一 AZ に ~1.7 cpu 空き（~1 cpu の placeholder には十分）のノードを持つ状態でも、nrc-poc の surge は nrc-poc に留まった — surge `NodeClaim` は `karpenter.sh/nodepool=nrc-poc`、placeholder は `pool=poc` ノードにバインド、他プールの `NodeClaim` 一覧は不変（吸収もプロビジョニングもなし）|
| 明示的な `NodeClaim` 削除が旧ノードを Karpenter の **voluntary（PDB 尊重）** 経路で drain する（§3.3）| **検証済** — EKS Auto Mode（2026-06-22）| ブロッキング PDB（`minAvailable=2`、2 レプリカ）で drain が **停滞** — 両レプリカが旧ノードに留まり旧 `NodeClaim` も残存（forceful 経路なら即 evict）。`minAvailable=1` に緩和するとレプリカが 1 台ずつ移動しローテーション完了 |
| コントローラが surge 中の **新旧両ノード** に `karpenter.sh/do-not-disrupt` を付与し、完了時に除去する（§3.3）| **検証済** — EKS Auto Mode（2026-06-22）| surge 中、旧候補ノードと新 surge ホストの両方が `karpenter.sh/do-not-disrupt=true` とコントローラの `do-not-disrupt-owned` マーカーを保持。完了時の unfreeze で両方除去（`expireAfter` をブロックしないことは文書化済み Karpenter 挙動で再検証せず）|
| pending 中に **捕捉された強制失効** が success/failure ではなく `outcome="expired"` を記録する（§5.2）| **検証済** — EKS Auto Mode（2026-06-22）| プールを freeze し pending 中の候補 `NodeClaim` を削除すると `abortPendingExpiry` が走り、`state=expired`・プールアンカー clear・surge 残存なし・`noderotation_completed_total{outcome="expired"}` が増加 |
| `expireAfter` がコントローラの lead time が勝ち切る **バックストップ** であり続ける（R6）| **検証済**（構成上 + 実観測）— EKS Auto Mode（2026-06-22）| 候補は `expireAfter=336h` を保持しつつ、コントローラは `ageThreshold=5m` でローテーション。`noderotation_rotation_chances=13`・`noderotation_short_lead_nodes=0`。スケール版 soak（3 回連続ローテーション）で全ノードが約 5m で入れ替わり（`completed_total{outcome="success"}` +3、`expired`/`failure` なし、`short_lead_nodes` 0 一貫）、バックストップの遥か手前。フルな数時間の *接戦* soak は先送り（日次ウィンドウは大きな `expireAfter` を強制するため接戦にならない）|
| placeholder が新ノードを誘発せず **同一プールの既存空きキャパへ bin-pack** する（capacity-absorb 経路、§3.3）| **検証済** — EKS Auto Mode（2026-06-23）| *若い*（`ageThreshold` 未満＝非候補）同一 AZ の空きノードが ~1970m 空きを持つ状態で、候補の 250m placeholder が **そのノードへ直接** スケジュールされ（`Successfully assigned`、`FailedScheduling` なし）、**新規 `NodeClaim` は一切誘発されなかった** — プールの claim 数はローテーション全体で 2 のまま（3 にならず）。`surge-for` マーカーは **既存** の空きノード（surge ターゲット）に付き、drain された候補 Pod は予約済みヘッドルームへ再着地、プールは 1 ノードへ収束し、`noderotation_completed_total{outcome="success"}` が増加。（この経路を強制するのは非自明: 空きノードは若くなければならない — 期限間近の空きノードは soft 除外され（§3.3、issue #96）Karpenter がプロビジョニングのレースに勝ってしまう — かつ両ワークロードは相互の Pod anti-affinity を持ってはならない。anti-affinity は対称的で、drain された Pod が空きノードへ再着地するのを阻むため。2 ノードはサイズ設定で分離する。）|
| 新 leader が **進行中ローテーションを annotation のみから再開** する（§5.1）| **検証済** — EKS Auto Mode（2026-06-23）| ローテーション途中（`state=pending`、`surge-claim`/`started-at` 記録済み）で leader Pod を kill すると、新レプリカが Lease を取得し **同一** ローテーションを継続 — `surge-claim` と `started-at` が同一のまま pending→draining→complete へ進行 — in-memory 状態なし、ローテーションのやり直しなし |
| 進行中ローテーションは **ウィンドウ境界を越えて完了** し、境界後は新規ローテーションが始まらない（§3.1）| **検証済** — EKS Auto Mode（2026-06-23）| ウィンドウ内で開始したローテーション（`state=pending`）は、ウィンドウを閉じても（`maintenanceWindows` 変更 + コントローラ再起動）中断されず完了。一方 2 つ目の適格候補（`noderotation_candidates=1`、`noderotation_window_active=0`）は開始せず、`noderotation_in_progress=0` |
| placeholder は **preempt の被害者**（負優先度、`preemptionPolicy: Never`、bare Pod）であり、持続的な高優先度の圧力は **`readyTimeout` で有界な rollback** に終わる（§3.3）| **検証済** — EKS Auto Mode（2026-06-23）| 高優先度 Pod が placeholder を preempt（`Preempted` イベント）。placeholder は場所を空けるために他を preempt せず（`preemption: not eligible due to preemptionPolicy=Never`）、bare Pod なので再 pending しない（コントローラのみが再作成）。高優先度 blocker が唯一の同一 AZ 空きを占有し新ノードが `limits` で禁止された状態で、placeholder は `readyTimeout` まで `Pending` のまま → クリーンな rollback: 候補は保持 + uncordon、surge は reap、`noderotation_completed_total{outcome="failure"}` +1、`retry-count=1` |
| Karpenter が voluntary（Drift）disruption に対して `do-not-disrupt` を **honor する**（§3.3、#95）| **検証済** — EKS Auto Mode（2026-06-23）| `karpenter.sh/do-not-disrupt=true`（コントローラが付与するのと同じ値）を持ち、NodePool `spec.template` 変更で `Drifted=True` にしたノードは 3 分以上置換されなかった。annotation を除去すると Karpenter は即座に drift で置換（make-before-break）。上の「適用する」行を補完する — コントローラの annotation は surge 中に honor される（`expireAfter` をブロックしないことは文書化済み Karpenter 挙動で再検証せず）|
| ウィンドウ有界の **surge-less forceful fallback** が、graceful surge を期限までに完了できないとき、危険な `NodeClaim` を voluntary 経路でウィンドウ内削除する（§3.3、ADR-0001、#156）| **検証済** — EKS Auto Mode / K8s 1.36（2026-07-04）| 同期した 12 ノードのバッチ（共有の **固定** 2h `expireAfter`）で `N=12 > K·C=2` の状況が graceful + forceful の **混在** としてローテーション: 先頭 6 台は graceful（placeholder surge）、余剰 6 台は共有期限の `t_rot` 内に入った時点で **surge-less** — 進行中は NodePool アンカー `noderotation.io/rotation-mode=forceful-fallback`、claim ごとに `Warning`/`ForcefulFallback` イベント（*"…surge-less: a graceful surge cannot complete before its deadline; deleting in-window via the voluntary path (PDBs apply)"*）、forceful 候補には **placeholder Pod なし**（`surge_wait` ヒストグラムは graceful 6 台のみを計上）、`noderotation_forceful_fallback_total` は **`0→6`** にクリーンに増加（surge-less 1 本ごとに +1、コントローラは全期間 `restartCount=0`）。PDB（`minAvailable=11`）は一貫して維持（voluntary 経路）、`expired` バックストップは **ゼロ**。`expireAfter` は一切 patch せず — forceful はバッチのスループット不足のみで誘発（トリックなし）|
| ローテーションは候補を **最早期限順**（`creationTimestamp + expireAfter`、次に `creationTimestamp`、次に `Name`）で消費する（§3.2、#157）| **検証済** — EKS Auto Mode / K8s 1.36（2026-07-04）| 12 ノードのバッチは `creationTimestamp`（＝期限）を共有するため順序は `Name` タイブレークに縮退し、正確に昇順で消費された: `2rvd5 < 6ssql < dtkgz < fswsg < gxsfs < krcdc < nkfbh < pdfwl < s7l9r < vvsqr < w9kx7 < wcmwr` |
| オペレータの `karpenter.sh/do-not-disrupt` がノードをローテーション候補選択から **除外** する（§3.2、#170）| **検証済** — EKS Auto Mode / K8s 1.36（2026-07-04）| 進行中でない候補のノードに `karpenter.sh/do-not-disrupt=true`（オペレータ所有 — コントローラの `do-not-disrupt-owned` マーカーなし）を付与すると `noderotation_candidates{nodepool="nodepool-ff"}` が **4→3** に低下し、そのノードは選ばれなかった（`NodeClaim` 保持、`deletionTimestamp` なし）。annotation を **除去** するとゲージは再び上昇 — 双方向で除外を確認（idle で凍結中のプールは reconcile 間隔が長いため nudge が必要）|

> **なぜ surge 機構ではなく「能力」として記録するか。** standalone NodeClaim の結果は、コントローラが作成した NodeClaim を Karpenter が尊重することを示し、プロジェクトのリスクを下げる。しかし surge 設計（§3.3）はこれを **使わない**: standalone ノードは NodePool に owned されず、Pod が NodePool 会計・expiry・drift・budget の外のノードに載り続け、意図的な NodePool 分離を壊すため。placeholder 方式が成立しない場合の **fallback** として文書化しておく。
>
> **現在の検証状況（PoC スコープ）:** primary な placeholder Pod surge 機構、新規プロビジョニング経路、同一 AZ zonal-PV 再バインド、`readyTimeout` ロールバック + クリーンアップ、NodePool `limits` ゲート、複数 NodePool の閉じ込め、voluntary な PDB 尊重 drain、新旧両 surge ノードの `do-not-disrupt` マーカー、強制失効の `expired` outcome、スケール版 R6 soak、capacity-absorb 経路、placeholder の preemption（被害者 + `readyTimeout` 有界 rollback）、ウィンドウ境界の開始/停止、leader 交代の再開、voluntary disruption に対する `do-not-disrupt` の honor は上の検証済み行に記録済みである。v0.4 以降の追加 — ウィンドウ有界の **surge-less forceful fallback**（#156）、**最早期限**の候補順序付け（#157）、候補選択からの **`do-not-disrupt` 除外**（#170）— は 2026-07-04 の 3 行に記録済みで、`expireAfter` を固定したまま（トリックなし）graceful + forceful の混在として実 EKS 上で end-to-end に検証した。実クラウドで未了: 本物の同一 AZ **キャパシティ不足（ICE）** による rollback 誘発（オンデマンドで決定的に再現できないため短い `readyTimeout` で代替）、そしてフルな数時間の *接戦* `expireAfter` soak（日次ウィンドウでは到達不能）。RBAC の十分性と `karpenter.sh/v1` CRD デコードは暗黙に検証済み — コントローラは surge オブジェクトの create/patch/delete と実 `NodeClaim` の reconcile をエラーなく実施。

## 7.3 未決事項

1. **祝日対応**（土曜が祝日と重なる場合スキップ）。v1 は意図的に無視
2. v2 の pre-pull イメージ取得方式（Karpenter NodeClass 標準機能 vs 専用 Job）
3. EKS Auto Mode 以外（AKS NAP / GKE）への **マルチクラウド検証**

> `RotationPolicy` CRD（issue #119）により解決済み: *NodePool が別々のローテーションポリシーを必要とする場合の CRD ベースのポリシー移行* と *NodePool ごとのメンテナンスウィンドウ vs クラスタ単一窓* — いずれも NodePool ごとの `RotationPolicy`（§5.4）で提供される。

---

