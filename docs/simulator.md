---
layout: page
title: Policy simulator
---

<div class="policy-simulator">
<div class="vp-doc">

# Policy simulator

**Check your rotation schedule before deploying.** Enter your `RotationPolicy` configuration and fleet size, and instantly see which day each node gets rotated — and whether every node makes it before its `expireAfter` backstop fires.

## When to use

- Before choosing a maintenance window — test whether the window is wide enough for your fleet
- When adjusting `minRotationChances`, `cooldownAfter`, or `expireAfter` — see the effect immediately
- To understand why `ThroughputBurstShortfall` or `ThroughputBelowArrival` warnings fire
- To visualize how a synchronized batch (all nodes the same age) interacts with your schedule

## How to read the result

- **Green nodes** complete their rotation before their `expireAfter` deadline — the graceful path works.
- **Orange nodes** are rotated via the surge-less forceful fallback (if enabled) — still inside the window, still PDB-respecting, but without make-before-break.
- **Red nodes** reach their `expireAfter` deadline before the controller can rotate them — they fall back to Karpenter's native forceful expiration.

A healthy configuration should show all green. Orange means throughput is tight but controlled. Red means the schedule needs widening.

::: warning Scope
The simulator models rotation starts and completions, including forceful fallback. It does **not** model failures (surge timeouts, `retryBackoff`, `failurePause`). The result is a best-case projection, not a production guarantee.
:::

::: details How it works (technical)
This page runs the controller's **own** Go code — the `ageThreshold` derivation, candidate-selection predicate, and start gates — compiled to WebAssembly. The simulator and the controller share one implementation and cannot drift apart (a CI check guards this).
:::

</div>

<PolicySimulator />

</div>
