# クラスタ全体の Pod キャッシュ / list スケーラビリティ

Status: **決定を記録済み — 現行の全件 list 方式を v0.x のターゲット規模で受け入れる。
`spec.nodeName` のフィールドインデックスは今後の最適化として記録する。**

issue #80 を追跡する。本ノートはコントローラのクラスタ全体 Pod 読み取りのコストを
測定し、そこから導いた決定を記録するものである。実行時の挙動を変更するものではない。

## なぜコントローラは全 Pod を読むのか

placeholder Pod は**候補ノード上の再スケジュール可能な Pod requests の総和**（spec §3.3）に
サイズ設定され、それらの Pod は*任意の namespace* に存在しうる。したがってコントローラは
クラスタ全体の Pod 可視性を必要とする — namespace スコープに絞ると namespace 横断の
ワークロードサポートが壊れるため、そうはできない。

現状その可視性はキャッシュ由来の全件 Pod list であり、`internal/controller/rotation_controller.go`
の `allPods()` がそれを担い、次の各所に供給される:

| ホットパス | 関数 | 備考 |
| --- | --- | --- |
| 候補のヘッドルームチェック | `candidateRequests` → `surge.ReschedulableRequests` | pending の間、ローテーションパスごと |
| placeholder 作成 | `createPlaceholder` → `surge.ReschedulableRequests` | placeholder の(再)作成ごとに 1 回 |
| surge-claim 回収ガード | `reapSurgeClaim` → `hostsRealPods` | ロールバック / absorb ホストのガード |

これらはいずれも**全体**の Pod スライスを受け取ってスキャンするが、実際に関係するのは
単一ノード（候補ノードまたは surge ノード）上の Pod だけである。いかなる最適化でも保たれ
なければならない除外セマンティクスは `internal/surge/requests.go` に存在する: DaemonSet、
mirror/static、完了済み（`Succeeded`/`Failed`）、そしてノードに固定された Pod である。

## ベンチマーク

`internal/surge/requests_bench_test.go` は 1k / 10k / 50k Pod を 200 namespace に分散させた
合成クラスタスナップショットを構築し、候補ノード上には現実的なミックス（通常のワーク
ロードに加え DaemonSet、完了済み、hostname 固定の Pod）を、大半は約 500 の他ノードに
スケジュール済みとして配置する。**実際にエクスポートされている** `surge.ReschedulableRequests`
と `surge.IsInfraOrCompleted`、加えて候補ノードへ事前フィルタする `Scoped` バリアント
（`spec.nodeName` インデックスの想定入力に相当）と、粗いスナップショットフットプリントの
プロキシをベンチマークする。

再現方法:

```
go test -bench='ReschedulableRequests|IsInfraOrCompleted|SnapshotFootprint' \
  -benchmem -run=^$ ./internal/surge/
```

結果（Apple M4 Pro、`goarch=arm64`、Go 1.26）:

```
BenchmarkReschedulableRequests/pods=1000-14         128328      8779 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequests/pods=10000-14         55826     20118 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequests/pods=50000-14         10000    104697 ns/op    17152 B/op   86 allocs/op
BenchmarkIsInfraOrCompleted-14                       380966      3047 ns/op        0 B/op    0 allocs/op
BenchmarkReschedulableRequestsScoped/pods=1000-14    162265      7521 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequestsScoped/pods=10000-14   159470      7349 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequestsScoped/pods=50000-14   171224      7066 ns/op    17152 B/op   86 allocs/op
BenchmarkSnapshotFootprint/pods=1000-14                1627    741693 ns/op   3714746 B/op   10333 allocs/op
BenchmarkSnapshotFootprint/pods=10000-14                223   5365664 ns/op  37091770 B/op  104730 allocs/op
BenchmarkSnapshotFootprint/pods=50000-14                 45  25918844 ns/op 185405625 B/op  524254 allocs/op
```

### 数値の読み方

- **Reconcile パスの CPU は無視できる。** **50,000 Pod** に対する全件スキャンでも 1 回
  あたり **約 105 µs** で、固定の **約 17 KB / 86 allocs** を確保するだけである（クラスタ
  規模に依存しない — この確保は結果の `ResourceList` によるものでスキャンによるものでは
  ない）。10k Pod では約 20 µs、1k では約 9 µs。これらの呼び出しは Pod イベントごとでは
  なくローテーションパスごとに数回走るだけなので、50k Pod クラスタでもパスあたりの CPU
  増加は 1 ミリ秒を大きく下回る。これは reconcile のネットワークラウンドトリップ（Get
  Node、Create/Delete NodeClaim）よりはるかに小さい。
- **スキャンは総 Pod 数に対して線形にスケールする**（約 2 ns/Pod）。予想どおり、すべての
  Pod を訪れて最初にその `spec.nodeName` を比較する。
- **インデックス化された想定パスはフラットである。** `Scoped`（候補ノードへ事前フィルタ
  済み）はクラスタ規模によらず約 7 µs にとどまる — 50k での約 15 倍の差は、`spec.nodeName`
  フィールドインデックスが取り除くであろう作業である。絶対値では 1 回あたり約 100 µs の
  CPU を取り除くが、これはボトルネックではない。
