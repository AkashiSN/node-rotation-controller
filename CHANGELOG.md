# Changelog

Released versions, newest first. The project follows semantic versioning and is
**pre-1.0**: the `RotationPolicy` schema, Prometheus metric names, and annotation
keys may change between minor releases (spec
[§6.1](docs/specification/06-release.md#61-versioning-and-release)).

Every entry states the **upgrade action** an operator must take. Helm does not
upgrade CRDs, so a release that changes the `RotationPolicy` schema needs
`kubectl apply -f charts/node-rotation-controller/crds/` before `helm upgrade`.
The [runbook](docs/runbook.md#8-upgrading-and-rolling-back) has the full upgrade
and rollback procedure.

## v0.7.0 — 2026-09-08

- **Upgrade action:** apply `crds/` first — `surge.wholeNodeReservation` was
  added. Without it the old structural schema prunes the field silently, and a
  policy that asks for the mode runs with it off.
- **Behavioral change:** a NodePool with `spec.replicas` set (a static NodePool)
  no longer starts rotations. Karpenter's provisioner does not consider such a
  pool for a pending Pod, so the surge placeholder could neither induce a
  NodeClaim nor be absorbed: every attempt stalled to `readyTimeout` and spent
  one of that node's guaranteed rotation chances. The controller now refuses at
  the start gate and says so once per pool (spec §3.3, §5.2). A rotation already
  in flight is unaffected and runs to completion. Rotating static pools is
  **rejected, not deferred** — the reasoning is in §3.3.
- **Behavioral change:** the re-selection backoff after a failed attempt is
  clamped to the maintenance-window occurrence the failure happened in (spec
  §3.2). Past the window's remaining time every escalation step means the same
  thing — "skip the rest of this occurrence" — so a pool that failed twice early
  could spend the rest of its window with no eligible candidate. Retries are now
  more frequent within a window: expect `noderotation_completed_total{outcome="failure"}`
  and `noderotation_retry_count` to rise faster, and `NodeRotationRetryCountHigh`
  to fire sooner, for the same underlying fault.
- **Alerting change:** `NodeRotationStalledInWindow` fires in strictly more
  situations. Its suppression arm is now restricted to `outcome="success"`, so a
  `readyTimeout` rollback no longer counts as progress — previously a window
  spent entirely on failed attempts silenced the alert. It also gains an
  `in_backoff` arm (a pool whose every candidate is inside its backoff reports
  zero candidates) and a freeze exclusion. Review the alert's `for` and severity
  against your own noise budget before upgrading.
- **Values schema addition:** `prometheusRule.windowMissed.{range,for,severity}`
  for the new `NodeRotationWindowMissed` alert. Defaults ship; no action needed.
- **New NodePool annotation:** `noderotation.io/window-opened-at`, stamped when
  the controller first observes a window open and cleared when it closes. It is
  written on **every** governed NodePool, including pools with nothing to
  rotate — so a pool that was previously never written to now sees two
  annotation writes per window occurrence. GitOps drift detection on NodePool
  objects will see them.
- A maintenance window that closes with candidates outstanding and no rotation
  attributable to the occurrence is now reported: the
  `noderotation_window_missed_total` counter, a `WindowMissed` Warning Event,
  and the `NodeRotationWindowMissed` alert (spec §4.2). `noderotation_in_backoff`
  exports the other half of that verdict live, so an in-window alert can test the
  same outstanding-work count the counter judges by.
- **Opt-in whole-node surge reservation** (`surge.wholeNodeReservation`; spec
  §3.3, ADR-0005, default off) sizes the placeholder to a whole node's worth of
  cpu and memory, so a host whose free capacity in those dimensions is short of a
  node's cannot absorb it. It raises the bar; it does not guarantee an empty
  host, and it costs an extra instance on every rotation whose reservation is not
  absorbed plus a `surge_headroom` gate that tests a whole-node footprint.
  **Its coverage is unit and controller-level tests only** — the mode is not
  exercised by the KWOK e2e suite and has not been run on real cloud
  infrastructure. Enable it deliberately.
- The "surge node ready" and "rotation complete" lines, and the completion Event,
  name which of the two §3.3 paths reserved the capacity, carried on the anchor
  as `noderotation.io/surge-path`. On the absorb path the reservation is
  aggregate capacity on a host already running other Pods, so a short
  `surge_wait` does not bound the time until the evicted Pods are running.
- A blocked `surge_headroom` gate now names the resource, the request, the
  remaining budget and the configured limit, instead of reporting only that the
  surge could not proceed (spec §5.2 step 3).
- Completion counters and the startup sweep's announcements are emitted only by
  the pass whose write actually landed (spec §5.2, claim-then-announce), so a
  reconcile working from a cache-lagged read can no longer double-count a
  completion or announce work it did not do.
- Dependencies: Go 1.27.1, the Kubernetes 1.37 client libraries,
  controller-runtime 0.25 and Karpenter 1.14.1. The compatibility contract is
  unchanged — the stable `karpenter.sh/v1` CRD surface, not a Karpenter
  controller minor.

## v0.6.1 — 2026-07-15

- **Upgrade action:** none.
- Chart: the controller Deployment accepts an optional top-level
  `priorityClassName`, so on a shared pool the component that rotates the pool
  need not run at the default priority. This is the controller's own priority,
  unrelated to the surge placeholder's negative-priority class (spec §3.3).
- No controller behavior, CRD schema, annotation, or metric changed.

## v0.6.0 — 2026-07-14

- **Upgrade action:** apply `crds/` first — `surge.failurePause`,
  `surge.drainEstimate`, and `surge.provisioningEstimate` were added.
- **Behavioral change:** `cooldownAfter` no longer doubles as the post-failure
  pause. That is now `surge.failurePause`, defaulting to
  `max(10m, cooldownAfter)` (ADR-0004). An install that had lowered
  `cooldownAfter` below `10m` sees the failure pause go back up on upgrade; set
  `failurePause` explicitly to keep the old value.
- **Values schema change:** the chart seals the `rotationPolicies[].spec`
  subtree, so a typo that was silently dropped before now fails the upgrade.
  Dry-run with
  `helm template node-rotation-controller charts/node-rotation-controller -f your-values.yaml >/dev/null`
  before upgrading.
- The layer-2 throughput forecast models a rotation from what a healthy rotation
  costs — `provisioningEstimate + drainEstimate + cooldownAfter` — instead of
  deriving it from the force-kill deadline (ADR-0003). Its inputs are exported
  as metrics rather than logged once at startup.
- The documentation site hosts a browser
  [policy simulator](https://akashisn.info/node-rotation-controller/simulator):
  the controller's own schedule and selection code compiled to WebAssembly and
  guarded in CI against drift.

## v0.5.0 — 2026-07-09

- **Upgrade action:** apply `crds/` first — `surge.forcefulFallback` was added.
- Opt-in, window-bounded, surge-less forceful fallback (spec §3.6, ADR-0001),
  default off.
- Candidates are ordered by earliest deadline.
- A node carrying an operator-owned `karpenter.sh/do-not-disrupt` annotation is
  excluded from selection.
- `ThroughputBurstShortfall` finding; the documentation site.

## v0.4.0 — 2026-06-25

- **Upgrade action:** none.
- The chart renders one `RotationPolicy` per `rotationPolicies` entry, so each
  NodePool can carry its own window, `ageThreshold`, and surge settings.

## v0.3.0 — 2026-06-25

- **Upgrade action:** first install — introduces the `RotationPolicy` CRD
  (spec §5.4).
- The v1 surge MVP: reconcile loop, surge placeholder, drain via Karpenter's
  voluntary path, metrics, and the Helm chart.

## Before v0.3.0

Pre-release scaffolding, published only as git tags:

- **v0.2** — project layout, controller-runtime bootstrap, leader election, CI.
- **v0.1** — the specification itself; no published artifacts.
