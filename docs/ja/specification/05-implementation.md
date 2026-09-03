# 5. 実装

## 5.1 アーキテクチャ

::: tip このセクションの定義
コントローラーは controller-runtime マネージャー（Deployment、replicas=2、リーダー選出）であり、NodePool ごとに reconcile し、各パスで governing `RotationPolicy` を解決する。
:::

```mermaid
flowchart TD
    subgraph cluster["クラスター (Karpenter v1+)"]
        subgraph ns["Namespace: node-rotation-system (設定可能)"]
            ctrl["node-rotation-controller (Deployment)<br/>controller-runtime manager, replicas=2 + leader election (1 active)<br/>NodePool reconciler; NodeClaim / Pod / Node + RotationPolicy を watch<br/>1分 self-requeue, /metrics エンドポイント"]
        end
        rp["RotationPolicy<br/>noderotation.io/v1alpha1, cluster-scoped<br/>nodePoolSelector で NodePool を選択<br/>maintenanceWindows / minRotationChances / surge を保持"]
        nc["NodeClaims (karpenter.sh/v1)<br/>nc-aaa 15d (旧) / nc-bbb 14d (旧) / nc-ccc 08d (surge)"]
    end
    ctrl -->|"プールごとに governing policy を解決"| rp
    ctrl -->|"watch / placeholder 作成 / 旧 NodeClaim 削除"| nc
```

### ポリシーと状態の分離

- **ポリシー** = `RotationPolicy` spec（オペレーターが作成する望ましい設定）
- **状態** = `NodeClaim`/`NodePool` 上のアノテーション + 一時的な Node/placeholder マーカー（§5.3）
- CRD は権威的なランタイム状態を保持しない — `status` は観測用のみ

### 起動時 preflight

reconcile 開始前に、以下の場合コントローラーは即座に失敗:
- クラスターが `karpenter.sh/v1` の `nodeclaims`/`nodepools` リソースを提供しない
- RBAC がそれらを読み取れない

互換性の契約は `karpenter.sh/v1` **グループ/バージョン** であり、管理された Karpenter マイナーとは独立（EKS Auto Mode はそれを公開しない）。v1 型のデコード成功はワイヤー互換スキーマを確認。フィールドごとの CRD イントロスペクションは行わない。

## 5.2 Reconcile ループ

::: tip このセクションの定義
各 `Reconcile` 呼び出しは **正確に 1 つのノンブロッキングステップ** を実行し `Requeue` を返す。ブロッキング待機なし — すべての状態はアノテーションから読み取られ、再起動を生き延びる。
:::

Reconciler は `NodePool` をキーとし、以下を watch:
- `NodeClaim`（所有 NodePool にマッピング）
- placeholder `Pod` が `Running` に到達
- surge ホスト `Node` が `Ready` に到達

定期的な self-requeue がウィンドウエッジ、freeze 解除、ドレイン進捗、force-expiry の backstop として残る。

### 判断フロー

```mermaid
flowchart TD
    entry(["Reconcile (NodePool)"]) --> q1{"active-rotation<br/>anchor セット?"}
    q1 -->|yes| adv["advance(): 進行中の<br/>ローテーションを1ステップ進める (§5.3)"]
    q1 -->|no| q1b{"spec.replicas あり?<br/>(static capacity)"}
    q1b -->|yes| stat["StaticNodePool を警告;<br/>Requeue (1m)"]
    q1b -->|no| q2{"開始ゲート通過?<br/>ウィンドウ内、未 freeze、<br/>cooldown + failure-pause 経過"}
    q2 -->|no| rq["Requeue (1m)"]
    q2 -->|yes| pick["earliest-deadline の適格候補を選択"]
    pick --> q3{"候補<br/>あり?"}
    q3 -->|no| rq
    q3 -->|yes| q4{"surge_headroom?<br/>クランプ済みフットプリント<br/>vs spec.limits バジェット"}
    q4 -->|no| warn["warn: limits ヘッドルーム不足;<br/>Requeue (1m)"]
    q4 -->|yes| anchor["active-rotation anchor を書き込み<br/>(conflict-checked, only-if-absent)"]
    anchor --> adv
```

### static capacity ゲート（ステップ 1a）

`spec.replicas` を設定した NodePool は surge を完了できない（§3.3）ため、ローテーションを開始しない。そのパスは 1 度だけ警告し（`StaticNodePool`、§4.3）、requeue する。

