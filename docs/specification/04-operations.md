# 4. Operations

## 4.1 Capacity / Availability

| Concern | Treatment |
|---------|-----------|
| Pod pending time during rotation | Approaches zero thanks to surge (matches Karpenter Graceful semantics) |
| `readyReplicas` dipping below the desired count | A structural Kubernetes limitation when pods leave via the Eviction API — even with surge, the new pod isn't `Ready` instantly. Mitigation belongs at the application layer (over-provision replicas + PDB) and is not in scope here |
| Concurrent surge nodes | v1 is fixed at `surge.maxUnavailable = 1` **per NodePool** (serial within a NodePool; distinct NodePools may surge concurrently). The replacement node is **NodePool-owned** (induced via the placeholder Pod, see §3.3). `maxUnavailable > 1` is reserved for a later version and would require headroom for `m` nodes. See the note below on how the single-node surge budget is enforced. |

> **Note — how the one-node surge budget is enforced.** `spec.limits` is a **resource budget** (`{cpu, memory, …}`), **not** a node count — so the actual precondition is that the placeholder's requests (the surge node's resource footprint, §3.3) fit within the NodePool's *remaining* budget (`limits − currently-provisioned`), alongside any external EC2 vCPU quota. Intuitively this is "+1 node over baseline," but it is enforced as a **resource** check, not a count. The controller **pre-checks this headroom before starting a rotation** (§5.2 step 3 — after candidate selection, because the placeholder's requests are defined by the selected candidate) and skips with a warning if the remaining budget cannot fit one more node's worth of resources.

## 4.2 Observability

Prometheus metrics exposed on `/metrics`:

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `noderotation_candidates` | Gauge | `nodepool` | Eligible NodeClaim count |
| `noderotation_in_progress` | Gauge | `nodepool` | Active rotation count |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` | Cumulative completions; outcome ∈ {success, failure, expired} — `expired` = the old NodeClaim was force-expired before a graceful rotation completed (caught by its `deletionTimestamp` appearing while still `pending` or `failed`, or by its disappearance with no `draining` mirror — §5.2; emitted once per rotation, never counted as success) |
| `noderotation_forceful_fallback_total` | Counter | `nodepool` | Cumulative surge-less window-bounded forceful-fallback rotations initiated (§3.3); incremented when a rotation starts surge-less, not at completion (completion still increments `noderotation_completed_total`) |
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` | Per-phase duration; phase ∈ {surge_wait, drain}. `surge_wait` = `started-at → surge_ready`; `drain` = `draining-at → old-NodeClaim finalization`, anchored by the NodePool `draining-at` annotation stamped at the `pending → draining` transition because the old NodeClaim's `deletionTimestamp` has finalized away by the single completion point where the histogram is observed once (§5.3) |
| `noderotation_window_active` | Gauge | `nodepool` | 0/1 indicator of the NodePool's governing-policy window membership |
| `noderotation_policy_conflict` | Gauge | `nodepool` | 0/1: the NodePool is blocked from rotating by a `RotationPolicy` conflict — an equal-specificity selector tie or a runtime-invalid governing policy (§5.4). 1 while blocked, 0 once a single valid policy governs it |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` | Unix timestamp of active freeze (0 = no freeze) |
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` | Derived `ageThreshold` (§3.2) |
| `noderotation_rotation_chances` | Gauge | `nodepool` | Guaranteed rotation chances `G` for the derived threshold |
| `noderotation_window_period_seconds` | Gauge | `nodepool` | Worst-case window period `P` of the schedule union |
| `noderotation_short_lead_nodes` | Gauge | `nodepool` | NodeClaims whose **own** `spec.expireAfter` can no longer guarantee `K` chances (per-node `A ≤ 0`; §3.2 layer 3) |
| `noderotation_drain_stuck` | Gauge | `nodepool` | 0/1: the in-flight rotation's drain has exceeded `tGP + buffer` (§5.2) |
| `noderotation_retry_count` | Gauge | `nodepool` | Highest `noderotation.io/retry-count` (§5.3) across the NodePool's NodeClaims (0 when none) — the systematic-failure signal; annotations alone cannot feed Prometheus alerts |

