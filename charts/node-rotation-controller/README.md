# node-rotation-controller (Helm chart)

Proactively rotates Karpenter-managed nodes **make-before-break (surge)** within
a maintenance window, before Karpenter's forceful `expireAfter` fires. Targets
EKS Auto Mode and any Karpenter v1+ environment where node expiration is forceful
and Disruption Budgets do not apply.

This chart deploys the controller, its RBAC, the cluster-scoped `RotationPolicy`
CRD, the surge placeholder `PriorityClass`, and (optionally) Prometheus Operator
integration. For the design, read the
[specification](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/specification/);
for day-2 operations, the
[runbook](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/runbook.md).

## Prerequisites

- **Karpenter v1+** already installed. This chart does **not** install Karpenter
  or its CRDs — it only operates the `NodeClaim`/`NodePool` resources Karpenter
  owns. A startup preflight fails fast if `karpenter.sh/v1` (`nodeclaims`/
  `nodepools`) is not served or not readable (spec §5.1).
- **Kubernetes >= 1.29** (`karpenter.sh/v1` requires a recent control plane).
- **Helm 3.8+** for OCI registry support.
- (Optional) the Prometheus Operator (`monitoring.coreos.com/v1` CRDs) if you
  enable the `ServiceMonitor` or `PrometheusRule`.

## Installing

Install the published chart from the GitHub Container Registry (OCI):

```sh
helm install node-rotation-controller \
  oci://ghcr.io/akashisn/charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  --set-json 'rotationPolicies=[{"spec":{"nodePoolSelector":{"matchLabels":{"workload":"api"}},"maintenanceWindows":[{"timezone":"Asia/Tokyo","days":["Wed","Sat"],"start":"02:00","end":"06:00"}]}}]'
```

> The commands above install the latest published chart. Pin a release with
> `--version X.Y.Z` — the chart version has **no** leading `v` (the release guard
> strips it from the `vX.Y.Z` git tag). Pre-1.0, the chart `version` and
> `appVersion` move together (spec §6.1), and the values surface may change
> between minor releases: `rotationPolicies` as shown here needs chart 0.5.0+.

Or install from a local checkout of this repository:

```sh
helm install node-rotation-controller charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  --set-json 'rotationPolicies=[{"spec":{"nodePoolSelector":{"matchLabels":{"workload":"api"}},"maintenanceWindows":[{"timezone":"Asia/Tokyo","days":["Wed","Sat"],"start":"02:00","end":"06:00"}]}}]'
```

After install, `NOTES.txt` prints the next steps and the commands to confirm the
controller is healthy and which replica holds the leader Lease.

## What the chart installs

| Resource | Purpose | Toggle |
|----------|---------|--------|
| `Deployment` | The controller (`replicaCount=2` with leader election) | — |
| `ServiceAccount` + `ClusterRole`/`ClusterRoleBinding` + namespaced `Role`/`RoleBinding` | RBAC for the controller (spec §4.3) | `serviceAccount.create` |
| `RotationPolicy` CRD | Cluster-scoped policy schema (`noderotation.io/v1alpha1`), from the chart's `crds/` directory | always |
| `RotationPolicy` (one per `rotationPolicies` entry) | Governing policies you point at your NodePools | `rotationPolicies` |
| `PriorityClass` | Dedicated negative-priority class for the surge placeholder Pod (spec §3.3) | `placeholder.priorityClass.create` |
| `PodDisruptionBudget` | Keeps a leader replica alive during a node drain (R1, spec §7.1) | `podDisruptionBudget.enabled` |
| `Service` (`/metrics`) | Exposes the controller metrics (spec §4.2) | `metrics.service.enabled` |
| `ServiceMonitor` | Prometheus Operator scrape config | `metrics.serviceMonitor.enabled` |
| `PrometheusRule` | The seven §4.2 alerts | `prometheusRule.enabled` |

> **CRDs are installed from the chart's `crds/` directory.** Per Helm's CRD
> handling, they are created on first install but **not** upgraded or deleted by
> Helm. To update the CRD on a chart upgrade, apply it manually:
> `kubectl apply -f charts/node-rotation-controller/crds/`. `helm uninstall`
> leaves the CRD (and any `RotationPolicy` objects) in place.