このゲートは in-flight の `advance()` の**後**、すべての開始ゲートの前に置かれる。このゲートが存在しなかった頃に書かれた anchor（旧バージョンのコントローラによるもの。Karpenter 自体は稼働中の NodePool への `spec.replicas` 追加を拒否する）でも、そのローテーションは完了まで進む — そうしないと cordon されたノードと placeholder が行き場を失って残る。`advance()` の失敗リトライ分岐は新しい試行にあたるため static プールでは別途閉じてあり、エスカレートするバックオフごとに無駄な試行を繰り返す代わりに anchor を解放する。

### 開始ゲート（ステップ 2）

新しいローテーションを開始するには以下のすべてが通過する必要がある:

- `in_window(now)` — メンテナンスウィンドウがオープン
- `not frozen(np)` — freeze アノテーションなし
- `since_last_rotation(np) >= cooldownAfter` — gate A: 成功後の安定化待機
- `since_last_failure(np) >= failurePause` — gate B: 失敗後の一時停止（§4.4、ADR-0004）

### 候補選定（ステップ 3）

`pick_earliest_deadline_eligible` は以下の claim を選定:
- `deletionTimestamp` なし
- `state` が空（新規）または `failed` でエスカレーティングバックオフ経過（`retryBackoff · 2^(retry-count − 1)`、8× で上限）
- `pending`/`draining` は再選定されない; `expired` は終端

### anchor のセマンティクス

`active-rotation` anchor は:
- 開始時にすべての他の副作用 **より前** に書き込み
- 完了/失敗時に **最後** にクリア
- **Conflict-checked, only-if-absent** 書き込み（楽観的並行制御）
- Tick と NodeClaim イベントが同一 NodePool でレース可能 — 前提条件によりレースは無害

### 完了結果

NodePool 側の `active-rotation-state` ミラーにより決定:
- `draining` あり → **success**（cooldown 消費）
- `draining` なし → **expired**（アラート、cooldown なし）

### force-expiry の検出

2 つのパスで捕捉:
- **早期:** `pending` のまま `deletionTimestamp` 出現 — すべてに先行してチェック
- **後期:** `draining` ミラーなしで旧 NodeClaim 消失

早期パスは anchor 解放前に `state=expired` を書き込む（Auto Mode の `tGP = 24h` 下でのライブロック防止）。

### ドレイン停滞

`tGP + buffer` を超えるドレインは `noderotation_drain_stuck` を発生させるが **シリアルゲートを保持** — `draining` のローテーションはロールバック不可（delete 済み）、ゲート解放は `maxUnavailable = 1` に違反する。

### cooldown anchor

`last-rotation-at` は **NodePool** 上に存在（削除された旧 NodeClaim ではない）。一時停止は完了境界とリーダー変更を跨いで永続的。

::: details 完全な擬似コード — クリックで展開

