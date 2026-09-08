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
