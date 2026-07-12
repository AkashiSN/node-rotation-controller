# 1. Overview

## 1.1 Background

Karpenter (and EKS Auto Mode, which is built on Karpenter) classifies node disruption into two categories:

| Category | Examples | NodePool Disruption Budgets | Pre-provisioned replacement | PDB |
|----------|----------|------------------------------|------------------------------|-----|
| Graceful | Drift, Consolidation | Applied | Yes (make-before-break) | Strictly respected |
| **Forceful** | **Expiration**, Spot Interruption | **Not applied** | **No** | Respected, but capped by `terminationGracePeriod` |

Expiration is intentionally classified as forceful so that AMI patches and security-critical updates cannot be indefinitely delayed by misconfigured budgets or PDBs. This rationale is documented in the upstream Karpenter design [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md), which explicitly identifies "operators implement their own graceful rotation" as an acceptable solution path.

EKS Auto Mode further enforces a **21-day maximum node lifetime** that users can *reduce* but not remove — nodes have "a maximum lifetime of 21 days … after which they are automatically replaced" ([EKS Auto Mode user guide](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)). Because a node's true end-of-life is its `expireAfter` expiry **plus** up to `terminationGracePeriod` of drain, this ceiling is enforced as a constraint on their **sum**: `expireAfter + terminationGracePeriod ≤ 21d` (AWS states the sum "cannot exceed 21 days" — [AWS builders.flash, 2025-04](https://aws.amazon.com/jp/builders-flash/202504/dive-deep-eks-node-automated-update/)). For reference, the Auto Mode defaults are `expireAfter` 336h (≈14d) and `terminationGracePeriod` 24h ([Create a Node Pool](https://docs.aws.amazon.com/eks/latest/userguide/create-node-pool.html)).

The practical consequence: in any non-trivial cluster, nodes **will be force-drained at unpredictable times**, regardless of PDB settings, and Karpenter will only begin provisioning a replacement *after* the drain starts. For latency-sensitive workloads with strict capacity requirements (e.g., `request == limit`), this creates a window of forced pod-pending that can collide with peak business hours.

## 1.2 Goals

| # | Goal |
|---|------|
| G1 | **Prevent Forceful Expiration from firing in practice** by replacing `NodeClaim` resources that approach an age threshold (derived per NodePool from the maintenance schedule and a target number of rotation chances — see §3.2) during a defined maintenance window, using the voluntary disruption path |
| G2 | **Eliminate the pending-pod window** by adding a NodePool-owned replacement node first and waiting for it to be `Ready` before deleting the old one (node-level surge / make-before-break; Pod-level ordering is delegated to PDB — see §3.3) |
| G3 | **Confine rotation to business-safe time slots** via a configurable maintenance window (weekday / time-of-day / timezone) |
| G4 | **Compose with existing protections** — PDB, `topologySpreadConstraints`, preStop hooks, Pod Readiness Gates, ALB slow start — without replacing them |

## 1.3 Non-Goals