```text
Reconcile(req):
  if req is Tick:
      for np in in_scope_nodepools():
          reconcile_nodepool(np)
      return Requeue(1m)
  return reconcile_nodepool(nodepool(req.obj))

reconcile_nodepool(np):
  # ── 0. ウィンドウクローズ評価（§4.2）。すべてのゲートより前段: これはウィンドウに
  #        何が起きたかを述べるものであり、コントローラーが動かなかった理由ではない。
  match window_edge(np, census(np), in_window(now)):
    case stamp:    annotate(np, window-opened-at=now)        # only-if absent
    case defer:    pass                                      # ローテーションがまだ成功しうる
    case settled:  clear(np, window-opened-at)               # only-if present
    case missed:   won := clear(np, window-opened-at)        # only-if unchanged and no anchor
                   if won: emit_metrics(window_missed); event(WindowMissed)

  # ── 1. 進行中のローテーションを先に駆動（シリアル: NodePool あたり最大 1）
  if name := np[active-rotation]:
      return advance(np, name)

  # ── 1a. static capacity ゲート（§3.3）: replica 固定のプールに surge は使えない。
  #        advance() の後なので、in-flight のローテーションは完了まで進む。
  if np.spec.replicas is set:
      warn_once(np, StaticNodePool)
      return Requeue(1m)

  # ── 2. 開始ゲート
  start_gates(np) :=
      in_window(now) and not frozen(np)
      and since_last_rotation(np) >= cooldownAfter   # gate A
      and since_last_failure(np)  >= failurePause    # gate B
  if not start_gates(np): return Requeue(1m)

  # ── 3. 候補選定、ヘッドルームチェック、anchor
  cand := pick_earliest_deadline_eligible(np)
  if cand == nil: return Requeue(1m)
  surgeless := forceful_fallback(np, cand)
  if not surgeless and not surge_headroom(np, cand):
      warn("insufficient limits headroom"); return Requeue(1m)
  annotate(np, active-rotation=cand.name)    # conflict-checked, only-if-absent
  if surgeless:
      annotate(np, rotation-mode=forceful-fallback,
               active-rotation-state=draining, draining-at=now)
      annotate(cand, state=draining)
      emit_metrics(forceful_fallback); event
      delete(cand)
      return Requeue(30s)
  return advance(np, cand.name)

advance(np, name):
  cand := nodeclaim(name)
  if cand == nil:                            # 旧 NodeClaim ファイナライズ完了
      delete(placeholder(name))
      for node in nodes_with(surge-for=name):
          unfreeze(node)
      # active-rotation == name のときだけ書く、conflict チェック付きの単一書き込み。
      # 検証対象と同じ最新コピーから結果を判定し、そのコピーが draining なら
      # last-rotation-at を刻み、anchor をクリアし、解放したのが「このパス」か
      # どうかを返す。古いキャッシュの np を持つパスは競争に負け、何も発行しない（§5.2）
      won, rotated := release_anchor(np, name)
      if not won:                            # 先行パスが既に完了させている
          return Requeue(1m)
      if rotated:
          emit_metrics(success, duration)
      else:
          emit_metrics(expired); alert
      return Requeue(1m)

  switch cand.state:
  case (none) | pending:
      if cand.deletionTimestamp != nil:      # force-expiry 捕捉
          # claim が「このハンドラー自身の遷移前状態」を保持している場合のみ書き込む、
          # conflict チェック済みの単一書き込み。何をしたかを返す。クリーンアップより
          # 前に実行する: 遷移を所有しないパスが、進行中のドレインが依存している
          # surge ノードを unfreeze してはならない（§5.2）。
          out := mark_expired(cand, from=[none, pending],
                              clear=[started-at, surge-claim])
          if out in {gone, raced}:           # 何も書いていない = 何も所有しない
              return Requeue(30s)            # gone なら release_anchor が abort を数える
          # 失敗しうるクリーンアップより前に発行する: クリーンアップが失敗すると
          # 次の reconcile は `expired` ハンドラーへ渡り、そこは修復するだけで
          # 発行しないため、後ろに置いた発行は遅延ではなく消失する（§5.2）
          if out == claimed: emit_metrics(expired); alert
          delete(placeholder(name))
          for node in nodes_with(surge-for=name): unfreeze(node)
          clear(np, anchor)
          return Requeue(1m)
      # advance() がここへディスパッチする状態からのみ書く: 永続状態が先へ進んだ
      # claim の `pending` ビューがロールバックを取り消してはならない —
      # started-at の再スタンプは readyTimeout の期限をリセットする（§5.2）
      wrote := annotate_if(cand, from=[none, pending],
                           state=pending, once(started-at=now))
      if not wrote: return Requeue(30s)   # このパスは何も所有しない
      if elapsed(cand.started-at) > readyTimeout:
          reap_surge_claim(cand[surge-claim])
          delete(placeholder(name))
          for node in nodes_with(surge-for=name): unfreeze(node)
          wrote := annotate(cand, state=failed, failed-at=now, retry-count+=1,
                            clear=[started-at, surge-claim])
          if not wrote:                      # ロールバック中に claim が finalize された
              return Requeue(30s)            # 失敗した試行ではなく force-expiry
          # alert が報告する retry-count は、呼び出し元のキャッシュコピーではなく
          # この書き込みが実際に生成した値
          emit_metrics(failure); alert
          annotate(np, last-failure-at=now, clear=anchor)
          return Requeue(1m)
      freeze(cand.node, surge-for=name)
      cordon(cand.node)
      if c := induced_claim(name):
          annotate(cand, surge-claim=c.name)
      if frozen(np): return Requeue(1m)      # エスカレーション保留
      if placeholder(name) is missing:
          create_placeholder(np, cand)
          return Requeue(30s)
      if surge_ready(cand):
          host := placeholder_node(name)
          freeze(host, surge-for=name)
          annotate(np, active-rotation-state=draining, draining-at=now,
                   surge-wait=now − cand.started-at)
          annotate(cand, state=draining)
          delete(cand)
          return Requeue(30s)
      return Requeue(30s)

  case draining:
      annotate(np, active-rotation-state=draining)
      if cand.deletionTimestamp == nil:      # クラッシュリカバリ
          delete(cand)
          return Requeue(30s)
      if elapsed(cand.deletionTimestamp) > drain_bound(np):
          alert(stuck_drain)
      return Requeue(30s)

  case failed:
      if cand.deletionTimestamp != nil:
          out := mark_expired(cand, from=[failed])   # 上と同じ条件付き書き込み
          if out in {gone, raced}: return Requeue(30s)
          if out == claimed: emit_metrics(expired); alert
          clear(np, anchor)
          return Requeue(1m)
      # リトライは新しい試行: このパスが上位にある step 1a の static ゲート
      # （anchor により先に advance() へ入る）も通過する必要がある。
      if start_gates(np) and np.spec.replicas is unset
         and elapsed(cand.failed-at) >= escalated_backoff(cand)
         and surge_headroom(np, cand):
          # failed からのみ。同じガードが下の再入も抑える: advance() はキャッシュ
          # 経由で読み直すため、この書き込みにまだ遅れている読み取りは、すべての
          # ゲートが開いたままここへ戻ってくる（§5.2）
          wrote := annotate_if(cand, from=[failed], state=pending)
          if not wrote: return Requeue(30s)
          return advance(np, name)
      annotate(np, last-failure-at=max(np[last-failure-at], cand.failed-at),
               clear=anchor)
      return Requeue(1m)

  case expired:                              # 終端クリーンアップ
      delete(placeholder(name))
      for node in nodes_with(surge-for=name): unfreeze(node)
      clear(np, anchor)
      return Requeue(1m)
```

