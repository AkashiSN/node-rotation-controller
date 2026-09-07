# 5. Whole-node surge reservation vs. capacity absorb (`surge.wholeNodeReservation`)

- Status: Accepted
- Date: 2026-09-07
- Issue: [#326](https://github.com/AkashiSN/node-rotation-controller/issues/326)

## Context

The surge placeholder reserves the **sum** of the candidate's reschedulable Pod requests as a single Pod ([spec §3.3](../../specification/03-design.md#33-surge-sequence)). Whether that reservation is worth anything depends on where it lands, and there are two landing paths:

- **new-provision** — Karpenter launches a NodePool-owned node for the placeholder;
- **capacity-absorb** — kube-scheduler bin-packs the placeholder onto pre-existing spare capacity.

On the absorb path the reservation is an aggregate hole on a host that is **already running other Pods**. It is fungible with the individual evicted Pods' placement only if that host can accept all of them — and a host can offer a big enough hole while a Pod's own `podAntiAffinity`, `hostPort`, or topology constraint still refuses it. Karpenter then provisions for that Pod *after* the drain has started: behind the surge rather than in front of it, which is the one thing the surge exists to prevent.

[#305](https://github.com/AkashiSN/node-rotation-controller/issues/305) measured it on a pool that keeps two CPU-heavy services off the same node with a hostname-level `podAntiAffinity`. Across one window, 11 rotations:

| surge path | rotations | extra node provisioned after the drain started |
| --- | --- | --- |
| new-provision (`surgeWait` 23-35s) | 4 | 0 |
| capacity-absorb (`surgeWait` 4-6s) | 7 | 4 |

Nothing broke — PDBs held unavailable replicas at 1 throughout, and §4.1 already places the `readyReplicas` dip out of scope. The gap is narrower: the surge's practical purpose ("the evicted Pods have somewhere to land immediately") is only partly met on the absorb path, and `noderotation_duration_seconds{phase=surge_wait}` under-reports the real cost there — a 4-second wait can hide a 20-30 second node launch that has simply moved behind the drain. [#325](https://github.com/AkashiSN/node-rotation-controller/pull/325) made the two paths distinguishable in the log; it did not close the gap.

## Decision

Add **`surge.wholeNodeReservation`**, an opt-in per-NodePool `FeatureToggle`, default off. When enabled the placeholder is sized to

```
limit    = NodeClaim.status.allocatable − DaemonSet overhead   (the ceiling the clamp already computes)
requests = max(requests, limit)                                (per resource)
```

applied to **cpu and memory** whenever the candidate has reschedulable Pods, plus every other resource the drain itself requests.

### What it establishes, and what it does not

It raises the bar substantially: every host whose free cpu or memory is short of a whole node's is excluded, which is most occupied hosts. A **genuinely empty** host (DaemonSets only) still absorbs the placeholder, and should — an empty host has nothing for a hostname-topology anti-affinity to bite on, so it is as good as a fresh one.

It does **not** prove a host is empty, and must not be described as if it did. The reservation is sized from the **candidate's** allocatable, and three ordinary situations let an occupied host take it anyway:

- **A larger host.** The placeholder pins the NodePool and the replicated requirements, not the instance type, so on a heterogeneous NodePool — the default, since Karpenter chooses among many types — an 8-CPU host running 2 CPU of workload has more than a 4-CPU candidate's worth free. This is the common case, not a corner one.
- **A host with less DaemonSet overhead than the candidate**, whose free capacity is correspondingly larger than the reservation assumed.
- **Pods that request nothing**, which occupy nothing the scheduler counts, so a host running any number of them still has a whole node's worth free — and such a Pod can be exactly the one whose anti-affinity or `hostPort` then refuses an evicted Pod.

There is no way to express "a host with no other Pods" here: a **required** `kubernetes.io/hostname NotIn` term makes Karpenter's provisioner refuse to provision for the Pod at all (the key is in `sigs.k8s.io/karpenter` RestrictedLabels, [#96](https://github.com/AkashiSN/node-rotation-controller/issues/96)), and a required `podAntiAffinity` matching every Pod would also exclude the DaemonSets every node carries.

An operator who needs the first residual narrowed can add `node.kubernetes.io/instance-type` to `surge.matchNodeRequirements.required`, which replicates the candidate's own type onto the placeholder as a required term — it is a well-known Karpenter label and not restricted. That trades away Karpenter's freedom to substitute types on the provision path, which is a real cost when capacity for that type is short, so it is the operator's call rather than a default.

**The mode is a bounded reduction of the failure mode, not a guarantee against it.** Every statement of its behaviour — field documentation, spec, Helm values — is worded to that standard.

### Both bin-packing dimensions, whatever the drain declares

An earlier shape of this decision raised only the resources the drain requested. That left the central claim false in an ordinary case: a drain requesting only cpu reserved only cpu, so a host filled with memory-only Pods still had the cpu free to absorb the placeholder — and one of those Pods can be exactly the one an evicted Pod refuses to share a node with. cpu and memory are the two dimensions scheduling actually packs against, both are always reported in `allocatable`, and both are ordinary container requests, so both are raised whenever there is workload to re-land.

Nothing else is invented. `pods` is a node dimension the scheduler counts, not a resource a container may request. Ephemeral storage and accelerators are raised only when the drain requests them — demanding all of a node's accelerators would block far more than the rotation.

The count of reschedulable Pods, not the request sum, decides whether to reserve at all: a candidate carrying only zero-request Pods has an empty drain and real workload that still has to land somewhere. A candidate with no reschedulable Pods reserves nothing, which is correct — there is nothing to guarantee.

### It does not force a larger instance

`limit + DaemonSet = allocatable`, so the mode's request never exceeds what the candidate's own class provides: on resource fit alone it never requires a larger type. It does not *pin* the type either — the placeholder's required affinity is the NodePool and the configured replicated requirements, so Karpenter remains free to choose a different type by availability, price, or those requirements, and an existing host of a larger type can absorb the reservation outright (see the residuals above).

### Interaction with the clamp

The clamp caps requests at the same `limit` this mode raises them to, so the two are the opposite directions of one ceiling and share `provisionableLimit`. Where the raw drain is at or below the limit, whole-node lifts it to the limit and the clamp then has nothing to cut. Where the raw drain already **exceeds** the limit (the [#224](https://github.com/AkashiSN/node-rotation-controller/issues/224) case) whole-node leaves it alone and the clamp lowers it, reporting `Clamped` and the shortfall exactly as it does without this mode. A **non-positive** limit is likewise left to the clamp's refusal: raising to it would reserve nothing and satisfy `surge_ready` with an empty placeholder, a silent break-before-make.

## Consequences

**Positive**

- On the hosts the mode now selects, the reservation is one the evicted Pods can actually use for node-level constraints: on an empty or fresh host, hostname-topology anti-affinity and `hostPort` collisions cannot arise.
- `surge_wait` regains its meaning on the affected pools — the provisioning it hides on the absorb path moves back in front of the drain, where it is measured.
- The layer-2 forecast becomes more honest, not less: `t_rot_est = provisioningEstimate + drainEstimate` ([ADR-0003](0003-provisioning-estimate-vs-surge-abandon-deadline.md)) already assumes provisioning happens on every rotation, and the absorb path is the one that ran under the model.

**Negative**

- **An extra instance on most rotations**, for the rotation's duration, plus possibly a consolidation cycle immediately after completion when a nearly-empty surge host is released from `do-not-disrupt`. Not every rotation: the reservation is still absorbed by an empty host, by a larger one, or by one running only zero-request Pods.
- **The `surge_headroom` gate (§5.2 step 3) then tests a whole-node footprint**, so a NodePool whose `spec.limits` are nearly exhausted stops starting rotations that the drain-sized placeholder would have fitted. This is a real behaviour regression for such pools and the main reason the mode is opt-in and default off. The same change makes that block announce itself (`InsufficientHeadroom`, §4.3) rather than only logging, so enabling the mode cannot silently stop a pool from rotating.

**Neutral / unresolved**

- **Constraints coarser than the node are not addressed** — zone-level `podAntiAffinity` and `topologySpreadConstraints` are decided by what is in the *zone*, not by what is on the host, so no choice of host helps. Modelling the Pods faithfully (N placeholders mirroring their constraints) does not address them either: at placeholder time the Pods being modelled are still running on the candidate, so a mirrored zone-level term counts the very Pods it models and excludes the candidate's own zone — which is a *required* replicated key by default. Required same-zone plus required not-in-zone is a placeholder that is unschedulable on every attempt, and a label selector cannot say "except these specific Pods". The honest ceiling for v1 is node-level, constraint-compatible capacity.
- The mode does not change **which** host is chosen among those that qualify; per-Pod placement remains the scheduler's and PDB's domain (§3.5).
- **The `surge_headroom` gate is a pre-check, not an invariant.** It runs before the anchor on its own Pod snapshot; the placeholder is built on a later pass from another. Pods can bind to or leave the candidate in between and `status.resources` moves independently, so a placeholder can end up outside the budget the gate admitted. The enforcement is Karpenter's own `spec.limits`: such a placeholder is not provisioned for, stays unschedulable, and the rotation rolls back at `readyTimeout`. This is pre-existing, and the mode makes the footprint larger rather than introducing it.

## What must not change

- The controller still **never bypasses Karpenter**: the mode changes only the placeholder's requests, and replacement capacity is still induced through the NodePool-owned placeholder Pod.
- **Serial per NodePool** (`maxUnavailable = 1`) is untouched: the mode reserves a whole node's worth of capacity, not more than one node's.
- The mode changes **only the placeholder's requests**. It adds no node affinity, no anti-affinity and no host exclusion.
- The clamp keeps its refusal, its shortfall reporting and its band check ([#224](https://github.com/AkashiSN/node-rotation-controller/issues/224)); whole-node never lowers a drain that already exceeds the limit.
- Default off. An install that does not set the toggle behaves exactly as before.
