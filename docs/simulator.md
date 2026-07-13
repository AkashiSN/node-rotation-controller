---
layout: page
title: Policy simulator
---

<!-- layout: page renders full-bleed with no gutters; this wrapper is the page's
     .policy-simulator CSS scope and carries the page padding (see custom.css). -->
<div class="policy-simulator">

<!-- layout: page also means VitePress does NOT wrap this markdown in .vp-doc — the class
     that carries the heading sizes and paragraph rhythm. Scope the prose with it by hand,
     and keep <PolicySimulator /> OUTSIDE that scope: vp-doc restyles tables and inputs, and
     the component brings its own. -->
<div class="vp-doc">

# Policy simulator

Enter a `RotationPolicy` and a fleet, and see **which day each node gets rotated** —
and whether every node makes it before its `expireAfter` backstop fires.

This is not a re-implementation. The page runs the controller's **own** Go code —
the §3.2 derivation, the candidate-selection predicate and the start gates —
compiled to WebAssembly, so the simulator and the controller cannot drift.

::: warning Scope
This simulator models rotation start/completion including the window-bounded forceful
fallback. It does not model failures — a surge that times out, `retryBackoff`, or
`failurePause`. The result is not a production guarantee.
:::

</div>

<PolicySimulator />

</div>