## Configuring rotation (`RotationPolicy`)

Rotation policy lives in **cluster-scoped `RotationPolicy` objects** (spec §5.4),
not in chart values directly — the controller resolves each NodePool's governing
policy at reconcile time. The chart renders one object per `rotationPolicies`
entry, and the default values ship a single-entry sample. An entry's `spec` is
rendered verbatim and the values schema validates it before anything reaches the
cluster.

> **`rotationPolicies` is a list, and Helm replaces list values whole.** A
> `--set-json 'rotationPolicies[0].spec.x=y'` does not merge into the default
> sample — it replaces the entry, dropping the rest of the spec. Pass the entry
> entire (as the install commands above do) or use `-f values.yaml`. The schema
> catches a partial entry at render time rather than installing a half-policy.

A complete `RotationPolicy`:

```yaml
apiVersion: noderotation.io/v1alpha1
kind: RotationPolicy
metadata:
  name: api               # cluster-scoped (no namespace)
spec:
  # NodePools this policy governs, matched by label (spec §3.2, §5.4).
  # A NodePool matched by no RotationPolicy is not rotated (its expireAfter
  # backstop still applies). An equal-specificity tie between policies is a
  # hard error.
  nodePoolSelector:
    matchLabels:
      workload: api

  # Age at which a node becomes a rotation candidate. "auto" derives it from the
  # window cadence so K chances stay below expireAfter (spec §3.2). An explicit
  # Go duration is allowed but still validated (recomputed G < 1 is fatal,
  # G < K warns).
  ageThreshold: auto
  # K: desired rotation chances per node before expiry. Floor 1; values < 2 warn.
  minRotationChances: 2

  # The effective window is the UNION of all entries (spec §3.1).
  maintenanceWindows:
    - timezone: Asia/Tokyo
      days: [Wed, Sat]
      start: "02:00"
      end: "06:00"

  surge:
    maxUnavailable: 1       # v1 fixed at 1 (serial); > 1 reserved for later
    readyTimeout: 15m       # surge node must reach Ready within this, else fail
    cooldownAfter: 10m      # settle pause between rotations; also post-failure pause
    retryBackoff: 30m       # base wait before re-selecting a failed NodeClaim;
                            # doubles per consecutive failure, capped at 8x
    # Which candidate-node requirements the placeholder replicates (spec §3.3).
    # The karpenter.sh/nodepool selector is always applied and is NOT listed.
    matchNodeRequirements:
      required:
        - topology.kubernetes.io/zone
        - kubernetes.io/arch
        - karpenter.sh/capacity-type
      preferred: []
    # Opt-in, default off (spec §3.3, ADR-0001). A candidate that cannot finish a
    # graceful surge before its own expireAfter deadline is rotated surge-less
    # inside the window — still via the voluntary, PDB-respecting path — rather
    # than left to Karpenter's forceful expiration at an uncontrolled time.
    forcefulFallback:
      enabled: false

  prePull:
    enabled: false          # v2 (disabled in v1)
```

> Enabling `surge.forcefulFallback` trades a make-before-break guarantee for
> control over *when* the disruption happens. Watch
> `noderotation_forceful_fallback_total`: a rising count means the graceful surge
> keeps losing the race to node deadlines, which the
> [runbook](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/runbook.md)
> tells you how to remediate.

### Multiple policies (per-NodePool)

When different NodePools need different rotation behavior — a different window,
`ageThreshold`, or surge per pool — add an entry per policy and the chart renders
one `RotationPolicy` each (no need to install the chart, and the controller
Deployment, more than once). Each entry is `{name, spec, [create]}` and its `spec`
is the shape shown above. These objects are cluster-scoped, so `name` must be
unique; the single-entry default omits it and falls back to the chart fullname,
which a multi-entry list cannot do.

