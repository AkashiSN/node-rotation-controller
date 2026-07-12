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
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` | Per-phase duration; phase ∈ {surge_wait, drain}. `surge_wait` = `started-at → surge_ready`; `drain` = `draining-at → old-NodeClaim finalization`, anchored by the NodePool `draining-at` annotation stamped at the `pending → draining` transition because the old NodeClaim's `deletionTimestamp` has finalized away by the single completion point where the histogram is observed once (§5.3). Observed **at most once per successful transition**: each sample is taken only *after* the durable annotation write that makes its phase transition real, so a retried write (a `resourceVersion` conflict or transient API error re-drives the same phase) never double-counts. The trade is deliberate for a histogram — a controller that dies between the write and the observation drops that one sample (lowering `_count`/`_sum` truthfully) rather than injecting a phantom sample for a duration no rotation took. This differs from the `noderotation_completed_total` counters, which keep at-least-once placement (a lost increment is worse than a duplicated one) |
| `noderotation_window_active` | Gauge | `nodepool` | 0/1 indicator of the NodePool's governing-policy window membership |
| `noderotation_policy_conflict` | Gauge | `nodepool` | 0/1: the NodePool is blocked from rotating by a `RotationPolicy` conflict — an equal-specificity selector tie or a runtime-invalid governing policy (§5.4). 1 while blocked, 0 once a single valid policy governs it |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` | Unix timestamp of active freeze (0 = no freeze) |
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` | Derived `ageThreshold` (§3.2) |
| `noderotation_rotation_chances` | Gauge | `nodepool` | Guaranteed rotation chances `G` for the derived threshold |
| `noderotation_throughput_capacity` | Gauge | `nodepool` | Layer-2 throughput forecast `C` — rotation starts per window occurrence (§3.2 layer 2). Exporting it makes a `ThroughputBelowArrival`/`ThroughputBurstShortfall` finding traceable to its capacity input (the arrival side `N` is a node count from kube-state/Karpenter, not a `noderotation_*` series) |
| `noderotation_t_rot_estimate_seconds` | Gauge | `nodepool` | Forecast service time `t_rot_est = provisioningEstimate + drainEstimate` — how long a healthy rotation takes; the layer-2 denominator that produces `C` (ADR-0003) |
| `noderotation_t_rot_bound_seconds` | Gauge | `nodepool` | Deadline-side rotation-duration bound `t_rot = readyTimeout + tGP + buffer` — attempt start through surge wait + force-completed drain; feeds `leadTime`/`short_lead` and the forceful-fallback deadline race. **Not** the `drain_stuck` bound (that is `tGP + buffer` off the old NodeClaim's `deletionTimestamp`) |
| `noderotation_window_period_seconds` | Gauge | `nodepool` | Worst-case window period `P` of the schedule union |
| `noderotation_short_lead_nodes` | Gauge | `nodepool` | NodeClaims whose **own** `spec.expireAfter` can no longer guarantee `K` chances (per-node `A ≤ 0`; §3.2 layer 3) |
| `noderotation_drain_stuck` | Gauge | `nodepool` | 0/1: the in-flight rotation's drain has exceeded `tGP + buffer` (§5.2) |
| `noderotation_retry_count` | Gauge | `nodepool` | Highest `noderotation.io/retry-count` (§5.3) across the NodePool's NodeClaims (0 when none) — the systematic-failure signal; annotations alone cannot feed Prometheus alerts |

> **Label note.** With per-NodePool maintenance windows (each NodePool resolves its own governing `RotationPolicy`, §5.4), `noderotation_window_period_seconds` and `noderotation_window_active` both carry a **load-bearing** `nodepool` label — `P` and window *membership* can differ across pools when their policies carry different `maintenanceWindows`. `noderotation_age_threshold_seconds` and `noderotation_rotation_chances` likewise vary per NodePool (they fold in each pool's representative `expireAfter`/`terminationGracePeriod` *and* its policy's `K`). `noderotation_throughput_capacity`, `noderotation_t_rot_estimate_seconds` and `noderotation_t_rot_bound_seconds` likewise vary per NodePool. Two "no derivation" cases differ: for `expireAfter: Never` all three read 0 (the derivation is skipped entirely), whereas for an expiring template with **no window occurrence** (`P ≤ 0`) the bound and estimate are non-zero while `throughput_capacity` is 0 — the service-time and deadline forecasts are defined without a window; only the per-occurrence throughput is not. This resolves the v1 simplification noted in earlier drafts, where a single cluster-wide window made these series identical across pools.

> **Lifecycle note.** The per-`nodepool` series are **cleared when the NodePool is deleted** — the controller drops them on the delete reconcile. The gauges are recomputed each reconcile, so a deleted pool whose reconciles stop would otherwise latch its last value forever (a since-removed `noderotation_drain_stuck = 1` would alert indefinitely). A NodePool that loses its governing policy (no `RotationPolicy` matches it any longer) has its series dropped the same way, since it is no longer rotated (§5.4); an in-flight rotation anchored on such a pool is rolled back first so its placeholder and `do-not-disrupt` marker are not orphaned (§5.4).

**Kubernetes Events.** Warning-level conditions that are computed every
reconcile are also surfaced as Kubernetes `Warning` Events, so operators see
them with `kubectl describe` without reading metrics:

| Surface | Object | Reason | When |
|---------|--------|--------|------|
| Non-fatal schedule finding (§3.2 layers 1–2) | NodePool | the finding code (e.g. `KBelowTwo`, `AVeryAggressive`, `TGPUnset`, `HardCapExceeded`, `RetryBackoffShort`, `DrainEstimateAboveTGP`, `ThroughputBelowArrival`, `ThroughputBurstShortfall`, `RotationSpansNextWindow`, `OverrideGBelowK`) | the finding becomes active for the NodePool |
| Short-lead NodeClaim (§3.2 layer 3) | NodeClaim | `ShortLead` | the claim's own `expireAfter` can no longer guarantee `K` chances |
| Surge-less forceful fallback (§3.3) | NodePool | `ForcefulFallback` | a surge-less window-bounded rotation begins because a graceful surge cannot complete before the candidate's deadline (opt-in `surge.forcefulFallback`) |
| Rotation started (§5.2 step 3) | NodePool | `RotationStarted` | a candidate is picked and the pool's serial gate is anchored (`Normal`) |
| Rotation completed (§5.2) | NodePool | `RotationCompleted` | the old NodeClaim finalized away out of `draining` (`Normal`) |
| Rotation attempt failed (§5.2) | NodeClaim | `RotationFailed` | the surge node did not become `Ready` within `readyTimeout`; the attempt is rolled back |
| Unschedulable surge placeholder (§3.3) | NodeClaim | `SurgeUnschedulable` | the placeholder carries `PodScheduled=False`; the scheduler's reason and message are copied into the Event |
| Surge placeholder clamped (§3.3) | NodeClaim | `SurgeClamped` | the placeholder was clamped to `NodeClaim.status.allocatable − DaemonSet overhead` so a nearly-full node stays rotatable (`Normal`); the shortfall is bounded by the per-AZ band and absorbed by preemption + Karpenter follow-up |
| Clamp shortfall exceeds band (§3.3) | NodeClaim | `SurgeClampBandExceeded` | a fired clamp gave up more than the measured per-AZ band explains — request accounting has diverged from the scheduler's (`Warning`); the rotation still proceeds |
| Surge clamp refused (§3.3) | NodeClaim | `SurgeClampRefused` | DaemonSet overhead exhausts `NodeClaim.status.allocatable`, so no clamp induces a node; the placeholder keeps the full drain, stays unschedulable, and the rotation rolls back (`Warning`) |

Events are **deduplicated by emitting on the transition into the condition**:
a finding/claim that clears and later returns re-fires. The dedup state is
in-memory, so a controller restart re-emits each active warning once. Fatal
findings are not events — they block the start of a rotation and are logged by
the §5.2 feasibility gate.

**State-machine transitions.** Every transition of the rotation state machine
(§5.2) emits one `INFO` log line, so a rotation is reconstructable from the log
alone rather than from `NodeClaim` timestamps and Karpenter's Events. The volume
is a handful of lines per rotation, so they are **not** behind `V(1)`.

Each line is emitted **after** the durable annotation write that makes its
transition real (§5.3), never before. A reconcile whose write fails is retried
from the same phase — the state machine's writes are idempotent by design — so a
line placed before the write would repeat on every retry rather than mark the
transition once. The consequence is the opposite trade: a controller that dies
between the write and the log **drops** that line. The rotation is unaffected,
and the durable state on the object remains the authority; the log is a
best-effort narration of it, not a ledger.

| Line | Fields |
|------|--------|
| `rotation candidate selected` | `nodeclaim`, `age`, `deadline`, `eligible`, `surgeless` |
| `no rotation candidate` | `reason` — the blocking start gate (`outOfWindow`, `frozen`, `cooldownAfterSuccess`, `failurePause`), or `noEligibleClaim` plus the census (`claims`, `notTriggered`, `inBackoff`, `inFlight`, `optedOut`, `deleting`, `notReady`, `terminal`) |
| `surge placeholder created` | `placeholder`, `requests`, `reschedulablePods`, `daemonSetPods`, `mirrorPods`, `completedPods`, `nodePinnedPods`; when the clamp fires (§3.3) also `clamped`, `unclamped`, `limit`, `shortfall`, and `bandExceeded` if the shortfall exceeds the band; when refused, `clampRefused` names the resource |
| `surge placeholder is not schedulable` | `placeholder`, `reason`, `detail` — the placeholder's `PodScheduled=False` condition |
| `surge node ready` | `surgeNode`, `surgeWait` |
| `drain started` | `node`, `mode` ∈ {`surge`, `forceful-fallback`} |
| `rotation attempt failed` | `reason`, `readyTimeout`, `retryCount`, `backoffUntil` |
| `rotation complete` | `mode`, `drain`; on the surge path also `surgeNode`, `surgeWait`, and `total` (= `surgeWait` + `drain`) — the whole rotation on one self-contained line, so auditing it end-to-end needs no join back to the `surge node ready` line emitted in an earlier pass. `surgeWait` is carried forward on the NodePool `surge-wait` anchor (§5.3); `surgeNode` is recovered from the surge target's `surge-for` marker before unfreeze. All three are absent on the surge-less forceful-fallback path, which has no surge phase |

Two of these describe **level-triggered** conditions rather than edges — the
reconcile re-evaluates them every pass — so they carry the same transition dedup
as the Warning Events above: `no rotation candidate` re-fires only when its
reason or census changes, and `surge placeholder is not schedulable` only when
the scheduler's message changes. Without that dedup an idle NodePool would log
every `longRequeue` and a `readyTimeout`-long stall would log every
`shortRequeue`.

> **Why `surge placeholder created` reports its exclusions.** Karpenter's own
> `FailedScheduling` message reports the total capacity it must find — the
> placeholder's requests **plus** the DaemonSet overhead it adds to any node it
> provisions — which is easy to misread as the controller double-counting
> DaemonSet Pods in `ReschedulableRequests` (§3.3). The line states both the
> computed requests and the Pods excluded from them, so the two numbers can be
> reconciled without inspecting the live placeholder.

> **Liveness is judged from metrics, not from the warning log.** The same
> transition dedup applies to the `INFO`-level warning **log** lines, so in
> steady state — stable findings, no transitions, no rotation in flight — a
> healthy reconcile loop can run for many passes emitting **zero** log lines.
> (A rotation in flight does log: see *State-machine transitions* above. Silence
> means nothing is changing, not that nothing is running.) The deduped warning
> log is therefore **not** a liveness signal: reconcile liveness must be read from the
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

Two mechanisms bound how fast failures can repeat that cost: the escalating `retryBackoff` (§5.3) bounds retries of the *same* claim, and the **pool-level inter-attempt pause** (`last-failure-at` + `failurePause`, §5.2 step 2, ADR-0004) bounds candidate *cycling* — without it, a systematic cause (sustained placeholder preemption, persistent same-AZ capacity shortage) that fails every claim it touches would simply move on to the next candidate within ~a minute, burning a `readyTimeout`-worth of failed-surge billing per candidate. With the pause, a NodePool under a systematic failure runs at most **one attempt per `readyTimeout + failurePause`** (~25m at defaults, where an unset `failurePause` resolves to `max(10m, cooldownAfter)`), and `noderotation_retry_count` alerts on the pattern (§4.2). `failurePause` is separate from `cooldownAfter` (the post-success settle) so lowering the settle for window throughput never weakens this cost bound.
