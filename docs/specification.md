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
| G1 | **Prevent forceful Expiration from firing in practice** by replacing `NodeClaim` resources that approach an age threshold (default: `expireAfter - 4d`) during a defined maintenance window, using the voluntary disruption path |
| G2 | **Eliminate the pending-pod window** by creating a replacement `NodeClaim` first and waiting for `Ready=True` before deleting the old one (surge / make-before-break) |
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
| **maintenance window** | A configured weekday/time-of-day range during which the controller may *start* a rotation. In-flight rotations are allowed to complete past the window boundary |
| **age threshold** | The `creationTimestamp` age beyond which a `NodeClaim` becomes a rotation candidate. Defaults to `expireAfter - 4d`, configurable |
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
maintenanceWindow:
  timezone: Asia/Tokyo   # IANA tz database name
  days: [Sat]            # ISO weekday names: Mon/Tue/Wed/Thu/Fri/Sat/Sun
  start: "02:00"
  end:   "06:00"
```

**Semantics**:

- The reconciler is **always running**; window membership is evaluated on each reconcile tick (1-minute Ticker).
- Outside the window the reconcile loop is a no-op.
- The window controls only **rotation starts**. An in-flight rotation continues past the window boundary (aborting mid-drain is more dangerous than letting it complete).
- A single `NodePool` may be **frozen** by an annotation (e.g., `noderotation.io/freeze=<RFC3339 timestamp>`) to suppress rotation until that time (use case: business-critical periods).

## 3.2 Candidate Selection

A `NodeClaim` becomes a rotation candidate when **all** of the following hold:

| Condition | Default | Notes |
|-----------|---------|-------|
| `now() - metadata.creationTimestamp > ageThreshold` | `ageThreshold = expireAfter - 4d` (e.g., 10d if `expireAfter=14d`) | Configurable. Must be calibrated against window frequency — see Note below |
| Belongs to a `NodePool` matched by the configured selector | Required | A `NodePool` matched by `nodepoolSelectors` is in scope |
| `status.conditions[Ready] == True` | Required | NotReady NodeClaims are skipped |
| `metadata.annotations["noderotation.io/state"]` is empty or `pending` | Required | Already-in-progress or completed claims are skipped |

When multiple claims are eligible they are sorted by age (oldest first).

> **Note on `ageThreshold` calibration**
>
> The threshold must be tight enough that every node passes through at least one maintenance window before `expireAfter` fires. For a weekly window, `expireAfter - ageThreshold ≥ 7d` is a safe lower bound. For example, with `expireAfter = 14d` and weekly windows, `ageThreshold = 10d` gives 4 days of headroom.

## 3.3 Surge Sequence (v1)

A single reconcile cycle handles **one** `NodeClaim`. v1 enforces serial processing (parallelism = 1) to minimize blast radius.

```
[Reconcile]
  │
  ├─ if not in_window or frozen or already_active: requeue
  │
  ├─ candidate := pick_oldest()
  │     if none: requeue
  │
  ├─ surge_nc := create_replacement_nodeclaim(
  │     spec_from = candidate.spec,
  │     labels    = {"noderotation.io/surge-for": candidate.name},
  │   )
  │
  ├─ wait_until_ready(surge_nc, timeout = 15m)
  │     on timeout: delete(surge_nc); annotate(candidate, failed); alert
  │
  ├─ annotate(candidate, "noderotation.io/state=draining")
  │
  ├─ delete(candidate)
  │     // Karpenter termination controller drains gracefully,
  │     // respecting PDBs up to terminationGracePeriod.
  │
  ├─ wait_until_gone(candidate, timeout = terminationGracePeriod + buffer)
  │
  └─ cooldown(10m); requeue
