---
layout: home
hero:
  name: node-rotation-controller
  text: Karpenter ノードの Graceful ローテーション
  tagline: "Karpenter の expireAfter は予測不能なタイミングでノードを強制 drain する — 本コントローラーはそれを、メンテナンスウィンドウ内の graceful な置換で先回りする。"
  actions:
    - theme: brand
      text: はじめに
      link: /ja/getting-started
    - theme: alt
      text: 仕様書
      link: /ja/specification
    - theme: alt
      text: ランブック
      link: /ja/runbook
features:
  - title: ダウンタイムゼロの surge
    details: 旧ノードを drain する前に代替ノードを Ready にする。一時的な placeholder Pod で NodePool 所有の容量を誘導し、Karpenter がノードを起動、PDB が drain を制御する。
  - title: ウィンドウ有界のローテーション
    details: ローテーション開始はメンテナンスウィンドウ内に限定。自動導出される age 閾値により、各ノードは expireAfter の期限前に複数回のローテーション機会を得る。
  - title: デフォルトで安全
    details: expireAfter はバックストップとして温存し、撤廃しない。コントローラーが停止、またはローテーションが失敗しても、ノードはコントローラーなしの場合と同じように期限切れする — 現状より悪くならない。
  - title: EKS Auto Mode で検証済み
    details: 実 EKS Auto Mode クラスタでの E2E 検証を完了 — 12 時間の無人 soak、ゾーン制約 PV の再アタッチ、リーダーフェイルオーバー、graceful→forceful fallback 境界を含む。
---
