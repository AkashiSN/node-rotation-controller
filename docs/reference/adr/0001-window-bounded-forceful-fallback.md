# 1. Window-bounded forceful fallback (relax the surge-only invariant)

- Status: Accepted
- Date: 2026-06-30
- Issue: [#156](https://github.com/AkashiSN/node-rotation-controller/issues/156)

## Context

The controller's core promise (G1) is to **prevent Forceful Expiration from firing in practice** by replacing nodes gracefully, in a maintenance window, before each node's `expireAfter` deadline. Two architectural invariants encode *how*: the controller **never bypasses Karpenter** (it deletes the old `NodeClaim` and lets the termination controller drain via the voluntary path, where PDBs apply — G4), and **v1 is surge-only** (every replacement is a node-level make-before-break: a placeholder Pod induces replacement capacity that must be `Ready` before the old node is drained).

The §3.2 derivation guarantees `K` rotation chances **per node** and is independent of the node count `N`: as long as `A = E − (K·P + t_rot) > 0`, a single node always sees `K` windows before its deadline. `N` enters only through the layer-2 throughput check, which compares window capacity against a **steady-state average** arrival rate (`C·A ≥ N·P`, with `C = m·floor(D / (t_rot + cooldown))` and `m = surge.maxUnavailable = 1` in v1). That model assumes node ages are uniformly distributed. Real clusters create nodes in **synchronized batches** (initial bring-up, scale-up, NodePool migration, post-consolidation re-packing), which violates the assumption: `N` nodes sharing one `creationTimestamp` share one deadline and contend for the same windows. A synchronized batch completes before its common deadline only when `K·C ≥ N`; if `N > K·C`, the surplus nodes miss every window and hit **Forceful Expiration at an uncontrolled time** — defeating G1. Layer 2 does not detect this; a batch can pass the average and still overflow (issue #156, edge cases E2/E3).

Once `C·A < N·P` (capacity below demand), purely-graceful rotation is **already impossible** — the only remaining choice is between a *controlled* forceful disruption (inside the maintenance window, at a time the controller picks) and an *uncontrolled* one (at the random per-node deadline, possibly during peak hours). The controller cannot move the deadline: `NodeClaim.spec` is immutable (`self == oldSelf`, `sigs.k8s.io/karpenter@v1.13.0` `pkg/apis/v1/nodeclaim.go:185`), so `expireAfter` cannot be retimed after creation and the backstop's firing time is fixed. The only lever the controller has over a node is replacement — deleting its `NodeClaim`.

## Decision

Introduce an **opt-in, window-bounded forceful fallback**. When a candidate cannot complete a graceful surge before its own deadline (it is in the last window before the deadline, or the backlog will not clear it in time) **and** the governing `RotationPolicy` has opted in, the controller deletes the `NodeClaim` **inside the maintenance window without the make-before-break surge** (break-before-make).

This **relaxes only the "surge-only" invariant**, and only on the opt-in fallback path. It does **not** relax the other invariants: the drain still follows the voluntary path through Karpenter's termination controller, so **PDBs are respected up to `terminationGracePeriod`** — "never bypasses Karpenter" and G4 ("compose with PDB") both hold. Bypassing the Eviction API to force past a blocking PDB ("3b-2") is explicitly **out of scope**.

The fallback is **disabled by default** (working name `surge.forcefulFallback.enabled`, default `false`). With it disabled, behavior is exactly today's: graceful surge only, and surplus nodes degrade to the native `expireAfter` baseline. This change is **targeted for the v1.0 release** (#156). Because the fallback is opt-in and off by default, surge-only remains v1's *default* behavior; accepting this ADR therefore amends the surge-only invariant in `CLAUDE.md` and `docs/specification.md` to read "surge-only **by default**, with an opt-in window-bounded forceful fallback", and that wording change lands in the spec-sync PR that flips this ADR to `Accepted` (see Follow-up) rather than deferring the feature to a later v1.x.

## Consequences

**Positive**

- The forceful disruption that was otherwise inevitable (`C·A < N·P`) happens at a **controlled time inside the maintenance window** instead of at the random `expireAfter` deadline, restoring the spirit of G1 (predictable, low-traffic disruption) even when a graceful guarantee is unreachable.
- It **relieves the capacity deficit**: dropping the surge removes `readyTimeout` and the provisioning wait from `t_rot` (it collapses to roughly `tGP + Buffer`) and removes the surge-capacity constraint, so `C` rises sharply (and, combined with the reserved surge-parallelism work, the surge-less deletes could later run concurrently) — making `K·C ≥ N` achievable where a serial graceful surge cannot.
- The "never bypass Karpenter" and PDB-respect (G4) invariants are **preserved**; the change is narrowly scoped to one of the four invariants and to an opt-in path.

**Negative / trade-offs**

- **Pod-level make-before-break is lost on the fallback path.** Without a pre-staged landing zone, evicted Pods reschedule after termination; workloads with a strict PDB still stay within budget (the voluntary drain blocks on the Eviction API up to `tGP`), but PDB-less or loosely-budgeted workloads see `readyReplicas` dip. This is acceptable because the fallback triggers **only when a graceful guarantee is already impossible** and the operator has explicitly opted in for that policy.
- **A second rotation mode** adds state-machine and observability surface: the controller must distinguish graceful from forceful-fallback rotations (metrics/events) and likely needs a new transient state for the surge-less path. More code and test surface.
- The opt-in flag is a new public configuration knob on `RotationPolicy` that must be validated and documented.

**Alternatives rejected** (detailed in #156)

- **Retime `expireAfter` to land the backstop in-window ("3a").** Impossible: `NodeClaim.spec` is immutable.
- **Dynamic graceful `maxUnavailable` (`m > 1`).** Helps only PDB-loose workloads; tight PDBs throttle the drain regardless of `m`, surge nodes sit idle, and transient surge cost multiplies. Reserved.
- **Bypass PDB in-window ("3b-2").** Breaks G4; turns the controller into a forceful disruptor.
- **Do nothing; operator widens windows or lowers `N`.** Remains the first recommendation, but cannot help when capacity is genuinely insufficient and cannot move the uncontrolled forceful into the window.

## Follow-up

This spec-sync PR synchronizes the canonical surface — the spec (§3.2 layer-2 burst condition, §3.3 surge-less path, §3.5 backstop semantics) and its `docs/ja/` translation, plus the surge-only invariant wording in `CLAUDE.md` and `docs/specification.md` — and flips this ADR to `Accepted` in the same PR, so the recorded decision and the canonical spec do not diverge. The `RotationPolicy` CRD schema, the controller behavior, and metrics follow in the implementation PRs tracked under #156; the §5.4 field is documented as **planned** until that lands. This work is targeted for the **v1.0** milestone. Candidate ordering under heterogeneous `expireAfter` (edge case E4) is handled separately in #157.