> **Label note.** With per-NodePool maintenance windows (each NodePool resolves its own governing `RotationPolicy`, §5.4), `noderotation_window_period_seconds` and `noderotation_window_active` both carry a **load-bearing** `nodepool` label — `P` and window *membership* can differ across pools when their policies carry different `maintenanceWindows`. `noderotation_age_threshold_seconds` and `noderotation_rotation_chances` likewise vary per NodePool (they fold in each pool's representative `expireAfter`/`terminationGracePeriod` *and* its policy's `K`). This resolves the v1 simplification noted in earlier drafts, where a single cluster-wide window made these series identical across pools.

> **Lifecycle note.** The per-`nodepool` series are **cleared when the NodePool is deleted** — the controller drops them on the delete reconcile. The gauges are recomputed each reconcile, so a deleted pool whose reconciles stop would otherwise latch its last value forever (a since-removed `noderotation_drain_stuck = 1` would alert indefinitely). A NodePool that loses its governing policy (no `RotationPolicy` matches it any longer) has its series dropped the same way, since it is no longer rotated (§5.4); an in-flight rotation anchored on such a pool is rolled back first so its placeholder and `do-not-disrupt` marker are not orphaned (§5.4).

**Kubernetes Events.** Warning-level conditions that are computed every
reconcile are also surfaced as Kubernetes `Warning` Events, so operators see
them with `kubectl describe` without reading metrics:

| Surface | Object | Reason | When |
|---------|--------|--------|------|
| Non-fatal schedule finding (§3.2 layers 1–2) | NodePool | the finding code (e.g. `KBelowTwo`, `AVeryAggressive`, `TGPUnset`, `HardCapExceeded`, `RetryBackoffShort`, `ThroughputBelowArrival`, `ThroughputBurstShortfall`, `RotationSpansNextWindow`, `OverrideGBelowK`) | the finding becomes active for the NodePool |
| Short-lead NodeClaim (§3.2 layer 3) | NodeClaim | `ShortLead` | the claim's own `expireAfter` can no longer guarantee `K` chances |
| Surge-less forceful fallback (§3.3) | NodePool | `ForcefulFallback` | a surge-less window-bounded rotation begins because a graceful surge cannot complete before the candidate's deadline (opt-in `surge.forcefulFallback`) |

Events are **deduplicated by emitting on the transition into the condition**:
a finding/claim that clears and later returns re-fires. The dedup state is
in-memory, so a controller restart re-emits each active warning once. Fatal
findings are not events — they block the start of a rotation and are logged by
the §5.2 feasibility gate.

> **Liveness is judged from metrics, not from the warning log.** The same
> transition dedup applies to the `INFO`-level warning **log** lines, so in
> steady state — stable findings, no transitions — a healthy reconcile loop can
> run for many passes emitting **zero** log lines. The deduped warning log is
> therefore **not** a liveness signal: reconcile liveness must be read from the
> `controller_runtime_reconcile_total` / `controller_runtime_reconcile_time_seconds_*`
> counters and the `workqueue_*` metrics (depth, adds, work duration), which tick
> on every pass regardless of whether findings change. To *see* per-pass activity
> in the log when debugging, raise the controller's log verbosity
> (`--zap-devel` / a higher `-v`): at debug verbosity (`V(1)`) the controller
> additionally emits, **un-deduplicated, every pass**, the current findings and a
> lightweight per-reconcile `reconcile` heartbeat (phase, candidate count, claim
> count, in-window, findings count). This debug output is purely additive — it
> does not change the dedup of the `INFO` log or the Warning Events, nor any
> metric — and is a human-readable aid only; the metrics above remain the
> authoritative liveness signal.

Suggested alerts:

- `increase(noderotation_completed_total{outcome=~"failure|expired"}[1h]) > 0`
- `noderotation_candidates > 0` for two consecutive windows (controller falling behind)
- `noderotation_window_active == 1` for the full window with zero completions and non-zero candidates
- `noderotation_drain_stuck == 1` (drain blocked past `tGP + buffer` — blocking PDB or stuck finalizer; §5.2)
- `noderotation_short_lead_nodes > 0` (NodeClaims whose stamped `expireAfter` can no longer guarantee `K` chances; §3.2 layer 3)
- `noderotation_retry_count >= 3` (the same rotation keeps failing — systematic cause such as sustained placeholder preemption or same-AZ capacity shortage; §5.3)
- `increase(noderotation_forceful_fallback_total[1h]) > 0` (a window-bounded forceful fallback fired — a graceful surge lost the race to the deadline; §3.3, ADR-0001. A single fallback is by design, so this ships at `severity: info`; tighten it to a sustained rate for your environment)

