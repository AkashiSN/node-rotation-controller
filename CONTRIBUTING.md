# Contributing to node-rotation-controller

Thanks for your interest! This project is in the **v0.3 MVP phase**: the v1 surge
implementation is in place, and the design is still being refined toward v1.0.
Feedback on the design and implementation is welcome via Issues.

## Ways to contribute

- **Design feedback**: open an Issue against the spec in [`docs/specification/`](docs/specification/)
- **Documentation**: clarify the spec, fix typos, improve the Japanese translation
- **Implementation**: once a milestone's issues are defined, pick one up

## Workflow

1. **Find or open an Issue.** Any behavior-, schema-, or API-affecting change
   starts as an Issue so the design can be agreed before code is written.
   Trivial fixes (typos, formatting) may skip the Issue and go straight to a PR.
2. **Branch** from `main`:
   - `feat/<issue#>-<short-topic>`
   - `fix/<issue#>-<short-topic>`
   - `docs/<short-topic>`
   - `chore/<short-topic>`
   - `refactor/<short-topic>`
3. **Commit** using [Conventional Commits](https://www.conventionalcommits.org/):
   `type(scope): subject` — type ∈ {feat, fix, docs, chore, refactor, test, perf}.
4. **Open a PR** to `main`:
   - One concern per PR.
   - Reference the issue: `Closes #<issue>`.
   - If the change affects behavior, update [`docs/specification/`](docs/specification/)
     **and** [`docs/ja/specification/`](docs/ja/specification/) in the same PR.
   - Ensure CI is green.
5. **Review & merge**: PRs are squash-merged once approved and CI passes.

## Language

- English is the default for code, comments, docs, issues, and PRs.
- Japanese documentation lives under `docs/ja/` and mirrors the English spec.

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
  Fixes / Documentation / Maintenance via `.github/release.yml`) are sufficient
  for a pre-1.0 project with infrequent releases, and an unmaintained changelog
  file is worse than none.
- **The release-note category is derived automatically from your PR title.** A
  workflow reads the Conventional Commit type (`feat`, `fix`, `docs`, `chore`,
  `refactor`, `test`, `perf`) and applies the matching label, so you do not need
  to add it by hand — just keep the title well-formed.

## Scope reminder

This is a vendor-neutral OSS project. Please do not include organization-specific
or proprietary information in contributions.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
