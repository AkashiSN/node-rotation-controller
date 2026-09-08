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

The v1 surge MVP specified in §3–§5 is implemented and released pre-1.0. What each released version changed is recorded in the [changelog](https://github.com/AkashiSN/node-rotation-controller/blob/main/CHANGELOG.md); this section states only what is still ahead.

**v1.0** requires a stable `RotationPolicy` CRD (`v1`), the production runbook, and a soak-tested EKS Auto Mode deployment.

- **Open:** a genuine same-AZ capacity shortage (ICE) driving rollback (§7.2)
- **Settled:** the multi-hour tight-race `expireAfter` soak (§7.2)

### Not scheduled

Image **pre-pull** remains a reserved v2 expansion point behind a disabled config flag. The v1 parser accepts only `prePull.enabled: false`.