```yaml
rotationPolicies:
  - name: api
    spec:
      nodePoolSelector:
        matchLabels: { workload: api }
      maintenanceWindows:
        - { timezone: Asia/Tokyo, days: [Sat], start: "02:00", end: "06:00" }
  - name: batch
    spec:
      nodePoolSelector:
        matchLabels: { workload: batch }
      ageThreshold: 168h
      maintenanceWindows:
        - { timezone: Asia/Tokyo, days: [Sun], start: "03:00", end: "05:00" }
```

Selector overlap needs no chart-side guard: the controller resolves a single
governing policy per NodePool by selector specificity and treats an
equal-specificity tie as a hard `PolicyConflict` (spec §5.4).

To manage your own policies outside the chart instead, set `rotationPolicies: []`
and `kubectl apply` one `RotationPolicy` per
divergent policy. The full schema, conflict resolution, and the `status`
subresource are documented in
[specification §5.4](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/specification/05-implementation.md#54-configuration-schema).

## Metrics and alerts

The controller serves Prometheus metrics on `:8080/metrics` (spec §4.2). With the
Prometheus Operator installed you can let the chart wire scraping and alerting:

```sh
helm upgrade --install node-rotation-controller \
  oci://ghcr.io/akashisn/charts/node-rotation-controller \
  --namespace node-rotation-system \
  --set metrics.serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

The `PrometheusRule` ships seven alerts. Several depend on your window cadence —
tune `prometheusRule.candidatesNotDraining.windowRange` to roughly two window
periods (2·P) and `prometheusRule.stalledInWindow.completionRange` to roughly one
window's duration, and adjust each alert's `for`/`severity` as needed. See the
[runbook](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/runbook.md)
for how to read each metric and respond.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `2` | Controller replicas. v1 runs 2 with leader election (1 active) so a drain/crash never leaves the cluster un-reconciled (spec §5.1). |
| `image.repository` | `ghcr.io/akashisn/node-rotation-controller` | Controller image repository. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `image.tag` | `""` | Image tag. Defaults to the chart `appVersion` when empty. |
| `imagePullSecrets` | `[]` | Image pull secrets for the controller image. |
| `nameOverride` | `""` | Override the chart name portion of resource names. |
| `fullnameOverride` | `""` | Override the full name of chart resources. |
| `serviceAccount.create` | `true` | Create a ServiceAccount for the controller. |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations (e.g. an IRSA role ARN — note v1 needs no cloud IAM, all node ops route through Karpenter, spec §4.3). |
| `serviceAccount.name` | `""` | ServiceAccount name. Generated from the fullname when empty. |
| `podAnnotations` | `{}` | Extra annotations for the controller Pod. |
| `podLabels` | `{}` | Extra labels for the controller Pod. |
| `podSecurityContext` | see `values.yaml` | Pod-level security context (nonroot, RuntimeDefault seccomp). |
| `securityContext` | see `values.yaml` | Container-level security context (no privilege escalation, read-only rootfs, drop ALL caps). |
| `resources` | `50m`/`64Mi` req, `200m`/`128Mi` lim | Controller resource requests/limits. The controller is light; surge nodes are the real cost (spec §4.4). |
| `nodeSelector` | `{}` | Node selector for the controller Pod. |
| `tolerations` | `[]` | Tolerations for the controller Pod. |
| `affinity` | `{}` | Affinity for the controller Pod. |
| `topologySpreadConstraints` | `[]` | Topology spread constraints for the controller Pod. |
| `metrics.service.enabled` | `true` | Create a ClusterIP Service exposing `/metrics` (spec §4.2). |
| `metrics.service.type` | `ClusterIP` | The metrics Service type. |
| `metrics.service.port` | `8080` | The metrics Service port. |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor. Requires the `monitoring.coreos.com/v1` CRD. |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval. |
| `metrics.serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout. |
| `metrics.serviceMonitor.labels` | `{}` | Extra labels for the ServiceMonitor (e.g. a Prometheus release selector). |
| `prometheusRule.enabled` | `false` | Create a PrometheusRule with the seven §4.2 alerts. Requires the `monitoring.coreos.com/v1` CRD. |
| `prometheusRule.labels` | `{}` | Extra labels for the PrometheusRule. |
| `prometheusRule.<alert>.for` / `.severity` | per alert | Per-alert `for` duration and severity. Alerts: `completedFailure`, `candidatesNotDraining`, `stalledInWindow`, `drainStuck`, `shortLeadNodes`, `retryCountHigh`, `forcefulFallback` (ships `severity: info` — a single fallback is by design per ADR-0001; tighten it for your environment). |
| `prometheusRule.candidatesNotDraining.windowRange` | `8d` | `min_over_time` range; set to ~2·P for your schedule. |
| `prometheusRule.stalledInWindow.completionRange` | `4h` | Range covering ~one window's duration. |
| `podDisruptionBudget.enabled` | `true` | Create a PDB for the controller so a node drain cannot take out both replicas at once (R1, spec §7.1). |
| `podDisruptionBudget.minAvailable` | `1` | `minAvailable` for the controller PDB. |
| `placeholder.image` | `registry.k8s.io/pause:3.10` | The pause image the surge placeholder Pod runs (spec §3.3). |
| `placeholder.priorityClass.create` | `true` | Install the dedicated negative-priority PriorityClass. The controller does NOT create it (spec §4.3). |
| `placeholder.priorityClass.name` | `noderotation-placeholder` | PriorityClass name. Must match the controller's `--priority-class` flag. |
| `placeholder.priorityClass.value` | `-10` | Priority value. Negative so the placeholder is the deliberate preemption victim (spec §3.3). |
| `logging.development` | `false` | Development-mode (human-readable) logging. Production uses JSON. |
| `rotationPolicies` | a one-entry sample; see `values.yaml` | List of RotationPolicy objects, one per entry (`{name, spec, [create]}`), for per-NodePool differentiation (see [Multiple policies](#multiple-policies-per-nodepool)). Set to `[]` to author your own out-of-band. |
| `rotationPolicies[].name` | chart fullname | RotationPolicy object name. Cluster-scoped, so it must be unique across entries; omit it to fall back to the chart fullname. |
| `rotationPolicies[].create` | `true` | Set `false` to skip just that entry. |
| `rotationPolicies[].spec` | see `values.yaml` | The RotationPolicy spec, rendered verbatim (see [Configuring rotation](#configuring-rotation-rotationpolicy)). |

The chart ships a [`values.schema.json`](values.schema.json) that validates
values at install time as a fast first line of defense; the CRD and controller
remain the source of truth and re-validate (spec §5.4).

## Multiple releases in one cluster

v1 assumes a **single install per cluster**. The `PriorityClass` and the sample
`RotationPolicy` are **cluster-scoped objects with fixed names**, so a second
release would collide on them. To run an additional release, set
`placeholder.priorityClass.create=false` (share the one PriorityClass) and
`rotationPolicies: []` (so the release renders no `RotationPolicy` objects —
manage them on the primary release or out-of-band).

## Upgrading

Pre-1.0, treat every upgrade as potentially breaking and read the release notes.

- **CRD updates** are not applied automatically by Helm (see the CRD note above).
  Apply `crds/` **before** `helm upgrade` when the CRD changed, so a policy using
  a new field is never rejected at admission. Which releases changed it is
  recorded in the runbook's
  [per-release CRD table](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/runbook.md#8-upgrading-and-rolling-back-the-controller).

## Uninstalling

```sh
helm uninstall node-rotation-controller --namespace node-rotation-system
```

This removes the controller and its namespaced resources. It does **not** remove
the `RotationPolicy` CRD or any `RotationPolicy` objects — delete those manually
if you want them gone:

```sh
kubectl delete rotationpolicies --all
kubectl delete crd rotationpolicies.noderotation.io
```

## Links

- [Specification](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/specification/)
  ([日本語](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/ja/specification/))
- [Production runbook](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/runbook.md)
  ([日本語](https://github.com/AkashiSN/node-rotation-controller/blob/main/docs/ja/runbook.md))
- [Project README](https://github.com/AkashiSN/node-rotation-controller/blob/main/README.md)
