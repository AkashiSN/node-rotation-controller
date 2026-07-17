# 1. Overview

## 1.1 Background

::: tip What this section explains
Karpenter's Forceful Expiration fires at unpredictable times, ignoring Disruption Budgets and pre-provisioning. This controller exists to move that rotation earlier, into a controlled maintenance window, using the voluntary path.
:::

Karpenter (and EKS Auto Mode) classifies node disruption into two categories:

| Category | Examples | Budgets applied? | Pre-provisioned? | PDB |
|----------|----------|------------------|------------------|-----|
| Graceful | Drift, Consolidation | Yes | Yes | Strictly respected |
| **Forceful** | **Expiration**, Spot | **No** | **No** | Capped by `tGP` |

- **Why Expiration is forceful:** AMI patches and security updates must not be delayed indefinitely by misconfigured budgets or PDBs. Documented in upstream [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md), which explicitly identifies "operators implement their own graceful rotation" as an acceptable solution.
- **EKS Auto Mode constraint:** a **21-day maximum node lifetime** that users can reduce but not remove. The sum `expireAfter + terminationGracePeriod ≤ 21d` is enforced ([AWS builders.flash, 2025-04](https://aws.amazon.com/jp/builders-flash/202504/dive-deep-eks-node-automated-update/)). Defaults: `expireAfter` 336h (≈14d), `terminationGracePeriod` 24h ([Create a Node Pool](https://docs.aws.amazon.com/eks/latest/userguide/create-node-pool.html)).

**Practical consequence:** in any non-trivial cluster, nodes are force-drained at unpredictable times regardless of PDB settings. Karpenter begins provisioning a replacement only *after* the drain starts. For latency-sensitive workloads with strict capacity requirements (`request == limit`), this creates a forced pod-pending window that can collide with peak business hours.

## 1.2 Goals

| # | Goal |
|---|------|
| G1 | Prevent Forceful Expiration from firing |
| G2 | Eliminate the pending-pod window |
| G3 | Confine rotation to business-safe slots |
| G4 | Compose with existing protections |

- **G1:** Replace `NodeClaim` resources approaching an age threshold (derived per NodePool, §3.2) during a maintenance window, using the voluntary disruption path.
- **G2:** Add a NodePool-owned replacement node first, wait for `Ready`, then delete the old one (node-level surge / make-before-break). Pod-level ordering is delegated to PDB (§3.5).
- **G3:** Configurable maintenance window (weekday / time-of-day / timezone).
- **G4:** PDB, `topologySpreadConstraints`, preStop hooks, Pod Readiness Gates, ALB slow start — all remain active and are never replaced.

## 1.3 Non-Goals

| # | Non-Goal | Rationale |
|---|----------|-----------|
| N1 | Replace Consolidation / Drift | Only the Expiration path is taken over |
| N2 | Handle Spot interruption | 2-minute hard limit; use [AWS NTH](https://github.com/aws/aws-node-termination-handler) |
| N3 | Application-side warm-up | Belongs to `readinessProbe` / `readinessGate` / ALB slow start |
| N4 | Allow `expireAfter == 0` or bypass 21-day cap | Cannot be bypassed; retained as backstop |
| N5 | OS-patch reboot orchestration | Out of scope; see [kured](https://github.com/kubereboot/kured) |

## 1.4 Terminology

### Core concepts

- **NodeClaim:** Karpenter v1 CRD; a 1:1 representation of an underlying instance (e.g., EC2)
- **surge:** Creating a replacement node, waiting until `Ready`, then draining the old one (make-before-break)
- **placeholder (Pod):** A low-priority "pause" Pod the controller creates to induce NodePool-owned replacement capacity — Karpenter provisions a new node (or the scheduler bin-packs it onto existing spare) to host it. Never a standalone `NodeClaim` (§3.3)
- **maintenance window:** The **union** of one or more configured weekday/time-of-day ranges during which the controller may *start* a rotation. In-flight rotations complete past the window boundary
- **freeze:** A per-NodePool hold (`noderotation.io/freeze` annotation) that pauses even an in-flight `pending` rotation until it expires (§3.1) — distinct from the window, which gates only *starts*
- **age threshold:** The `creationTimestamp` age beyond which a `NodeClaim` becomes a rotation candidate. **Derived** per NodePool from the schedule and target rotation chances (`minRotationChances`), not set directly (§3.2)
- **candidate:** A `NodeClaim` meeting every selection condition (§3.2), eligible for rotation
- **governing policy:** The `RotationPolicy` (§5.4) that wins selector specificity for a given NodePool
- **backstop:** Karpenter's native `expireAfter` (Forceful Expiration), retained as a safety net

### Disruption paths

- **voluntary path:** Consolidation, Drift, and this controller's `NodeClaim` delete — honors PDBs
- **forceful path:** `expireAfter`, Spot Interruption — respects PDBs only up to `terminationGracePeriod`
- **forceful fallback:** Opt-in, window-bounded mode (`surge.forcefulFallback`, default off; ADR-0001) that deletes an at-risk `NodeClaim` in-window **without** the surge — still via the voluntary path (PDBs apply, §3.3)

### Symbols

Used throughout §3–§5. See §3.2 for the full derivation.

| Symbol | Meaning |
|--------|---------|
| `E` | `expireAfter` — NodeClaim lifetime before Forceful Expiration |
| `tGP` | `terminationGracePeriod` — drain bound |
| `P` | Worst-case window period (largest gap between consecutive occurrences, §3.1) |
| `t_rot` | Rotation duration bound = `readyTimeout + tGP + buffer` |
| `t_rot_est` | Expected rotation service time = `provisioningEstimate + drainEstimate` (layer-2 only) |
| `K` | `minRotationChances` — desired guaranteed rotation chances (floor 1) |
| `leadTime` | How early a node is selected = `K·P + t_rot` |
| `A` | `ageThreshold` — derived `A = E − (K·P + t_rot)` |
| `G` | Rotation chances the schedule actually guarantees |
| `D` | Maintenance-window duration (single occurrence) |
| `gap` | Shortest closed interval between consecutive occurrences |
| `m` | `surge.maxUnavailable` — concurrent rotations per NodePool (fixed `1` in v1) |
| `C` | Per-occurrence window capacity = `m · ceil(D / (t_rot_est + cooldownAfter))` |
| `N` | NodePool node count (layer-2 throughput check only) |

::: details Additional symbol notes — click to expand

- **`drainEstimate`:** expected healthy PDB-respecting drain (`surge.drainEstimate`); unset ⇒ `min(tGP, 10m)`. Layer-2 forecast only
- **`provisioningEstimate`:** expected surge provisioning (`surge.provisioningEstimate`); unset ⇒ `min(readyTimeout, 5m)`. Layer-2 forecast only (ADR-0003)
- **`cooldownAfter`:** post-success settle pause (gate A). Feeds `C` but not `t_rot`
- **`failurePause`:** post-failure inter-attempt pause (gate B, §4.4, ADR-0004). Feeds no derived symbol
- **`buffer`:** fixed controller constant (`4·shortRequeue = 2m`) covering detection lag. Feeds `t_rot` only — **not** `t_rot_est` or any operator config
- **`readyTimeout`:** configuration field feeding `t_rot`

:::

## 1.5 Position in the Karpenter Ecosystem

This controller operates in a layer **above** Karpenter. It does not alter Karpenter's behavior.

### Why upstream will not absorb this

The [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) design records the deliberate decision to keep Expiration forceful and lists three options:

1. (Recommended by upstream) Keep Expiration forceful as-is
2. Add a per-NodePool `expirationPolicy: Forceful | Graceful` field
3. **"Operators implement their own graceful rotation"**

This controller is option 3. The risk of upstream absorption is low.

### Why Disruption Budgets are not sufficient

| Requirement | Achievable? |
|-------------|-------------|
| Allow disruption only in window | △ Awkward |
| Apply window to Expiration | ✗ |
| Surge during Expiration | ✗ |

- **△ Awkward:** only via blacklisting (`nodes: "0"` outside the window via multiple budgets), because the algorithm takes the *minimum* across overlapping budgets — see [Discussion #1079](https://github.com/kubernetes-sigs/karpenter/discussions/1079)
- **✗ Budgets don't apply to Expiration** — Consolidation/Drift only
- **✗ No pre-provisioning** — Expiration is forceful

This controller fills the second and third gaps and substantially simplifies the first.

### Adjacent projects

| Project | Scope | Overlap |
|---------|-------|---------|
| Karpenter Disruption Budgets | Rate-limit Drift/Consolidation | Complementary |
| [kured](https://github.com/kubereboot/kured) | Reboot for OS patching | None |
| [AWS NTH](https://github.com/aws/aws-node-termination-handler) | Spot / scheduled events | None |
| [descheduler](https://github.com/kubernetes-sigs/descheduler) | Pod rebalancing | None |
| EKS Node Auto Repair | Replace unhealthy nodes | None |