```

### Rollback behavior

| Failure | Action |
|---------|--------|
| Replacement `NodeClaim` not `Ready` within timeout | Delete the surge NodeClaim to avoid orphaned EC2 cost; leave the original in place; emit alert |
| Replacement `NodeClaim` becomes `NotReady` after old one was deleted | The old node's drain is already in flight and cannot be reversed; rely on Karpenter to reconcile the failed replacement |
| Karpenter API unavailable | Skip; the next reconcile retries |

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

> **Important**: backstop paths 2–4 are forceful — PDBs are respected only until `terminationGracePeriod` expires. Extended controller downtime restores the original risk profile.

---

## 4.1 Capacity / Availability

| Concern | Treatment |
|---------|-----------|
| Pod pending time during rotation | Approaches zero thanks to surge (matches Karpenter Graceful semantics) |
| `readyReplicas` dipping below the desired count | A structural Kubernetes limitation when pods leave via the Eviction API — even with surge, the new pod isn't `Ready` instantly. Mitigation belongs at the application layer (over-provision replicas + PDB) and is not in scope here |
| Concurrent surge nodes | v1 is fixed at parallelism=1. The NodePool `limits.nodes` (or `limits.cpu`) must allow `+1` over normal capacity |

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

Each rotation creates a brief overlap during which both the old and new nodes are billed. Order of magnitude per rotation: 10–20 minutes of one extra on-demand instance. For weekly rotation of N nodes, additional cost is `≈ N × 4 × hourly_rate × 0.25` per month, which is small relative to baseline node cost.

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
│  │    - maintenanceWindow / ageThreshold / selectors         ││
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

```text
Reconcile(NodeClaim or Tick):
  if not in_window(now): return Requeue(1m)
  if frozen(nodepool):   return Requeue(1m)
  if rotation_active(nodepool): return Requeue(30s)

  candidate := pick_oldest_eligible(nodepool)
  if candidate == nil: return Requeue(1m)

  surge := create_surge_nodeclaim(candidate)
  if !wait_ready(surge, 15m): rollback; alert; return Requeue(10m)

  annotate(candidate, state=draining)
  delete(candidate)
  wait_gone(candidate, terminationGracePeriod + buffer)

  emit_metrics(success)
  return Requeue(cooldown=10m)
```

Leader election uses the standard `coordination.k8s.io/Lease`.

## 5.3 State Model

All state lives on `NodeClaim` annotations/labels. No external datastore is required.

| Key | Target | Value | Purpose |
|-----|--------|-------|---------|
| `noderotation.io/state` | Old NodeClaim | `pending` / `draining` / `failed` | Progress state |
| `noderotation.io/started-at` | Old NodeClaim | RFC3339 timestamp | Observability |
| `noderotation.io/surge-for` | New NodeClaim | Old NodeClaim's `metadata.name` | Pairing; used for rollback cleanup |
| `noderotation.io/freeze` | NodePool | RFC3339 timestamp (freeze-until) | Suppresses rotation until the given time |

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

    ageThreshold: 10d

    maintenanceWindow:
      timezone: Asia/Tokyo
      days: [Sat]
      start: "02:00"
      end:   "06:00"

    surge:
      parallelism: 1           # v1 only supports 1
      readyTimeout: 15m
      cooldownAfter: 10m

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
| R2 | Maintenance window too short to drain all candidates | Alert on `noderotation_candidates` failing to decrease for two consecutive windows; consider parallelism > 1 in a later version |
| R3 | Surge NodeClaim cannot be launched (capacity / AZ shortage) | Ready-timeout triggers rollback; NodePool should already permit multi-AZ / multi-instance-type |
| R4 | Drain blocks on a misconfigured PDB | Karpenter's `terminationGracePeriod` ultimately forces drain; PDB review is the application owner's responsibility |
| R5 | Forgotten freeze during business-critical period | The freeze annotation is meant to be managed declaratively (e.g., via GitOps) rather than ad-hoc |
| R6 | Verification gap when test clusters routinely turn over (e.g., nightly shutdown) | Disable shutdown for a soak period that exceeds the age threshold to validate end-to-end rotation |

## 7.2 Open Questions

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
