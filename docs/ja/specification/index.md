# node-rotation-controller — 仕様書

Karpenter の強制的な `expireAfter` が発動する前に、設定可能なメンテナンスウィンドウ内で Karpenter 管理ノードを make-before-break（surge）方式でプロアクティブにローテーションする Kubernetes コントローラーの機能仕様。

英語版（正本）: [docs/specification/](../../specification/)

---

## 目次

1. **[概要](./01-overview)** — 背景 · 目標 · 非目標 · 用語 · エコシステムにおける位置づけ
2. **[スコープ](./02-scope)** — 互換性 · 既存メカニズムとの連携
3. **[設計](./03-design)** — メンテナンスウィンドウ · 候補選定 · surge シーケンス · surge 中の保護 · Pod レベル動作 · 強制フォールバック · ゾーンワークロード · ロールバック · バックストップ
4. **[運用](./04-operations)** — キャパシティ/可用性 · オブザーバビリティ · RBAC · コスト
5. **[実装](./05-implementation)** — アーキテクチャ · Reconcile ループ · 状態モデル · 設定スキーマ
6. **[リリース](./06-release)** — バージョニング · ロードマップ
7. **[リスクと状況](./07-risks)** — リスク · 検証済み前提 · 未決事項

## 参考資料

- [Karpenter Disruption（公式ドキュメント）](https://karpenter.sh/docs/concepts/disruption/)
- [Karpenter forceful-expiration 設計](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) — 「ユーザー側での実装」を有効なパスとして確立
- [Karpenter Discussion #1079 — Schedule for disruption](https://github.com/kubernetes-sigs/karpenter/discussions/1079) — Disruption Budgets のホワイトリスト制限
- [EKS Auto Mode ドキュメント](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)
- [EKS Auto Mode と「Drifted」ノードのメンテナンスウィンドウ (AWS re:Post)](https://repost.aws/articles/ARbff3_8A_R7uiPMpCfjHznw/eks-auto-mode-and-maintenance-window-for-drifted-nodes)