:::

### 冪等リカバリ

各状態ハンドラーはフェーズの望ましい状態を **再アサート** する（ワンショットアクションではない）:
- `pending` は各パスで freeze、cordon、placeholder 存在を再アサート
- `draining` は `deletionTimestamp` がない場合に冪等な `delete` を再発行（状態書き込みと delete 間のクラッシュ）
- 完了はクリーンアップを再実行するが、ローテーションの完了は **条件付き書き込みで主張する**: anchor の解放と success/expired の判定はどちらもその書き込みが検証される最新の読み取りから決まる。したがって、すでに解放済みの anchor をキャッシュ経由で見たパスは冪等なクリーンアップだけを行い、何も発行しない
- **キャッシュ遅延したディスパッチが到達しうる 4 つの書き込みが遷移を主張する**。受け付けるのはそのハンドラーがディスパッチされる状態からのみ — `expired` へ入る 2 つの書き込み、`pending` の入口アサート、`failed` → `pending` のリトライ。終端状態が既に書かれた claim をキャッシュ経由で見たパスはクリーンアップと anchor 解放だけを行い何も発行しない。claim が別の状態へ進んでいたパスは一切書き込まず、ローテーションのランタイムオブジェクトにも触れず、それを所有するハンドラーに委ねる
- reconcile の残りの claim 状態書き込みは無条件のままだが、veto ではなく構造的に安全 — ただし同じ構造によるわけではない。`pending` ハンドラーが行う 2 つ（`pending` → `draining`、`pending` → `failed`）は、そのハンドラー自身のガードされた入口の後に続く。forceful fallback は候補選択から直接開始され、このハンドラーを通らないため、その `draining` 書き込みを守るのは直前に獲得した only-if-absent の NodePool anchor である。3 つとも、所有するパスが実際に行った作業を記録し、3 つとも claim を前へ進める。§5.3 の起動時 sweep の書き込みはこのディスパッチの外側にある。そちらも条件付きだが、条件はハンドラーの遷移前状態ではなく、その claim を選んだ述語である
- このガードが、遅れた `pending` ビューによるロールバックの取り消し — `pending` へ戻し、`started-at` を再スタンプして `readyTimeout` の期限をリセットする一方、`retry-count` はエスカレーションの根拠となった値のまま残る — を止め、リトライ分岐からディスパッチャーへの再入も抑える（再入側のキャッシュ読み取りは、直前に行った書き込みにまだ遅れうる）

### オブザーバビリティのスキュー（v1 で許容）

