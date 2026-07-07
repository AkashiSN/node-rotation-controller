# Forceful fallback — Scenario O

ウィンドウ有界の forceful fallback（#156）、最早期限順（#157）、do-not-disrupt
除外（#170）を同一デッドライン上で同時に走らせた実 EKS 検証。この検証が実証する前提
については [仕様書 §7.2](/ja/specification/07-risks#72-検証済み前提) を、
`noderotation_forceful_fallback_total` メトリクスと `ForcefulFallback` Warning
イベントが運用上何を意味するかについては
[ランブック](/ja/runbook#3-noderotation_-メトリクスの読み方) を参照。

<TimelineForcefulFallback />

## シナリオカバレッジ

以下のマトリクスは、これまでに実施した EKS Auto Mode PoC の各シナリオを、仕様書の
ロードマップおよび未決事項セクションにある前提・エッジケースに対して追跡したもの
である。各シナリオは、実クラスタで再実行・観測されるまで「計画中」のままとする —
コード上のカバレッジ（unit / envtest / KWOK）は必要条件だが、それだけでは行を「検証済み」
に切り替えられない。

<CoverageMatrix />
