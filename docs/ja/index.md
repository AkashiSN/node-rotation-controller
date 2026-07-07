---
layout: home
hero:
  name: node-rotation-controller
  text: Karpenter のための Make-before-break Node ローテーション
  tagline: メンテナンスウィンドウ内で Karpenter 管理ノードを先回りローテーションし、expireAfter の発火を実質起こさせない。
  actions:
    - theme: brand
      text: はじめに
      link: /ja/getting-started
    - theme: alt
      text: 仕様書
      link: /ja/specification
    - theme: alt
      text: 検証
      link: /ja/validation/forceful-fallback
features:
  - title: Surge-first
    details: 低優先度の placeholder Pod を介して NodePool 所有の代替キャパシティを誘導する — Karpenter を迂回せず、単独の NodeClaim も作らない。
  - title: ウィンドウ有界
    details: expireAfter を下回る ageThreshold を導出し、expireAfter はバックストップとして温存する（撤廃しない）。
  - title: 実 EKS で検証済み
    details: Scenario O が graceful→forceful fallback の分岐、最早期限順、do-not-disrupt 除外を同一デッドライン上で実証。
---
