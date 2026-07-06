# node-rotation-controller — 仕様書

Karpenter 管理下のノードを、Karpenter の forceful な `expireAfter` が発火する前に、設定可能なメンテナンスウィンドウ内で make-before-break（surge）方式に先回りローテーションする Kubernetes コントローラの機能仕様書。

English: [docs/specification/](../../specification/)

---

## 目次

1. **[概要](./01-overview)** — [1.1 背景](./01-overview#11-背景) · [1.2 ゴール](./01-overview#12-ゴール) · [1.3 非ゴール](./01-overview#13-非ゴール) · [1.4 用語](./01-overview#14-用語) · [1.5 Karpenter エコシステムでの位置付け](./01-overview#15-karpenter-エコシステムでの位置付け)
2. **[スコープ](./02-scope)** — [2.1 スコープと互換性](./02-scope#21-スコープと互換性) · [2.2 既存メカニズムとの関係](./02-scope#22-既存メカニズムとの関係)
3. **[設計](./03-design)** — [3.1 メンテナンスウィンドウ](./03-design#31-メンテナンスウィンドウ) · [3.2 候補選定](./03-design#32-候補選定) · [3.3 surge シーケンス（v1）](./03-design#33-surge-シーケンスv1) · [3.4 将来バージョン（v2）](./03-design#34-将来バージョンv2) · [3.5 バックストップ挙動](./03-design#35-バックストップ挙動)
4. **[運用](./04-operations)** — [4.1 Capacity / 可用性](./04-operations#41-capacity--可用性) · [4.2 観測性](./04-operations#42-観測性) · [4.3 RBAC と クラウド権限](./04-operations#43-rbac-と-クラウド権限) · [4.4 コスト](./04-operations#44-コスト)
5. **[実装](./05-implementation)** — [5.1 アーキテクチャ](./05-implementation#51-アーキテクチャ) · [5.2 Reconcile ループ](./05-implementation#52-reconcile-ループ) · [5.3 状態モデル](./05-implementation#53-状態モデル) · [5.4 設定スキーマ](./05-implementation#54-設定スキーマ)
6. **[リリース](./06-release)** — [6.1 バージョニングとリリース](./06-release#61-バージョニングとリリース) · [6.2 ロードマップ](./06-release#62-ロードマップ)
7. **[リスクと状況](./07-risks)** — [7.1 リスク](./07-risks#71-リスク) · [7.2 検証済み前提](./07-risks#72-検証済み前提) · [7.3 未決事項](./07-risks#73-未決事項)

## 参考

- [Karpenter Disruption（公式）](https://karpenter.sh/docs/concepts/disruption/)
- [Karpenter forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) — 「ユーザ側実装」を妥当解として位置付ける根拠
- [Karpenter Discussion #1079 — Schedule for disruption](https://github.com/kubernetes-sigs/karpenter/discussions/1079) — Disruption Budgets の whitelist 限界
- [EKS Auto Mode 公式ドキュメント](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)
- [EKS Auto Mode and maintenance window for "Drifted" nodes (AWS re:Post)](https://repost.aws/articles/ARbff3_8A_R7uiPMpCfjHznw/eks-auto-mode-and-maintenance-window-for-drifted-nodes)