| # | Non-Goal | Rationale |
|---|----------|-----------|
| N1 | Replace Karpenter Consolidation / Drift | Karpenter's autonomous optimization remains active and beneficial. Only the Expiration path is taken over |
| N2 | Handle Spot interruption | Spot interruption is an AWS-side event with a 2-minute hard limit; use [AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) |
| N3 | Application-side warm-up | JVM warm-up, connection pool priming, and similar concerns belong to `readinessProbe` / `readinessGate` / ALB slow start. The controller orchestrates *node* placement; the application orchestrates its own readiness |
| N4 | Allow `expireAfter == 0` or removal of the 21-day hard cap | The Auto Mode cap cannot be bypassed. `expireAfter` is intentionally kept as a backstop in case the controller is unavailable |
| N5 | OS-patch reboot orchestration | Out of scope; see [kured](https://github.com/kubereboot/kured) |

## 1.4 Terminology

| Term | Definition |
|------|------------|
| **NodeClaim** | Karpenter v1 CRD; a 1:1 representation of an underlying instance (e.g., EC2) |
| **surge** | Creating a replacement node and waiting until it is `Ready` before draining the old one (make-before-break) |
| **placeholder (Pod)** | A low-priority "pause" Pod the controller creates to induce NodePool-owned replacement capacity — Karpenter provisions a new node (or the scheduler bin-packs it onto existing spare) to host it, never a standalone `NodeClaim` (§3.3) |
| **maintenance window** | The **union** of one or more configured weekday/time-of-day ranges during which the controller may *start* a rotation. In-flight rotations are allowed to complete past the window boundary |
| **freeze** | A per-NodePool hold on rotation, set via the `noderotation.io/freeze` annotation, that pauses even an in-flight `pending` rotation until it expires (§3.1) — distinct from the window, which gates only *starts* |
| **age threshold** | The `creationTimestamp` age beyond which a `NodeClaim` becomes a rotation candidate. **Derived** per NodePool from the schedule and the target rotation chances (`minRotationChances`), not set directly (§3.2). The actual per-node trigger anchors on each NodeClaim's own `spec.expireAfter` deadline; `ageThreshold` is its age-equivalent representative (§3.2) |
| **candidate** | A `NodeClaim` that meets every selection condition (§3.2) and is therefore eligible to be rotated |
| **governing policy** | The `RotationPolicy` (§5.4) that wins selector specificity for a given NodePool and thus supplies its schedule, `minRotationChances`, and `surge` settings |
| **backstop** | Karpenter's native `expireAfter` (Forceful Expiration), which still fires if the controller is unavailable. Intentionally retained as a safety net |
| **voluntary / forceful path** | Karpenter's two disruption classes (§1.1). The **voluntary path** (Consolidation, Drift, and the controller's own `NodeClaim` delete) honors PDBs; the **forceful path** (`expireAfter`, Spot Interruption) respects PDBs only up to `terminationGracePeriod`. This controller always routes through the voluntary path |
| **forceful fallback** | An opt-in, window-bounded mode (`surge.forcefulFallback`, default off; ADR-0001) that deletes an at-risk `NodeClaim` in-window **without** the surge — still via the voluntary path, so PDBs apply (§3.3) |

**Symbols** — used throughout §3–§5. See §3.2 for the full derivation and the authoritative per-node vs NodePool-template distinction (the **Source** column there).

| Symbol | Meaning |
|--------|---------|
| `E` | `expireAfter` — a NodeClaim's lifetime before Forceful Expiration (per-node, authoritative: `NodeClaim.spec.expireAfter`) |
| `tGP` | `terminationGracePeriod` — the bound Karpenter can hold a drain to |
| `P` | worst-case window period — the largest gap between consecutive maintenance-window occurrences (§3.1) |
| `t_rot` | upper bound on one node's rotation time = `readyTimeout + tGP + buffer`; the deadline bound (`leadTime`, `A`, `G`, §3.3, §5.2) — **not** used by layer 2 |
| `drainEstimate` | expected healthy PDB-respecting drain (`surge.drainEstimate`); unset ⇒ `min(tGP, 10m)`; layer-2 forecast only (§3.2) |
| `provisioningEstimate` | expected surge provisioning, candidate → Ready (`surge.provisioningEstimate`); unset ⇒ `min(readyTimeout, 5m)`; layer-2 forecast only (§3.2, ADR-0003) |
| `t_rot_est` | expected rotation service time = `provisioningEstimate + drainEstimate`; the layer-2 throughput denominator (§3.2). Carries no deadline term and no `buffer` — those are `t_rot`'s |
| `K` | `minRotationChances` — desired guaranteed rotation chances before expiry (floor 1) |
| `leadTime` | how early a node is selected before its deadline = `K·P + t_rot` |
| `A` | `ageThreshold` — the age at which a node becomes a candidate; derived `A = E − (K·P + t_rot)` |
| `G` | rotation chances the schedule actually guarantees; `G = K` under auto-derivation, recomputed for an explicit `ageThreshold` override |
| `D` | maintenance-window duration — the length of a single window occurrence (§3.2 layer 2) |
| `gap` | the shortest interval the window union stays **closed** between consecutive occurrences (§3.2 layer 2) |
| `m` | `surge.maxUnavailable` — concurrent rotations per NodePool; fixed at `1` in v1 (§3.2 layer 2) |
| `C` | per-occurrence window capacity — rotations one window occurrence can start, `C = m · ceil(D / (t_rot_est + cooldownAfter))` (§3.2 layer 2) |
| `N` | NodePool node count — used only by the layer-2 throughput check, not by the per-node derivation (§3.2) |