- **意味のあるコストはメモリであり、それはスキャンではなく informer キャッシュに存在する。**
  フットプリントプロキシは、controller-runtime の Pod キャッシュが保持しなければならない
  *オブジェクトデータ*を示している: ここでは約 **3.7 KB/Pod**、すなわち約 **3.7 MB @ 1k**、
  約 **37 MB @ 10k**、約 **185 MB @ 50k** である。実際にキャッシュされる `corev1.Pod` は
  この合成プロキシより重い（status・conditions・env・volumes・managedFields のフル）ため、
  これらは控えめな下限として扱うこと。本番の 50k Pod キャッシュは数百 MB から約 1 GB に
  なることが一般的である。

## 評価した選択肢（issue #80）

1. **全件のキャッシュ由来スキャンを維持する（現状維持）。** 最も単純で、すでに正しく、
   すべての除外セマンティクスを保つ。CPU コストはターゲット規模で無視できる。唯一の実質的な
   コストはクラスタ全体の Pod **キャッシュメモリ**であり、これは absorb ガードのために
   namespace 横断・全ノードの Pod 可視性を必要とすることに内在するもので、スキャンを
   変えても*回避されない*。

2. **`spec.nodeName` のフィールドインデックス。** controller-runtime のフィールドインデクサ
   （`mgr.GetFieldIndexer().IndexField(&corev1.Pod{}, "spec.nodeName", …)`）を登録し、3 つの
   `allPods()` スキャンを `List(ctx, &pods, client.MatchingFields{"spec.nodeName": node})` に
   置き換える。これにより呼び出しごとの線形スキャン（50k での約 100 µs が約フラットになる）が
   取り除かれ、各呼び出しのワーキングセットが 1 ノードの Pod に縮小する。ただし**キャッシュ
   メモリは削減しない** — controller-runtime はインデックスを維持するために依然としてすべての
   Pod をキャッシュする — ため、これはメモリではなく CPU/明快さの改善である。除外セマンティクスは
   変わらない: 同じ `surge.ReschedulableRequests` / `hostsRealPods` のフィルタがインデックスで
   絞られたスライスに対して走る。

3. **キャッシュセレクタ / namespace スコープ**（`cache.Options.ByObject` の label/field
   セレクタ、または `DefaultNamespaces`）。これはキャッシュ**メモリ**を削減する唯一の選択肢
   だが、**namespace 横断の要件と非互換**である。再スケジュール可能総和と absorb ホストガードは
   候補/surge ノード上の*任意の* namespace の Pod を見なければならないため、namespace で
   スコープできず、「ローテーション中のノードに着地しうるすべての Pod」を選ぶ安定した
   label 述語も存在しない。v1 では却下。（`spec.nodeName in {ローテーション中のノード}` に
   制限した将来のフィールドセレクタキャッシュは、ノード集合が動的であるため、静的なキャッシュ
   セレクタとしては表現できない。）

## 決定

**現行の全件 list 方式を v0.x で受け入れる。** MVP で想定するターゲットクラスタ規模
（最大で約 10k〜50k Pod）では reconcile パスの CPU はサブミリ秒であり、確保プロファイルは
固定かつ小さい。支配的なコスト — クラスタ全体の Pod キャッシュメモリ — は**設計が要求する
namespace 横断・全ノードの可視性に内在**するもので、いずれのスキャン最適化でも取り除かれ
ない。それを縮小するのは namespace/label スコープだけであり、それはコア要件を壊す
（選択肢 3、却下）。

**推奨するフォローアップ（ここでは未実装）: `spec.nodeName` フィールドインデックスを追加し**、
3 つのホットパスをノードスコープの list に切り替える（選択肢 2）。これは小さく、低リスクで、
明らかに有益な変更であり:

- 呼び出しごとの線形スキャンを取り除く（フラットな約 7 µs 対 最大約 105 µs）、
- 各呼び出しのワーキングセットを 1 ノードの Pod に絞る、
- 除外セマンティクスを変えずに保つ（既存の `internal/surge/requests` とコントローラの
  テストがフィルタをカバーし続ける）、
- ただし**キャッシュメモリは削減しない**ため、メモリ上限の解消ではなくレイテンシ/明快さの
  改善にとどまる。

これを本 PR で実装するのではなくフォローアップとして記録するのは、measure-and-decide の
変更をレビュー可能に保つためであり、また得られるものが正しさやメモリの修正ではなく CPU の
マイクロ最適化であるため — 「迷ったら計測して推奨する」というガイダンスにおける妥当な
しきい値である。実際の EKS Auto Mode ソークテスト（issue #77/#78）がキャッシュメモリ圧を
表面化させた場合、それは別個の懸念（controller-runtime レベルでのキャッシュスコープ/
ページネーション）であり、本スキャン最適化とは独立して追跡する。

### 提案するフォローアップ issue

> **perf(controller): Pod を `spec.nodeName` でインデックス化し、3 つの Pod 読み取り
> ホットパスをスコープする。** `spec.nodeName` フィールドインデクサを登録し、
> `candidateRequests`・`createPlaceholder`・`reapSurgeClaim` の `allPods()` を
> ノードスコープの `MatchingFields` list に置き換える。`internal/surge/requests` と
> コントローラのテストをグリーンに保ち、DaemonSet / mirror / 完了済み / ノード固定の
> 除外セマンティクスを保つこと。ベンチマークは `docs/reference/perf/pod-cache-scalability.md`
> を参照。
