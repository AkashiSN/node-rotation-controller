---
layout: page
title: ポリシーシミュレーター
---

<div class="policy-simulator">
<div class="vp-doc">

# ポリシーシミュレーター

**デプロイ前にローテーションスケジュールを確認。** `RotationPolicy` の設定とノード数を入力すると、各ノードがいつローテーションされるか、そして `expireAfter` のバックストップ前に間に合うかが即座に分かる。

## いつ使うか

- メンテナンスウィンドウを決める前 — そのウィンドウ幅でフリートをさばけるかテスト
- `minRotationChances`、`cooldownAfter`、`expireAfter` を変えるとき — 効果を即座に確認
- `ThroughputBurstShortfall` や `ThroughputBelowArrival` の警告が出る理由を理解したいとき
- 同期バッチ（全ノードが同じ age）とスケジュールの相互作用を可視化したいとき

## 結果の読み方

- **緑のノード** — `expireAfter` 期限前にローテーションが完了。graceful パスが機能している。
- **オレンジのノード** — surge-less の forceful fallback で回された（有効時）。ウィンドウ内かつ PDB 尊重だが、make-before-break ではない。
- **赤のノード** — コントローラーが回しきれず `expireAfter` 期限に到達。Karpenter のネイティブな forceful expiration にフォールバック。

健全な設定では全て緑になる。オレンジはスループットが逼迫しているが制御下。赤はスケジュールの拡張が必要。

::: warning 対象範囲
シミュレーターはローテーションの開始・完了（forceful fallback 含む）をモデル化する。障害（surge タイムアウト、`retryBackoff`、`failurePause`）はモデル化しない。結果はベストケースの予測であり、本番環境の保証ではない。
:::

::: details 仕組み（技術詳細）
このページはコントローラー**自身**の Go コード — `ageThreshold` 導出、候補選択の述語、開始ゲート — を WebAssembly にコンパイルして実行する。シミュレーターとコントローラーは同一実装を共有し、乖離できない（CI がこれを保証する）。
:::

</div>

<PolicySimulator />

</div>
