# 2. Scope

## 2.1 Scope and Compatibility

### Supported environments

| Environment | Status |
|-------------|--------|
| EKS Auto Mode | Primary target |
| Self-managed Karpenter v1+ on EKS | Supported |
| Karpenter on other CNCF (AKS NAP, etc.) | Best-effort |

- **EKS Auto Mode:** the 21-day hard cap is the strongest motivating constraint.
- **Other CNCF distributions:** the CRD API is the same, but underlying operator behavior may differ.

### Karpenter compatibility policy

`karpenter.sh/v1` is required; earlier versions (`v1beta1`, `v1alpha5`) are not supported.

The compatibility contract is the **stable `karpenter.sh/v1` CRD surface — not a specific Karpenter controller minor.** This matters for **EKS Auto Mode**, which does not expose the exact managed Karpenter minor to users.

- **Runtime target:** any cluster serving a compatible `karpenter.sh/v1` `NodePool`/`NodeClaim` API
- **Build/test baseline:** the bundled `sigs.k8s.io/karpenter` Go module version in [`go.mod`](../../go.mod) (currently `v1.13.0`). This pins the typed Go API, **not** a runtime requirement
- **Interaction boundary:** solely through Kubernetes API objects (`NodeClaim`/`NodePool` CRDs, plus core `Node`/`Pod`). No Karpenter internals, no cloud-provider API
- **Runtime enforcement:** a startup preflight (§5.1) fails fast when the cluster does not serve `karpenter.sh/v1` or RBAC cannot read it

### Required compatibility surface

The controller depends only on the following public `karpenter.sh/v1` fields:

| Kind / field / key | Used for |
|--------------------|----------|
| `NodeClaim`, `NodePool` | Rotation unit and owning pool |
| `NodeClaim.spec.expireAfter` | Per-node deadline (§3.2) |
| `NodeClaim.spec.terminationGracePeriod` | Drain bound for `t_rot` (§3.2) |
| `NodeClaim.spec.requirements` | Placeholder requirement fallback (§3.3) |
| `NodeClaim.status.nodeName` | Claim → Node mapping (§3.3, §5.2) |
| `NodeClaim.status.conditions[Ready]` | Selection eligibility (§3.2) |
| `NodePool.spec.template.spec.expireAfter` | Representative `E` for validation |
| `NodePool.spec.template.spec.terminationGracePeriod` | Representative `tGP` |
| `NodePool.spec.template.spec.requirements` | Placeholder requirement replication (§3.3) |
| `NodePool.spec.template.spec.taints` | Placeholder tolerations (§3.3) |
| `NodePool.spec.limits` | Surge headroom check (§3.2, §5.2) |
| `NodePool.status.resources` | Provisioned footprint for headroom |
| label `karpenter.sh/nodepool` | Pairing nodes/placeholder to pool |
| annotation `karpenter.sh/do-not-disrupt` | Freeze surge pair (§3.3) |

Anything outside this set — including all Karpenter controller internals — is irrelevant to compatibility.

## 2.2 Composition with Existing Mechanisms

| Mechanism | Relationship |
|-----------|--------------|
| Consolidation / Drift | Coexists |
| NodePool `expireAfter` | Coexists as backstop |
| `terminationGracePeriod` | Depended on |
| PodDisruptionBudget | Depended on |
| `topologySpreadConstraints` | Depended on |

- **Consolidation / Drift:** the controller takes over only the Expiration path. Voluntary disruption from Consolidation/Drift still flows through Karpenter.
- **`expireAfter`:** the derived `ageThreshold` sits below `expireAfter` by construction (`A = E − (K·P + t_rot)`, §3.2). Validation fails (**fatal**) when the schedule cannot guarantee the configured rotation chances — the gap is not hand-tuned.
- **`terminationGracePeriod`:** after the controller deletes an old `NodeClaim`, Karpenter's termination controller honors PDBs during drain, bounded by `terminationGracePeriod`.
- **PodDisruptionBudget:** drain follows the voluntary path, so PDBs are strictly respected.
- **`topologySpreadConstraints`:** even with surge, all pods on a node disappear together when that node is drained. Spreading remains essential.
