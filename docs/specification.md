# node-rotation-controller — Specification

Functional specification for a Kubernetes controller that proactively rotates Karpenter-managed nodes in a make-before-break (surge) fashion within a configurable maintenance window, before Karpenter's forceful `expireAfter` is triggered.

Japanese translation: [docs/ja/specification.md](ja/specification.md)

---

## 1.1 Background

Karpenter (and EKS Auto Mode, which is built on Karpenter) classifies node disruption into two categories:

| Category | Examples | NodePool Disruption Budgets | Pre-provisioned replacement | PDB |
|----------|----------|------------------------------|------------------------------|-----|
| Graceful | Drift, Consolidation | Applied | Yes (make-before-break) | Strictly respected |
| **Forceful** | **Expiration**, Spot Interruption | **Not applied** | **No** | Respected, but capped by `terminationGracePeriod` |

Expiration is intentionally classified as forceful so that AMI patches and security-critical updates cannot be indefinitely delayed by misconfigured budgets or PDBs. This rationale is documented in the upstream Karpenter design [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md), which explicitly identifies "operators implement their own graceful rotation" as an acceptable solution path. EKS Auto Mode further enforces a **21-day hard cap** on node lifetime (`expireAfter + terminationGracePeriod ≤ 21d`) that cannot be removed by users.

The practical consequence: in any non-trivial cluster, nodes **will be force-drained at unpredictable times**, regardless of PDB settings, and Karpenter will only begin provisioning a replacement *after* the drain starts. For latency-sensitive workloads with strict capacity requirements (e.g., `request == limit`), this creates a window of forced pod-pending that can collide with peak business hours.

## 1.2 Goals

| # | Goal |
|---|------|
| G1 | **Prevent forceful Expiration from firing in practice** by replacing `NodeClaim` resources that approach an age threshold (derived per NodePool from the maintenance schedule and a target number of rotation chances — see §3.2) during a defined maintenance window, using the voluntary disruption path |
| G2 | **Eliminate the pending-pod window** by adding a NodePool-owned replacement node first and waiting for it to be `Ready` before deleting the old one (node-level surge / make-before-break; Pod-level ordering is delegated to PDB — see §3.3) |
| G3 | **Confine rotation to business-safe time slots** via a configurable maintenance window (weekday / time-of-day / timezone) |
| G4 | **Compose with existing protections** — PDB, `topologySpreadConstraints`, preStop hooks, Pod Readiness Gates, ALB slow start — without replacing them |

## 1.3 Non-Goals

