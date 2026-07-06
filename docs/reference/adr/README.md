# Architecture Decision Records

This directory records decisions that change an [architectural invariant](../../../CLAUDE.md) — the constraints `CLAUDE.md` and `AGENTS.md` mark as "do not break without an ADR/Issue". An ADR is the artifact that justifies relaxing or changing one of those invariants; ordinary design proposals that do not touch an invariant stay as GitHub Issues (see `CONTRIBUTING.md`).

## Format

Each ADR is one file, `NNNN-kebab-title.md`, numbered sequentially. Use the [Nygard format](https://github.com/joelparkerhenderson/architecture-decision-record): **Title**, **Status**, **Context**, **Decision**, **Consequences**. Status is one of `Proposed`, `Accepted`, `Superseded by NNNN`, or `Rejected`. An ADR opens as `Proposed` and reaches agreement on its Issue.

The ADR records the decision; the canonical behavior lives in [`docs/specification/`](../../specification/), which leads the implementation and must never diverge from it. A behavior-changing ADR therefore moves to `Accepted` **only in (or after) the PR that synchronizes the canonical surface** — `docs/specification/`, its `docs/ja/` translation, and any architectural-invariant wording in [`CLAUDE.md`](../../../CLAUDE.md) — never before. Until that spec-sync PR lands the ADR stays `Proposed`, so an `Accepted` ADR and the canonical spec can never contradict each other. Implementation that does not change the documented surface (code, CRD schema, metrics) can follow in later PRs.

## Index

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-window-bounded-forceful-fallback.md) | Window-bounded forceful fallback (relax the surge-only invariant) | Accepted |