- **ミラーから delete 間のギャップ:** そこでのクラッシュ後に force-expiry が発生すると `success` と記録（surge は確保済み — 実質的結果は一致）
- **メトリクス発行（完了）:** anchor 解放の書き込み後に、その書き込みを行ったパスだけが発行する。カウンター・ヒストグラム・完了ログ・Event は解放された anchor 1 つにつき 1 回発火する。書き込みと発行の間でクラッシュすると発行は失われる（at-most-once）
- **メトリクス発行（claim スコープ）:** `expired` へ入る 2 つの遷移 — `abortPendingExpiry` と `advanceFailed` の削除分岐 — は、ディスパッチ元ハンドラー自身の遷移前状態だけを受け付ける条件付き NodeClaim 書き込みで遷移を主張するため、`expired` は遷移を行ったパスが 1 回だけ発行する。これは既に終端状態の claim を再発行しない `advanceExpired` と整合する
- **発行は「試行」ではなく「書き込み」に従う:** 条件付き claim 書き込みの結果は書き込みループ自身が生成し、試行ごとにリセットされる。したがって、最初の試行が conflict し、リトライで claim が finalize 済みだった場合の結果は成功ではなく *gone* になる。終端書き込み前に消えた claim は anchor を残し、その結果は完了パス（`expired`、cooldown なし）が引き受ける。これは `failure` のロールバックにも適用され、試行の発行と failure pause のスタンプは、それを記録する書き込みが成立したときにのみ行い、報告する retry count はその書き込みが生成した値を使う
- **発行は書き込みの直後・クリーンアップより前に置く:** クリーンアップは失敗しうる。そこでエラーになると次の reconcile は `advanceExpired` に渡るが、そのハンドラーは修復するだけで意図的に発行しない。したがってクリーンアップの後ろに置いた発行は、通常の一時的な API エラーでリトライされずに失われる。残る消失窓は完了パスが既に受け入れているものと同じ既約な窓 — 書き込みと発行の間でコントローラーが死ぬ場合（at-most-once）

## 5.3 状態モデル

::: tip このセクションの定義
すべての状態は Kubernetes オブジェクト上に存在 — 外部データストアなし。NodePool の `active-rotation` anchor が **どの** ローテーションが進行中かを記録; 旧 NodeClaim の `state` が **どこ** にあるかを記録。
:::

### アノテーションリファレンス

| キー | ターゲット | 値 | 目的 |
|-----|--------|-------|---------|
| `active-rotation` | NodePool | NodeClaim 名 | 永続 anchor + シリアルゲート |
| `active-rotation-state` | NodePool | `draining` | 完了結果のフェーズミラー |
| `draining-at` | NodePool | RFC3339 | ドレイン所要時間 anchor（§4.2） |
| `surge-wait` | NodePool | Go duration | 完了ログの surge フェーズ所要時間 |
| `rotation-mode` | NodePool | `forceful-fallback` | surge なしパスマーカー |
| `window-opened-at` | NodePool | RFC3339 | 観測されたウィンドウの発生（§4.2） |
| `state` | 旧 NodeClaim | `pending`/`draining`/`failed`/`expired` | 進捗状態 |
| `started-at` | 旧 NodeClaim | RFC3339 | `readyTimeout` 期限 |
| `failed-at` | 旧 NodeClaim | RFC3339 | バックオフ anchor |
| `retry-count` | 旧 NodeClaim | 整数 | バックオフをエスカレート |
| `surge-claim` | 旧 NodeClaim | NodeClaim 名 | 誘導された surge の特定 |
| `surge-for` | Pod + freeze ノード | NodeClaim 名 | ローテーションのペアリング |
| `do-not-disrupt` | 旧 + surge ノード | `true` | voluntary disruption をブロック |
| `do-not-disrupt-owned` | 旧 + surge ノード | `true` | コントローラーオーナーシップマーカー |
| `cordoned` | 旧ノード | `true` | コントローラーの cordon マーカー |
| `last-failure-at` | NodePool | RFC3339 | 試行間一時停止 anchor |
| `freeze` | NodePool | RFC3339 | 指定時刻までローテーション抑制 |
| `last-rotation-at` | NodePool | RFC3339 | `cooldownAfter` ゲート anchor |

すべてのキーは `noderotation.io/` プレフィックスを使用（`karpenter.sh/do-not-disrupt` を除く）。

::: details アノテーション詳細 — クリックで展開

