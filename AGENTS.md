## Project

`node-rotation-controller` is a Kubernetes controller that proactively rotates
Karpenter-managed nodes in a make-before-break (surge) fashion within a
configurable maintenance window, before Karpenter's forceful `expireAfter`
fires. It targets EKS Auto Mode and any Karpenter v1+ environment.

The **source of truth for design** is [`docs/specification.md`](docs/specification.md)
(Japanese translation: [`docs/ja/specification.md`](docs/ja/specification.md)).
Read it before making design-affecting changes.

The project is in the **specification phase** (v0.1). Implementation has not
started. See the roadmap in the specification (§6.2) for the planned milestones.

## Development process

- **Design changes require an Issue first.** Anything that changes behavior,
  the configuration schema, annotation keys, or the public surface must start
  as a GitHub Issue and reach agreement before implementation.
- **Branch naming**: `feat/<issue#>-<topic>`, `fix/<issue#>-<topic>`,
  `docs/<topic>`, `chore/<topic>`, `refactor/<topic>`.
- **One PR = one concern.** Keep PRs focused and reviewable.
- Every PR body must reference its issue with `Closes #<issue>` (or `Refs #<issue>`).
- **`main` is protected**: PR-only, CI must be green, squash merge.
- **Conventional Commits** for commit messages and PR titles:
  `type(scope): subject` where type ∈ {feat, fix, docs, chore, refactor, test, perf}.
  Examples:
  - `feat(reconciler): add age-threshold candidate selection`
  - `fix(window): handle DST timezone transitions`
  - `docs(spec): clarify backstop semantics`
- **Milestones** (`v0.2`, `v0.3`, …) group issues toward each release.

## Parallel development with worktrees

- **One Issue = one PR = one branch = one git worktree.** Each unit of work
  lives in its own `git worktree` so concurrent work streams never share a
  working tree and cannot interfere with one another.
- **Tear down after merge.** Once a PR is squash-merged, remove its worktree
  (`git worktree remove`) and delete the branch. Worktrees are disposable and
  must not accumulate.
- **No stacked branches.** Do not branch off another in-flight feature branch.
  If a change depends on work that is not yet merged, land the base change
  first: open and merge the base PR, then start the dependent work on a fresh
  branch off the updated `main`. Keep every branch rooted at `main`.

## Specification rules

- `docs/specification.md` (English) is the canonical spec. `docs/ja/specification.md`
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
- v1 is **surge-only and serial per NodePool** (`surge.maxUnavailable = 1`);
  distinct NodePools may rotate concurrently. Pre-pull (v2) and warm-up (v3)
  are reserved expansion points behind disabled config flags.
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

Human-facing process lives in [`CONTRIBUTING.md`](CONTRIBUTING.md). This file
is the single source of truth for process; `CLAUDE.md` only adds AI-specific
emphasis and points back to it.
