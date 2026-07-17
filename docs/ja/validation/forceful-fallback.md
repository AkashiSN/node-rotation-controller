# Forceful Fallback — シナリオ O

::: tip この検証の対象
ウィンドウ有界の forceful fallback（#156）、earliest-deadline ソート（#157）、do-not-disrupt 除外（#170）を同一デッドライン上で同時に走らせた実 EKS Auto Mode 検証。
:::

上記 3 機能を同時に行使した実 EKS 検証。この検証が validated に切り替える前提は [§7.2](/ja/specification/07-risks#72-検証済み前提) を、`noderotation_forceful_fallback_total` メトリクスと `ForcefulFallback` Warning イベントの運用上の意味は [ランブック](/ja/runbook#3-メトリクスリファレンス) を参照。

<TimelineForcefulFallback />

## シナリオカバレッジ

以下のマトリクスは、これまでの EKS Auto Mode PoC 各シナリオを仕様書のロードマップおよび未決事項に対して追跡したもの。各シナリオは実クラスターで再実行・観測されるまで「計画中」のまま — コードカバレッジ（unit / envtest / KWOK）は必要条件だが十分ではない。

<CoverageMatrix />