> The Helm chart ships these seven alerts as an **optional** `PrometheusRule`, gated behind `prometheusRule.enabled` (default `false`). The schedule-dependent ranges (two-window and full-window) are chart values, since `P`/`D` come from the operator's `maintenanceWindows`. See the [production runbook](../runbook.md) for how to read each metric and tune the alerts.

## 4.3 RBAC and Cloud Permissions

### Kubernetes RBAC

```yaml
- apiGroups: ["karpenter.sh"]
  resources: ["nodeclaims"]
  # no create: v1 never creates a NodeClaim (§3.3). update/patch carry the
  # noderotation.io/* state annotations; delete drives rotation and the failure reap
  verbs: ["get", "list", "watch", "update", "patch", "delete"]
- apiGroups: ["karpenter.sh"]
  resources: ["nodepools"]
  # update/patch: the active-rotation anchor, active-rotation-state mirror,
  # last-rotation-at and last-failure-at annotations (§5.2/§5.3) live on the NodePool
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["nodes"]
  # update/patch: do-not-disrupt / do-not-disrupt-owned / surge-for / cordoned annotations + spec.unschedulable
  # (cordon, §3.3). Node writes use the same full-object update-under-retry path as
  # nodeclaims/nodepools (§5.3), so update is required alongside patch
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["pods"]
  # the placeholder Pod is created and managed directly by the controller (§3.3)
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: [""]
  # core/v1 Events: leader election records its Lease events via the legacy recorder
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  # events.k8s.io/v1 Events: the §4.2 / §3.2-layer-3 warning Events on the
  # cluster-scoped NodePool/NodeClaim objects use the new recorder API, which
  # writes those Events into the "default" namespace (granted cluster-wide)
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

The placeholder's dedicated `PriorityClass` (§3.3) is installed **statically by the Helm chart**, not created at runtime — the controller therefore needs no `priorityclasses` permission.

### Cloud (e.g., AWS) IAM

v1 performs no direct cloud API calls. All operations route through Karpenter via the `NodeClaim` CRD.

v2 (image pre-pull) Jobs run as pods on the new node and inherit that node's role; no extra controller-level cloud permissions are introduced.

## 4.4 Cost

Each rotation creates a brief overlap during which both the old and new nodes are billed. Order of magnitude per rotation: 10–20 minutes of one extra on-demand instance. For weekly rotation of N nodes, additional cost is `≈ N × 4 × hourly_rate × 0.25` per month, which is small relative to baseline node cost. Because rotation is serial *per NodePool* but concurrent *across* NodePools (§3.3), peak overlap — and thus peak instantaneous extra cost — scales with the number of NodePools rotating at the same time.

A **failed** surge attempt can also briefly bill a surge node (up to `readyTimeout`, after which it is explicitly reaped **when still unoccupied** — §3.3 *Rollback*; a surge node onto which an unrelated preemptor has meanwhile landed is deliberately *not* reaped and simply remains in the NodePool as normal capacity — a repurpose, not a leak).

Two mechanisms bound how fast failures can repeat that cost: the escalating `retryBackoff` (§5.3) bounds retries of the *same* claim, and the **pool-level inter-attempt pause** (`last-failure-at` + `cooldownAfter`, §5.2 step 2) bounds candidate *cycling* — without it, a systematic cause (sustained placeholder preemption, persistent same-AZ capacity shortage) that fails every claim it touches would simply move on to the next candidate within ~a minute, burning a `readyTimeout`-worth of failed-surge billing per candidate. With the pause, a NodePool under a systematic failure runs at most **one attempt per `readyTimeout + cooldownAfter`** (~25m at defaults), and `noderotation_retry_count` alerts on the pattern (§4.2).
