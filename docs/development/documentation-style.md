# Documentation style guide

This guide is the shared writing standard for humans and AI agents that create
or modify project documentation. Its purpose is to keep documentation accurate,
consistent, and easy to use without forcing every document into the same shape.

## 1. Scope and authority

Apply this guide to:

- Markdown under `docs/`
- `README.md` and `README.ja.md`
- documentation sections in contributor-facing files such as `CONTRIBUTING.md`

Agent adapter files such as `.kiro/steering/*.md`, `AGENTS.md`, and `CLAUDE.md`
may point to this guide, but must not duplicate it as a second source of truth.

This guide controls presentation and organization. It does not override:

1. the canonical product behavior in `docs/specification/`
2. the repository instructions and architectural invariants in `AGENTS.md`
3. the contributor process in `CONTRIBUTING.md`
4. the rationale and consequences recorded in accepted ADRs
5. document-specific conventions already established in the page being edited

If two sources conflict, preserve the higher-authority source and report the
conflict. Do not silently resolve a design or process conflict as a style edit.

The keywords **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY**
indicate requirement strength. Requirements marked **MUST** are correctness or
project-consistency rules. Guidance marked **SHOULD** may be departed from when
clarity or technical accuracy requires it.

## 2. Core writing principles

- **Accuracy before brevity.** Never omit a condition, limitation, or exception
  merely to shorten a paragraph, list, table, or diagram.
- **Reader task before background.** State what the reader needs to know, decide,
  or do before giving supporting rationale.
- **Structure by meaning.** Use headings, lists, tables, and diagrams when they
  reveal relationships or help navigation, not to meet a numeric layout target.
- **Scannable wording.** Use descriptive headings and lead each section or list
  item with its main point.
- **One source of truth.** Link to canonical behavior or rationale instead of
  copying text that can drift.
- **Minimal necessary change.** Preserve correct terminology, anchors, examples,
  and surrounding structure unless changing them is part of the task.

There are no mandatory limits based on rendered line count, word count, or
diagram source length. Those measures vary by language and editor. Split
content when it contains multiple independent ideas or becomes difficult to
navigate.

## 3. Document profiles

Use the profile that matches the page. If no profile matches, apply the core
principles and follow nearby documents of the same kind.

| Document type | Primary audience | Preferred organization |
|---|---|---|
| Getting Started | First-time users | Outcome, install, verify, explanation |
| Runbook | Operators | Trigger, signal, action, escalation |
| Specification | Implementers and reviewers | Complete normative behavior |
| Simulator guide | Users evaluating policy | Purpose, use, interpretation |
| ADR | Maintainers | Context, decision, consequences |
| Validation report | Reviewers | Claim, method, evidence, limitations |
| Performance note | Operators and maintainers | Workload, method, result, limits |
| Development guide | Contributors | Goal, prerequisites, procedure, checks |
| Index page | All readers | Orientation and descriptive navigation |

### 3.1 Getting Started

- Put a minimal successful path and a concrete verification step first.
- Explain concepts after the reader can see the expected outcome.
- Link to the simulator before recommending production configuration.
- Distinguish prerequisites from commands the reader must run.

### 3.2 Runbook

Organize operational procedures around:

- **When:** the symptom, alert, or condition that selects the procedure
- **What to inspect:** the metric, event, log, or command and expected signal
- **What to do:** ordered actions, decision points, and stop conditions
- **When to escalate:** evidence that the procedure is unsafe or insufficient

Keep design rationale in the specification or an ADR and link to it. Commands
MUST make their assumptions and destructive effects clear.

### 3.3 Specification

- Precision and completeness take priority over concision.
- State normative behavior separately from rationale and examples.
- Keep terminology, numbering, cross-references, tables, and diagrams aligned
  with the Japanese specification.
- Express edge cases as explicit conditions and outcomes.
- Do not change behavior, configuration, annotations, or public surfaces without
  following the Issue-first process in `CONTRIBUTING.md`.

### 3.4 Simulator guide

- Start with the decision the simulator helps the reader make.
- Explain when to use it and how to interpret statuses, colors, and diagnostics.
- Put implementation details in an expandable section unless they affect use.
- Keep field help concise, but include a concrete example where ambiguity is
  otherwise likely.

### 3.5 ADR

- Record the context, decision, consequences, status, and relevant alternatives.
- Preserve the historical decision; supersede an accepted ADR instead of
  rewriting history.
- Link to the canonical specification for current behavior.
- ADRs are English-only by project policy.

### 3.6 Validation and performance reports

- Separate the claim, environment, method, evidence, result, and limitations.
- Include enough detail for another contributor to reproduce or challenge the
  conclusion.
- Never present an estimate, simulation, or synthetic result as field evidence.
- Keep raw or exhaustive evidence accessible without hiding the conclusion.

### 3.7 Development guides

- State prerequisites and the expected result before the procedure.
- Use reproducible commands and identify required tools or environment.
- End with relevant verification and troubleshooting steps.

## 4. Markdown and VitePress conventions

### 4.1 Headings

- Use one `#` page title.
- Use `##` for primary page sections, then `###` and `####` without skipping
  levels.
- Make headings descriptive enough to understand in a table of contents.
- Preserve published heading text when changing it would break inbound anchors,
  unless the task includes updating all affected links.

### 4.2 Tables and lists

- Use a table for repeated-field comparison, matrices, and compact lookups.
- Use a list for steps, conditions, exclusions, definitions, or independent
  points.
- Keep a table cell focused on one value or idea. Move multi-paragraph rationale
  below the table.
- Use `- **Label:** explanation` when readers benefit from scanning named items.
- Use numbered lists only when order or sequence matters.