> `cooldownAfter`, `drainEstimate`, `provisioningEstimate`, and `readyTimeout` are configuration fields (§5.4), not derived symbols. `readyTimeout` feeds `t_rot` (the deadline bound); `drainEstimate` and `provisioningEstimate` feed `t_rot_est` (the forecast); `cooldownAfter` (the post-success settle, gate A) feeds `C`. `buffer` feeds `t_rot` too but is **not** a configuration field — it is a fixed controller constant (`4·shortRequeue = 2m`) covering the controller's own detection lag (§3.2), and it is deadline-side only (it does **not** feed `t_rot_est`). `failurePause` (the post-failure pause, gate B, §4.4, ADR-0004) is a configuration field too but feeds **no** derived symbol — it is a start gate only.

## 1.5 Position in the Karpenter Ecosystem

This controller is intentionally aligned with upstream Karpenter's design direction. It does not attempt to alter Karpenter's behavior; instead it operates in a layer above.

### Why upstream will not absorb this functionality

The Karpenter design [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) records the deliberate decision to keep Expiration forceful. It lists three options for users who want graceful expiration semantics:

1. (Recommended by upstream) Keep Expiration forceful as-is
2. Add a per-NodePool `expirationPolicy: Forceful | Graceful` field
3. **"Operators implement their own graceful rotation"**

This controller is option 3. Because upstream has explicitly identified user-side implementation as a legitimate solution, the risk of this project being made redundant by upstream absorption is low.

### Why Disruption Budgets are not sufficient

Karpenter's `NodePool.spec.disruption.budgets` supports `schedule + duration`, which superficially looks like a maintenance window. In practice it has two structural limitations — the two ✗ rows below; the first requirement (△) is achievable only awkwardly:

| Requirement | Achievable with vanilla Karpenter? |
|-------------|------------------------------------|
| Allow disruption only inside a window, deny outside | △ Only via blacklisting (set `nodes: "0"` outside the window via multiple budgets), because the budget algorithm takes the *minimum* across overlapping budgets — see [Discussion #1079](https://github.com/kubernetes-sigs/karpenter/discussions/1079) |
| Apply the window to Expiration | ✗ Budgets apply to Consolidation/Drift only, **not to Expiration** |
| Surge replacement during Expiration | ✗ Expiration is forceful; no pre-provisioning |

This controller fills the second and third rows above and substantially simplifies the first.

### Adjacent projects

| Project | Scope | Overlap |
|---------|-------|---------|
| Karpenter NodePool Disruption Budgets | Rate-limit Drift/Consolidation | Complementary; not applicable to Expiration |
| [kured](https://github.com/kubereboot/kured) | Reboot nodes for OS patching | None; doesn't manipulate NodeClaims |
| [AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) | Spot interruption / scheduled events | None; different trigger |
| [descheduler](https://github.com/kubernetes-sigs/descheduler) | Pod rebalancing | None; doesn't touch nodes |
| EKS Node Auto Repair (AWS managed) | Replace unhealthy nodes | None; not expiration-driven |
