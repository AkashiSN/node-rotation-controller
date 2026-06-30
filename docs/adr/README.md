# Architecture Decision Records

This directory records decisions that change an [architectural invariant](../../CLAUDE.md) — the constraints `CLAUDE.md` and `AGENTS.md` mark as "do not break without an ADR/Issue". An ADR is the artifact that justifies relaxing or changing one of those invariants; ordinary design proposals that do not touch an invariant stay as GitHub Issues (see `CONTRIBUTING.md`).

## Format

Each ADR is one file, `NNNN-kebab-title.md`, numbered sequentially. Use the [Nygard format](https://github.com/joelparkerhenderson/architecture-decision-record): **Title**, **Status**, **Context**, **Decision**, **Consequences**. Status is one of `Proposed`, `Accepted`, `Superseded by NNNN`, or `Rejected`. An ADR opens as `Proposed`, reaches agreement on its Issue, and is updated to `Accepted` when implementation is authorized.

The ADR records the decision; the canonical behavior still lives in [`docs/specification.md`](../specification.md). When an accepted ADR changes behavior, the spec (and its `docs/ja/` translation) is updated in the implementation PR, not here.

## Index

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-window-bounded-forceful-fallback.md) | Window-bounded forceful fallback (relax the surge-only invariant) | Proposed |
