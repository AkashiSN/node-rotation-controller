# node-rotation-controller

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-v0.3_MVP_(pre--1.0)-blue.svg)](docs/specification.md)

A Kubernetes controller that proactively rotates Karpenter-managed nodes within a defined maintenance window, using **make-before-break (surge)** semantics, before Karpenter's forceful `expireAfter` triggers.

Designed for EKS Auto Mode and any Karpenter v1+ environment where node expiration is forceful and disruption budgets do not apply.

## Status

**v0.3 — v1 surge MVP implemented (pre-1.0).** The v1 make-before-break rotation state machine (spec §5.2), `ageThreshold`/candidate derivation (§3.2), surge placeholder (§3.3), metrics and Warning Events (§4.2), the Helm chart, and the Karpenter v1 startup preflight (§5.1) are implemented, with unit tests and an envtest smoke test in CI. This is **early validation, not yet production-ready** — EKS Auto Mode PoC runs have validated the core surge path, but edge cases and a full multi-hour tight-race soak remain open (see the [roadmap](docs/specification.md#62-roadmap) toward v1.0). [docs/specification.md](docs/specification.md) remains the source of truth for the design; see [Compatibility](#compatibility) for the Karpenter contract.

日本語版: [README.ja.md](README.ja.md) / [docs/ja/specification.md](docs/ja/specification.md)

## Why

Karpenter classifies node disruption into two categories:

| Category | Examples | NodePool Disruption Budgets | Pre-provisioned replacement |
|----------|----------|------------------------------|------------------------------|
| Graceful | Drift, Consolidation | Applied | Yes (make-before-break) |
| **Forceful** | **Expiration**, Spot Interruption | **Not applied** | **No** |

Expiration is intentionally forceful (see the upstream [forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md)) so that AMI patches and security updates cannot be indefinitely delayed by misconfigured budgets. The upstream design explicitly lists "operators implement their own graceful rotation" as one acceptable path. EKS Auto Mode further enforces a 21-day hard cap on node lifetime that cannot be lifted.

The practical consequence: nodes **will be force-drained** at some point within 21 days, regardless of PDBs, and Karpenter will only provision a replacement *after* the drain begins. This can land in peak business hours.

This controller closes that gap by:

1. Watching `NodeClaim` resources approaching expiration
2. Restricting rotation to a configurable **maintenance window** (e.g., Saturday 02:00–06:00)
3. Inducing a NodePool-owned replacement node first via a low-priority **placeholder Pod** (never a standalone `NodeClaim` — see the spec, §3.3), waiting until the reserved capacity is ready, then deleting the old `NodeClaim` (**surge**)
4. Letting Karpenter's standard termination controller graceful-drain the old node, where PDBs *do* apply

## What it is not

- **Not** a replacement for Karpenter Consolidation, Drift, or Disruption Budgets — it composes with them
- **Not** a Spot interruption handler (use [AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler))
- **Not** an OS-patch reboot tool (use [kured](https://github.com/kubereboot/kured))
- **Not** a pod descheduler (use [descheduler](https://github.com/kubernetes-sigs/descheduler))
- **Not** a replacement for application-side warm-up (`readinessProbe`, `readinessGate`, `slow_start`) — surge places nodes, applications must place themselves

## Compatibility

The compatibility contract is the **stable `karpenter.sh/v1` CRD surface — not a specific Karpenter controller minor.** This matters for **EKS Auto Mode**, which does not expose the managed Karpenter version to users: the controller runs against any cluster serving a compatible `karpenter.sh/v1` `NodePool`/`NodeClaim` API.

- **Runtime target:** EKS Auto Mode and any Karpenter v1+ cluster exposing a `karpenter.sh/v1`-compatible `NodePool`/`NodeClaim` API.
- **Build/test baseline:** the repository compiles and tests against the `sigs.k8s.io/karpenter` version pinned in [`go.mod`](go.mod). That pins the typed Go API it is built against — it is **not** a requirement that the cluster run that exact Karpenter minor.
- **No internals, no cloud APIs:** the controller interacts only through Kubernetes API objects (`NodeClaim`/`NodePool` CRDs, core `Node`/`Pod`); it never calls Karpenter controller internals or a cloud-provider API. Unknown Auto Mode internals are fine as long as the public `karpenter.sh/v1` surface is compatible.
- A startup preflight fails fast if `karpenter.sh/v1` (`nodeclaims`/`nodepools`) is not served or not readable.

See the [compatibility policy](docs/specification.md#21-scope-and-compatibility) for the full list of required CRD fields, labels, and annotations.

## Project layout

```
.
├── docs/
│   ├── specification.md       Full design specification (English)
│   ├── runbook.md             Production runbook (English)
│   ├── ja/specification.md    Japanese translation
│   └── ja/runbook.md          Production runbook (Japanese)
├── charts/                    Helm chart (node-rotation-controller)
├── examples/                  Ready-to-adapt RotationPolicy manifests
├── cmd/                       Controller entry point (manager bootstrap + startup preflight)
└── internal/                  Reconciler and supporting packages: rotation state machine
                               (controller), schedule/selection, surge placeholder, window,
                               policy, metrics, preflight
```

## Installation

> Requires Karpenter v1+ already installed. This chart does **not** install
> Karpenter or its CRDs — it only operates the `NodeClaim`/`NodePool` resources
> Karpenter owns.

Install the published chart from the GitHub Container Registry (OCI):

```sh
helm install node-rotation-controller \
  oci://ghcr.io/akashisn/charts/node-rotation-controller \
  --version 0.3.0 \
  --namespace node-rotation-system --create-namespace \
  --set-json 'rotationPolicy.spec.nodePoolSelector.matchLabels={"workload":"api"}'
```

Or install from a local checkout of this repository:

```sh
helm install node-rotation-controller charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  --set-json 'rotationPolicy.spec.nodePoolSelector.matchLabels={"workload":"api"}'
```

The chart installs the controller (`replicas=2` with leader election), its RBAC,
the cluster-scoped `RotationPolicy` CRD (from the chart's `crds/` directory) plus
a sample `RotationPolicy` object, and the dedicated negative-priority
`PriorityClass` for the surge placeholder Pod (spec §3.3, §4.3, §5.1). Configure
rotation by editing `rotationPolicy.spec` (the spec §5.4 schema) — see
[`charts/node-rotation-controller/values.yaml`](charts/node-rotation-controller/values.yaml).
Set `rotationPolicy.create=false` to author your own `RotationPolicy` objects
(one per divergent policy); a NodePool matched by none is simply not rotated. See
[`examples/`](examples/) for ready-to-adapt policies — a single catch-all,
divergent per-NodePool policies, specificity resolution, and maintenance-window
composition.

> **Maintainer note (first release only):** the ghcr.io image and chart
> packages may be created **private** on first publish. Make
> `node-rotation-controller` and `charts/node-rotation-controller` public in the
> GitHub *Packages* settings so unauthenticated `helm install` / image pulls
> work, then **verify** with a logged-out client — e.g.
> `helm pull oci://ghcr.io/akashisn/charts/node-rotation-controller --version <X.Y.Z>`
> (the chart version has **no** leading `v` — the release guard strips it),
> or fetch the image manifest anonymously and expect HTTP 200. (Querying or
> changing package visibility via the GitHub API needs a token with
> `read:packages` / `write:packages`; the *Packages* settings UI needs no token.)
> Releases are cut by pushing a `vX.Y.Z` tag (see the Release workflow).

### Upgrading from the ConfigMap (pre-#119)

Releases before [#119](https://github.com/AkashiSN/node-rotation-controller/issues/119)
carried policy in a single `node-rotation-config` ConfigMap (`config.policy.*`).
That ConfigMap is removed; policy now lives in cluster-scoped `RotationPolicy`
objects. The field shapes are 1:1 — lift each entry of the old
`config.policy.nodepoolSelectors[]` into its own `RotationPolicy`, with
`matchLabels` moving to `spec.nodePoolSelector.matchLabels` and every other field
(`ageThreshold`, `minRotationChances`, `maintenanceWindows`, `surge`, `prePull`)
copied verbatim under `spec`. Pre-1.0, this is an outright replacement with no
dual-support path (spec §5.4).

## Getting involved

This project is pre-1.0 and under active development; the v1 scope is the surge MVP described in the specification. Design feedback and implementation contributions are both welcome via GitHub Issues and PRs.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community standards.

## Development

Requires [aqua](https://aquaproj.github.io) and `make`; Docker is needed only
for `make docker-build`. The **Go toolchain** and all CLI tooling (golangci-lint,
gopls, setup-envtest, kind, ko, kustomize, helm, kubectl, terraform, awscli) are
version-pinned in [`aqua.yaml`](aqua.yaml) — install aqua, and the `make` targets
provision and use the pinned versions automatically (aqua lazily installs each on
first use; a `make` run links them onto `$PATH` for you). The Go version in
`aqua.yaml` is kept in sync with the `go` directive in `go.mod`.

| Command | Purpose |
|---------|---------|
| `make build` | Compile the manager binary into `bin/manager` |
| `make test` | Run unit tests and the envtest-based smoke test |
| `make lint` | Run golangci-lint |
| `make helm-lint` | Lint and render the Helm chart |
| `make docker-build` | Build the container image |

`make test` downloads the envtest control-plane binaries on first run.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow (issues, branches, PRs).

## License

Apache 2.0 — see [LICENSE](LICENSE).
