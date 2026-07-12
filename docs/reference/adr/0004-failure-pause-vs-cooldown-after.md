# 4. Split the post-failure pause off `cooldownAfter` (`surge.failurePause`)

- Status: Accepted
- Date: 2026-07-12
- Issue: [#216](https://github.com/AkashiSN/node-rotation-controller/issues/216)

## Context

`surge.cooldownAfter` was read by **two per-NodePool start gates with opposite requirements** ([spec §5.2](../../specification/05-implementation.md#52-reconcile-loop) step 2):

- **Gate A — post-success settle.** Written on the success path (`last-rotation-at`), it waits for the drained node's Pods to re-land. It **wants to be small**: every second is dead time inside a maintenance window, and it feeds the layer-2 throughput forecast `C = m·ceil(D / (t_rot_est + cooldownAfter))`.
- **Gate B — post-failure inter-attempt pause.** Written on the rollback path (`last-failure-at`), it bounds candidate *cycling* under a systematic failure cause (zonal capacity shortage, instance-type unavailability, `limits` headroom) so a NodePool runs at most one attempt per `readyTimeout + cooldownAfter` ([§4.4](../../specification/04-operations.md#44-cost)). It **wants to be large**.

The two anchors and the two blocked-gate reason codes were already distinct; only the *value* was shared. An operator therefore could not express "settle 1m after a success, but pause 30m after a failure". Lowering `cooldownAfter` to reclaim window throughput **silently weakened the failure pause**; raising it to harden the failure pause **silently deflated `C`** and spuriously fired `RotationSpansNextWindow`. This is the same "one symbol, two callers" defect resolved for `t_rot` ([ADR-0002](0002-drain-estimate-vs-force-kill-deadline.md)), `buffer` ([#215](https://github.com/AkashiSN/node-rotation-controller/issues/215)) and the `t_rot_est` terms ([ADR-0003](0003-provisioning-estimate-vs-surge-abandon-deadline.md)).

Gate A is also **weaker than it looks**. For PDB-covered workloads the settle is already enforced demand-driven: with `maxUnavailable: 1` the Eviction API blocks the next node's eviction until the drained node's replacement is `Ready`. The fixed timer is redundant there and unsound for PDB-less workloads (no fixed duration is defensible without knowing startup time). So gate A deserves an allowed `0`, with PDBs documented as the primary settle mechanism — not a `must be positive` rejection.

## Decision

Give each gate its own value.

- **`cooldownAfter` becomes the post-success settle only** (gate A). It keeps feeding the layer-2 forecast. It **may be `0`** (validated non-negative, not positive) — PDBs are the primary settle. Its `10m` default is unchanged.
- **`surge.failurePause` is a new field** (gate B), read only against `last-failure-at`, feeding **no** forecast. Unset, it resolves to `max(10m, cooldownAfter)`. It **must be positive** — a `0` pause would disable the §4.4 cost bound. There is deliberately no schema default: the fallback depends on `cooldownAfter` and admission cannot compute `max()` across fields, so it resolves in the controller's `resolve()`.
- **Layer 2 continues to use `cooldownAfter` alone.** `C` and `RotationSpansNextWindow` then model only *successful* rotations, which is what their derivations already claim.
- The blocked-gate reason code `cooldownAfterFailure` is renamed **`failurePause`** to match.

### Why the default is `max(10m, cooldownAfter)`

Direction-of-error. A **too-short** failure pause lets a systematic cause burn a `readyTimeout`-worth of failed-surge billing on every candidate it cycles through — the exact harm §4.4 exists to prevent. A **too-long** pause only slows recovery from a transient failure. So the prior must err **long**. `max(10m, cooldownAfter)` never shortens any existing install's failure pause on upgrade: installs with `cooldownAfter ≥ 10m` are unchanged, and installs that lowered `cooldownAfter < 10m` for throughput have their failure pause *lengthened* back to `10m` — the corrective direction, and exactly the case the shared value could not express. It is a field rather than a fixed prior for the same reason [#212](https://github.com/AkashiSN/node-rotation-controller/issues/212)/[#220](https://github.com/AkashiSN/node-rotation-controller/issues/220) chose fields: a cluster whose systematic causes clear slowly can raise it.

### Flat, not escalating (v1)

The issue floats an escalating `failurePause` mirroring `retryBackoff`'s doubling. Deferred: `retryBackoff` already escalates at *claim* scope, and a *pool*-scope escalation needs a new durable pool-level consecutive-failure counter (only per-claim `retry-count` exists today). A flat pause already bounds cycling to one attempt per `readyTimeout + failurePause`; escalation is a separable follow-up.

## What must not change

- Both gates stay **per NodePool**; distinct pools rotate concurrently.
- `retryBackoff` keeps its meaning — the per-NodeClaim re-selection backoff, escalating, capped at 8×. It does **not** subsume gate B (different scope: same-claim vs. pool).
- `drain_bound` (`tGP + buffer`) is untouched — a deadline, not a pause.
- Deleting `cooldownAfter` is **not** on the table: it would take gate A's throughput term and the only unconditional pause on the capacity-absorb path (`surge_wait ≈ 0`) along with gate B.

## Consequences

**Positive**

- Operators can express the two pauses independently ("settle `1m` after success, pause `30m` after failure").
- `C` and `RotationSpansNextWindow` stop being corrupted by the post-failure pause's value — they model only successful rotations.
- `cooldownAfter: 0` lets PDB-serialized workloads reclaim the window throughput the fixed settle timer consumed.

**Negative / trade-offs**

- Upgrading changes failure-pause behavior **only** for installs that set `cooldownAfter < 10m`: their pause lengthens to `10m` (the safe direction). Every other install is fully additive — the failure pause, the settle, and `C` are all unchanged.
- The blocked-gate reason code `cooldownAfterFailure` becomes `failurePause` — an observable label change on the "no rotation candidate" diagnostic (pre-1.0, `v1alpha1`).

**Scope**

- Splits the field only. `cooldownAfter` keeps gate A and its layer-2 term; escalation is deferred. `drainEstimate`/`provisioningEstimate` (ADR-0002/0003) are unaffected.
