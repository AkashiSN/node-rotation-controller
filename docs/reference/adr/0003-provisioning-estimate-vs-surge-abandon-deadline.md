# 3. Provisioning estimate vs. surge-abandon deadline (drop `buffer` and `readyTimeout` from `t_rot_est`)

- Status: Accepted
- Date: 2026-07-11
- Issue: [#220](https://github.com/AkashiSN/node-rotation-controller/issues/220)

## Context

[ADR-0002](0002-drain-estimate-vs-force-kill-deadline.md) ([#212](https://github.com/AkashiSN/node-rotation-controller/issues/212)) split the single rotation cost into a **deadline bound** `t_rot = readyTimeout + tGP + buffer` and a **throughput forecast** `t_rot_est = readyTimeout + drainEstimate + buffer`, so layer 2 stops budgeting the drain phase at the force-kill deadline `tGP`. It fixed the drain term and left the other two untouched.

Those other two terms carry the same defect ADR-0002 removed from the drain term, one step down. `t_rot_est` claims to be an **expected service time** — how long a healthy rotation actually takes — yet:

- `readyTimeout` is not how long provisioning takes; it is the deadline the surge attempt is **abandoned** at (the node failed to reach Ready and the attempt rolls back). A provision that ever succeeds did so *within* `readyTimeout`, typically far below it — real EKS Auto Mode reaches Ready in 1–3m against a `15m` default.
- `buffer` is not service time at all; it is fixed slack covering the controller's own detection lag ([#215](https://github.com/AkashiSN/node-rotation-controller/issues/215)). Slack has no place in an *expected* duration.

So on stock Auto Mode `t_rot_est = readyTimeout 15m + drainEstimate 10m + buffer 2m = 27m`, of which only the `10m` drain term is an actual expected phase duration and the `~15m` provisioning phase is over-stated by a timeout. The denominator is inflated by roughly `17m` over the genuine `~15m` of expected service time, which nearly halves `C`. This is exactly the over-conservatism ADR-0002 corrected for the drain phase, now visible on the provisioning phase.

### The crux: the residual conservatism is not neutral

`C` appears in `ThroughputBelowArrival` (`C·A < N·P`), `ThroughputBurstShortfall` (`N > K·C`) and, via the shared denominator, `RotationSpansNextWindow` (`denom > gap`). A **smaller** `C` makes all three fire more readily — annoying, but safe. Correcting the inflation reverses that: every minute removed from the denominator raises `C`, and a `C` that over-states capacity makes all three findings go **silent in exactly the situations they exist to catch**. That is a worse failure than the one being fixed. Whatever prior replaces `readyTimeout` must therefore err **high**, and an operator whose cluster provisions slower than the prior assumes must be able to raise it.

## Decision

Drop `buffer` and `readyTimeout` from the forecast and model provisioning with its own expected-duration term, shaped exactly like `drainEstimate`.

```
t_rot_est = provisioningEstimate + drainEstimate
```

- `t_rot = readyTimeout + tGP + buffer` stays the **deadline bound**, unchanged. `buffer` and the `readyTimeout`/`tGP` deadline terms remain here, where slack and deadlines belong; they feed `leadTime`, `A`, `G`, the [§3.3](../../specification/03-design.md#33-surge-sequence-v1) forceful-fallback deadline race and the [§5.2](../../specification/05-implementation.md#52-reconcile-loop) `drain_stuck` bound.
- `surge.provisioningEstimate` is a new optional `RotationPolicy` field. Unset, the forecast falls back to `min(readyTimeout, 5m)`, silently. The default is bounded above by the timeout it estimates, so it can never claim a provision takes longer than the attempt is allowed to run. An explicit value above `readyTimeout` is unreachable (the surge is abandoned at `readyTimeout`), so it emits a `ProvisioningEstimateAboveReadyTimeout` `Warn` and is clamped. `readyTimeout` is always resolved to a positive value upstream, so — unlike `drainEstimate`'s `tGP` — there is no unset-deadline fork.

`ProvisioningEstimateDefault = 5m` errs high against the observed `1–3m` while staying well under the `15m` `readyTimeout` default, per the direction-of-error argument above.

### Why a field rather than a fixed prior

The same reasoning ADR-0002 used for `drainEstimate`: a cluster whose provisioning is genuinely slow (large images, cold AMIs, constrained capacity) must be able to raise the estimate rather than discover a silent finding. The "two overlapping margin knobs" objection [#215](https://github.com/AkashiSN/node-rotation-controller/issues/215) raised against a *tunable* `buffer` does not apply here: this term estimates a **duration**, it does not add slack.

### The containment boundary

`provisioningEstimate` influences **no** rotation timing, **no** candidate selection, **no** start gate, **no** drain bound, and **no** `Fatal` finding — identical to `drainEstimate`'s boundary in ADR-0002. Everything that can gate a NodePool out of rotating, or make a node race its own Forceful Expiration, is on the deadline side and keeps using `readyTimeout` and `tGP`. The only thing a wrong `provisioningEstimate` can do is make a forecast-side `Warn` too loud or too quiet.

This is pinned by `TestDeriveProvisioningEstimateContainment` in `internal/schedule/schedule_test.go`, mirroring `TestDeriveDrainEstimateContainment`: holding every other input fixed and sweeping `provisioningEstimate` across `nil`, `1m`, `5m`, `15m`, and `20m` (clamped), it asserts `TRot`, `A`, `G`, and the **entire layer-1 finding set** are invariant, while only `C`, `TRotEst`, `ProvisioningEstimate`, and the forecast-side findings may move. It forces a non-empty layer-1 set (`RetryBackoffShort`) first, so the invariance is not vacuous.

## Consequences

**Positive**

- The forecast now sums only genuine expected phase durations — provisioning and drain — with no deadline term and no slack. On the [§3.2](../../specification/03-design.md#32-candidate-selection) worked schedule `t_rot_est` goes `27m → 15m`, `C` goes `7 → 10` and `K·C` goes `14 → 20`, with `A` and `G` unchanged.
- The over-conservatism ADR-0002 fixed for the drain phase is now also removed from the provisioning phase, so `C` no longer under-counts by a `readyTimeout`-sized margin.

**Negative / trade-offs**

- Upgrading changes **which layer-2 warnings appear**, with **no** change in rotation behavior, candidate selection, or any deadline — the same non-additive class as ADR-0002. Operators who tuned alerting to the old (louder) warnings will see them quiet further.
- A `provisioningEstimate` set too low silences a carry-over or throughput warning that a genuinely slow provision would have justified. `5m` is a prior, not a measurement. The signal for the *real* event — a provision actually failing to reach Ready — is the surge timing out at `readyTimeout` and rolling back, which is unaffected by `provisioningEstimate`.

**Scope**

- This supersedes only the `t_rot_est` **formula** from ADR-0002. The deadline/forecast split, the containment boundary, and the "never fit a deadline to observation" argument all stand. `drainEstimate` is unchanged.
