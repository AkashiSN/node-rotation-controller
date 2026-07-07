# 2. Scope

## 2.1 Scope and Compatibility

### Supported environments

| Environment | Status |
|-------------|--------|
| EKS Auto Mode | Primary target (the 21-day hard cap is the strongest motivating constraint) |
| Self-managed Karpenter v1+ on EKS | Supported |
| Karpenter on other CNCF distributions (AKS NAP, etc.) | Best-effort; the CRD API is the same, but the underlying Karpenter operator's (controller's) behavior may differ |

### Karpenter compatibility policy

`karpenter.sh/v1` is required; earlier versions (`v1beta1`, `v1alpha5`) are not supported.

The compatibility contract is the **stable `karpenter.sh/v1` CRD surface — not a specific Karpenter controller minor.** This matters for the primary target, **EKS Auto Mode**, which does not expose the exact managed Karpenter minor to users: the controller works against any cluster that serves a compatible `karpenter.sh/v1` `NodePool`/`NodeClaim` API, regardless of the Karpenter version Auto Mode runs underneath.

- **Runtime target.** EKS Auto Mode and any Karpenter v1+ cluster exposing a `karpenter.sh/v1`-compatible `NodePool`/`NodeClaim` API.
- **Build/test baseline.** The repository compiles and tests against the bundled `sigs.k8s.io/karpenter` Go module version pinned in [`go.mod`](../../go.mod) (currently `v1.13.0`). That pins the *typed Go API* the controller is built against; it is **not** a runtime requirement that the cluster run that exact Karpenter minor.
- **Interaction boundary.** The controller never calls Karpenter controller internals or any cloud-provider API — it interacts solely through Kubernetes API objects (the `NodeClaim`/`NodePool` CRDs, plus core `Node`/`Pod`). Unknown Auto Mode internals are therefore acceptable as long as the public `karpenter.sh/v1` surface is compatible (and §4.3 requires no cloud IAM).
- **Runtime enforcement.** A startup preflight (§5.1) fails fast when the cluster does not serve `karpenter.sh/v1` with the `nodeclaims`/`nodepools` resources, or RBAC cannot read them — turning a compatibility gap into an immediate, actionable error rather than a deferred reconcile failure.

**Required compatibility surface.** The controller depends only on the following public `karpenter.sh/v1` fields, labels, and annotations; anything outside this set (including all Karpenter controller internals) is irrelevant to compatibility:

| Kind / field / key | Used for |
|--------------------|----------|
| `NodeClaim`, `NodePool` (`karpenter.sh/v1`) | the rotation unit and its owning pool (§3.2, §3.3) |
| `NodeClaim.spec.expireAfter` | per-node deadline anchoring the trigger (§3.2) |
| `NodeClaim.spec.terminationGracePeriod` | per-node drain bound feeding `t_rot` / lead time (§3.2) |
| `NodeClaim.spec.requirements` | fallback source for placeholder requirement replication when a parity key is not surfaced as a node label (§3.3) |
| `NodeClaim.status.nodeName` | mapping a claim to its Node (§3.3, §5.2) |
| `NodeClaim.status.conditions[Ready]` | selection eligibility (§3.2) |
| `NodePool.spec.template.spec.expireAfter` | representative `E` for per-pool validation (§3.2) |
| `NodePool.spec.template.spec.terminationGracePeriod` | representative `tGP` (§3.2) |
| `NodePool.spec.template.spec.requirements` | placeholder requirement replication (§3.3) |
| `NodePool.spec.template.spec.taints` | placeholder tolerations (§3.3) |
| `NodePool.spec.limits` | surge headroom check (§3.2, §5.2) |
| `NodePool.status.resources` | provisioned footprint for the headroom check (§5.2) |
| label `karpenter.sh/nodepool` | pairing nodes / placeholder to the pool (§3.3) |
| annotation `karpenter.sh/do-not-disrupt` | freezing the surge pair against voluntary disruption (§3.3) |

## 2.2 Composition with Existing Mechanisms

| Mechanism | Relationship |
|-----------|--------------|
| Karpenter Consolidation / Drift | **Coexists.** This controller takes over only the Expiration path. Voluntary disruption from Consolidation/Drift still flows through Karpenter |
| NodePool `expireAfter` | **Coexists** as backstop. The derived `ageThreshold` sits below `expireAfter` by construction (`A = E − (K·P + t_rot)`, §3.2), and validation fails (**fatal**) when the schedule cannot guarantee the configured rotation chances — the gap is not hand-tuned |
| NodePool `terminationGracePeriod` | **Depended on.** After the controller deletes an old `NodeClaim`, Karpenter's termination controller honors PDBs during drain, bounded by `terminationGracePeriod` |
| PodDisruptionBudget | **Depended on.** Drain after the controller's `NodeClaim` deletion follows the voluntary path, so PDBs are strictly respected |
| `topologySpreadConstraints` | **Depended on.** Even with surge, all pods on a node disappear together when that node is finally drained. Spreading remains essential |
