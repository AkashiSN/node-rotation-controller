# 4. Operations

## 4.1 Capacity / Availability

::: tip What this section covers
How surge affects pod availability during rotation, and how the one-node surge budget is enforced.
:::

| Concern | Treatment |
|---------|-----------|
| Pod pending time | Approaches zero (surge) |
| `readyReplicas` dip | Application-layer mitigation |
| Concurrent surge nodes | Serial per NodePool (v1) |

- **Pod pending time:** surge matches Karpenter Graceful semantics (make-before-break).
- **`readyReplicas` dip:** a structural Kubernetes limitation — even with surge, the new Pod isn't `Ready` instantly after eviction. Mitigation: over-provision replicas + PDB. Not in scope.
- **Concurrent surge:** v1 is `surge.maxUnavailable = 1` per NodePool (serial within; distinct pools may surge concurrently). The replacement node is NodePool-owned (induced via placeholder, §3.3).

### How the one-node surge budget is enforced

`spec.limits` is a **resource budget** (`{cpu, memory, …}`), not a node count. The precondition is that the placeholder's requests fit within the NodePool's remaining budget (`limits − currently-provisioned`).

The controller **pre-checks headroom before starting** (§5.2 step 3, after candidate selection — because the placeholder's requests depend on the selected candidate). Skips with a warning if budget cannot fit one more node's worth.

## 4.2 Observability

### Prometheus metrics

Exposed on `/metrics`:

| Metric | Type | Labels |
|--------|------|--------|
| `noderotation_candidates` | Gauge | `nodepool` |
| `noderotation_in_progress` | Gauge | `nodepool` |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` |
| `noderotation_forceful_fallback_total` | Counter | `nodepool` |
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` |
| `noderotation_window_active` | Gauge | `nodepool` |
| `noderotation_policy_conflict` | Gauge | `nodepool` |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` |
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` |
| `noderotation_rotation_chances` | Gauge | `nodepool` |
| `noderotation_throughput_capacity` | Gauge | `nodepool` |
| `noderotation_t_rot_estimate_seconds` | Gauge | `nodepool` |
| `noderotation_t_rot_bound_seconds` | Gauge | `nodepool` |
| `noderotation_window_period_seconds` | Gauge | `nodepool` |
| `noderotation_short_lead_nodes` | Gauge | `nodepool` |
| `noderotation_drain_stuck` | Gauge | `nodepool` |
| `noderotation_retry_count` | Gauge | `nodepool` |

::: details Metric details — click to expand

- **`noderotation_candidates`:** eligible NodeClaim count per pool
- **`noderotation_in_progress`:** active rotation count per pool
- **`noderotation_completed_total`:** cumulative completions; `outcome` ∈ {`success`, `failure`, `expired`}. `expired` = force-expired before graceful rotation completed (emitted once, never counted as success)
- **`noderotation_forceful_fallback_total`:** surge-less forceful-fallback rotations initiated (§3.6); incremented at start, not completion
- **`noderotation_duration_seconds`:** per-phase; `phase` ∈ {`surge_wait`, `drain`}. `surge_wait` = `started-at → surge_ready`; `drain` = `draining-at → old-NodeClaim finalization`. Observed at most once per successful transition (no double-count on retried writes; dropped sample preferred over phantom sample)
- **`noderotation_window_active`:** 0/1 window membership indicator
- **`noderotation_policy_conflict`:** 0/1 blocked by selector tie or invalid policy (§5.4)
- **`noderotation_freeze_until_timestamp`:** Unix timestamp of active freeze (0 = none)
- **`noderotation_age_threshold_seconds`:** derived `ageThreshold` (§3.2)
- **`noderotation_rotation_chances`:** guaranteed chances `G`
- **`noderotation_throughput_capacity`:** layer-2 forecast `C` — starts per occurrence (§3.2)
- **`noderotation_t_rot_estimate_seconds`:** forecast service time `t_rot_est = provisioningEstimate + drainEstimate`
- **`noderotation_t_rot_bound_seconds`:** deadline-side bound `t_rot = readyTimeout + tGP + buffer`
- **`noderotation_window_period_seconds`:** worst-case period `P`
- **`noderotation_short_lead_nodes`:** NodeClaims whose own `spec.expireAfter` can no longer guarantee `K` chances (§3.2 layer 3)
- **`noderotation_drain_stuck`:** 0/1 drain exceeded `tGP + buffer` (§5.2)
- **`noderotation_retry_count`:** highest `retry-count` across pool's NodeClaims (0 = none)

:::

#### Label notes

With per-NodePool windows (each pool resolves its own `RotationPolicy`, §5.4), `noderotation_window_period_seconds` and `noderotation_window_active` carry a **load-bearing** `nodepool` label — `P` and membership can differ across pools.

- **`expireAfter: Never`:** all derived gauges read `0` (derivation skipped)
- **No window occurrence (`P ≤ 0`):** bound/estimate are non-zero; only `throughput_capacity` is `0`

#### Lifecycle

- Series **cleared when the NodePool is deleted** — gauges are recomputed each reconcile
- A pool losing its governing policy has series dropped the same way (§5.4)

### Kubernetes Events

Warning-level conditions surfaced via `kubectl describe`:

| Object | Reason | When |
|--------|--------|------|
| NodePool | `KBelowTwo`, `AVeryAggressive`, `TGPUnset`, `HardCapExceeded`, `RetryBackoffShort`, `DrainEstimateAboveTGP`, `ThroughputBelowArrival`, `ThroughputBurstShortfall`, `RotationSpansNextWindow`, `OverrideGBelowK` | Schedule finding active |
| NodeClaim | `ShortLead` | Claim can't guarantee `K` chances |
| NodePool | `ForcefulFallback` | Surge-less rotation begins |
| NodePool | `RotationStarted` | Candidate picked (`Normal`) |
| NodePool | `RotationCompleted` | Old NodeClaim finalized (`Normal`) |
| NodeClaim | `RotationFailed` | `readyTimeout` expired; rolled back |
| NodeClaim | `SurgeUnschedulable` | Placeholder `PodScheduled=False` |
| NodeClaim | `SurgeClamped` | Placeholder clamped (`Normal`) |
| NodeClaim | `SurgeClampBandExceeded` | Clamp shortfall > band (`Warning`) |
| NodeClaim | `SurgeClampRefused` | DaemonSet exhausts allocatable (`Warning`) |

- **Deduplication:** emitted on transition into the condition; clears and re-fires on return
- **Fatal findings** are not events — they block rotation start and are logged by the §5.2 feasibility gate

### State-machine log lines

Every state transition emits one `INFO` log line (after the durable annotation write):

| Line | Key fields |
|------|------------|
| `rotation candidate selected` | `nodeclaim`, `age`, `deadline`, `surgeless` |
| `no rotation candidate` | `reason`, census counts |
| `surge placeholder created` | `placeholder`, `requests`, exclusion counts, clamp info |
| `surge placeholder is not schedulable` | `placeholder`, `reason`, `detail` |
| `surge node ready` | `surgeNode`, `surgeWait` |
| `drain started` | `node`, `mode` ∈ {`surge`, `forceful-fallback`} |
| `rotation attempt failed` | `reason`, `readyTimeout`, `retryCount`, `backoffUntil` |
| `rotation complete` | `mode`, `drain`, `surgeNode`, `surgeWait`, `total` |

- **Level-triggered lines** (`no rotation candidate`, `surge placeholder is not schedulable`) use transition dedup — re-fire only when reason/census/message changes
- **Debug verbosity** (`V(1)`) adds un-deduplicated per-pass findings and a heartbeat
- **Liveness signal:** read from `controller_runtime_reconcile_total` / workqueue metrics, not from log silence

### Suggested alerts

| Alert | Condition |
|-------|-----------|
| Failure/expired | `increase(noderotation_completed_total{outcome=~"failure|expired"}[1h]) > 0` |
| Falling behind | `noderotation_candidates > 0` for two consecutive windows |
| Window wasted | `window_active == 1` full window, zero completions, non-zero candidates |
| Drain stuck | `noderotation_drain_stuck == 1` |
| Short lead | `noderotation_short_lead_nodes > 0` |
| Systematic failure | `noderotation_retry_count >= 3` |
| Forceful fallback | `increase(noderotation_forceful_fallback_total[1h]) > 0` (severity: info) |

The Helm chart ships these as an optional `PrometheusRule` (gated behind `prometheusRule.enabled`, default `false`). See the [production runbook](../runbook.md) for tuning.

## 4.3 RBAC and Cloud Permissions

### Kubernetes RBAC

```yaml
- apiGroups: ["karpenter.sh"]
  resources: ["nodeclaims"]
  verbs: ["get", "list", "watch", "update", "patch", "delete"]
- apiGroups: ["karpenter.sh"]
  resources: ["nodepools"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

- **`nodeclaims`:** no `create` — v1 never creates a `NodeClaim` (§3.3). `update`/`patch` carry state annotations; `delete` drives rotation and failure reap
- **`nodepools`:** `update`/`patch` for the active-rotation anchor, state mirror, and completion annotations
- **`nodes`:** `update`/`patch` for `do-not-disrupt`/markers + `spec.unschedulable` (cordon)
- **`pods`:** the placeholder Pod is directly managed
- **`events`:** Warning Events on NodePool/NodeClaim + leader election records
- **`leases`:** leader election

The placeholder's `PriorityClass` is installed **statically by the Helm chart** — no `priorityclasses` permission needed.

### Cloud (e.g., AWS) IAM

- **v1:** no direct cloud API calls. All operations route through `NodeClaim` CRD.
- **v2 (pre-pull):** Jobs run as pods on the new node, inheriting its role. No extra controller-level cloud permissions.

## 4.4 Cost

::: tip Key point
Each rotation creates ~10–20 minutes of overlap billing. Two mechanisms bound failure-driven cost: escalating `retryBackoff` and pool-level `failurePause`.
:::

### Normal rotation cost

Brief overlap: old + new nodes billed simultaneously during surge.

- **Per rotation:** ~10–20 minutes of one extra on-demand instance
- **Monthly (weekly rotation, N nodes):** `≈ N × 4 × hourly_rate × 0.25`
- **Peak overlap:** scales with the number of NodePools rotating concurrently

### Failed surge cost

A failed attempt can bill a surge node up to `readyTimeout` (after which it is reaped when still unoccupied; a repurposed node stays as normal capacity).

### Cost-bounding mechanisms

| Mechanism | Bounds |
|-----------|--------|
| Escalating `retryBackoff` | Retries of the same claim |
| Pool-level `failurePause` | Candidate cycling under systematic failure |

- **Without `failurePause`:** a systematic cause would move to the next candidate within ~1 minute, burning a `readyTimeout`-worth of billing per candidate
- **With `failurePause`:** at most one attempt per `readyTimeout + failurePause` (~25m at defaults)
- `failurePause` is separate from `cooldownAfter` — lowering settle for throughput never weakens cost bounds
- `noderotation_retry_count` alerts on the pattern (§4.2)