- **`active-rotation`:** すべての副作用に先行して書き込み、最後にクリア。旧 NodeClaim の削除を生き延びる（成功時に削除されるため）。`maxUnavailable = 1` のシリアルゲートも兼ねる
- **`active-rotation-state`:** `delete(cand)` の直前に書き込み。不在 = ローテーションが `pending` を離れなかった。旧 NodeClaim 消失後に完了ハンドラーが読み取り
- **`draining-at`:** `pending → draining` で write-once。旧 NodeClaim の `deletionTimestamp` は完了時に消失 — この anchor が必要
- **`surge-wait`:** `pending → draining` で write-once。旧 NodeClaim（`started-at` のキャリア）がその遷移で削除される
- **`rotation-mode`:** forceful-fallback 開始時に anchor にスタンプ。不在 = デフォルト surge。すべての終了パスで anchor とともにクリア
- **`window-opened-at`:** in-window の reconcile でこのアノテーションが不在だと判明した最初の回にスタンプされ、window 外の reconcile で存在すると判明した最初の回にクリアされる。その**存在**が発生の識別子であり、スケジュールから発生の開始時刻を導出することはしない — 週次の投影は DST をアンカー週に固定しているため、復元した壁時計上の開始時刻は最大 1 時間ずれうる。in-flight のローテーションはクリアを遅延させる; 読み取れない値は in-window では再スタンプされ、window 外では黙ってクリアされる
- **`state`:** `expired` は終端 — forceful drain 下でファイナライズ中の claim の再選定をブロック
- **`started-at`:** 試行ごとに write-once。failed 書き込み時にクリア（`state=failed` と単一更新）。リトライ時に再スタンプ
- **`surge-claim`:** placeholder の bind ターゲット（`spec.nodeName`）が観測可能になり次第永続化。failed 書き込み時にクリア
- **`surge-for`:** freeze ノード上で、freeze をこのローテーションに帰属。Pod 上で発見用にペアリング
- **`do-not-disrupt-owned`:** コントローラーが実際に `do-not-disrupt` を適用した場合のみセット。オペレーターの既存アノテーション（マーカーなし）は変更しない
- **`cordoned`:** コントローラーが `spec.unschedulable` をフリップした場合のみセット。オペレーターの cordon（マーカーなし）は採用しない
- **`last-failure-at`:** クラッシュリカバリブランチで `max` セマンティクスにより一時停止の無効化を防止

:::

### 状態遷移

```mermaid
stateDiagram-v2
    [*] --> pending: ウィンドウ内で選定
    [*] --> draining: forceful fallback (surge なし, §3.6)
    pending --> draining: surge_ready
    pending --> failed: readyTimeout 経過
    pending --> expired: 旧 NodeClaim force-expiring
    draining --> [*]: 旧 NodeClaim 消失 (success + cooldown)
    failed --> pending: バックオフ経過 + 開始ゲート通過 (リトライ)
    failed --> expired: deletionTimestamp 観測 (backstop)
    expired --> [*]: 終端クリーンアップ、ゲート解放
    draining --> draining: ドレインが tGP+buffer を超過 (停滞、ゲート保持)
    note right of expired
        終端: ローテーションされず、
        cooldown なし
    end note
```

::: details 遷移の副作用 — クリックで展開

| From | イベント | To | 副作用 |
|------|-------|----|--------------|
| *(none)* | ウィンドウ内で選定 | `pending` | anchor 書き込み（最初）; 旧ノード freeze; 旧ノード cordon; placeholder 作成 |
| *(none)* | forceful fallback | `draining` | anchor + `rotation-mode` + `draining-at` 書き込み; `state=draining`; 旧 NodeClaim 削除（surge なし） |
| `pending` | 各 reconcile | `pending` | `none`/`pending` から `state=pending` を**主張**（条件付き、他の一切より前）; freeze + cordon 再アサート; `surge-claim` 永続化; placeholder 再作成（freeze 中は保留） |
| `pending` | `surge_ready` | `draining` | surge ターゲット freeze; `draining-at` + `surge-wait` 書き込み; 旧 NodeClaim 削除 |
| `pending` | `readyTimeout` | `failed` | surge claim reap; placeholder 削除; unfreeze; `state=failed` + `last-failure-at`; anchor クリア。ロールバック中に消えた claim は何も書かないため、試行を発行せず pause もスタンプせず、anchor を残して完了パスに force-expiry を記録させる |
| `pending` | force-expiring | `expired` | `pending` から `state=expired` を**主張**（条件付き、クリーンアップより前）; expired を 1 回発行; placeholder 削除; unfreeze; anchor クリア |
| `draining` | `deletionTimestamp` なし | `draining` | delete 再発行（クラッシュリカバリ） |
| `draining` | ドレイン > `tGP + buffer` | `draining` | stuck-drain ゲージ; ゲート保持 |
| `draining` | NodeClaim 消失 | *(success)* | unfreeze; `last-rotation-at`; success 発行; anchor クリア |
| `failed` | バックオフ + ゲート通過 | `pending` | `failed` から `state=pending` を**主張**（条件付き）; 新試行で `started-at` 再スタンプ |
| `failed` | `deletionTimestamp` | `expired` | `failed` から `state=expired` を**主張**（条件付き）; expired を 1 回発行; anchor クリア |
| `expired` | まだ anchor あり | `expired` | 冪等クリーンアップ; anchor クリア（メトリクスは再発行しない） |

