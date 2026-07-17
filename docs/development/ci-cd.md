# CI/CD Design

::: tip What this page covers
How this repository keeps required status checks fast without ever leaving one stuck `pending` — via step-level gating driven by a centralized change-detection classifier.
:::

## Workflows

| Workflow | Trigger | Purpose |
|---|---|---|
| `ci.yaml` | push to `main`, every PR | Required: `lint`, `test`, `build`, `chart` |
| `e2e.yaml` | push to `main`, every PR | KWOK-based Karpenter e2e (single `e2e` job) |
| `release.yaml` | push of a `v*` tag | Multi-arch image + Helm chart OCI + attestation + GitHub Release |
| `pages.yaml` | push to `main` (docs paths), manual | VitePress site → GitHub Pages |

## The `pending` trap

::: warning Why this matters
A required check with no conclusion is not "green" — it is stuck `pending`. A PR can never merge while any required check sits in that state.
:::

Branch protection on `main` names exact required status checks. If a workflow or job is **skipped outright** (via `paths-ignore` or job-level `if:`), GitHub never reports a conclusion → stuck `pending`.

**Solution:** always run the *job*, skip only its expensive *steps* when nothing they care about changed.

- Every step that matters carries a per-step `if:`
- The job always reaches a real conclusion (success) in seconds when inputs are untouched
- Full work runs only when relevant files change

This is why the repository does **not** use `paths-ignore` or job-level `if:`.

## `ci.yaml`: the `changes` job

### How it works

1. A dedicated `changes` job computes gating flags (always runs, no `if:`)
2. `lint`, `test`, `build`, `chart` each declare `needs: changes` and read its outputs
3. Classification logic lives in one place (DRY)

The classifier is `.github/scripts/detect-ci-changes.sh`:
- A small, pure shell script (no `git`, no GitHub Actions context)
- Reads newline-separated changed paths on stdin
- Prints four booleans

### Input sources

| Context | Input |
|---------|-------|
| Pull request | `git diff --name-only "$BASE_SHA" HEAD` |
| Push to `main` | All flags `true` (no base to diff; always run everything) |

### Self-test

`.github/scripts/detect-ci-changes.test.sh` unit-tests the classifier against a table of sample path sets. It runs on every CI invocation — gating logic cannot silently rot.

### Path → flag → job

| Path pattern | Flag | Gated jobs/steps |
|---|---|---|
| `*.go`, `go.mod`, `go.sum`, `api/`, `config/`, `.golangci.yml` | `go` | `lint`, `test`, `build` |
| `charts/**` | `chart` | `chart` |
| `Dockerfile`, `.dockerignore` | `docker` | `build` |
| `Makefile`, `aqua.yaml`, `aqua-policy.yaml`, `aqua/**`, `.github/workflows/ci.yaml`, `.github/scripts/**` | `infra` | all four jobs |

### Resulting step gates

| Job | Runs real steps when |
|-----|---------------------|
| `lint` | `go \|\| infra` |
| `test` | `go \|\| infra` |
| `build` | `go \|\| docker \|\| infra` |
| `chart` | `chart \|\| infra` |

- **`infra` is deliberately broad:** CI workflow, shared Makefile, or aqua toolchain pins can affect every job — fans out to all four rather than guessing.

## `e2e.yaml`: single-job in-step detection

A different shape from `ci.yaml` on purpose:

- **One job** (`e2e`) = one required check
- Change detection runs as the job's **first step** (not a separate upstream job)
- No DRY benefit from a separate `changes` job when there's only one consumer

### Detection scope

Inspects the diff for e2e-relevant paths:
- `internal/`, `cmd/`, `charts/`, `test/e2e/`
- `go.mod`, `go.sum`, `Makefile`, `Dockerfile`, `aqua.yaml`
- `.github/workflows/e2e.yaml`
- Excludes pure Markdown changes

### Behavior

| Context | Result |
|---------|--------|
| PR touching none of those paths | `e2e: success` in seconds |
| PR touching relevant paths | Full ~45-minute suite |
| Push to `main` | Always full suite |

## `release.yaml`: tag-driven OCI publish

Triggers only on `v*` tags — not part of branch protection, so no `pending`-check exposure.

### Four sequential jobs

| Job | Purpose |
|-----|---------|
| `guard` | Fail fast if tag disagrees with `Chart.yaml` version |
| `image` | Multi-arch build + push + SBOM + SLSA provenance + attest + cosign |
| `chart` | Package Helm chart → OCI push + attest + cosign |
| `release` | Create GitHub Release with chart + SBOM attached |

### Image job details

- Architectures: `linux/amd64`, `linux/arm64`
- Registry: `ghcr.io/akashisn/node-rotation-controller`
- Tags `latest` unless hyphenated pre-release (e.g. `v0.4.0-rc.1`)
- In-registry SBOM + SLSA provenance from build
- Attestation on the pushed index digest + keyless cosign signature
- Emits SPDX SBOM for the Release

### Chart job details

- Packages Helm chart → `oci://ghcr.io/akashisn/charts`
- Attests and keyless-signs the pushed manifest digest

### Release job details

- Downloads packaged chart + SPDX SBOM
- Creates GitHub Release with both attached
- Marks as pre-release for hyphenated tags

### Permissions

Scoped per job (not workflow-level):

| Job | Permissions |
|-----|-------------|
| `image`, `chart` | `id-token`, `attestations`, `packages: write` |
| `release` | `contents: write` |

Attestation runs for pre-release tags too. See [`SECURITY.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/SECURITY.md#verifying-releases) for consumer verification.

## `pages.yaml`: docs deploy

Builds this VitePress site with `npm run docs:build` and deploys to GitHub Pages.

### Triggers

- Pushes to `main` touching: `docs/**`, `README.md`, `README.ja.md`, `package.json`, `package-lock.json`, or the workflow file
- `workflow_dispatch` for manual runs

### Not a required check

This workflow publishes docs — it does not gate merges. No change-detection gating needed.
