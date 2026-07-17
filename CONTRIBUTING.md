# Contributing to node-rotation-controller

Thanks for your interest! The latest release is **v0.6.1**. The v1 surge MVP,
per-NodePool `RotationPolicy`, opt-in forceful fallback, throughput forecast,
observability, Helm chart, and browser policy simulator are implemented. The
project remains pre-1.0 while the CRD and compatibility surface stabilize
toward v1.0.

The core paths and a 12-hour tight-race soak are validated on EKS Auto Mode. A
genuine same-AZ capacity shortage (ICE) driving rollback remains a documented
real-cloud validation gap. See the
[`roadmap`](docs/specification/06-release.md#62-roadmap) and
[`validated assumptions`](docs/specification/07-risks.md#72-validated-assumptions).

## Ways to contribute

- **Design feedback**: open an Issue against the spec in [`docs/specification/`](docs/specification/)
- **Documentation**: clarify the spec, fix typos, improve the Japanese translation
- **Implementation**: pick up a scoped open Issue or propose one before changing behavior
- **Validation**: extend the real-cloud evidence or reproduce a documented limitation

## Workflow

1. **Find or open an Issue when required.** Any behavior-, schema-, annotation-,
   metric-, or public-API-affecting change starts as an Issue so the design can
   be agreed before code is written. Trivial typo and formatting-only fixes may
   skip the Issue.
2. **Create a branch and worktree** from `main`:
   - `feat/<issue#>-<short-topic>`
   - `fix/<issue#>-<short-topic>`
   - `docs/<short-topic>`
   - `chore/<short-topic>`
   - `refactor/<short-topic>`
3. **Commit** using [Conventional Commits](https://www.conventionalcommits.org/):
   `type(scope): subject` — type ∈ {feat, fix, docs, chore, refactor, test, perf}.
4. **Open a PR** to `main`:
   - One concern per PR.
   - Use `Closes #<issue>` or `Refs #<issue>` when an Issue exists.
   - For an exempt typo or formatting-only change, write `No issue:` and the
     reason in the PR body.
   - If the change affects behavior, update [`docs/specification/`](docs/specification/)
     **and** [`docs/ja/specification/`](docs/ja/specification/) in the same PR.
   - Describe the local checks that were run.
   - Ensure CI is green.
5. **Review & merge**: PRs are squash-merged once approved and CI passes.

## Isolated and parallel work

Each PR uses its own branch and git worktree so concurrent changes cannot
interfere with one another.

- Place worktrees under `.worktrees/<branch-topic>`; the directory is git-ignored.
- Create the branch from an up-to-date `main`, for example:

  ```sh
  git worktree add -b docs/example .worktrees/docs-example origin/main
  ```

- Do not branch from another in-flight feature branch. Land the dependency first,
  then start the dependent change from the updated `main`.
- After the PR is squash-merged, remove the worktree and delete its branch.

## Language

- English is the default for code, comments, docs, issues, and PRs.
- Japanese documentation lives under `docs/ja/` and mirrors the English spec.

### Documentation style

Humans and AI agents use the shared
[`documentation style guide`](docs/development/documentation-style.md) when
creating or modifying project documentation. The guide defines presentation,
document profiles, terminology, translation handling, safe-editing rules, and
validation. This file remains the source of truth for contributor process when
the two documents overlap.

**Which docs are translated.** The EN/JA sync obligation covers
[`docs/specification/`](docs/specification/) and the user-facing performance
notes in [`docs/reference/perf/`](docs/reference/perf/): update the English page
and its `docs/ja/` mirror in the same PR. The ADRs in
[`docs/reference/adr/`](docs/reference/adr/) are **English-only by design** and
intentionally not translated — they are dated decision records addressed to
maintainers, and a translation that drifts from the original is worse than none.
The Japanese docs-site sidebar therefore links to the English ADR pages
explicitly rather than omitting them.

### Japanese translation conventions

These conventions keep `docs/ja/` translations consistent. They record existing
practice; apply them to new and updated translations.

- **Fenced code blocks — translate comments only.** In fenced code blocks under
  `docs/ja/`, translate the comments into Japanese, and keep everything else
  (commands, identifiers, YAML keys and values, pseudocode, and program output)
  verbatim so it matches the English source exactly. For example, spec §5 renders
  `# remove the annotation` as `# アノテーション削除` while the surrounding
  command stays unchanged.
- **`stuck-drain` — keep the term in English, translate the prose.** Keep the
  compound term in English where it names the metric or mechanism (`stuck-drain`,
  `noderotation_drain_stuck`); in running Japanese prose describing the condition,
  write "詰まった drain". The same split applies to other identifier-derived
  terms: the identifier stays verbatim, the surrounding prose is translated.

## Specification is the source of truth

The design in `docs/specification/` leads the implementation. Code and spec
must not diverge — if your change alters documented behavior, update the spec
in the same PR.

## Releases

- Semantic Versioning (`vMAJOR.MINOR.PATCH`).
- Pre-1.0 (`v0.x.y`) while the configuration schema and CRD shape stabilize.
- The compatibility surface is: the `RotationPolicy` CRD schema, Prometheus
  metric names, and annotation keys.
- **The GitHub Release page is the changelog.** There is intentionally no
  `CHANGELOG.md`: each release's auto-generated notes (grouped into Features /
  Fixes / Documentation / Maintenance via `.github/release.yml`) are the
  release history; an additional hand-maintained changelog would duplicate it.
- **Chart and app versions move together before 1.0.** A release-preparation PR
  updates `Chart.yaml` `version` and `appVersion`; the current-release wording in
  `README.md`, `README.ja.md`, `AGENTS.md`, and this file; and the other
  release-facing records. The release workflow rejects a tag that does not match
  the chart and app versions.
- **Every release adds a row to the runbook's CRD-change table.** There is no
  changelog file, so the per-release record of `RotationPolicy` schema changes
  lives in [`docs/runbook.md` §8](docs/runbook.md#8-upgrading-and-rolling-back-the-controller)
  and its JA mirror. Append the row in the release-preparation PR — "None" is a
  valid answer, and the row is what tells an operator crossing several versions
  whether they must `kubectl apply` the CRDs before `helm upgrade`. The release
  notes link to the table rather than restating it.
- **The release-note category is derived automatically from your PR title.** A
  workflow reads the Conventional Commit type (`feat`, `fix`, `docs`, `chore`,
  `refactor`, `test`, `perf`) and applies the matching label, so you do not need
  to add it by hand — just keep the title well-formed.

### Release version synchronization

A release-preparation PR **MUST** update every current-release marker in one
change. Do not update only `Chart.yaml` or only the README badge.

| Location | Required update |
|---|---|
| `charts/node-rotation-controller/Chart.yaml` | `version` and `appVersion` |
| `README.md` | Status badge, current heading, release summary |
| `README.ja.md` | Same changes in Japanese |
| `AGENTS.md` | Latest-release wording and implemented status |
| `CONTRIBUTING.md` | Latest-release wording |
| `docs/runbook.md` | New CRD-change row, including `None` |
| `docs/ja/runbook.md` | Matching Japanese CRD-change row |

Before merging the release-preparation PR, run:

```sh
.github/scripts/check-chart-version.sh vX.Y.Z
.github/scripts/check-release-version-sync.sh vX.Y.Z
```

The second command also runs on every PR in CI and again before a tagged release
publishes artifacts. A version must not be tagged until both guards pass. When a
new current-release marker is introduced, add it to this checklist and the sync
guard in the same PR.

## Scope reminder

This is a vendor-neutral OSS project. Please do not include organization-specific
or proprietary information in contributions.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
