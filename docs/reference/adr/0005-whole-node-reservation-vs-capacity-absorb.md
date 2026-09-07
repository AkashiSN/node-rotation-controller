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
requests = max(reschedulable sum, limit)                       (per resource)
```

instead of to the reschedulable sum.

### It is not "force a new node"

The mechanism requires **a host with a whole node's worth of free space**, and that is a strictly better-targeted thing to require:

- a **partially-filled** host — precisely the class on which the aggregate assumption breaks — can no longer take the placeholder;
- a **genuinely empty** host (DaemonSets only) still can, and should: an empty host has nothing for a hostname-topology anti-affinity to bite on, and is as good as a fresh one.

So the rule selects exactly the hosts on which "one aggregate hole" and "N individual Pods" are equivalent, rather than banning absorb outright. Sizing by *excluding* hosts was not available: a **required** `kubernetes.io/hostname NotIn` term makes Karpenter's provisioner refuse to provision for the Pod at all (the key is in `sigs.k8s.io/karpenter` RestrictedLabels), which is why the candidate/near-deadline exclusion is already only a *preferred* term ([#96](https://github.com/AkashiSN/node-rotation-controller/issues/96)).

### It never upsizes

`limit + DaemonSet = allocatable`, so Karpenter needs an instance type with at least the candidate's allocatable — which the candidate's own class satisfies exactly. The mode costs one node of that class, never a larger one.

### Only resources the drain requests are raised

A resource the evicted Pods do not request is not one they need reserved, and `pods` — which `allocatable` always carries — is not a resource a container may request at all. A drain that requests nothing therefore reserves nothing, which is correct: a candidate with no reschedulable Pods has nothing to guarantee, and reserving a whole node for it would be pure cost.

The consequence to state plainly: bin-packing turns on cpu *and* memory, so a drain that requests only cpu reserves only cpu, and a host with free cpu but no free memory can still absorb it. That is consistent — the Pods being modelled request no memory either — but it means the mode's guarantee is scoped to what the drain declares, not to the node's whole shape.

### Interaction with the clamp

The clamp caps requests at the same `limit` this mode raises them to, so the two are the opposite directions of one ceiling and share `provisionableLimit`. On the whole-node path the clamp is a no-op except where it **refuses** — a non-positive limit, which whole-node deliberately leaves to it: raising to a non-positive limit would reserve nothing and satisfy `surge_ready` with an empty placeholder, a silent break-before-make. Keeping the drain there preserves the existing rollback.

## Consequences

**Positive**

- The reservation is one the evicted Pods can actually use for node-level constraints: on an empty or fresh host, hostname-topology anti-affinity and `hostPort` collisions cannot arise.
- `surge_wait` regains its meaning on the affected pools — the provisioning it hides on the absorb path moves back in front of the drain, where it is measured.
- The layer-2 forecast becomes more honest, not less: `t_rot_est = provisioningEstimate + drainEstimate` ([ADR-0003](0003-provisioning-estimate-vs-surge-abandon-deadline.md)) already assumes provisioning happens on every rotation, and the absorb path is the one that ran under the model.

**Negative**

- **An instance of the candidate's class per rotation**, for the rotation's duration, plus possibly a consolidation cycle immediately after completion when a nearly-empty surge host is released from `do-not-disrupt`.
- **The `surge_headroom` gate (§5.2 step 3) then tests a whole-node footprint**, so a NodePool whose `spec.limits` are nearly exhausted stops starting rotations that the drain-sized placeholder would have fitted. This is a real behaviour regression for such pools and the main reason the mode is opt-in and default off. The same change makes that block announce itself (`InsufficientHeadroom`, §4.3) rather than only logging, so enabling the mode cannot silently stop a pool from rotating.

**Neutral / unresolved**

- **Constraints coarser than the node are not addressed** — zone-level `podAntiAffinity` and `topologySpreadConstraints` are decided by what is in the *zone*, not by what is on the host, so no choice of host helps. Modelling the Pods faithfully (N placeholders mirroring their constraints) does not address them either: at placeholder time the Pods being modelled are still running on the candidate, so a mirrored zone-level term counts the very Pods it models and excludes the candidate's own zone — which is a *required* replicated key by default. Required same-zone plus required not-in-zone is a placeholder that is unschedulable on every attempt, and a label selector cannot say "except these specific Pods". The honest ceiling for v1 is node-level, constraint-compatible capacity.
- The mode does not change **which** host is chosen among those that qualify; per-Pod placement remains the scheduler's and PDB's domain (§3.5).

## What must not change

- The controller still **never bypasses Karpenter**: the mode changes only the placeholder's requests, and replacement capacity is still induced through the NodePool-owned placeholder Pod.
- **Serial per NodePool** (`maxUnavailable = 1`) is untouched: the mode reserves a whole node's worth of capacity, not more than one node's.
- The clamp keeps its refusal, its shortfall reporting and its band check ([#224](https://github.com/AkashiSN/node-rotation-controller/issues/224)); whole-node never lowers a drain that already exceeds the limit.
- Default off. An install that does not set the toggle behaves exactly as before.