### 4.3 Code and commands

- Add a language identifier to fenced code blocks when one is available.
- Keep commands, identifiers, annotation keys, metric names, YAML fields, and
  program output exact.
- Do not invent command output or claim that a command was run when it was not.
- Explain placeholders and environment-specific values close to the example.

### 4.4 Links and cross-references

- Prefer descriptive link text over “here” or a bare path in reader-facing prose.
- Within the specification, section references such as `§3.2` MAY supplement a
  descriptive link.
- Use repository-relative links that work on both GitHub and VitePress.
- Link to the canonical source instead of duplicating definitions.

### 4.5 Diagrams and custom containers

Use a diagram only when it makes a relationship, state transition, decision, or
sequence materially easier to understand:

- `sequenceDiagram` for component interactions
- `flowchart` for decisions and branching
- `stateDiagram-v2` for states and transitions
- `gantt` for schedules or timelines

Keep each diagram focused on one primary concept. Split it when independent
flows cannot be followed clearly in one view.

VitePress custom containers MAY be used as follows:

- `::: tip` for a concise orientation or key takeaway
- `::: warning` for a limitation, risk, or prerequisite readers must not miss
- `::: details` for supplementary, exhaustive, or implementation-level material

Keep the conclusion, primary procedure, and essential safety information visible
without requiring the reader to expand a details block.

## 5. Language and terminology

English is the default language for project documentation. Japanese content
lives under `docs/ja/`, except for `README.ja.md`.

### 5.1 English

- Use active voice when it identifies the actor clearly.
- State facts directly; remove filler such as “It should be noted that”.
- Use the project's established Kubernetes and Karpenter terminology.
- Keep code identifiers formatted as code.

### 5.2 Japanese

- Use 「ウィンドウ」, not 「窓」.
- Use 「コントローラー」 and 「シミュレーター」 with the long vowel mark.
- Use 「ローテーション」 for rotation in prose.
- Keep `NodeClaim`, `NodePool`, `placeholder`, `surge`, `drain`, `cordon`,
  `freeze`, and `backstop` in English where they name project concepts.
- Show durations with concrete syntax such as `168h`, `30m`, or `2h30m`.
- Use Japanese headings in translated specifications and runbooks.
- Use descriptive links in user-facing pages; section-only references are
  acceptable inside the specification.

In Japanese fenced code blocks, translate comments only. Commands, identifiers,
YAML keys and values, pseudocode, and program output MUST remain identical to
the English source.

Keep identifier-derived terms such as `stuck-drain` in English when they name a
metric or mechanism. Translate the surrounding prose; for example, use
「詰まった drain」when describing the condition rather than naming it.

## 6. Translation policy

Translation requirements depend on document type; the existence of a Japanese
directory does not make every English document subject to the same policy.

| English source | Japanese counterpart | Same-PR obligation |
|---|---|---|
| `docs/specification/` | `docs/ja/specification/` | **MUST** update |
| `docs/reference/perf/` | `docs/ja/reference/perf/` | **MUST** update |
| `docs/reference/adr/` | None; English-only | **MUST NOT** translate |
| Other documents | Existing page, if any | Update when the task requires it |

For the specification:

- headings, section numbering, tables, and diagram structure MUST remain aligned
  between English and Japanese
- translated labels and prose MAY differ in length
- identifiers, annotation keys, metrics, and commands MUST remain exact
- both languages MUST describe the same behavior, status, and limitations

For translated performance notes, results, conditions, and limitations MUST
remain semantically equivalent. Matching layout is preferred but not required
when a language-specific presentation is clearer.

If a change outside the mandatory sets updates only one of an existing EN/JA
pair, state that scope explicitly in the change description. Do not imply that
an untouched translation has been synchronized.

## 7. Safe change process

Before editing:

1. identify the page's audience and document profile
2. read the relevant canonical specification, ADR, and nearby related pages
3. determine whether the change affects behavior or only presentation

While editing:

- preserve technical meaning and distinguish current, planned, and validated
  behavior
- do not make broad rewrites solely to enforce stylistic uniformity
- do not fabricate design decisions, compatibility claims, test results, links,
  commands, or examples
- update affected cross-references and mandatory translations in the same change
- report ambiguity or source conflicts instead of guessing

After editing, review the diff as a reader and verify that:

- the requested information is easy to find
- prerequisites, limitations, and destructive actions remain visible
- terminology and links are consistent
- no unrelated content was rewritten

## 8. Validation and definition of done

Run the checks relevant to the changed files:

```bash
npm run docs:build
npm run test:docs:pure
git diff --check
```

The documentation change is complete when:

- the rendered documentation builds without new broken links or Markdown errors
- documentation tests pass
- mandatory EN/JA counterparts are synchronized
- commands and examples have been checked against the implementation or an
  authoritative source
- any check that could not be run is reported with the reason

## 9. Adding support for another AI agent

Keep this file agent-independent. When the project adopts another AI agent, add
a small adapter in that agent's native instruction location rather than copying
this guide.

An adapter SHOULD:

1. activate only for the documentation files covered by this guide when the
   agent supports path-based matching
2. require the agent to read this file in full before editing
3. name the protected areas: authority, translation, safe changes, and validation
4. explain that inconsistent output can break specification accuracy,
   operational safety, or EN/JA synchronization
5. identify this file as the single source of truth

Keep adapter content short. A duplicated guide will drift and turn agent-specific
files into competing sources of truth. If an agent cannot reliably follow a
pointer, enforce the verifiable parts through repository tests and review rather
than weakening or silently forking the shared standard.
