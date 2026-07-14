# 6. Release

## 6.1 Versioning and Release

- Semantic versioning (`vMAJOR.MINOR.PATCH`)
- Pre-1.0 releases (`v0.x.y`) until v1 scope and CRD shape are stable
- API compatibility surface: the `RotationPolicy` CRD schema (stabilizing `v1alpha1` → `v1`), Prometheus metric names, annotation keys
- **Distribution.** A `vX.Y.Z` git tag publishes, to GitHub Container Registry
  (ghcr.io) as OCI artifacts at the same version: the multi-arch controller image
  (`ghcr.io/akashisn/node-rotation-controller`, `linux/amd64,linux/arm64`) and the
  Helm chart (`oci://ghcr.io/akashisn/charts/node-rotation-controller`). The
  release pipeline guards that the tag matches `Chart.yaml` `version`==`appVersion`
  before publishing; the tag is the source of truth.
- **Supply-chain attestations.** Both OCI artifacts are published with a keyless
  cosign signature and a GitHub build-provenance (SLSA) attestation bound to the
  release workflow's OIDC identity — no key material. The image also carries an
  in-registry SBOM and SLSA provenance from the build, and each GitHub Release
  attaches a downloadable SPDX SBOM. Attestation and signing run for pre-release
  tags too. Verification instructions (`cosign verify`, `gh attestation verify`)
  live in [`SECURITY.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/SECURITY.md#verifying-releases).
- Install: `helm install ... oci://ghcr.io/akashisn/charts/node-rotation-controller --version X.Y.Z`.

## 6.2 Roadmap

| Milestone | Content |
|-----------|---------|
| v0.1 (spec) | This document |
| v0.2 (skeleton) | Project layout, controller-runtime bootstrap, leader election, CI |
| v0.3 (MVP, v1 surge) | Reconcile + surge + drain + metrics + Helm chart; the cluster-scoped `RotationPolicy` CRD (§5.4) supersedes the ConfigMap |
| v0.4 | The chart renders one `RotationPolicy` per `rotationPolicies` entry, so a single install can give each NodePool its own window, `ageThreshold`, and surge |
| v0.5 | Opt-in window-bounded surge-less forceful fallback (§3.3, ADR-0001); earliest-deadline candidate ordering (§3.2); operator-owned `karpenter.sh/do-not-disrupt` excludes a node from candidate selection (§3.2); `ThroughputBurstShortfall` detection for synchronized batches (§3.2); the published documentation site |
| v0.6 | The layer-2 throughput forecast models a rotation as `provisioningEstimate + drainEstimate + cooldownAfter` rather than deriving it from the force-kill deadline (§3.2, ADR-0003), and exports its inputs (`C`, `t_rot` estimate, `t_rot` bound) as metrics (§4.2); `surge.failurePause` splits the post-failure inter-attempt pause off `cooldownAfter`, which may now be `0` (§5.2, ADR-0004); the browser policy simulator on the documentation site, running the controller's own pure schedule/selection core compiled to wasm, with a CI guard on that purity |
| v1.0 | Stable `RotationPolicy` CRD (`v1`), documented production runbook, soak-tested on a real EKS Auto Mode cluster. Two items from §7.2 remain open: a genuine same-AZ capacity shortage (ICE) driving rollback, and a full multi-hour tight-race `expireAfter` soak |

Image **pre-pull** is not scheduled against a milestone. It remains a reserved v2
expansion point behind a disabled config flag (§3.4); the v1 parser accepts only
`prePull.enabled`, which must be `false`.
