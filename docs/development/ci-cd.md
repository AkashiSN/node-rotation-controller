# CI/CD design

This repository runs three GitHub Actions workflows plus a docs-deploy
workflow. This page documents the change-detection gating scheme that keeps
required status checks fast without ever leaving one stuck `pending`.

## Workflows

| Workflow | Trigger | Purpose |
|---|---|---|
| `ci.yaml` | push to `main`, every PR | Required checks: `lint`, `test`, `build`, `chart` (plus `changes`, see below) |
| `e2e.yaml` | push to `main`, every PR | KWOK-based Karpenter e2e for the surge mechanism, single `e2e` job |
| `release.yaml` | push of a `v*` tag | Builds and pushes the multi-arch controller image and the Helm chart (OCI) to `ghcr.io`, then creates a GitHub Release |
| `pages.yaml` | push to `main` (docs-relevant paths), manual dispatch | Builds this VitePress site and deploys it to GitHub Pages |

## The `pending` trap

Branch protection on `main` names exact required status checks. If a workflow
or job is skipped outright for a given push/PR — for example via a top-level
`paths-ignore` on the workflow, or an `if:` condition on the *job* — GitHub
never reports a conclusion for that check. A required check with no
conclusion is not "green", it is stuck `pending`, and a PR can never merge
while any required check sits in that state. This is why this repository does
**not** use `paths-ignore` (or job-level `if:`) to skip CI on documentation-only
or otherwise irrelevant changes.

The fix is to always run the *job*, but skip its expensive *steps* when
nothing they care about changed. Every step that matters is gated with a
per-step `if:`, so the job itself always reaches a real conclusion (success)
in seconds when its inputs are untouched, and runs the full work when they
are.

## `ci.yaml`: the `changes` job

`ci.yaml` computes the gating flags once, in a dedicated `changes` job, and
has `lint`, `test`, `build`, and `chart` each declare `needs: changes` and
read its outputs. Centralizing the classification in one job (rather than
duplicating it in each of the four) keeps the logic DRY. `changes` itself
carries no `if:` and always runs — including a self-test of the classifier
script before it trusts its output — so it can never contribute a stuck
`pending` status either.

The classifier is `.github/scripts/detect-ci-changes.sh`: a small, pure shell
script (no `git`, no GitHub Actions context) that reads a newline-separated
list of changed paths on stdin and prints four booleans. `ci.yaml` feeds it
`git diff --name-only "$BASE_SHA" HEAD` on a pull request, or treats every
flag as `true` on a direct push to `main` (there is no PR base to diff
against, and a push to `main` should always run everything).
`.github/scripts/detect-ci-changes.test.sh` unit-tests the classifier against
a table of sample path sets and runs on every CI invocation, so the gating
logic itself cannot silently rot.

### Path → flag → job

| Path pattern | Flag | Gated jobs/steps |
|---|---|---|
| `*.go`, `go.mod`, `go.sum`, `api/`, `config/`, `.golangci.yml` | `go` | `lint`, `test`, `build` |
| `charts/**` | `chart` | `chart` |
| `Dockerfile`, `.dockerignore` | `docker` | `build` |
| `Makefile`, `aqua.yaml`, `aqua-policy.yaml`, `aqua/**`, `.github/workflows/ci.yaml`, `.github/scripts/**` | `infra` | all of `lint`, `test`, `build`, `chart` |

The resulting per-job step gates:

- **`lint`**, **`test`** run their real steps when `go || infra`.
- **`build`** runs its real steps when `go || docker || infra`.
- **`chart`** runs its real steps when `chart || infra`.

`infra` is deliberately broad: a change to the CI workflow, the shared
Makefile, or the aqua toolchain pins can affect every job's behavior, so it
fans out to all four rather than trying to guess which ones it actually
touches.

## `e2e.yaml`: single-job in-step detection

`e2e.yaml` takes a different shape from `ci.yaml` on purpose: it has exactly
one job, `e2e`, matching the single required check named `e2e`. Change
detection runs as the job's *first step* rather than in a separate upstream
job, because a separate `changes`-style job would introduce a cross-job
`needs` dependency for a single-job workflow with no other consumer — added
indirection with no DRY benefit here (`ci.yaml` shares its classification
across four jobs; `e2e.yaml` has only one). The detection step inspects the
diff for e2e-relevant paths (`internal/`, `cmd/`, `charts/`, `test/e2e/`,
`go.mod`/`go.sum`, `Makefile`, `Dockerfile`, `aqua.yaml`,
`.github/workflows/e2e.yaml`), excluding pure Markdown changes, and every
subsequent step — including the ~45-minute kind/Karpenter KWOK bootstrap and
the `go test` run — is gated on its result. A PR that touches none of those
paths still reports `e2e: success` in seconds; on a push to `main` the full
suite always runs.

## `release.yaml`: tag-driven OCI publish

`release.yaml` triggers only on `v*` tags, not on every push, so it has no
`pending`-required-check exposure — release workflows are not part of branch
protection. It runs four sequential jobs: `guard` fails fast if the pushed
tag disagrees with `Chart.yaml`'s version; `image` builds and pushes the
multi-arch (`linux/amd64`, `linux/arm64`) controller image to
`ghcr.io/akashisn/node-rotation-controller`, tagging `latest` unless the tag
is a hyphenated pre-release (e.g. `v0.4.0-rc.1`); `chart` packages the Helm
chart and pushes it as an OCI artifact to `oci://ghcr.io/akashisn/charts`;
and `release` downloads the packaged chart and creates a GitHub Release from
it, marked as a pre-release for the same hyphenated tags the image job skips
for `latest`.

## `pages.yaml`: docs deploy

A fourth workflow, `pages.yaml`, builds this VitePress site with
`npm run docs:build` and deploys the result to GitHub Pages. It triggers on
pushes to `main` that touch `docs/**`, `package.json`, `package-lock.json`,
or the workflow file itself, plus `workflow_dispatch` for manual runs. It is
not part of the required-check set discussed above — it publishes docs, it
does not gate merges — so it needs none of the change-detection gating this
page describes.
