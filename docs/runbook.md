# Production runbook

Operational guidance for running `node-rotation-controller` on a real cluster.
The [specification](specification.md) is the source of truth for *why* the
controller behaves as it does; this runbook is the operator-facing *how*. Every
section links back to the relevant spec section.

Japanese translation: [docs/ja/runbook.md](ja/runbook.md).

> The controller is pre-1.0. EKS Auto Mode PoC runs have validated the core surge
> path, but edge cases and a full multi-hour tight-race soak remain open (see
> [§7.2 validated assumptions](specification.md#72-validated-assumptions)). Treat
> this runbook as the starting point for a production rollout, not a guarantee.

## Contents

1. [Per-AZ surge headroom for zonal-PV workloads](#1-per-az-surge-headroom-for-zonal-pv-workloads)
2. [Lowering Auto Mode `terminationGracePeriod`](#2-lowering-auto-mode-terminationgraceperiod)
3. [Interpreting the `noderotation_*` metrics](#3-interpreting-the-noderotation_-metrics)
4. [The freeze workflow](#4-the-freeze-workflow)
5. [Handling a stuck drain](#5-handling-a-stuck-drain)
6. [Alerting (PrometheusRule)](#6-alerting-prometheusrule)

---

## 1. Per-AZ surge headroom for zonal-PV workloads

**Applies to:** any NodePool that fronts workloads bound to a **zonal**
PersistentVolume — EBS `gp3`/`io2`, or any PV whose `nodeAffinity` carries a
`topology.kubernetes.io/zone` constraint.

**Why it matters.** Surge is make-before-break: the controller adds a
replacement node *before* draining the old one. For zonal-PV workloads the
replacement node is **pinned to the candidate's AZ** so the existing volume can
re-attach — `topology.kubernetes.io/zone` is in the default `required` set of
`surge.matchNodeRequirements`
([§3.3 *Stateful and zonal workloads*](specification.md#33-surge-sequence-v1)).
The controller cannot and does not migrate zonal storage across AZs.

The consequence is a **hard constraint**: a same-AZ capacity shortage **cannot
fall back to another zone**. If the candidate's AZ has no schedulable capacity
for a same-zone replacement, the surge cannot complete. The placeholder Pod
never goes `Running`, `readyTimeout` fires, and the rotation rolls back to the
`expireAfter` baseline ([§3.3 *Rollback*](specification.md#33-surge-sequence-v1)).
Repeated same-AZ shortages surface as an escalating `noderotation_retry_count`
(risk [R3](specification.md#71-risks)).

**Guidance.** For each NodePool fronting zonal-PV workloads, **keep one node's
worth of surge headroom per in-use AZ**:

- Ensure the NodePool's `requirements` permit **every in-use AZ** (do not narrow
  `topology.kubernetes.io/zone` to a single zone if your volumes span zones).
- Size the NodePool `spec.limits` resource budget so that, in *each* AZ, there
  is room for one additional node beyond the steady-state footprint. The
  controller pre-checks pool-wide `limits` headroom before starting a rotation
  (§5.2 step 3), but `limits` is a **pool-wide resource budget, not a per-AZ
  count** — it cannot express "one spare node in `us-east-1a`". Per-AZ headroom
  is therefore the operator's responsibility.
- Confirm the underlying provider has capacity (and that any EC2 vCPU quota
  leaves room) in **each** AZ the workload uses, not just in aggregate.
- Where the cloud provider supports it, consider a capacity reservation in each
  in-use AZ for the surge node's instance shape.

**How to detect a shortfall.** A same-AZ shortage manifests as
`readyTimeout`-driven rollbacks (a `failure` outcome on
`noderotation_completed_total`) and a climbing `noderotation_retry_count`
(alert: `NodeRotationRetryCountHigh`, see [§6](#6-alerting-prometheusrule)). When
that alert fires for a zonal-PV NodePool, suspect a per-AZ capacity gap first.

---

## 2. Lowering Auto Mode `terminationGracePeriod`

**Applies to:** EKS Auto Mode NodePools (where the stock
`terminationGracePeriod` (`tGP`) default is `24h`).

**Why it matters.** The controller's throughput model
([§3.2 layer 2](specification.md#32-candidate-selection)) must budget the full
`tGP` as the potential drain time for *each* rotation, because that is the bound
Karpenter can hold a drain to. The single-node rotation bound is
`t_rot = readyTimeout + tGP + buffer`, and a window of duration `D` can rotate
`C = floor(D / (t_rot + cooldownAfter))` nodes serially.

With the stock `tGP = 24h`, `t_rot ≈ 24.5h`, so a typical 4-hour window computes
`C = floor(4h / (24.5h + 10m)) = 0` — the controller **warns on every window**
(`ThroughputZero`) even though PDB-respecting drains usually finish in minutes.
The model is correct: it cannot assume the drain will be fast.

**Guidance.** Lower the NodePool `spec.template.spec.terminationGracePeriod` to a
**realistic per-node drain bound** — the worked example in
[§3.2](specification.md#32-candidate-selection) uses `1h`. With `tGP = 1h`,
`t_rot ≈ 1.5h` and the same 4-hour window gives `C = floor(4h / (1.5h + 10m)) = 2`
rotations per occurrence.

Lowering `tGP` has two further effects, both beneficial here:

- **It relaxes the Auto Mode 21-day hard cap** (`E + tGP ≤ 21d`,
  [§1.1](specification.md#11-background)). `tGP = 1h` admits `expireAfter` up to
  ~`20d`, which is exactly the headroom needed to satisfy the lead-time
  derivation for sparser (e.g. weekly) windows.
- **It tightens the stuck-drain bound.** `noderotation_drain_stuck` fires at
  `tGP + buffer`, so a lower `tGP` surfaces a wedged drain sooner (see
  [§5](#5-handling-a-stuck-drain)).

**Trade-off.** A genuinely slow drain is **force-completed after `tGP`** instead
of `24h`. Pick `tGP` from the workload's real PDB-respecting drain time — long
enough that healthy drains finish voluntarily, short enough that the throughput
model passes. Do **not** copy the `1h` from the example blindly.

> If `tGP` is unset (self-managed Karpenter allows nil), the drain is unbounded
> by Karpenter; the controller substitutes a fixed fallback bound (e.g. `1h`)
> for both the throughput model and the stuck-drain alert
> ([§3.2 layer-1 `TGPUnset` warning](specification.md#32-candidate-selection)).

---

## 3. Interpreting the `noderotation_*` metrics

Exposed on `/metrics` ([§4.2](specification.md#42-observability)). Names and
labels below are the **exact** strings emitted by the controller. Per-NodePool
series are **cleared when the NodePool is deleted** so a removed pool does not
latch its last value forever; the cluster-wide `noderotation_window_active` is
unaffected.

| Metric | Type | Labels | Read it as |
|--------|------|--------|------------|
| `noderotation_candidates` | Gauge | `nodepool` | Eligible NodeClaims awaiting rotation. **Should trend to 0** inside/after each window. Stuck > 0 across two windows → controller is falling behind ([R2](specification.md#71-risks)). |
| `noderotation_in_progress` | Gauge | `nodepool` | Active rotations (0 or 1 in v1 — serial per pool). |
| `noderotation_completed_total` | Counter | `nodepool`, `outcome` | Cumulative completions. `outcome ∈ {success, failure, expired}`. `expired` = the old node was **force-expired before** a graceful rotation finished (the lead-time race was lost — [§3.5](specification.md#35-backstop-behavior)); it is never counted as `success`. |
| `noderotation_duration_seconds` | Histogram | `nodepool`, `phase` | Per-phase latency. `phase ∈ {surge_wait, drain}`. Rising `surge_wait` ≈ slow/failing provisioning; rising `drain` ≈ slow eviction. |
| `noderotation_window_active` | Gauge | — | `0/1` cluster-wide window membership. **Label-free by design** (the window is a single union in v1). |
| `noderotation_freeze_until_timestamp` | Gauge | `nodepool` | Unix timestamp of the active freeze (`0` = no freeze). Non-zero → rotation is **deliberately suppressed** (see [§4](#4-the-freeze-workflow)). |
| `noderotation_age_threshold_seconds` | Gauge | `nodepool` | The derived `ageThreshold` `A` ([§3.2](specification.md#32-candidate-selection)). Varies per pool. |
| `noderotation_rotation_chances` | Gauge | `nodepool` | Guaranteed rotation chances `G` for the derived threshold. With auto-derivation `G = K`; an override may lower it (and is validated). |
| `noderotation_window_period_seconds` | Gauge | `nodepool` | Worst-case window period `P`. Identical across pools in v1 (window is cluster-wide); the `nodepool` label is forward-looking. |
| `noderotation_short_lead_nodes` | Gauge | `nodepool` | NodeClaims whose **own** stamped `expireAfter` can no longer guarantee `K` chances ([§3.2 layer 3](specification.md#32-candidate-selection)). Rotated best-effort; may reach Forceful Expiration. |
| `noderotation_drain_stuck` | Gauge | `nodepool` | `0/1`: the in-flight drain exceeded `tGP + buffer`. `1` → operator action needed ([§5](#5-handling-a-stuck-drain)). |
| `noderotation_retry_count` | Gauge | `nodepool` | Highest `noderotation.io/retry-count` across the pool's NodeClaims (`0` when none). `≥ 3` → a **systematic** failure (sustained preemption or same-AZ shortage, [R3](specification.md#71-risks)). |

**Warning Events.** Non-fatal findings are *also* surfaced as Kubernetes
`Warning` Events on the NodePool / NodeClaim
([§4.2](specification.md#42-observability)), so `kubectl describe nodepool <name>`
shows them without a metrics stack. Reasons include `KBelowTwo`,
`AVeryAggressive`, `TGPUnset`, `HardCapExceeded`, `ThroughputZero`,
`ThroughputBelowArrival`, `OverrideGBelowK`, and `ShortLead`.

**Judging reconcile liveness — use the metrics, not the warning log.** Both the
Warning Events *and* the `INFO`-level warning **log** lines are deduplicated on
the transition into a condition, so in steady state — stable findings, no
transitions — a perfectly healthy reconcile loop can run for many passes
emitting **zero** log lines. **Do not** read "no recent warning log line" as
"reconcile stalled" (a real past mis-diagnosis). Judge liveness from the
controller-runtime metrics on `/metrics`, which tick on **every** pass:

- `controller_runtime_reconcile_total{controller="rotation"}` — increments per
  reconcile; a rising `rate()` means the loop is alive.
- `controller_runtime_reconcile_time_seconds_count{controller="rotation"}` — same
  per-pass count, via the latency histogram.
- `workqueue_*` (e.g. `workqueue_adds_total`, `workqueue_depth`,
  `workqueue_work_duration_seconds_*` for `name="rotation"`) — queue activity and
  backlog.

To *see* per-pass activity in the log while debugging, raise the controller's log
verbosity (`--zap-devel`, or a higher `-v`). At debug verbosity (`V(1)`) the
controller additionally emits, **un-deduplicated, every pass**, the current
findings and a lightweight `reconcile` heartbeat line (phase, candidate count,
claim count, in-window, findings count). This is additive debug visibility only —
it does not change the dedup of the `INFO` log or the Warning Events, nor any
metric.

---

## 4. The freeze workflow

**Purpose.** Suppress rotation of a single NodePool until a chosen time — e.g. a
business-critical period — without uninstalling the controller or losing the
`expireAfter` backstop.

**Mechanism.** Set the freeze annotation on the **NodePool**, with an RFC3339
timestamp value ([§3.1](specification.md#31-maintenance-window)):

```sh
kubectl annotate nodepool <name> \
  noderotation.io/freeze='2026-12-31T23:59:59Z' --overwrite
```

While the freeze is in effect the controller:

- **does not start** new rotations on that pool;
- **holds an in-flight rotation that is still `pending`** — the drain has not
  begun, so pausing is safe; placeholder (re)creation and the `pending →
  draining` transition are suspended;
- **keeps passive bookkeeping running** (re-asserting the protective
  `do-not-disrupt`/cordon markers, persisting the surge-claim identity), so a
  freeze never weakens the crash-recovery guarantees;
- **lets a rotation already in `draining` run to completion** — a drain cannot
  be safely aborted mid-flight.

If a freeze outlasts `readyTimeout`, the held `pending` attempt simply rolls
back through the normal failure path. `noderotation_freeze_until_timestamp`
reports the active freeze (`0` = none).

**Manage it via GitOps, not ad hoc** (risk [R5](specification.md#71-risks)). A
forgotten ad-hoc freeze silently disables rotation for the pool until its
timestamp passes; the backstop still bounds node age at `expireAfter`, but the
graceful path is off. Declare the freeze in your GitOps repo so it is visible in
review and **expires when removed from source**, not when someone remembers to
unset it. To lift early:

```sh
kubectl annotate nodepool <name> noderotation.io/freeze- # remove the annotation
```

---

## 5. Handling a stuck drain

**Symptom.** `noderotation_drain_stuck == 1` for a NodePool (alert:
`NodeRotationDrainStuck`).

**Meaning.** The in-flight rotation entered `draining` (the controller already
deleted the old NodeClaim, so Karpenter is draining the node via the voluntary,
PDB-respecting path), and the drain has now exceeded `tGP + buffer`
([§5.2](specification.md#52-reconcile-loop)). The gauge is recomputed from live
state every reconcile, so it **clears on its own** the moment the drain finishes.

**Important — the serial gate is held on purpose.** A `draining` rotation
**cannot be rolled back** (the old NodeClaim already carries a
`deletionTimestamp`), and the controller deliberately **does not start a second
rotation** while this one is stuck — releasing the per-NodePool gate would
disrupt a second node while the first is half-drained, violating
`surge.maxUnavailable = 1`. So a stuck drain **blocks all rotation for that
NodePool** until it clears. Other NodePools are unaffected.

**Remediation is operator-side.** Almost always the blocker is a **PDB that
cannot be satisfied** or a **stuck finalizer** on a Pod.

1. Find the draining node and the NodeClaim:

   ```sh
   kubectl get nodeclaim -l karpenter.sh/nodepool=<name> \
     -o wide | grep -i terminating
   kubectl get node <node> -o yaml | grep -A3 deletionTimestamp
   ```

2. Find what is blocking eviction:

   ```sh
   # Pods still on the node
   kubectl get pods --all-namespaces \
     --field-selector spec.nodeName=<node> -o wide
   # PDBs and their allowed disruptions
   kubectl get pdb --all-namespaces \
     -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,ALLOWED:.status.disruptionsAllowed,CURRENT:.status.currentHealthy,DESIRED:.status.desiredHealthy
   ```

   A PDB with `ALLOWED = 0` is the usual culprit — the workload has too few
   healthy replicas to give one up. **Fix the PDB or the workload** (scale up so
   the PDB allows a disruption, or correct a `minAvailable`/`maxUnavailable`
   that can never be met), rather than the controller.

3. For a **stuck finalizer**, identify the Pod whose `metadata.finalizers` never
   clears and resolve the controller that owns that finalizer.

4. **When `tGP` is set**, Karpenter ultimately **force-completes the drain** at
   `tGP` regardless — so a stuck drain self-resolves within `tGP`, and the
   stuck-drain alert is a heads-up that a *graceful* drain is not finishing, not
   that the node is stuck forever. This is the practical reason to keep `tGP`
   bounded ([§2](#2-lowering-auto-mode-terminationgraceperiod)). **When `tGP` is
   unset** (self-managed Karpenter), there is **no Karpenter-side force** — a
   blocking PDB or stuck finalizer can hold the drain indefinitely, so operator
   action is the only resolution.

Do **not** attempt to "unstick" the rotation by deleting the controller's
annotations or the placeholder — the rotation resumes from the NodePool anchor
([§5.2](specification.md#52-reconcile-loop)) and the handlers are idempotent.
Fix the underlying PDB/finalizer and let the drain complete.

---

## 6. Alerting (PrometheusRule)

The Helm chart ships an **optional** `PrometheusRule` with the six
[§4.2](specification.md#42-observability) alerts. It is **off by default** (so
existing installs and clusters without the Prometheus Operator are unaffected).
Enable it with:

```sh
helm upgrade --install rot charts/node-rotation-controller \
  --set prometheusRule.enabled=true
```

| Alert | Expression | Means |
|-------|------------|-------|
| `NodeRotationCompletedFailureOrExpired` | `increase(noderotation_completed_total{outcome=~"failure\|expired"}[1h]) > 0` | A rotation failed or a node was force-expired in the last hour. |
| `NodeRotationCandidatesNotDraining` | `min_over_time(noderotation_candidates[<2·P>]) > 0 and noderotation_candidates offset <2·P> > 0` | Candidates have not cleared across two consecutive windows ([R2](specification.md#71-risks)). The `offset` guard keeps a freshly created non-zero series from alerting before two windows of history exist. |
| `NodeRotationStalledInWindow` | window active **and** candidates `> 0` **and** zero completions | Rotation is wedged inside the maintenance window. |
| `NodeRotationDrainStuck` | `noderotation_drain_stuck == 1` | Drain blocked past `tGP + buffer` — see [§5](#5-handling-a-stuck-drain). |
| `NodeRotationShortLeadNodes` | `noderotation_short_lead_nodes > 0` | NodeClaims whose stamped `expireAfter` can no longer guarantee `K` chances. |
| `NodeRotationRetryCountHigh` | `noderotation_retry_count >= 3` | The same rotation keeps failing — systematic cause ([R3](specification.md#71-risks)). |

**Tune the schedule-dependent ranges.** Two alerts depend on your window
period `P` and window duration `D`; their ranges are **chart values**, not
hard-coded:

- `prometheusRule.candidatesNotDraining.windowRange` — set to roughly **two
  window periods** (`2·P`). The default `8d` matches a `{Wed, Sat}` schedule
  (`P = 4d`). For a weekly window (`P = 7d`) raise it to `14d`.
- `prometheusRule.stalledInWindow.completionRange` — set to roughly a **full
  window duration** (`D`). The default `4h` matches a `02:00–06:00` window.

Each alert's `for` and `severity` are also configurable (see
[`values.yaml`](../charts/node-rotation-controller/values.yaml)). The
`min_over_time`/`increase` ranges intentionally avoid Prometheus subqueries so
the rules stay cheap; widen the recording window rather than nesting a subquery
if you change the schedule substantially.
