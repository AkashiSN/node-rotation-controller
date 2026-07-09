# Security Policy

Thanks for helping keep `node-rotation-controller` and its users safe.

## Supported versions

The project is **pre-1.0** (`v0.x.y`); see the
[release roadmap](docs/specification/06-release.md#62-roadmap) for the path to a stable
v1.0. Security fixes are applied to the **latest released minor only** — there
are no long-term-support branches before 1.0. Always run the most recent release.

| Version | Supported |
|---------|-----------|
| Latest `0.x` release | ✅ |
| Any older release | ❌ — upgrade to the latest |

## Reporting a vulnerability

**Please report security issues privately. Do _not_ open a public GitHub issue,
pull request, or Discussion for a suspected vulnerability** — that discloses it
before a fix is available.

Report it through GitHub's **private vulnerability reporting**:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability** (or open
   <https://github.com/AkashiSN/node-rotation-controller/security/advisories/new>).
3. Describe the issue with enough detail to reproduce it (see below).

This opens a private advisory visible only to you and the maintainers.

> If private vulnerability reporting is not yet enabled on the repository, a
> maintainer can turn it on under **Settings → Code security and analysis →
> Private vulnerability reporting**. Until then, please wait rather than
> disclosing publicly.

### What to include

- A clear description of the issue and its security impact.
- The affected version(s) — chart version and/or controller image tag.
- Steps to reproduce, ideally a minimal `RotationPolicy` / `NodePool` setup.
- Any relevant logs, manifests, or RBAC configuration (redact secrets and
  account-identifying details).

### What to expect

This is a small, volunteer-maintained open-source project, so responses are
best-effort rather than bound by an SLA. We aim to:

- **acknowledge** your report within about a week;
- **assess and trial a fix** privately, keeping you updated on progress;
- **release a fix** and publish a [GitHub Security Advisory](https://github.com/AkashiSN/node-rotation-controller/security/advisories)
  crediting you (unless you prefer to stay anonymous), then disclose publicly.

We follow **coordinated disclosure**: please give us a reasonable window to ship
a fix before any public write-up.

## Verifying releases

Every `vX.Y.Z` tag publishes, for **both** the controller image and the Helm
chart pushed to ghcr.io:

- a **keyless [cosign](https://github.com/sigstore/cosign) signature** over the
  OCI digest, bound to this repository's GitHub Actions OIDC identity (no
  long-lived keys); and
- a **[GitHub build-provenance attestation](https://docs.github.com/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds)**
  (SLSA), verifiable with `gh attestation verify`.

The image additionally carries an in-registry SBOM and SLSA provenance emitted
by the build, and each Release attaches a downloadable SPDX SBOM
(`node-rotation-controller-sbom.spdx.json`) for inspection without a registry
client. These run for pre-release tags (`-rc`, `-beta`, …) too.

The signing identity is the release workflow, so verification **must** pin both
the certificate identity and the OIDC issuer — a missing or over-broad
`--certificate-identity(-regexp)` makes the check vacuous. Substitute the
version you are installing for `0.5.0`.

### With cosign (signatures)

```sh
# Controller image
cosign verify \
  --certificate-identity 'https://github.com/AkashiSN/node-rotation-controller/.github/workflows/release.yaml@refs/tags/v0.5.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/akashisn/node-rotation-controller:0.5.0

# Helm chart (OCI)
cosign verify \
  --certificate-identity 'https://github.com/AkashiSN/node-rotation-controller/.github/workflows/release.yaml@refs/tags/v0.5.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/akashisn/charts/node-rotation-controller:0.5.0
```

To verify any release tag with one command, pin the issuer and anchor the
identity regexp to this repo's workflow (note the escaped dots and the `$`
anchor — an unanchored pattern would accept forks):

```sh
cosign verify \
  --certificate-identity-regexp '^https://github\.com/AkashiSN/node-rotation-controller/\.github/workflows/release\.yaml@refs/tags/v.+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/akashisn/node-rotation-controller:0.5.0
```

### With the GitHub CLI (build provenance)

`gh attestation verify` resolves the attestation from the registry and checks it
was produced by this repository:

```sh
# Controller image
gh attestation verify \
  oci://ghcr.io/akashisn/node-rotation-controller:0.5.0 \
  --repo AkashiSN/node-rotation-controller

# Helm chart (OCI)
gh attestation verify \
  oci://ghcr.io/akashisn/charts/node-rotation-controller:0.5.0 \
  --repo AkashiSN/node-rotation-controller
```

Add `--signer-workflow AkashiSN/node-rotation-controller/.github/workflows/release.yaml`
to additionally pin the exact workflow that signed the artifact.

## Scope

In scope — vulnerabilities in this project's own code and packaging:

- the controller (`cmd/`, `internal/`, `api/`);
- the Helm chart (`charts/`), including its RBAC, the `RotationPolicy` CRD, and
  the surge `PriorityClass`;
- privilege escalation, unsafe node/`NodeClaim` deletion, or RBAC over-grants
  introduced by the above.

Out of scope — report these to their respective projects/vendors:

- **Karpenter** itself and its CRDs (`karpenter.sh/v1`);
- **Kubernetes**, EKS Auto Mode, or any cloud-provider control plane;
- a cluster operator's own misconfiguration (e.g. an over-broad
  `nodePoolSelector`, an unsatisfiable PDB) that the controller faithfully acts
  on. The controller never bypasses Karpenter — all node operations route through
  the `NodeClaim` CRD and Karpenter's voluntary, PDB-respecting drain path
  ([spec §3.3](docs/specification/03-design.md#33-surge-sequence-v1)).

Operational guidance for safe configuration lives in the
[production runbook](docs/runbook.md).
