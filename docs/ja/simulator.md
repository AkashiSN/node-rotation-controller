---
layout: page
title: ポリシーシミュレーター
---

<!-- layout: page renders full-bleed with no gutters; this wrapper is the page's
     .policy-simulator CSS scope and carries the page padding (see custom.css). -->
<div class="policy-simulator">

# ポリシーシミュレーター

`RotationPolicy` とノード群を入力すると、**どのノードがいつローテーションされるか**、
そして各ノードが `expireAfter` のバックストップより前に間に合うかが分かります。

これは再実装ではありません。このページはコントローラー**自身**の Go コード
（§3.2 の導出、候補選択の述語、開始ゲート）を WebAssembly にコンパイルして実行します。
そのため、シミュレーターとコントローラーが乖離することはありません。

::: warning 対象範囲
このシミュレーターは、window-bounded forceful fallback を含むローテーションの開始・完了を
モデル化します。障害はモデル化しません（surge のタイムアウト、`retryBackoff`、
`failurePause`）。結果は本番環境の保証ではありません。
:::

<PolicySimulator />

</div>
