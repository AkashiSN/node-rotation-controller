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
| `noderotation_in_backoff` | Gauge | `nodepool` |
| `noderotation_in_progress` | Gauge | `nodepool` |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` |
| `noderotation_forceful_fallback_total` | Counter | `nodepool` |
| `noderotation_window_missed_total` | Counter | `nodepool` |
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
- **`noderotation_in_backoff`:** NodeClaims **past the age trigger** that are held out of the candidate count *only* because a failed attempt put them inside their escalated `retryBackoff`. The age qualifier is load-bearing: a claim that failed while it was due and whose age stopped being due afterwards — a raised `ageThresholdOverride`, a *shortened* lead time (the trigger is `age > expireAfter − leadTime`, so a wider lead time triggers earlier), an extended `expireAfter` — is still blocked by its backoff but is owed nothing, and neither this gauge nor `window_missed_total` counts it. `candidates + in_backoff` is exactly the outstanding-by-age-and-state count `window_missed_total` judges a closed occurrence by, so an in-window alert can apply that same test live (§5.2)
- The window-aware backoff clamp (§3.2) moves a claim between `noderotation_candidates` and `noderotation_in_backoff` at an occurrence boundary; for a fixed claim snapshot their sum — the outstanding-work count `window_missed_total` judges by — does not change. The clamp is **not** observability-neutral beyond that, because it exists to produce more attempts: `noderotation_completed_total{outcome="failure"}` increments more often, `noderotation_retry_count` rises faster, `NodeRotationRetryCountHigh` can fire sooner, failure logs and Events multiply, and an additional attempt may succeed or still be in flight at the close, either of which changes whether `noderotation_window_missed_total` fires for that occurrence.
- The extra attempts are not a metrics-only effect: each one creates and, on failure, reaps another surge NodeClaim, churns another placeholder Pod, and cordons/uncordons the candidate production node again. A pool that would have made 4 attempts before the clamp and makes 6 after it — the shape of the incident that motivated §3.2's clamp — sees roughly 50% more of that cluster-side churn for the same occurrence.
- **`noderotation_in_progress`:** active rotation count per pool
- **`noderotation_completed_total`:** cumulative completions; `outcome` ∈ {`success`, `failure`, `expired`}. `expired` = force-expired before graceful rotation completed (emitted once, never counted as success)
- **`noderotation_forceful_fallback_total`:** surge-less forceful-fallback rotations initiated (§3.6); incremented at start, not completion
- **`noderotation_window_missed_total`:** maintenance window occurrences that closed with candidates outstanding by age and state (eligible, or past the age trigger and inside `retryBackoff`) and no rotation attributable to the occurrence ever completing (§5.2). A window gates only rotation *starts*, so an attempt that began inside the occurrence and succeeded after the boundary is attributable to it and settles it. "Outstanding" is not "the controller could have rotated": the evaluation runs above the pool-level gates on purpose (§5.2), so a static or fatally infeasible pool reports every occurrence that closes with age/state-outstanding claims. Incremented **at most** once per occurrence, by the pass that clears the `window-opened-at` stamp — the clear lands before the emission, so a stop in between drops the signal rather than inventing one
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
| NodePool | `KBelowTwo`, `AVeryAggressive`, `TGPUnset`, `HardCapExceeded`, `RetryBackoffShort`, `DrainEstimateAboveTGP`, `ProvisioningEstimateAboveReadyTimeout`, `ThroughputBelowArrival`, `ThroughputBurstShortfall`, `RotationSpansNextWindow`, `OverrideGBelowK` | Schedule finding active |
| NodeClaim | `ShortLead` | Claim can't guarantee `K` chances |
| NodePool | `ForcefulFallback` | Surge-less rotation begins |
| NodePool | `StaticNodePool` | `spec.replicas` set — surge can never rotate the pool (§3.3) |
| NodePool | `WindowMissed` | Window closed with candidates unrotated and no rotation attributable to it (§4.2) |
| NodePool | `PolicyConflict` | Equal-specificity RotationPolicy tie — the pool is not rotated (§5.4) |
| NodePool | `GovernanceLost` | In-flight rotation rolled back after the pool left governance (§5.4) |
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
| `maintenance window closed with candidates unrotated` | `windowOpenedAt`, `eligible`, `inBackoffTriggered` |

- **Level-triggered lines** (`no rotation candidate`, `surge placeholder is not schedulable`) use transition dedup — re-fire only when reason/census/message changes
- **Debug verbosity** (`V(1)`) adds un-deduplicated per-pass findings and a heartbeat
- **Liveness signal:** read from `controller_runtime_reconcile_total` / workqueue metrics, not from log silence

### Suggested alerts

| Alert | Condition |
|-------|-----------|
| Failure/expired | `increase(noderotation_completed_total{outcome=~"failure|expired"}[1h]) > 0` |
| Falling behind | `noderotation_candidates > 0` for two consecutive windows |
| Window lost | `increase(noderotation_window_missed_total[24h]) > 0` |
| Window wedged (in-window) | `window_active == 1`, not frozen, zero **successful** completions, and `noderotation_candidates > 0 or noderotation_in_backoff > 0` |
| Drain stuck | `noderotation_drain_stuck == 1` |
| Short lead | `noderotation_short_lead_nodes > 0` |
| Systematic failure | `noderotation_retry_count >= 3` |
| Forceful fallback | `increase(noderotation_forceful_fallback_total[1h]) > 0` (severity: info) |

The `in_backoff` arm of the in-window condition is load-bearing: a NodeClaim inside its escalated `retryBackoff` is not eligible, so `noderotation_candidates` falls to `0` while the pool still has work outstanding. A condition resting on `candidates` alone is silent through a window spent entirely on failed attempts — which is the case `window_missed_total` was added to report.

The arm is `in_backoff` rather than `retry_count` so this half of the condition is *the same test* the window-close evaluation applies. `candidates + in_backoff` is exactly its outstanding count, whereas `retry_count` is the highest retry across *all* of a pool's NodeClaims regardless of which bucket each falls in — it stays elevated for a claim the counter deliberately excludes, such as one whose Node now carries an operator-set `karpenter.sh/do-not-disrupt`, or one that is deleting or already expired. The freeze exclusion extends the agreement: a freeze is the operator instructing the pool to stop rotating, so a window that closes under one was declined rather than lost and the counter does not record it (§5.2); the in-window condition declines it on the same grounds.

The two signals are **not** equivalent beyond that arm, and the alert is not a predictor of the counter. The completions arm is a rolling `completionRange` lookback, while the counter attributes a success by `last-rotation-at ≥ window-opened-at`, so they diverge in both directions: a success shortly *before* an occurrence suppresses the alert although it cannot settle that occurrence, and in a window longer than `completionRange` an attributable success can age out of the lookback and let the alert fire although the occurrence will settle. Set `completionRange` to roughly one window's duration to keep both cases small.

One case is deliberately not alerted: a claim whose backoff has elapsed and which has been re-selected is `InFlight`, counted in neither arm, so the condition is silent from re-selection until `readyTimeout` rolls the attempt back into `retryBackoff`. The previous `retry_count` arm fired throughout that interval. The narrowing is accepted — a rotation that is actually running is not a wedged window — and the eventual signals are the failure alert and, if the occurrence closes with the claim still outstanding, `window_missed_total`.

Restricting the completions arm to `outcome="success"` is load-bearing for the same incident, and in the same direction. `noderotation_completed_total` counts `failure` and `expired` as well, and a `readyTimeout` rollback records a failure — so a condition that treats *any* completion as progress lets each failed attempt suppress it, and goes silent exactly through the window spent entirely on failed attempts. The condition to state is "the window is open, work is outstanding, and nothing is succeeding".

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
Each rotation creates ~10–20 minutes of overlap billing. Inside one maintenance-window occurrence, `readyTimeout` + `failurePause` bounds the pacing of repeated attempts, not the escalating `retryBackoff`: the window-aware backoff clamp (§3.2) holds a failed claim's retry inside the occurrence it failed in, so `retryBackoff` no longer ends the window early on its own, and it does not bound the pool-wide attempt rate — it escalates per claim, and several claims can alternate independently.
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
| `readyTimeout` + `failurePause` | Repeated attempts on the same claim, inside one maintenance-window occurrence (§3.2) |
| Pool-level `failurePause` | Candidate cycling under systematic failure |

- **Without `failurePause`:** a systematic cause would move to the next candidate within ~1 minute, burning a `readyTimeout`-worth of billing per candidate
- **With `failurePause`:** at most one attempt per `readyTimeout + failurePause` (~25m at defaults) — this is also the ceiling on same-claim retries inside one occurrence, since the window-aware clamp keeps `retryBackoff` from ending the window early
- `failurePause` is separate from `cooldownAfter` — lowering settle for throughput never weakens cost bounds
- `retryBackoff` still escalates per claim and still governs the wait between occurrences, but inside one occurrence it does not bound the pool-wide attempt rate: several claims can be in backoff and retrying independently at once
- `noderotation_retry_count` alerts on the pattern (§4.2)
