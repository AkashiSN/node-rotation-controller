# node-rotation-controller — Specification

Functional specification for a Kubernetes controller that proactively rotates Karpenter-managed nodes in a make-before-break (surge) fashion within a configurable maintenance window, before Karpenter's forceful `expireAfter` is triggered.

Japanese translation: [docs/ja/specification/](../ja/specification/)

---

## Contents

1. **[Overview](./01-overview)** — [1.1 Background](./01-overview#11-background) · [1.2 Goals](./01-overview#12-goals) · [1.3 Non-Goals](./01-overview#13-non-goals) · [1.4 Terminology](./01-overview#14-terminology) · [1.5 Position in the Karpenter Ecosystem](./01-overview#15-position-in-the-karpenter-ecosystem)
2. **[Scope](./02-scope)** — [2.1 Scope and Compatibility](./02-scope#21-scope-and-compatibility) · [2.2 Composition with Existing Mechanisms](./02-scope#22-composition-with-existing-mechanisms)
3. **[Design](./03-design)** — [3.1 Maintenance Window](./03-design#31-maintenance-window) · [3.2 Candidate Selection](./03-design#32-candidate-selection) · [3.3 Surge Sequence (v1)](./03-design#33-surge-sequence-v1) · [3.4 Future versions (v2)](./03-design#34-future-versions-v2) · [3.5 Backstop Behavior](./03-design#35-backstop-behavior)
4. **[Operations](./04-operations)** — [4.1 Capacity / Availability](./04-operations#41-capacity--availability) · [4.2 Observability](./04-operations#42-observability) · [4.3 RBAC and Cloud Permissions](./04-operations#43-rbac-and-cloud-permissions) · [4.4 Cost](./04-operations#44-cost)
5. **[Implementation](./05-implementation)** — [5.1 Architecture](./05-implementation#51-architecture) · [5.2 Reconcile Loop](./05-implementation#52-reconcile-loop) · [5.3 State Model](./05-implementation#53-state-model) · [5.4 Configuration Schema](./05-implementation#54-configuration-schema)
6. **[Release](./06-release)** — [6.1 Versioning and Release](./06-release#61-versioning-and-release) · [6.2 Roadmap](./06-release#62-roadmap)
7. **[Risks & Status](./07-risks)** — [7.1 Risks](./07-risks#71-risks) · [7.2 Validated Assumptions](./07-risks#72-validated-assumptions) · [7.3 Open Questions](./07-risks#73-open-questions)
## References

- [Karpenter Disruption (official docs)](https://karpenter.sh/docs/concepts/disruption/)
- [Karpenter forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) — establishes "user-side implementation" as a valid path
- [Karpenter Discussion #1079 — Schedule for disruption](https://github.com/kubernetes-sigs/karpenter/discussions/1079) — whitelist limitation of Disruption Budgets
- [EKS Auto Mode docs](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)
- [EKS Auto Mode and maintenance window for "Drifted" nodes (AWS re:Post)](https://repost.aws/articles/ARbff3_8A_R7uiPMpCfjHznw/eks-auto-mode-and-maintenance-window-for-drifted-nodes)
