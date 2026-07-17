# node-rotation-controller — Specification

Functional specification for a Kubernetes controller that proactively rotates Karpenter-managed nodes in a make-before-break (surge) fashion within a configurable maintenance window, before Karpenter's forceful `expireAfter` fires.

Japanese translation: [docs/ja/specification/](../ja/specification/)

---

## Contents

1. **[Overview](./01-overview)** — Background · Goals · Non-Goals · Terminology · Ecosystem Position
2. **[Scope](./02-scope)** — Compatibility · Composition with Existing Mechanisms
3. **[Design](./03-design)** — Maintenance Window · Candidate Selection · Surge Sequence · Mid-surge Protection · Pod-level Behavior · Forceful Fallback · Zonal Workloads · Rollback · Backstop
4. **[Operations](./04-operations)** — Capacity / Availability · Observability · RBAC · Cost
5. **[Implementation](./05-implementation)** — Architecture · Reconcile Loop · State Model · Configuration Schema
6. **[Release](./06-release)** — Versioning · Roadmap
7. **[Risks & Status](./07-risks)** — Risks · Validated Assumptions · Open Questions

## References

- [Karpenter Disruption (official docs)](https://karpenter.sh/docs/concepts/disruption/)
- [Karpenter forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) — establishes "user-side implementation" as a valid path
- [Karpenter Discussion #1079 — Schedule for disruption](https://github.com/kubernetes-sigs/karpenter/discussions/1079) — whitelist limitation of Disruption Budgets
- [EKS Auto Mode docs](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)
- [EKS Auto Mode and maintenance window for "Drifted" nodes (AWS re:Post)](https://repost.aws/articles/ARbff3_8A_R7uiPMpCfjHznw/eks-auto-mode-and-maintenance-window-for-drifted-nodes)
