# 6. Release

## 6.1 Versioning and Release

### Versioning

- **Semantic versioning** (`vMAJOR.MINOR.PATCH`)
- Pre-1.0 releases (`v0.x.y`) until v1 scope and CRD shape are stable
- **API compatibility surface:** `RotationPolicy` CRD schema, Prometheus metric names, annotation keys

### Distribution

| Artifact | Registry | Architectures |
|----------|----------|---------------|
| Controller image | `ghcr.io/akashisn/node-rotation-controller` | `linux/amd64`, `linux/arm64` |
| Helm chart | `oci://ghcr.io/akashisn/charts/node-rotation-controller` | — |

- A `vX.Y.Z` git tag publishes both OCI artifacts at the same version
- The pipeline guards that the tag matches `Chart.yaml` `version` == `appVersion`
- Install: `helm install ... oci://ghcr.io/akashisn/charts/node-rotation-controller --version X.Y.Z`

### Supply-chain attestations

- Keyless **cosign signature** + GitHub build-provenance (**SLSA**) attestation bound to the release workflow's OIDC identity
- Image carries an in-registry **SBOM** and SLSA provenance
- Each GitHub Release attaches a downloadable **SPDX SBOM**
- Attestation and signing run for pre-release tags too
- Verification instructions: [`SECURITY.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/SECURITY.md#verifying-releases)

## 6.2 Roadmap

| Milestone | Content |
|-----------|---------|
| v0.1 (spec) | This document |
| v0.2 (skeleton) | Project layout, controller-runtime bootstrap, leader election, CI |
| v0.3 (MVP) | Reconcile + surge + drain + metrics + Helm chart; `RotationPolicy` CRD (§5.4) |
| v0.4 | Chart renders one `RotationPolicy` per entry — per-NodePool policy |
| v0.5 | Forceful fallback (§3.6); earliest-deadline ordering; operator `do-not-disrupt` opt-out; `ThroughputBurstShortfall`; documentation site |
| v0.6 | Layer-2 forecast with `provisioningEstimate + drainEstimate` (ADR-0003); `failurePause` (ADR-0004); browser policy simulator (wasm) |
| v1.0 | Stable CRD (`v1`), production runbook, soak-tested on EKS Auto Mode |

- **v1.0 open item:** a genuine same-AZ capacity shortage (ICE) driving rollback (§7.2)
- **v1.0 validated:** the full multi-hour tight-race `expireAfter` soak (§7.2, issue #118)

### Not scheduled

Image **pre-pull** remains a reserved v2 expansion point behind a disabled config flag. The v1 parser accepts only `prePull.enabled: false`.