:::

### anchor のクリア

`clear(np, anchor)` はローテーションスコープのセット全体を削除する **単一更新**:
- `active-rotation`, `active-rotation-state`, `draining-at`, `surge-wait`, `rotation-mode`

コンパニオンフィールドがローテーションを超えて存続することはない。失敗パスは同じ更新に `last-failure-at` も追加で書き込む。

### 起動時 sweep

**最初の reconcile の前にゲートされ、1 回だけ** 実行。anchor が参照しないマーカーのみをクリーン:

- **Placeholder Pod** — `surge-for` の claim が不在/非 anchor → 削除
- **ノードマーカー**（`surge-for`、コントローラーの `do-not-disrupt`（owned マーカーによる））→ 削除
- **`cordoned` マーカー** — anchor なしのローテーション → uncordon して削除

ルール:
- anchor がある NodePool は **陳腐化していない** — ステップ 1 が通常通り再開
- `failed`/`expired` claim はアノテーションを保持（バックオフ再入 / 終端マーカー）
- anchor なしの `pending`/`draining` claim（クラッシュポイントからは不可能）→ `pending`/`draining` から `state=failed` を**主張**（条件付き）+ アラート。どちらもその書き込みが成立したときにのみ行う。sweep は List（キャッシュ読み）から選択し、書き込みはその後になる。その窓で finalize された claim や、永続状態が既にその 2 状態を離れた claim は、ここでは何も修復していないので、何も書かず何も発行しない。reconcile の各経路と違い、結果を引き渡す anchor は存在しない — anchor を持たないことがこの claim を選んだ理由だからである
- sweep が出すログ行は、いずれもその sweep が実際に行った作業を指す。placeholder の削除も node のマーカー解除も、上と同じ List から書き込みまでの窓で（読み側・書き側のどちらの端であっても）オブジェクトが消えていた場合や、マーカーが既に解除済みだった場合には no-op であり、そのときは何も発行しない。node 側も claim 側とまったく同じく、書き込みが検証される読みに対して、sweep 開始時に取得した anchor 集合を用いて選択述語を再適用する — その読みが「anchor されたローテーションのものだ」と示すマーカーは孤立ではなく現役であり、それを所有するローテーションに委ねる。何を解除したかも同じ読みから決まり、行はそれを名指す — surge で凍結されたノードなら *unfroze*、cordon のみのノード（凍結されたことがなく、どの claim にも属さない）なら *uncordoned*
- anchor なしの孤立 `active-rotation-state` → 単純に削除
- ベストエフォート: アイテムごとのエラーはログ、fatal にしない

## 5.4 設定スキーマ

::: tip このセクションの定義
`RotationPolicy` CRD（cluster-scoped、`v1alpha1`）が NodePool ごとのローテーション設定を保持。コントローラーはセレクタの specificity で各 NodePool の governing policy を解決する。
:::

### RotationPolicy CRD（`noderotation.io/v1alpha1`）

