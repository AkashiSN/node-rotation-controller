## Project

`node-rotation-controller` is a Kubernetes controller that proactively rotates
Karpenter-managed nodes in a make-before-break (surge) fashion within a
configurable maintenance window, before Karpenter's forceful `expireAfter`
fires. It targets EKS Auto Mode and any Karpenter v1+ environment.

The **source of truth for design** is [`docs/specification/`](docs/specification/)
(Japanese translation: [`docs/ja/specification/`](docs/ja/specification/)).
Read it before making design-affecting changes.

The latest release is **v0.6.1** and the project remains **pre-1.0**. The v1
surge MVP is implemented: the annotation-backed rotation state machine,
per-NodePool `RotationPolicy` resolution and observational status, surge
placeholder, opt-in window-bounded forceful fallback, throughput forecast,
metrics and Warning Events, Helm chart, and browser policy simulator are in
place. Unit, envtest, KWOK, and documentation tests run in CI. EKS Auto Mode
PoC runs have validated the core surge and fallback paths, including the
12-hour tight-race soak (Scenario P). A genuine same-AZ capacity shortage (ICE)
driving rollback remains the documented real-cloud validation gap before v1.0;
see the roadmap and validated assumptions in the specification (§6.2, §7.2).
The specification remains the source of truth — keep code and spec in sync.

## Development process

- [`CONTRIBUTING.md`](CONTRIBUTING.md) is the source of truth for the contributor
  workflow, branch/worktree isolation, validation, and release preparation.
- **Design changes require an Issue first.** Anything that changes behavior,
  the configuration schema, annotation keys, metric names, or the public
  surface must start as a GitHub Issue and reach agreement before implementation.
- **Branch naming**: `feat/<issue#>-<topic>`, `fix/<issue#>-<topic>`,
  `docs/<topic>`, `chore/<topic>`, `refactor/<topic>`.
- **One PR = one concern.** Keep PRs focused and reviewable.
- Reference the Issue with `Closes #<issue>` or `Refs #<issue>` when one exists.
  Trivial typo or formatting-only fixes may omit an Issue but must say so in the
  PR body.
- **`main` is protected**: PR-only, CI must be green, squash merge.
- **Conventional Commits** for commit messages and PR titles:
  `type(scope): subject` where type ∈ {feat, fix, docs, chore, refactor, test, perf}.
  Examples:
  - `feat(reconciler): add age-threshold candidate selection`
  - `fix(window): handle DST timezone transitions`
  - `docs(spec): clarify backstop semantics`
- **Release preparation is an atomic version-sync change.** Follow the mandatory
  checklist in
  [`CONTRIBUTING.md`](CONTRIBUTING.md#release-version-synchronization) and run
  both release-version guards before proposing or pushing a tag.

## Isolated development with worktrees

Use the worktree workflow in
[`CONTRIBUTING.md`](CONTRIBUTING.md#isolated-and-parallel-work). In particular,
each PR gets its own branch and worktree under `.worktrees/`, branches start
from `main`, and stacked in-flight branches are not used.

## Specification rules

- `docs/specification/` (English) is the canonical spec. `docs/ja/specification/`
  is a translation and **must be kept in sync** — update both in the same PR.
- English is the default language for code, comments, docs, issues, and PRs.
  Japanese content lives only under `docs/ja/`.
- Annotation/label keys use the `noderotation.io/` prefix consistently.
- The spec leads the implementation. Do not let code and spec diverge; if a PR
  changes behavior, update the spec in the same PR.

## Architectural invariants (do not break without an ADR/Issue)

- The controller **never bypasses Karpenter**. It induces NodePool-owned
  replacement capacity via a low-priority **placeholder Pod** (never a
  standalone `NodeClaim` — see spec §3.3), deletes old `NodeClaim` resources
  (`karpenter.sh/v1`), and lets Karpenter's termination controller drain
  nodes via the voluntary path, where PDBs apply.
- `expireAfter` is **retained as a backstop**, not removed. The controller's
  `ageThreshold` is derived to stay below `expireAfter` (spec §3.2);
  validation fails when the schedule cannot guarantee that.
- v1 is **surge-only by default and serial per NodePool** (`surge.maxUnavailable = 1`); an opt-in **window-bounded forceful fallback** (`surge.forcefulFallback`, default off; ADR-0001) may delete a NodeClaim in-window without the surge, still via the voluntary path (PDBs apply);
  distinct NodePools may rotate concurrently. Pre-pull (v2) is a reserved
  expansion point behind a disabled config flag.
- All controller state lives on Kubernetes objects — durable state on
  `NodeClaim`/`NodePool` annotations, plus transient markers on Nodes and the
  placeholder Pod (spec §5.3) — **no external datastore**.

## Must not

- Do not mix a design decision into an unrelated PR — open an Issue for it.
- Do not introduce organization-specific, internal, or proprietary information
  (company names, internal hostnames, business-cycle details, account IDs, etc.).
  This is a public, vendor-neutral OSS project.
- Do not add a dependency on a specific cloud provider's API in v1 — all node
  operations route through the Karpenter `NodeClaim` CRD.

## Contributor docs

Human-facing process lives in [`CONTRIBUTING.md`](CONTRIBUTING.md), the single
source of truth for contributor workflow. `CLAUDE.md` is a symlink to this file,
so Claude Code and agents that read `AGENTS.md` receive the same project
instructions without duplicated copies.

Before creating or modifying project documentation, read and follow
[`docs/development/documentation-style.md`](docs/development/documentation-style.md).
It is the shared style, translation, safe-editing, and validation standard for
humans and AI agents. Agent-specific files should point to it rather than copy
its rules.
