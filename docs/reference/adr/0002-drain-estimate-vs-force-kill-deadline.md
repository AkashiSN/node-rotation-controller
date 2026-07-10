# 2. Drain estimate vs. force-kill deadline (split `t_rot` into a deadline bound and a throughput forecast)

- Status: Accepted
- Date: 2026-07-10
- Issue: [#212](https://github.com/AkashiSN/node-rotation-controller/issues/212)

## Context

The §3.2 derivation collapsed the whole rotation cost into a single quantity, `t_rot = readyTimeout + terminationGracePeriod (tGP) + buffer`, and handed it to two callers with opposite requirements.

The **deadline side** — `leadTime`, the age threshold `A`, the guaranteed chances `G`, the [§3.3](../../specification/03-design.md#33-surge-sequence-v1) surge-less forceful-fallback deadline race, and the [§5.2](../../specification/05-implementation.md#52-reconcile-loop) `drain_stuck` bound — needs an **upper bound**: the latest instant a node can still complete before Karpenter force-completes its drain. For that, `tGP` is exactly right, because `tGP` *is* the deadline Karpenter force-kills a PDB-respecting drain at.

The **throughput side** — the per-window capacity `C = m·ceil(D / (t_rot + cooldownAfter))` and the `RotationSpansNextWindow` carry-over predicate — needs an **expected service time**: how long a healthy rotation actually takes, so it can forecast how many fit in a window. Here `tGP` is wrong by orders of magnitude. On stock EKS Auto Mode `tGP = 24h`, so on the [§3.2](../../specification/03-design.md#32-candidate-selection) worked schedule (`E = 14d`, `{Wed, Sat} 02:00–06:00`, `K = 2`) the denominator is dominated by a 24-hour term that no real drain approaches, and `C` collapses to `1` (`K·C = 2`).

[#211](https://github.com/AkashiSN/node-rotation-controller/issues/211) removed the `C < 1` early return and switched `C` to `ceil`, which restored two throughput findings — `ThroughputBelowArrival` and `ThroughputBurstShortfall` — that had been suppressed. Those findings are only as trustworthy as `C`, and a `C` computed from the force-kill deadline is not: it under-counts capacity by budgeting every rotation at a full `tGP`, so on stock Auto Mode the restored findings fire spuriously and `RotationSpansNextWindow` trips on every daily-or-more-frequent schedule.

## Decision

Split the single derived quantity in two.

- `t_rot = readyTimeout + tGP + buffer` stays the **deadline bound**, unchanged. It continues to feed `leadTime`, `A`, `G`, the §3.3 forceful-fallback deadline race, and the §5.2 `drain_stuck` bound.
- `t_rot_est = readyTimeout + drainEstimate + buffer` is a new **throughput forecast**, read by layer 2 alone: `C = m·ceil(D / (t_rot_est + cooldownAfter))` and the carry-over predicate.

`surge.drainEstimate` is a new optional `RotationPolicy` field. When it is unset the forecast falls back to `min(tGP, 10m)`, silently. That default is **non-additive** — the observable output (which layer-2 warnings appear) changes on upgrade with no configuration change and no change in rotation behavior — which is why this warrants an ADR under the issue's Process section rather than shipping as a plain refactor. An explicit estimate above `tGP` is unreachable (Karpenter force-completes the drain at `tGP`), so it emits a `DrainEstimateAboveTGP` `Warn` and is clamped to `tGP`. When `tGP` is unset there is no Karpenter deadline to clamp against, so an explicit estimate is used as-is.

`t_rot_est` is a forecast denominator, **not** a runtime gate. What actually blocks the next rotation start at runtime is `cooldownAfter`, the freeze marker, and the active-rotation anchor.

### The containment boundary

`drainEstimate` influences **no** rotation timing, **no** candidate selection, **no** start gate, **no** drain bound, and **no** `Fatal` finding. Everything that can gate a NodePool out of starting rotations, or that can make a node race its own Forceful Expiration, is on the deadline side and keeps using `tGP`. The only thing a wrong `drainEstimate` can do is make a layer-2 `Warn` too loud or too quiet.

This is pinned by `TestDeriveDrainEstimateContainment` in `internal/schedule/schedule_test.go`. Holding every other input fixed and sweeping `drainEstimate` across `nil`, `1m`, `10m`, `1h`, and `25h` (clamped), the test asserts that `TRot`, `A`, `G`, and the **entire layer-1 finding set** are invariant, while only `C`, `TRotEst`, `DrainEstimate`, and the forecast-side findings (the layer-2 warnings plus `DrainEstimateAboveTGP`) may move. It deliberately forces a non-empty layer-1 set (`RetryBackoffShort`) first, so the invariance is not vacuous.

### Rejected alternative — calibrate `tGP` from observed drains

The obvious cheaper move is to skip the new field and instead recommend operators lower `tGP` toward observed drain durations, so that the single `t_rot` serves layer 2 acceptably. It was rejected on two independent grounds.

**It does not work.** With a `90m` maintenance window the layer-2 denominator is `readyTimeout 15m + Buffer 15m + cooldownAfter 10m + tGP = 40m + tGP`. Reaching even `C = 2` therefore requires `tGP < 50m` — below the `1h` that §3.2's own calibration note recommends as a floor. `drainEstimate` reaches `C = 2` on that same window with `tGP = 24h` left untouched.

**It is unsafe.** `tGP` is not a measurement of how long drains take; it is a declaration of how long a PDB-respecting drain will be *waited for* before pods are force-killed. Fitting it to observation discards exactly the tail it exists to cover — an incident, a stuck finalizer, a PDB that changed. And unlike `drainEstimate`, `tGP` **censors its own observations**: lowering it right-truncates every subsequent drain, which biases the observed distribution downward and invites a further reduction — a ratchet. `drainEstimate` never influences the running rotation, so the drains it is estimated from stay unbiased. That asymmetry is precisely why a future EWMA over observed drains (issue #212's "Option 2") is safe for `drainEstimate` and would not be for `tGP`. The controller may report that an *estimate* disagrees with observation; it must never tell an operator to lower a *deadline*.

The one-shot rebuttal — *"under today's `tGP = 24h` nothing is ever force-killed, so the observations are uncensored and a single recommendation is mathematically sound"* — is valid arithmetic and still wrong to ship. The observed sample contains only ordinary drains; the tail `tGP` exists to cover is, by definition, exactly what has not yet been observed. Recommending a `tGP` fitted to that sample would strip the backstop of its reason to exist.

## Consequences

**Positive**

- Stock EKS Auto Mode stops over-warning: on the §3.2 worked schedule `C` goes `1 → 5` and `K·C` goes `2 → 10`, with `A` and `G` unchanged.
- The two findings #211 restored — `ThroughputBelowArrival` and `ThroughputBurstShortfall` — become trustworthy, because they now compare against a capacity forecast from an expected service time rather than from a force-kill deadline.
- `RotationSpansNextWindow` stops firing on every daily-or-more-frequent schedule.

**Negative / trade-offs**

- Upgrading changes **which layer-2 warnings appear**, with **no** change in rotation behavior, candidate selection, or any deadline. Operators who tuned alerting to the old (louder) warnings will see them quiet down.
- A `drainEstimate` set too low silences a carry-over warning that a genuinely slow drain would have justified. `10m` is a prior, not a measurement. The signal for the *real* event — a drain actually running long — remains `noderotation_drain_stuck`, which fires at `tGP + buffer` and is unaffected by `drainEstimate`.
- After this change `tGP` no longer affects `C`, `RotationSpansNextWindow`, or `ThroughputBurstShortfall`. In **auto** mode it still reaches `ThroughputBelowArrival` indirectly, because that check compares `C·A` against `N·P` and `A = E − (K·P + t_rot)` still carries `tGP`. Under an explicit `ageThreshold` override even that path closes, and `tGP` influences layer 2 not at all.