| # | Non-Goal | Rationale |
|---|----------|-----------|
| N1 | Replace Karpenter Consolidation / Drift | Karpenter's autonomous optimization remains active and beneficial. Only the Expiration path is taken over |
| N2 | Handle Spot interruption | Spot interruption is an AWS-side event with a 2-minute hard limit; use [AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler) |
| N3 | Application-side warm-up | JVM warm-up, connection pool priming, and similar concerns belong to `readinessProbe` / `readinessGate` / ALB slow start. The controller orchestrates *node* placement; the application orchestrates its own readiness. A v3 hook for synthetic warm-up jobs is reserved for the future |
| N4 | Allow `expireAfter == 0` or removal of the 21-day hard cap | The Auto Mode cap cannot be bypassed. `expireAfter` is intentionally kept as a backstop in case the controller is unavailable |
| N5 | OS-patch reboot orchestration | Out of scope; see [kured](https://github.com/kubereboot/kured) |

## 1.4 Terminology

| Term | Definition |
|------|------------|
| **NodeClaim** | Karpenter v1 CRD; a 1:1 representation of an underlying instance (e.g., EC2) |
| **surge** | Creating a replacement node and waiting until it is `Ready` before draining the old one (make-before-break) |
| **maintenance window** | The **union** of one or more configured weekday/time-of-day ranges during which the controller may *start* a rotation. In-flight rotations are allowed to complete past the window boundary |
| **age threshold** | The `creationTimestamp` age beyond which a `NodeClaim` becomes a rotation candidate. **Derived** per NodePool from the schedule and the target rotation chances (`minRotationChances`), not set directly (§3.2). The actual per-node trigger anchors on each NodeClaim's own `spec.expireAfter` deadline; `ageThreshold` is its age-equivalent representative (§3.2) |
| **backstop** | Karpenter's native `expireAfter` (Forceful Expiration), which still fires if the controller is unavailable. Intentionally retained as a safety net |

## 1.5 Position in the Karpenter Ecosystem

This controller is intentionally aligned with upstream Karpenter's design direction. It does not attempt to alter Karpenter's behavior; instead it operates in a layer above.

### Why upstream will not absorb this functionality

The Karpenter design [`forceful-expiration.md`](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) records the deliberate decision to keep Expiration forceful. It lists three options for users who want graceful expiration semantics:

1. (Recommended by upstream) Keep Expiration forceful as-is
2. Add a per-NodePool `expirationPolicy: Forceful | Graceful` field
3. **"Operators implement their own graceful rotation"**

This controller is option 3. Because upstream has explicitly identified user-side implementation as a legitimate solution, the risk of this project being made redundant by upstream absorption is low.

### Why Disruption Budgets are not sufficient

Karpenter's `NodePool.spec.disruption.budgets` supports `schedule + duration`, which superficially looks like a maintenance window. In practice it has two structural limitations:

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

---

## 2.1 Scope and Compatibility

### Supported environments

| Environment | Status |
|-------------|--------|
| EKS Auto Mode | Primary target (the 21-day hard cap is the strongest motivating constraint) |
| Self-managed Karpenter v1+ on EKS | Supported |
| Karpenter on other CNCF distributions (AKS NAP, etc.) | Best-effort; CRD API is the same but operator semantics may differ |

### Karpenter API version

`karpenter.sh/v1` is required. Earlier versions (`v1beta1`, `v1alpha5`) are not supported.

## 2.2 Composition with Existing Mechanisms

| Mechanism | Relationship |
|-----------|--------------|
| Karpenter Consolidation / Drift | **Coexists.** This controller takes over only the Expiration path. Voluntary disruption from Consolidation/Drift still flows through Karpenter |
| NodePool `expireAfter` | **Coexists** as backstop. Recommended to keep `expireAfter > age threshold` |
| NodePool `terminationGracePeriod` | **Depended on.** After the controller deletes an old `NodeClaim`, Karpenter's termination controller honors PDBs during drain, bounded by `terminationGracePeriod` |
| PodDisruptionBudget | **Depended on.** Drain after the controller's `NodeClaim` deletion follows the voluntary path, so PDBs are strictly respected |
| `topologySpreadConstraints` | **Depended on.** Even with surge, all pods on a node disappear together when that node is finally drained. Spreading remains essential |

---

## 3.1 Maintenance Window

```yaml
maintenanceWindows:        # a list; the effective window is the UNION of all entries
  - timezone: Asia/Tokyo   # IANA tz database name
    days: [Wed, Sat]       # ISO weekday names: Mon/Tue/Wed/Thu/Fri/Sat/Sun
    start: "02:00"
    end:   "06:00"
```

**Semantics**:

- The reconciler is **always running**; window membership is evaluated on each reconcile tick (1-minute Ticker).
- `maintenanceWindows` is a **list**; the effective maintenance window is the **union** of all entries. This lets operators combine schedules (e.g., a weekday slot plus a weekend slot) to increase rotation frequency.
- Outside the (union) window the reconcile loop is a no-op.
- The window controls only **rotation starts**. An in-flight rotation continues past the window boundary (aborting mid-drain is more dangerous than letting it complete).
- A single `NodePool` may be **frozen** by an annotation (e.g., `noderotation.io/freeze=<RFC3339 timestamp>`) to suppress rotation until that time (use case: business-critical periods).

The **worst-case window period `P`** — the largest gap between the start of one window occurrence and the start of the next over the recurring cycle — is derived from this union and feeds the `ageThreshold` derivation in §3.2. For example, the union `{Wed 02:00, Sat 02:00}` has gaps `Wed→Sat = 3d` and `Sat→Wed = 4d`, so `P = 4d`.

> **Note (DST).** `P` is computed over the recurring **wall-clock** cycle. A daylight-saving transition can shift an individual gap by ±1h; v1 treats this as a known approximation and does not special-case it.

## 3.2 Candidate Selection

A `NodeClaim` becomes a rotation candidate when **all** of the following hold:

| Condition | Default | Notes |
|-----------|---------|-------|
| `now() > deadline − leadTime`, where `deadline = NodeClaim.metadata.creationTimestamp + NodeClaim.spec.expireAfter` and `leadTime = K·P + t_rot` | `leadTime` is **derived** (see below), not set directly | Anchored on each NodeClaim's **own** `spec.expireAfter` (its authoritative expiry), **not** the NodePool template. The derived `ageThreshold` is the age-equivalent of this trigger; defaults to `auto`, an explicit override is allowed but still validated |
| Belongs to a `NodePool` matched by the configured selector | Required | A `NodePool` matched by `nodepoolSelectors` is in scope |
| `status.conditions[Ready] == True` | Required | NotReady NodeClaims are skipped — an already-unhealthy node is left to EKS Node Auto Repair and the `expireAfter` backstop, not rotated here (the controller only owns the health of nodes it created during a surge) |
| `metadata.annotations["noderotation.io/state"]` is empty, or `failed` past `retryBackoff` | Required | `pending`/`draining` are in-flight and driven by §5.2 step 1, not re-selected here; `failed` is retried after backoff (§5.3) |

When multiple claims are eligible they are sorted by age (oldest first).

### Deriving `ageThreshold` from the desired rotation chances

Rather than hand-tuning `ageThreshold` (which is error-prone — a too-loose value lets Forceful Expiration fire before a window arrives), the controller **derives it per NodePool** from the schedule and a target number of rotation chances.

> **This is the central race.** Forceful Expiration fires at each node's `deadline` regardless of maintenance windows or PDBs, so the controller must *finish* a graceful surge rotation **before** that moment — every cycle. Candidate selection **is** that lookahead: a node is selected once its `deadline` falls within `leadTime = K·P + t_rot` of now (equivalently, `age > ageThreshold`). Read `leadTime` left to right — `K` worst-case window cycles (`K·P`) to *catch* a window, plus one node's completion time (`t_rot`) to *finish* inside it — and it guarantees the node sees at least `K` maintenance windows with enough headroom to complete before `expireAfter` can fire. `K ≥ 2` keeps a retry in hand if a window is missed or slow. The derivation below picks the largest such threshold so rotation still happens as late as safely possible.

**Symbols**

| Symbol | Meaning | Source |
|--------|---------|--------|
| `E` | `expireAfter` | Per-node: **`NodeClaim.spec.expireAfter`** (authoritative; anchored at the NodeClaim's `creationTimestamp`). NodePool `spec.template.spec.expireAfter` is used only as the **representative** for per-NodePool validation/logging — it does **not** propagate to existing NodeClaims (see note below) |
| `tGP` | `terminationGracePeriod` | Per-node: `NodeClaim.spec.terminationGracePeriod`; NodePool `spec.template.spec.terminationGracePeriod` as the representative |
| `P` | worst-case window period (largest gap between consecutive window occurrences) | derived from the `maintenanceWindows` union (§3.1) |
| `t_rot` | upper bound on a single node's rotation time = `readyTimeout + tGP + buffer` (**`cooldownAfter` is not included** — the node is already drained before the cooldown; see the margin note below) | derived from config + NodePool |
| `K` | desired guaranteed rotation chances (`minRotationChances`) | user-set; floor **1** |

**Derivation** — pick the *largest* threshold that still guarantees `K` completable chances inside `[ageThreshold, E)`, so rotation happens as late as safely possible (minimizing churn and surge cost):

```
ageThreshold (A) = E − (K·P + t_rot)
```

This holds because the usable interval `[A, E − t_rot]` then spans at least `K` window occurrences in the worst phase (`floor(((E − t_rot) − A) / P) ≥ K`), each with `t_rot` of headroom to complete before `E`.

> **Margin.** The bound is **tight**: the worst-phase guarantee is *exactly* `K` (`floor(K·P / P) = K`), with no built-in slack. Any safety margin must therefore come from `K` itself — `K ≥ 2` is recommended so a single missed or slow window still leaves a retry. `cooldownAfter` is the settle pause *between* consecutive rotations within a window; it does **not** count toward a single node's completion time (`t_rot`, which is why it was removed above) but it **does** factor into throughput (layer 2 below).

> **Authoritative expiry source.** The deadline that drives the *per-node* trigger is read from each **`NodeClaim.spec.expireAfter`**, anchored at that NodeClaim's `creationTimestamp` — **not** from the NodePool's `spec.template.spec.expireAfter`. Karpenter stamps `expireAfter` onto the NodeClaim at creation, and Forceful Expiration fires at `creationTimestamp + NodeClaim.spec.expireAfter`. Later edits to the NodePool template do **not** propagate to existing NodeClaims; they only trigger drift-based replacement. The controller therefore anchors `leadTime` on each node's own `deadline`, and uses the template `E` solely as the **representative** value for the per-NodePool startup validation and the logged/derived `ageThreshold` (§4.2). When a node's own `spec.expireAfter` differs from the template (e.g. mid-drift, or after a template change), its trigger follows its own value — so the identity `now() > deadline − leadTime ⟺ age > ageThreshold` holds exactly only when the two coincide.

**Validation** (layer 1 — scheduling feasibility)

| Condition | Outcome |
|-----------|---------|
| `K < 1` | **fatal** — invalid config |
| `K < 2` (i.e. `K = 1`) | **warn** — a single missed/failed window leaves no retry before Forceful Expiration |
| `A ≤ 0` (i.e. `E ≤ K·P + t_rot`; the schedule cannot guarantee even `K` chances) | **fatal** — raise `E` (Auto Mode allows up to `21d − tGP`), add window occurrences to shrink `P`, or lower `K` |
| `0 < A < P` (a node becomes a candidate before it has lived even one window period) | **warn** — extremely aggressive: nodes rotate very young, maximizing churn/surge cost. Raise `E` or lower `K` |
| Auto Mode and `E + tGP > 21d` | **warn** — violates the hard cap |
| NodePool `spec.limits` leaves no room for a `+1` surge node (node count already at `limits`) | **warn** — surge cannot land while at the limit; raise `limits` to allow baseline `+1`. The controller re-checks this at rotation start (§5.2) |

**Validation** (layer 2 — throughput) — independent of the derivation; it only **warns** and never changes `A`. Because rotations are serial within a window and separated by `cooldownAfter`, each window occurrence of duration `D` can rotate `C = m · floor(D / (t_rot + cooldownAfter))` nodes (`m = surge.maxUnavailable`, fixed at `1` in v1). If the candidate arrival rate exceeds capacity (`C < N · P / A`, where `N` is the NodePool node count), candidates accumulate and some may reach Forceful Expiration:

- **warn**: widen windows (larger `D`), add occurrences (smaller `P`), or raise `maxUnavailable` (reserved for a later version).

> **Worked example.** Auto Mode, `E = 14d`, `tGP = 1h`, union `{Wed, Sat} 02:00–06:00` → `P = 4d`, `t_rot ≈ 1.5h` (`readyTimeout 15m + tGP 1h + buffer`), `K = 2`. Then `A = 14d − (2·4d + 1.5h) ≈ 5.9d`: nodes become candidates at ~5.9d and are guaranteed 2 windows before 14d. Throughput `C = floor(4h / (1.5h + 10m)) = 2` per occurrence.
>
> A **weekly-only** window `{Sat}` has `P = 7d`, so `A = 14d − (2·7d) = 0` → **fatal**: weekly windows cannot guarantee 2 chances at `E = 14d`. This is exactly why a fixed `expireAfter − 4d` default was unsafe; the derivation surfaces it and tells the operator to raise `E` (to ~`20d`, giving `A ≈ 6d`) or add a window day.

The derived `A`, the guaranteed chances `G`, and `P` are surfaced per NodePool via startup logs and metrics (§4.2). With the auto-derivation, `G = K` by construction; the separate `G` exists so that when an explicit `ageThreshold` override is used, `G` is **recomputed from that override** and can be observed diverging from the requested `K`.

## 3.3 Surge Sequence (v1)

A single reconcile cycle handles **one** node. v1 enforces serial processing **per NodePool** (`surge.maxUnavailable = 1`) to minimize blast radius; distinct NodePools may rotate concurrently.

### Surge into the *same* NodePool — not a standalone node

The replacement node must belong to the **same NodePool** as the node being replaced. The controller therefore does **not** rotate by creating a standalone `NodeClaim`. (A standalone NodeClaim *is* provisionable — see §7.2 — but the resulting node has no NodePool owner, so its pods would persist on an unmanaged node that sits outside NodePool accounting, expiry, drift, and disruption budgets. In a cluster that deliberately separates NodePools, e.g. `api` vs `batch`, that is unacceptable.)

Instead, the controller induces Karpenter to add a NodePool-owned node by creating a temporary **placeholder Pod** — a single low-priority "pause" Pod that the controller **creates and manages directly** (deliberately *not* via a Deployment/ReplicaSet/Job). Its scheduling requirements are copied from the **candidate node** — most importantly the AZ (`topology.kubernetes.io/zone`), plus the arch / instance-type / capacity-type constraints the rescheduled Pods depend on (see *Stateful and zonal workloads* below) — and its resource requests are set to the **sum of the resource requests of the Pods currently scheduled on the candidate node** — the workload that must re-land after the drain. This forces Karpenter to provision a new node, in the same zone and large enough to host that workload, rather than bin-pack the placeholder onto existing capacity. Karpenter provisions a new node *within that NodePool*. Once the old node is drained, the placeholder is removed and the new node is a normal member of the NodePool.

Because the placeholder is a **bare Pod** (not backed by any controller) and is low-priority, when the rescheduled workload Pods need its space the scheduler **preempts** it and the placeholder is simply **deleted with no replacement**. (A Deployment/Job-backed pod would instead be recreated and re-pend, inducing extra node churn — which is exactly why a bare, controller-managed Pod is used.) Its only role is to reserve one node's worth of capacity until the drain lands the real Pods on it.

### Guarding against mid-surge disruption

While the old and new nodes coexist, Karpenter's Consolidation/Drift could race the controller:

- the **new** node, briefly underutilized, could be judged "empty/underutilized" and consolidated away immediately;
- the **old** node could be consolidated/drifted before the controller has finished orchestrating, or be chosen for removal ahead of the intended order.

To prevent both, the controller applies `karpenter.sh/do-not-disrupt` to **both** the old and the new node for the duration of the surge. This blocks Karpenter's voluntary disruption **and Forceful Expiration (`expireAfter`)** on the annotated nodes — only the EKS Auto Mode **21-day hard cap** cannot be suppressed by it (it remains the absolute ceiling). The controller's own explicit `delete` of the old NodeClaim still drains it regardless (deletion is handled by the termination controller, which does not consult `do-not-disrupt`). The annotations are removed at the end so the new node rejoins normal management. (**Implication:** if the controller dies mid-surge with the annotation still on the old node, `expireAfter` is *suppressed* on that node until the startup sweep clears the marker or the 21-day cap fires — see §3.5.)

The diagram below is the **logical** sequence of one rotation. It is **not** executed as a single blocking call: the controller implements it as a **non-blocking, requeue-driven state machine** (§5.2), persisting progress in the `noderotation.io/state` annotation on the old NodeClaim (§5.3). Each `wait_*` step is therefore *a state that is re-evaluated on subsequent reconciles*, not a goroutine that blocks a worker. The `[state: …]` tags map each step to that annotation.

```
ROTATION (logical sequence; each step is a separate reconcile)
  │
  ├─ select candidate (old node to retire)              [state: (none) → pending]
  │     annotate(candidate, state=pending, started-at=now)
  │     annotate(candidate.node, do-not-disrupt=true)   // freeze old node from voluntary disruption
  │     placeholder := create_placeholder_workload(
  │         nodepool     = candidate.nodepool,          // SAME NodePool
  │         requirements = match(candidate.node, surge.matchNodeRequirements), // same zone/arch/... (zonal PV rebind)
  │         requests     = sum(requests of pods on candidate),
  │         annotations  = {do-not-disrupt: true},
  │         priority     = low,
  │         labels       = {surge-for: candidate.name},
  │     )                                               // Karpenter adds a NodePool-owned node in the same AZ
  │
  ├─ surge_ready?  (placeholder scheduled onto a NEW node, created after started-at → that node Ready)   [state: pending]
  │     yes → annotate(new_node, do-not-disrupt=true)
  │           annotate(candidate, state=draining)
  │           delete(candidate)                         // explicit; not blocked by do-not-disrupt
  │     no, placeholder missing (lost / crash after state write) →
  │           recreate_placeholder(candidate); requeue(30s)
  │     no, and elapsed(started-at) > readyTimeout(15m) → FAIL:
  │           delete(placeholder); unfreeze(candidate.node)
  │           annotate(candidate, state=failed, failed-at=now); alert
  │     else → requeue(30s)                             // still waiting; non-blocking
  │
  ├─ candidate_gone?  (old NodeClaim finalized away)              [state: draining]
  │     // Karpenter termination controller drains gracefully, respecting PDBs
  │     // up to terminationGracePeriod.
  │     yes → delete(placeholder)                       // release the pause pod;
  │           unfreeze(new_node)                        //   its node stays as NodePool capacity
  │           emit_metrics(success)
  │     else → requeue(30s)                             // bounded by terminationGracePeriod + buffer
  │
  └─ cooldown(10m); requeue                             [state: (cleared by deletion)]
```

> The placeholder's only job is to add exactly one node's worth of capacity to the NodePool ahead of the drain (make-before-break). Its requests are sized to the **sum of the candidate node's current Pod requests**, so Karpenter must launch a *new* node to fit it. As a second guard, the `surge_ready` check additionally requires that the placeholder actually landed on a node whose `creationTimestamp` is *after* `started-at` (i.e. genuinely new, not bin-packed onto pre-existing capacity) — so a falsely-satisfied surge can never delete the old node with no real headroom added. The exact request padding is finalized in the PoC.

### Pod-level behavior — node-level make-before-break only

The make-before-break in this design is at the **node** level, not the Pod level. The controller does **not** perform a rolling update of Pods: it does not pre-create new Pods on the surge node before terminating the old ones. The surge node is added as **empty capacity**.

When the old `NodeClaim` is deleted, Karpenter's termination controller drains the old node through the **Eviction API** (PDBs respected). Each evicted Pod is deleted, and its owning workload controller (Deployment/ReplicaSet/StatefulSet) creates a **replacement Pod** that the scheduler then places onto available capacity — typically the surge node. This is fundamentally **evict-then-reschedule**, so a replacement Pod is *not* guaranteed to be `Ready` before the old Pod terminates (see §4.1).

The surge node's role is therefore to **pre-stage a landing zone** so that PDB-gated eviction proceeds without a long pending window — not to order Pods. Pod-level safety is delegated to the workload's **PodDisruptionBudget** and replica headroom:

- With a strict PDB (e.g., `minAvailable` equal to the desired replica count), the Eviction API blocks further evictions until replacement Pods are `Ready`. Because the surge node provides the capacity for those replacements to schedule and become `Ready`, the drain effectively becomes Pod-level make-before-break.
- With a loose or absent PDB, evictions proceed in bulk and `readyReplicas` dips (§4.1).

In short: the controller guarantees a node-level surge; **Pod-level make-before-break is achieved by PDB + replica headroom, which the surge node's capacity enables — not by the controller itself** (consistent with G4).

### Stateful and zonal workloads — matching the replacement node's requirements

Because surge only **adds capacity** and never pins Pods to the new node (above), the rescheduled Pods land wherever the scheduler can place them. A Pod bound to a **zonal** PersistentVolume — EBS `gp3`/`io2`, or any volume whose PV carries a `topology.kubernetes.io/zone` `nodeAffinity` — can only reschedule onto a node in the **same AZ** as its volume. If the surge node is provisioned in a *different* AZ, that Pod has nowhere to land and stays `Pending` after the old node drains — defeating make-before-break for exactly the stateful workloads that need it most.

The placeholder therefore replicates the **candidate node's scheduling requirements**, not merely the NodePool's labels. **Which** requirements are replicated is **configurable** via `surge.matchNodeRequirements` (§6): each listed key is copied from the candidate node onto the placeholder — either as a **`required`** (hard `nodeAffinity` / `nodeSelector`, value = the candidate's) or a **`preferred`** (soft `nodeAffinity`, relaxed under capacity pressure) constraint.

- The default `required` set is **`topology.kubernetes.io/zone`** — pinning the surge node to the candidate's AZ so the existing EBS volume can re-attach — plus **`kubernetes.io/arch`** and **`karpenter.sh/capacity-type`** for arch/capacity parity. This is enough for zonal-PV rebind without pinning the exact instance type, which would needlessly shrink the schedulable pool and make same-AZ capacity harder to find.
- Operators add keys for stricter parity — e.g. `node.kubernetes.io/instance-type` (or family) for exact-type parity, or any custom node label the workload's `nodeAffinity` / `nodeSelector` / `topologySpreadConstraints` depend on — or move keys to `preferred` to trade strictness for schedulability.

The configured keys are read from the candidate `NodeClaim`'s `spec.requirements` and the candidate node's labels, **intersected with the NodePool's allowed requirements** — the intersection keeps the placeholder schedulable within the NodePool even if the NodePool template has since narrowed its allowed set (otherwise a now-disallowed candidate label would leave the placeholder unschedulable forever, tripping `readyTimeout` and rolling back). A key listed in config but absent on the candidate node is skipped. **Validation:** removing `topology.kubernetes.io/zone` from `required` **warns** — zonal-PV Pods may then strand if the surge node lands in another AZ.

This only re-creates a **same-AZ landing zone**; it does **not** move storage. The CSI driver re-attaches the existing zonal volume to the new node in that AZ once the replacement Pod is scheduled there. Cross-AZ migration of zonal storage is out of scope — surge neither can nor should do it. (Implication: if the candidate node's AZ has no schedulable capacity for a same-zone replacement, the surge cannot complete and rolls back via `readyTimeout` (§3.3 *Rollback*); the old node is left in place and the `expireAfter` backstop still applies. NodePools fronting zonal-PV workloads should ensure each in-use AZ retains surge headroom — see R3.)

### Rollback behavior

| Failure | Action |
|---------|--------|
| New node not `Ready` within timeout | Delete the placeholder workload (Karpenter reaps the unneeded node); remove `do-not-disrupt` from the old node; leave the old node in place; emit alert |
| New node becomes `NotReady` after old one was deleted | The old node's drain is already in flight and cannot be reversed; rely on Karpenter to reconcile capacity for the rescheduled pods |
| Karpenter API unavailable | Skip; the next reconcile retries |
| Controller dies mid-surge | `do-not-disrupt` and the placeholder may be left behind; a startup reconciliation sweep clears stale `noderotation.io/*` markers and orphaned placeholders |

> v1 processes one node per cycle. If the maintenance window is too short to accommodate all candidates, the unprocessed ones roll over to the next window. The `expireAfter` backstop ensures eventual rotation (in the forceful path) even in pathological cases.

## 3.4 Future versions (v2 / v3)

The v1 design intentionally stops short of application-level concerns. The following are reserved expansion points.

| Version | Addition | Trigger for adoption |
|---------|----------|----------------------|
| v1 | Surge + sequential delete | Initial release |
| v2 | Image pre-pull job pinned to the replacement node before deleting the old one | Observed image-pull latency on cold replacement nodes |
| v3 | Synthetic-traffic warm-up job (e.g., JVM JIT priming) before deleting the old one | Observed 5xx spikes after replacement that `readinessGate` alone does not absorb |

The configuration schema in §5.4 already includes placeholder fields for v2 and v3.

## 3.5 Backstop Behavior

If the controller is unavailable, the following safety net engages in order:

1. Karpenter Consolidation / Drift may still rotate some nodes (e.g., on AMI drift)
2. NodePool `expireAfter` triggers Forceful drain on overdue nodes
3. NodePool `terminationGracePeriod` bounds the drain
4. The Auto Mode 21-day hard cap is the final ceiling

> **Important**: backstop paths 2–4 are forceful — PDBs are respected only until `terminationGracePeriod` expires. Extended controller downtime restores the original risk profile. Note also that a **stale `karpenter.sh/do-not-disrupt`** left on a node by a controller that crashed mid-surge **suppresses path 2 (`expireAfter`)** on that node (§3.3); path 4 (the 21-day hard cap) is then the only ceiling until the startup sweep clears the marker.

> **Graceful degradation — never worse than the status quo.** Every failure mode degrades onto Karpenter's native Forceful Expiration (path 2). If a rotation fails, a maintenance window is missed, or the controller is absent entirely, the node is still expired and drained by `expireAfter` **exactly as it would be without this controller**. The controller only ever moves rotation *earlier* and makes it *graceful*; by design it never removes the safety net or extends a node's life beyond `expireAfter` (the lone exception — a stale `do-not-disrupt` from a mid-surge crash, suppressing `expireAfter` until the startup sweep clears it, §3.3). So the **worst case equals today's baseline** — forceful, but bounded — which is precisely why the design is safe to adopt incrementally and why the §3.2 lead time is sized to win the race in the *normal* case rather than depended on for safety in the *failure* case.

---

## 4.1 Capacity / Availability

| Concern | Treatment |
|---------|-----------|
| Pod pending time during rotation | Approaches zero thanks to surge (matches Karpenter Graceful semantics) |
| `readyReplicas` dipping below the desired count | A structural Kubernetes limitation when pods leave via the Eviction API — even with surge, the new pod isn't `Ready` instantly. Mitigation belongs at the application layer (over-provision replicas + PDB) and is not in scope here |
| Concurrent surge nodes | v1 is fixed at `surge.maxUnavailable = 1` **per NodePool** (serial within a NodePool; distinct NodePools may surge concurrently). The replacement node is **NodePool-owned** (induced via the placeholder Pod, see §3.3), so the NodePool's `limits` (and any external EC2 vCPU quota) must allow `+1` node over baseline for the surge to land. The controller **pre-checks this headroom before starting a rotation** (§5.2) and skips with a warning if the NodePool is already at its `limits`. `maxUnavailable > 1` is reserved for a later version and would require `+m` headroom |

## 4.2 Observability

Prometheus metrics exposed on `/metrics`:

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `noderotation_candidates` | Gauge | `nodepool` | Eligible NodeClaim count |
| `noderotation_in_progress` | Gauge | `nodepool` | Active rotation count |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` | Cumulative completions; outcome ∈ {success, failure} |
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` | Per-phase duration; phase ∈ {surge_wait, drain} |
| `noderotation_window_active` | Gauge | — | 0/1 indicator of window membership |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` | Unix timestamp of active freeze (0 = no freeze) |
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` | Derived `ageThreshold` (§3.2) |
| `noderotation_rotation_chances` | Gauge | `nodepool` | Guaranteed rotation chances `G` for the derived threshold |
| `noderotation_window_period_seconds` | Gauge | `nodepool` | Worst-case window period `P` of the schedule union |

Suggested alerts:

- `increase(noderotation_completed_total{outcome="failure"}[1h]) > 0`
- `noderotation_candidates > 0` for two consecutive windows (controller falling behind)
- `noderotation_window_active == 1` for the full window with zero completions and non-zero candidates

## 4.3 RBAC and Cloud Permissions

### Kubernetes RBAC

```yaml
- apiGroups: ["karpenter.sh"]
  resources: ["nodeclaims"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["karpenter.sh"]
  resources: ["nodepools"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

### Cloud (e.g., AWS) IAM

v1 performs no direct cloud API calls. All operations route through Karpenter via the `NodeClaim` CRD.

v2 (image pre-pull) and v3 (synthetic warm-up) Jobs run as pods on the new node and inherit that node's role; no extra controller-level cloud permissions are introduced.

## 4.4 Cost

Each rotation creates a brief overlap during which both the old and new nodes are billed. Order of magnitude per rotation: 10–20 minutes of one extra on-demand instance. For weekly rotation of N nodes, additional cost is `≈ N × 4 × hourly_rate × 0.25` per month, which is small relative to baseline node cost. Because rotation is serial *per NodePool* but concurrent *across* NodePools (§3.3), peak overlap — and thus peak instantaneous extra cost — scales with the number of NodePools rotating at the same time.

---

## 5.1 Architecture

```
┌─ Cluster (Karpenter v1+) ─────────────────────────────────────┐
│                                                               │
│  ┌─ Namespace: node-rotation-system (configurable) ──────────┐│
│  │                                                           ││
│  │  Deployment: node-rotation-controller                     ││
│  │    - controller-runtime manager                           ││
│  │    - replicas=2 with leader election (1 active)           ││
│  │    - NodeClaim watcher + 1-minute Ticker                  ││
│  │    - /metrics endpoint                                    ││
│  │                                                           ││
│  │  ConfigMap: node-rotation-config                          ││
│  │    - maintenanceWindows / minRotationChances / selectors  ││
│  └───────────────────────────────────────────────────────────┘│
│                          │ watch / create / delete            │
│                          ↓                                    │
│  ┌─ NodeClaims (karpenter.sh/v1) ────────────────────────────┐│
│  │   nc-aaa (15d) ← old, to be rotated                       ││
│  │   nc-bbb (14d) ← old                                      ││
│  │   nc-ccc (08d) ← new (surge)                              ││
│  └───────────────────────────────────────────────────────────┘│
└───────────────────────────────────────────────────────────────┘
```

## 5.2 Reconcile Loop

Implemented with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime). The reconciler watches `NodeClaim` and runs a periodic Ticker to detect window edges and freeze releases.

Each `Reconcile` call performs **exactly one non-blocking step** and returns a `Requeue`. There are **no blocking waits** (the 15-minute surge wait and the drain wait are *elapsed-time checks* against the `started-at`/deletion timestamps, re-evaluated on later reconciles), so a worker is never held and progress survives controller restarts — all state is read back from annotations (§5.3). Serial processing is enforced by handling any in-flight rotation *before* starting a new one.

```text
Reconcile(req):                              # req is a NodeClaim event or a periodic Tick
  if req is Tick:                            # a Tick is not tied to one object
      for np in in_scope_nodepools():        #   → fan out over every selected NodePool
          reconcile_nodepool(np)
      return Requeue(1m)
  return reconcile_nodepool(nodepool(req.obj))

reconcile_nodepool(np):
  # ── 1. Drive the in-flight rotation first (serial: at most one per NodePool) ──
  active := claim_in_state(np, {pending, draining})
  if active != nil:
      return advance(active)

  # ── 2. No rotation in flight → gate on window / freeze / surge headroom ──
  if not in_window(now): return Requeue(1m)
  if frozen(np):         return Requeue(1m)
  if not surge_headroom(np):                 # np already at spec.limits → a +1 surge node cannot land
      warn("at limits; cannot surge"); return Requeue(1m)

  # ── 3. Start a new rotation (write state only; do not block) ──
  cand := pick_oldest_eligible(np)           # empty state, or failed past retry backoff
  if cand == nil: return Requeue(1m)
  annotate(cand, state=pending, started-at=now)
  annotate(cand.node, do-not-disrupt=true)
  create_placeholder(np, cand)               # bare low-priority Pod; requests = Σ candidate-node Pod requests
  return Requeue(30s)

# advance() runs one step for the in-flight candidate, keyed by its state:
advance(cand):
  switch cand.state:
  case pending:                              # waiting for the surge node to become Ready
      if surge_ready(cand):                  # placeholder on a NEW node (created > started-at) that is Ready
          annotate(new_node(cand), do-not-disrupt=true)
          annotate(cand, state=draining)
          delete(cand)                       # explicit; not blocked by do-not-disrupt
          return Requeue(30s)
      if placeholder(cand) is missing:       # crash/leader-change after the state write, or lost placeholder
          create_placeholder(nodepool(cand), cand)
          return Requeue(30s)
      if elapsed(cand.started-at) > readyTimeout:        # default 15m
          delete(placeholder(cand)); unfreeze(cand.node)
          annotate(cand, state=failed, failed-at=now); emit_metrics(failure); alert
          return Requeue(1m)
      return Requeue(30s)
  case draining:                             # waiting for the old NodeClaim to finalize away
      if gone(cand):
          delete(placeholder(cand)); unfreeze(new_node(cand))
          emit_metrics(success)
          return Requeue(cooldown=10m)
      # bounded by terminationGracePeriod + buffer; Karpenter forces the drain
      return Requeue(30s)
```

`pick_oldest_eligible` selects claims whose `state` is empty (fresh) or `failed` with `now − failed-at > retryBackoff`; `pending`/`draining` claims are never re-selected (they are driven by step 1). Leader election uses the standard `coordination.k8s.io/Lease`; on leader change the new leader resumes purely from annotations.

## 5.3 State Model

Progress state lives entirely on Kubernetes objects (the old `NodeClaim`, the two nodes, the NodePool, and the transient placeholder Pod) — **no external datastore** is required. The placeholder Pod is a short-lived runtime object, not durable state: if it is lost, the startup sweep reconstructs the situation from the `noderotation.io/state` annotation on the old NodeClaim, which is the single source of truth for where a rotation is.

| Key | Target | Value | Purpose |
|-----|--------|-------|---------|
| `noderotation.io/state` | Old NodeClaim | `pending` / `draining` / `failed` | Progress state (source of truth) |
| `noderotation.io/started-at` | Old NodeClaim | RFC3339 timestamp | `readyTimeout` deadline + observability |
| `noderotation.io/failed-at` | Old NodeClaim | RFC3339 timestamp | `retryBackoff` anchor for re-selection after a failure |
| `noderotation.io/surge-for` | Placeholder workload | Old NodeClaim's `metadata.name` | Pairing; used to find/clean up the placeholder and its node |
| `karpenter.sh/do-not-disrupt` | Old node + new node | `true` | Blocks Karpenter voluntary disruption **and `expireAfter`** during the surge — but not the 21-day hard cap (removed at the end; a stale value suppresses `expireAfter`, see §3.5) |
| `noderotation.io/freeze` | NodePool | RFC3339 timestamp (freeze-until) | Suppresses rotation until the given time |

### State transitions

The old NodeClaim's `noderotation.io/state` drives the machine in §5.2. The annotation is **written before** each side effect so a crash/leader change is recoverable.

| From | Event | To | Side effects |
|------|-------|----|--------------|
| *(none)* | selected in window | `pending` | set `do-not-disrupt` on old node; create placeholder |
| `pending` | surge node `Ready` | `draining` | set `do-not-disrupt` on new node; `delete` old NodeClaim |
| `pending` | `readyTimeout` elapsed | `failed` | delete placeholder; unfreeze old node; alert |
| `draining` | old NodeClaim gone | *(deleted)* | delete placeholder; unfreeze new node; emit success |
| `failed` | `retryBackoff` elapsed, still in window | `pending` | re-enter (annotations reset); the `expireAfter` backstop covers repeated failure |

`pending` and `draining` are **driven by step 1** of §5.2 and are never re-picked as fresh candidates; this is also what enforces serial (parallelism=1) processing. A completed rotation leaves no state because the old NodeClaim — the carrier of the annotations — is deleted. On startup the controller sweeps for stale `noderotation.io/*` markers and orphaned placeholders (Rollback table, §3.3).

## 5.4 Configuration Schema

The v1 schema is a single ConfigMap. CRD-based configuration is reserved for a later version if multi-policy support is required.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: node-rotation-config
  namespace: node-rotation-system
data:
  policy.yaml: |
    nodepoolSelectors:
      - matchLabels:
          # example; adjust to your NodePool labels
          workload: api

    ageThreshold: auto         # derived per NodePool (§3.2); an explicit duration override is allowed but still validated
    minRotationChances: 2      # K; floor 1, values < 2 only warn

    maintenanceWindows:        # list; the effective window is the UNION of all entries (§3.1)
      - timezone: Asia/Tokyo
        days: [Wed, Sat]
        start: "02:00"
        end:   "06:00"

    surge:
      maxUnavailable: 1        # v1 fixed at 1 (serial); > 1 reserved for a later version
      readyTimeout: 15m        # surge node must reach Ready within this, else state=failed
      cooldownAfter: 10m       # settle pause between consecutive rotations in a window (not part of t_rot; affects throughput, §3.2)
      retryBackoff: 30m        # wait before re-selecting a failed NodeClaim (§5.3)
      matchNodeRequirements:   # which candidate-node requirements the placeholder replicates (§3.3 "Stateful and zonal workloads")
        required:              # hard nodeAffinity, value copied from candidate node; intersected with NodePool's allowed requirements
          - topology.kubernetes.io/zone   # default: same-AZ for zonal-PV (EBS) rebind; removing it only warns
          - kubernetes.io/arch
          - karpenter.sh/capacity-type
        preferred: []          # soft nodeAffinity; relaxed under capacity pressure. e.g. node.kubernetes.io/instance-type

    prePull:                   # v2 (disabled in v1)
      enabled: false
      images: []

    warmup:                    # v3 (disabled in v1)
      enabled: false
      jobTemplate: {}
```

---

## 6.1 Versioning and Release

- Semantic versioning (`vMAJOR.MINOR.PATCH`)
- Pre-1.0 releases (`v0.x.y`) until v1 scope and CRD shape are stable
- API compatibility surface: ConfigMap schema (`apiVersion: v1, ConfigMap` with documented `data.policy.yaml`), Prometheus metric names, annotation keys

## 6.2 Roadmap

| Milestone | Content |
|-----------|---------|
| v0.1 (spec) | This document |
| v0.2 (skeleton) | Project layout, controller-runtime bootstrap, leader election, CI |
| v0.3 (MVP, v1 surge) | Reconcile + surge + drain + metrics + Helm chart |
| v0.4 | Pre-pull (v2 feature) |
| v0.5 | Warm-up hook (v3 feature) |
| v1.0 | Stable ConfigMap schema, documented production runbook, soak-tested on a real EKS Auto Mode cluster |

---

## 7.1 Risks

| # | Risk | Mitigation |
|---|------|------------|
| R1 | Controller pod crashes / leader loss | `replicas=2` with leader election; the `expireAfter` backstop is retained; failure metrics alert |
| R2 | Maintenance window too short to drain all candidates | The §3.2 throughput check warns up front; alert on `noderotation_candidates` failing to decrease for two consecutive windows; consider `maxUnavailable > 1` in a later version |
| R3 | Surge NodeClaim cannot be launched (capacity / AZ shortage / NodePool at `limits`) | The controller pre-checks NodePool `limits` headroom before starting (§5.2) and warns if at the limit; ready-timeout triggers rollback otherwise; NodePool should already permit multi-AZ / multi-instance-type. **Zonal caveat:** the surge node is pinned to the candidate's AZ for zonal-PV rebind (§3.3 *Stateful and zonal workloads*), so a same-AZ capacity shortage cannot fall back to another zone — keep per-AZ surge headroom for NodePools fronting zonal-PV workloads |
| R4 | Drain blocks on a misconfigured PDB | Karpenter's `terminationGracePeriod` ultimately forces drain; PDB review is the application owner's responsibility |
| R5 | Forgotten freeze during business-critical period | The freeze annotation is meant to be managed declaratively (e.g., via GitOps) rather than ad-hoc |
| R6 | Verification gap when test clusters routinely turn over (e.g., nightly shutdown) | Disable shutdown for a soak period that exceeds the age threshold to validate end-to-end rotation |

## 7.2 Validated Assumptions

| Assumption | Status | Evidence |
|------------|--------|----------|
| A standalone (non-NodePool-owned) `NodeClaim` is *provisionable* on EKS Auto Mode | **Validated** — K8s 1.35, `karpenter.sh/v1` (2026-05-29) | A NodeClaim with only `nodeClassRef` (managed `eks.amazonaws.com/NodeClass`) + `requirements` reached `Ready` (real EC2, node registered) in ~30s; admission accepted it (`--dry-run=server`); graceful finalizer-driven deletion confirmed |

> **Why this is recorded as a capability, not the surge mechanism.** The standalone-NodeClaim result proves Karpenter will honor a controller-created NodeClaim, which de-risks the project. But the surge design (§3.3) deliberately does **not** use it: a standalone node is unowned by any NodePool, so pods would persist on a node outside NodePool accounting/expiry/drift/budgets, breaking intentional NodePool separation. It is kept as a documented **fallback** should the placeholder approach prove unworkable.
>
> **Not yet validated (PoC scope):** the *primary* mechanism — inducing a NodePool-owned node via a placeholder Pod sized to the candidate node's Pod requests, applying `karpenter.sh/do-not-disrupt` to both nodes during the surge, and confirming that the Auto Mode managed Karpenter honors `do-not-disrupt` against **both voluntary disruption and `expireAfter`** (while the 21-day hard cap still overrides it, and explicit NodeClaim deletion still drains), plus that a preempted bare placeholder Pod is deleted without re-pending. These are the first PoC items.

## 7.3 Open Questions

1. **Migration to CRD-based policy** if multiple NodePools require divergent rotation policies
2. **Per-NodePool window** vs single cluster-wide window
3. **Holiday-aware scheduling** (skip rotation if `Sat` falls on a holiday). The v1 design intentionally ignores holidays
4. **Pre-pull image source provisioning** for v2 — whether to use the standard Karpenter NodeClass image-pulling capability or a dedicated Job
5. **Multi-cloud verification** (AKS NAP, GKE) before claiming compatibility beyond EKS Auto Mode

---

## References

- [Karpenter Disruption (official docs)](https://karpenter.sh/docs/concepts/disruption/)
- [Karpenter forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md) — establishes "user-side implementation" as a valid path
- [Karpenter Discussion #1079 — Schedule for disruption](https://github.com/kubernetes-sigs/karpenter/discussions/1079) — whitelist limitation of Disruption Budgets
- [EKS Auto Mode docs](https://docs.aws.amazon.com/eks/latest/userguide/automode.html)
- [EKS Auto Mode and maintenance window for "Drifted" nodes (AWS re:Post)](https://repost.aws/articles/ARbff3_8A_R7uiPMpCfjHznw/eks-auto-mode-and-maintenance-window-for-drifted-nodes)