```yaml
apiVersion: noderotation.io/v1alpha1
kind: RotationPolicy
metadata:
  name: api                       # cluster-scoped; ポリシーごとに 1 つ
spec:
  nodePoolSelector:               # governed NodePool を選択
    matchLabels:
      workload: api
  ageThreshold: auto              # "auto"（導出、§3.2）または Go duration オーバーライド
  minRotationChances: 2           # K; 下限 1
  maintenanceWindows:             # ポリシーごと; 和集合セマンティクス（§3.1）
    - timezone: Asia/Tokyo
      days: [Wed, Sat]
      start: "02:00"
      end:   "06:00"
  surge:
    maxUnavailable: 1             # v1 では 1 固定（OpenAPI が他を拒否）
    readyTimeout: 15m             # > 0 必須
    cooldownAfter: 10m            # gate A; 0 も可
    # failurePause: 10m           # gate B; 未設定 → max(10m, cooldownAfter)
    # drainEstimate: 10m          # layer-2 のみ; 未設定 → min(tGP, 10m)
    # provisioningEstimate: 5m    # layer-2 のみ; 未設定 → min(readyTimeout, 5m)
    retryBackoff: 30m             # > 0 必須
    matchNodeRequirements:        # placeholder 要件の複製（§3.7）
      required:
        - topology.kubernetes.io/zone
        - kubernetes.io/arch
        - karpenter.sh/capacity-type
      preferred: []
    forcefulFallback:             # オプトイン surge なしフォールバック（§3.6）
      enabled: false
  prePull:                        # v2（v1 では無効）
    enabled: false
status:
  observedGeneration: 3
  matchedNodePools: 2
  rotatingNodePools: 1
  conditions:
    - type: Ready
      status: "True"
      reason: Accepted
```

### status サブリソース

- **`matchedNodePools`:** このポリシーがセレクタ specificity で勝利するプール数
- **`rotatingNodePools`:** そのうち進行中のローテーションがある数
- **`Ready` condition:**
  - `Accepted` — 有効かつ競合なし
  - `Invalid` — reconcile 時バリデーション失敗
  - `Conflict` — 同一 specificity タイ（§下記）
- `Invalid` が `Conflict` に優先
- status は観測用のみ — ローテーション判断の権威的ソースではない

専用の `RotationPolicyStatusReconciler` がこのビューを更新。楽観的並行制御の競合はサイレント requeue として扱う。

### ターゲティングと競合解決

| ルール | 動作 |
|------|----------|
| 最も specific が勝利 | Specificity = ラベルキー制約数 |
| 同一 specificity タイ | **ハードエラー** — その NodePool のローテーションを拒否 |
| ポリシーなし | ローテーションしない（安全な no-op） |

- **Specificity:** `matchLabels` エントリ + `matchExpressions` エントリ。空（catch-all）セレクターはスコア 0 — 任意のキー付きセレクターに負ける
- **タイ:** `PolicyConflict` Warning Event + `noderotation_policy_conflict{nodepool} = 1` をセット
- **マッチなし:** 暗黙のデフォルトなし; ブランケットカバレッジが必要ならオペレーターが catch-all を作成

### ローテーション中のガバナンス喪失

ローテーションが anchor されている間にプールのガバナンスが失われた場合、コントローラーは以下の順で **ロールバック**:
1. placeholder 削除
2. ノードの unfreeze（オペレーター独自の保護は維持）
3. anchor クリア — **anchor がまだこのローテーションを指している場合のみ**
4. `GovernanceLost` Warning Event 発行

孤立した placeholder と陳腐化した `do-not-disrupt` マーカーが Karpenter の voluntary 操作を無期限にブロックするのを防止。

この順序は規範的であり、理由は 2 つ:

- **ロールバックはクリアに先行する。** anchor は後続の reconcile をこのクリーンアップへ戻す唯一の手段である — anchor を持たないプールでは reap は即座に return し、そのプールを governing するポリシーはもはや存在しない。後続ステップが失敗しうる状態で先に anchor をクリアすると、成果物は恒久的に孤立する。
- **条件付きクリアが発行者を確定する。** reap は呼び出し元が受け取った anchor から entry するが、それはキャッシュ読み取りであり、先行パスが既にクリアした anchor をなお指していることがある。したがって anchor をクリアする書き込みこそが、そのローテーションを reap したパスを識別する: 発行権を得るパスは高々 1 つであり、それを得たパスは既に完了した作業を記述する。これは §5.2 の完了パスが用いる claim-then-announce の順序であり、同じ **at-most-once** セマンティクスを継承する — 通常運転では reap されたローテーション 1 件につき Event 1 件、書き込みと発行の間でコントローラーが停止した場合は 0 件。

### ポリシー変更の伝播

任意の `RotationPolicy` の create/update/delete は **すべての** NodePool を再解決のために再エンキュー（1 つの変更が任意のプールでどのポリシーが勝利するかを変更しうるため）。

### NodePool ごとのメンテナンスウィンドウ

`maintenanceWindows` は各ポリシーに存在するため、ウィンドウは NodePool ごと。和集合セマンティクス（§3.1）は 1 つのポリシーのリスト内で適用。`noderotation_window_active` と `noderotation_window_period_seconds` がロードベアリングな `nodepool` ラベルを持つ理由（§4.2）。