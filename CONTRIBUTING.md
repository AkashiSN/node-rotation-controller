# Contributing to node-rotation-controller

Thanks for your interest! This project is in the **specification phase** — the
design is being refined before implementation begins. Feedback on the design
(via Issues) is especially valuable right now.

## Ways to contribute

- **Design feedback**: open an Issue against the spec in [`docs/specification.md`](docs/specification.md)
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
   - If the change affects behavior, update [`docs/specification.md`](docs/specification.md)
     **and** [`docs/ja/specification.md`](docs/ja/specification.md) in the same PR.
   - Ensure CI is green.
5. **Review & merge**: PRs are squash-merged once approved and CI passes.

## Language

- English is the default for code, comments, docs, issues, and PRs.
- Japanese documentation lives under `docs/ja/` and mirrors the English spec.

## Specification is the source of truth

The design in `docs/specification.md` leads the implementation. Code and spec
must not diverge — if your change alters documented behavior, update the spec
in the same PR.

## Releases

- Semantic Versioning (`vMAJOR.MINOR.PATCH`).
- Pre-1.0 (`v0.x.y`) while the configuration schema and CRD shape stabilize.
- The compatibility surface is: the ConfigMap schema, Prometheus metric names,
  and annotation keys.

## Scope reminder

This is a vendor-neutral OSS project. Please do not include organization-specific
or proprietary information in contributions.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
