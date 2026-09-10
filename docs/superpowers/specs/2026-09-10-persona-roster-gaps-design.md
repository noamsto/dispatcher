# Persona roster gaps — the anti-over-building seam and an ecosystem survey

**Status:** design — revision 1
**Date:** 2026-09-10
**Repo:** dispatcher (personal / GitHub) · default branch `extract`
**Closes:** #116

**Artifacts touched:**

| file                                              | what changes                                                                                                                                                                                                       |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `adapters/core/reviewers/agent-docs-reviewer.md`  | new — the one persona this branch adds (Part 2b)                                                                                                                                                                   |
| `adapters/core/reviewers/*.md` (the other eleven) | `go-`, `python-`, `typescript-`, `terraform-`, `security-`, `shell-`, `yaml-`, `nix-`, `charm-tui-`, `postgres-`, `sqlite-reviewer`: one skeleton, the shared tail, idioms and padding cut, `globs:`/`when:` fixed |
| `adapters/core/critics/plan-critic.md`            | axis 7, **Over-building** (Part 2a)                                                                                                                                                                                |
| `adapters/core/critics/spec-critic.md`            | the explicit `revise` ⇒ blocking rule; scope-creep axis extended (Part 2a)                                                                                                                                         |
| `tests/adapters.bats`                             | new blocks: tail identity, idiom sweep, severity ladder, routing coherence, README count, protocol mapping                                                                                                         |
| `tests/fixtures/reviewer-tail.md`                 | new — the shared tail's independent source of truth                                                                                                                                                                |
| `README.md`                                       | the roster count and list move to twelve                                                                                                                                                                           |
| `adapters/core/protocols/WORKER_PROTOCOL.md`      | one add-only sentence: the severity mapping onto `review_high`                                                                                                                                                     |
| `adapters/core/protocols/REVIEW_TASK.md`          | one add-only sentence: the severity mapping onto the review-worker tally                                                                                                                                           |
| generated trees                                   | `scripts/gen-adapters.sh` (claude-code, codex, cursor)                                                                                                                                                             |

## Problem

The reviewer and critic bodies in `adapters/core/` ship verbatim to claude, codex and cursor, and each body _is_ the subagent's whole brief. They were written at different times against a claude-only agent registry and a separate pull-request review command, and the roster showed every seam: skill paths under `~/.claude`, one employer's monorepo paths, `gh pr view` runbooks at a gate where no pull request exists, descriptions phrased for an auto-invoke matcher only one of the three engines has.

Underneath sat three structural problems. There was no shared shape, so six bodies opened with a runbook and five did not, and the orientation paragraph was repeated nine times in nine wordings. The severity vocabulary — a live interface, since `review_high` counts HIGH findings and the review worker tallies its own three words — had four competing ladders, one of which (`blocker | should-fix | nit`) could never count toward `review_high` at all. And the reviewers carried no signal discipline, though a HIGH costs the worker a fix round and is the number `crew rate` reads.

Two smaller problems: padding, since a body is injected on every review of a matching diff on every worker forever, and routing drift, where a reviewer checked a file type its `globs:` did not route.

Then a real gap. Changed-path occurrences over this repo's last 200 commits, generated adapter trees excluded:

| file type | occurrences              |
| --------- | ------------------------ |
| markdown  | 145 (93 outside `docs/`) |
| bats      | 109                      |
| shell     | 80                       |
| nix       | 21                       |
| yaml      | 5                        |

The top file type is agent-facing prose — protocols, skills, commands, reviewer and critic bodies — and it had no reviewer. It fell through to the general `find-bugs` security checklist, which has nothing to say about prose.

## Goal

- One skeleton and one byte-identical shared tail across every reviewer, so the review gate gets the same contract from every persona on every engine.
- One severity vocabulary, `CRITICAL / HIGH / MEDIUM`, with its mapping onto `review_high` and onto the review-worker tally stated once per protocol and pinned by test.
- No engine-, repo- or employer-specific idiom in any shipped body.
- A coherent `globs:`/`when:` table: every `when:` names a discriminator a spawner can check, and no glob-matched file type can end with an empty batch.
- Part 2's two questions each answered with a recommendation. Exactly one persona added and defended; every other candidate argued down here or filed.

## Non-goals

- Changing the review gate's mechanism, `scripts/gen-adapters.sh`, or `nix/hm-module.nix`.
- Editing the two protocols beyond the add-only severity sentences. Existing sentences are test-pinned verbatim and are not reworded.
- Vendoring ponytail, adding an ambient-rules hook, or adding a "lean code" reviewer.
- Touching the user-level agents of the same names in the nix-config repo.
- Aligning `adapters/core/commands/autopilot.md`'s reviewer table, which names `database-reviewer` / `expo-mobile-reviewer` and triages must-fix/should-fix/nit. Filed as #118.
- Routing extensionless shebang scripts. Filed as #119; the mechanism reason is below.

## Part 1 — what changed in the thirteen

### The skeleton

Every reviewer body now has the same shape in the same order: frontmatter (`name`, `description`, `globs`, optional `when`), one identity paragraph saying why defects in this domain cost what they cost, an optional domain-specific `## Orientation`, `## Review priorities` with `### CRITICAL` / `### HIGH` / `### MEDIUM`, an optional `## Diagnostics`, and `## Findings and verdict`. Descriptions are for a human reading the roster; routing is `globs:`/`when:` alone, so no description carries auto-invoke phrasing.

`## Orientation` survives only where it says something a generic instruction cannot: nix (read `flake.nix` and the options tree), terraform (read whole files, verify provider semantics), charm-tui (find sibling render paths, note the lipgloss major version), postgres (the house assumptions to confirm).

### The shared tail, and why reviewers now carry an anti-inflation rule

From the `## Findings and verdict` heading to end of file, all twelve bodies are byte-identical, and the same text lives at `tests/fixtures/reviewer-tail.md` as an independent source of truth outside the roster directory. The tail carries scope and authority (review only, nothing beyond task doc, diff and brief), the check-the-base rule that keeps a pre-existing idiom from being filed as new, the house-conventions rule, the never-print-a-secret rule, the finding format, the severity ladder, group headings, and one verdict line.

The judgment call the task asked for was whether reviewers need the anti-inflation rule the critics already carry. They do, because HIGH is load-bearing twice over. It is a cost lever: a HIGH buys the author a fix round and, on the deep tier, a second review batch. It is also the rated number, because `review_high` is what `crew rate` reads when it rolls up run quality. A reviewer that promotes a MEDIUM to force a fix is therefore not merely noisy — it inflates the metric the whole crew is judged on. So the tail says it outright: do not pad to look thorough, do not raise a MEDIUM to HIGH to force a fix, and a clean diff gets "No findings" rather than a manufactured one. Modernization nudges that previously sat under HIGH headings as if they were defects are cut or demoted.

### The severity vocabulary and where it lands

The roster grades on three tiers. **CRITICAL** is a security hole, data loss, a silent wrong result, or code that will not build. **HIGH** is a bug that ships if unfixed, with a nameable input or state that produces a wrong result. **MEDIUM** is a correctness risk or maintenance cost worth fixing now, including a clarity finding where two competent readers would disagree about what the code does. Nothing lower; style is the linter's.

Two add-only sentences state where those words go, so nothing is re-derived at a gate. `WORKER_PROTOCOL.md` says a CRITICAL is HIGH-severity for `review_high` and for the deep tier's second re-review, and that a match set a `when:` line empties falls through to the general reviewer exactly as no match does. `REVIEW_TASK.md` says CRITICAL maps to `blocker`, HIGH to `should-fix`, MEDIUM to `clarity`. Neither redefines the field; both are pinned by test.

### The routing table

| reviewer              | globs                                                                                   | when                                                                                                                                                                                                           |
| --------------------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `go-reviewer`         | `*.go`, `go.mod`, `go.sum`                                                              | every Go diff; never skipped in favour of `charm-tui-reviewer`                                                                                                                                                 |
| `charm-tui-reviewer`  | `*.go`                                                                                  | only when the diff imports `charmbracelet/bubbletea`, `bubbles` or `lipgloss`; always alongside `go-reviewer`                                                                                                  |
| `nix-reviewer`        | `*.nix`, `flake.lock`                                                                   | —                                                                                                                                                                                                              |
| `shell-reviewer`      | `*.sh`, `*.bash`, `*.bats`, `.envrc`                                                    | —                                                                                                                                                                                                              |
| `python-reviewer`     | `*.py`, `pyproject.toml`, `requirements*.txt`                                           | —                                                                                                                                                                                                              |
| `typescript-reviewer` | `*.ts`, `*.tsx`, `*.js`, `*.jsx`, `*.mjs`, `*.cjs`, `tsconfig*.json`                    | —                                                                                                                                                                                                              |
| `yaml-reviewer`       | `*.yaml`, `*.yml`                                                                       | —                                                                                                                                                                                                              |
| `terraform-reviewer`  | `*.tf`, `*.tfvars`, `*.tfvars.json`, `*.tftpl`, `.terraform.lock.hcl`, `terragrunt.hcl` | — (`yaml-reviewer` owns stack YAML)                                                                                                                                                                            |
| `postgres-reviewer`   | `*.sql`                                                                                 | repo has an `atlas.hcl` or a `sqlc.yaml`/`sqlc.json` (these win over any SQLite marker); also the default for a `*.sql` diff with none of those markers. Never with `sqlite-reviewer`                          |
| `sqlite-reviewer`     | `*.sql`                                                                                 | no `atlas.hcl` and no `sqlc.yaml`/`sqlc.json`, plus either a `d1_databases` binding in `wrangler.toml`/`wrangler.json*` or SQLite-only syntax (`STRICT`, `WITHOUT ROWID`, `INTEGER PRIMARY KEY AUTOINCREMENT`) |
| `security-reviewer`   | `[]`                                                                                    | the diff touches an auth, crypto, input-parsing, SQL, or network path — when in doubt, include it                                                                                                              |
| `agent-docs-reviewer` | `*.md`, `*.mdc`                                                                         | only prose whose reader is an agent: `AGENTS.md`/`CLAUDE.md`, a skill, command, agent, reviewer, critic, rule or protocol body; not a README, changelog or design doc                                          |

The postgres/sqlite pair is disjoint and total with Postgres markers taking precedence, so exactly one runs for any `*.sql` diff and a Postgres repo carrying a test-only SQLite driver still routes to postgres. Dependency sniffing is deliberately not a discriminator. The one remaining shared-glob overlap, go plus charm-tui, is intentional, and both entries name the other in `when:`.

### Before and after

| body                  | before | after |
| --------------------- | ------ | ----- |
| `agent-docs-reviewer` | 0      | 46    |
| `charm-tui-reviewer`  | 57     | 63    |
| `go-reviewer`         | 128    | 78    |
| `nix-reviewer`        | 68     | 62    |
| `postgres-reviewer`   | 73     | 65    |
| `python-reviewer`     | 141    | 95    |
| `security-reviewer`   | 112    | 75    |
| `shell-reviewer`      | 108    | 80    |
| `sqlite-reviewer`     | 69     | 60    |
| `terraform-reviewer`  | 126    | 99    |
| `typescript-reviewer` | 102    | 75    |
| `yaml-reviewer`       | 97     | 77    |
| `plan-critic`         | 25     | 26    |
| `spec-critic`         | 26     | 27    |

The three bodies that grew did so because the shared tail is longer than what charm-tui had, and because the critics gained rules. Everything else shrank by cutting text, not domain surface.

### Per-file change log

- **`go-reviewer`** — modernization collapsed to "run the modernizer; do not hand-flag what it flags"; style-only lines cut (function length, nesting depth, table-driven tests, interface pollution, package naming, error-message case).
- **`python-reviewer`** — best-practices and modernization cut to three lines; the pandas block and the "this repo's choice" note on a type checker dropped.
- **`typescript-reviewer`** — the `gh pr view` runbook and two "modern X" sections dropped; `var`/`==`/return-type lines demoted to MEDIUM; `JSON.parse` reframed as untrusted input parsed without validation; `tsconfig*.json` added to `globs:`, which the body already checked.
- **`terraform-reviewer`** — the description no longer claims stack YAML; the two added globs each earned a body line, a lock diff moving a provider across a major version or missing the platform hashes CI builds on, and `dependency` `mock_outputs` masking a missing real output.
- **`security-reviewer`** — Core Responsibilities, Key Principles, When to Run, Emergency Response, the LOW tier and the Cloudflare line dropped; refocused from "web applications" to this harness's CLI tools, shell, infra, workers and web services.
- **`shell-reviewer`** — `.envrc` added to `globs:`; the named rules-directory paragraph removed in favour of the tail's neutral wording.
- **`yaml-reviewer`** — auto-invoke phrasing removed from the description and the named rule-file examples de-specified; the Helm umbrella-chart check kept.
- **`nix-reviewer`** — orientation kept, stripped of its "every Nix repo is laid out differently" padding and its duplicate house-conventions instruction.
- **`charm-tui-reviewer`** — the skill path under `~/.claude` and the named repo removed; the trap catalog kept as the body's substance; severity words now come from the tail.
- **`postgres-reviewer`** — one employer's monorepo paths, three internal skills and the "do NOT post comments or block" clause removed; the `blocker | should-fix | nit` ladder replaced, which is what previously kept its findings out of `review_high`.
- **`sqlite-reviewer`** — the description no longer claims Postgres repos ship their own project-level reviewer; `when:` now names checkable markers.
- **`plan-critic`** — axis 7, Over-building. **`spec-critic`** — the explicit `revise` ⇒ ≥1 blocking rule, and the scope-creep axis extended to a solution shape larger than the problem.

### A routing limitation, not a routing bug

An extensionless shebang script cannot reach `shell-reviewer`. The protocol's mechanism is "match your changed paths against every `globs:`, honour each matched reviewer's `when:`", so `when:` can only filter a set some glob already produced, never add a file no glob matched. The only additive shape is `globs: []` plus a `when:`, and that is reserved for `security-reviewer`. Fixing it means changing the routing mechanism, which is out of scope for this branch; filed as #119.

## Part 2a — ponytail and the anti-over-building seam

ponytail (github.com/DietrichGebert/ponytail) injects a "lazy senior dev" ruleset through lifecycle hooks, UserPromptSubmit and PreToolUse, on every turn and into spawned subagents, with a regex environment variable scoping which ones receive it. It has modes lite, full, ultra and off. Its core decision ladder is asked before code exists: does this need to exist, is it already in this codebase, does the standard library do it, is there a native platform feature, is it an installed dependency, is it one line, and only then the minimum that works. It deliberately keeps safety guardrails off the chopping block — trust-boundary validation, data-loss handling, security, accessibility — and claims roughly 54% less code across its benchmark tasks.

Where this harness covers that seam today, stated honestly. The user-level shared conventions file carries a "lean by default" rule: no speculative robustness, no escape hatches, delete rather than preserve, stay in scope. It reaches every session on the user's own machines, and it is not a harness guarantee for any other install. `/deslop` runs before every push, but it is post-hoc and targets slop shape — bloated comments, defensive blocks, casts — not a library-versus-one-liner decision. The critics carried a scope-creep axis on the spec side and a scope-breach blocking rule on the plan side, but nothing that named over-building.

**Recommendation.** Take ponytail's central claim seriously and act on it, without taking its mechanism. If the leverage really is before the first line, a reviewer is structurally the wrong home: it fires on a diff, which is after the over-building happened and after the author sank the work. The injection mechanism does not fit either, because this harness deliberately hands workers a protocol and a roster rather than ambient rules stitched into every turn, and a rule that arrives by hook is invisible to the two engines whose contract is the shipped body. So the fix that landed is at the planning gate: one over-building axis on `plan-critic`, blocking when a step adds a dependency or a new module and a note otherwise, plus one clause on `spec-critic`'s scope-creep axis for a solution shape larger than the problem. No vendoring, no hook, no fourteenth reviewer. What remains uncovered should be said plainly: an execute subagent running on a machine that is not the user's gets no lean-code rule at all, because that rule lives in user-level configuration rather than the shipped protocol. The `plan-critic` axis is the harness-level guarantee, and it is the only one.

## Part 2b — the wider persona ecosystem

Three collections are worth measuring against. wshobson/agents ships 202 agents organised by domain and tiered by model. VoltAgent's awesome-claude-code-subagents ships 158+ across 10 categories with YAML frontmatter carrying `name`, `description`, `tools` and `model`; the relevant names are docs-drift-editor, ai-writing-auditor, terraform-engineer, terragrunt-expert, docker-expert, sql-pro, postgres-pro, kubernetes-specialist, security-auditor, code-reviewer, architect-reviewer, and five powershell entries — and notably no nix, no bash or shell, and no Bubble Tea entry. SuperClaude ships about 20 agents activated by context through "behavioral instruction injection", an `/agent` command, and seven behavioral modes.

The workload this harness runs against is nix, shell, go, typescript, python, terraform, yaml, sql and charm TUIs. Two of those three collections have nothing for the two file types this repo changes most, and the breadth that makes them look comprehensive is what makes most entries dead weight here: a roster entry that never matches a diff still has to be maintained and still ships to three engines.

**Recommendation.** One real gap, one persona. `agent-docs-reviewer` is added, routed on `*.md` and `*.mdc` with a `when:` that narrows to agent-facing prose. Its checklist is diff-actionable at all three tiers: CRITICAL for engine leakage in a body that ships engine-neutral, a rule stated in two places that now disagree, and a test-pinned phrase changed without its test; HIGH for an instruction no reader can act on, a doc naming a path, command, flag or field that does not exist, and a step order that contradicts a stated dependency; MEDIUM for the same rule restated in three places, a missing why on a non-obvious constraint, and leading words that bury the trigger. The evidence is the file mix above, plus the regression the task document itself cites under "watch for": a verbatim-pinned phrase changed by an unrelated edit, which survived three checks. The general `find-bugs` checklist is a security sweep and has nothing to say about any of that.

Every other candidate is argued down here rather than filed, so the record is one document rather than a backlog of persona tickets.

**Dockerfile.** A nix-first shop builds images with `dockerTools`, so a Dockerfile diff is rare in this workload, and when one appears the compose side is covered by `yaml-reviewer` and the image-provenance and privilege questions by `security-reviewer`. The marginal review quality does not pay for another body shipped to three engines.

**Rust, Swift, Kotlin.** No repo in the workload list is written in any of them. The time to add a language reviewer is when a diff in that language appears at the gate; adding one now buys a body that never matches.

**Frontend CSS and accessibility.** There is no design-system repo in the workload, and `typescript-reviewer`'s React block already covers where component-level defects show up. Accessibility is a real concern in a product with a rendered UI; it is not one this harness's diffs raise, and ponytail's own accessibility carve-out is about not deleting guardrails, not about reviewing them.

**Performance.** Performance is a cross-cutting lens, not a file type, so it is not glob-routable in the first place. Each language reviewer already owns its MEDIUM performance lines, which is where such a finding can name a pattern in a diff rather than an aspiration.

**The role-shaped generalists — architect, test-engineer, docs-writer, code-reviewer.** The most common entries in public collections and the worst fit here. They are role-shaped rather than file-shaped, so nothing in the routing table can select them, and each duplicates something the harness already has. Architecture judgment belongs to the critics at the plan and spec gates. Test quality is a section in every language reviewer. Documentation review is now `agent-docs-reviewer` for the prose that matters. A general code reviewer is `find-bugs`.

**Expo and React Native.** Covered twice over. `typescript-reviewer`'s React block handles the component-level defects, and a same-name native agent already exists on the user's claude registry, which the protocol prefers when one is present. A roster copy would create a second definition of the same persona, free to drift from the first.

Two mechanism issues came out of this work and were filed rather than fixed on this branch: #118 for the `autopilot.md` reviewer-table drift, and #119 for the shebang routing limitation above. No persona candidate got an issue, because each is argued down here.

## Verification

- Tail identity: every reviewer's text from `## Findings and verdict` to end of file equals `tests/fixtures/reviewer-tail.md`.
- Idiom sweep: every reviewer and critic is grepped for `~/.claude`, `Agent tool`, `Task tool`, `MUST BE USED`, `PROACTIVELY`, `gh pr `, `Emergency Response`, the old repo and employer names, and `model:`, failing with file and line.
- Severity: every `### ` heading in a body is CRITICAL, HIGH or MEDIUM, and no body contains `should-fix`, `blocker |`, `NOTE:` or `LOW`.
- Routing, computed over the frontmatter rather than a hardcoded pair list: every glob shared by two entries carries a `when:` on both; the postgres and sqlite `when:` lines name their markers and neither mentions dependencies; typescript routes `tsconfig*.json`, shell routes `.envrc`, and the terraform description no longer says YAML.
- `README.md` pins twelve reviewers and their domains; both protocol severity sentences are pinned with `grep -F`.
- The branch-base `bats tests/` summary was recorded before any prose edit, so every "pre-existing failure" claim is checked against the base, not a mid-branch commit.

## Risks

- **The shared tail now has two copies**, the reviewers and the fixture. That is the deliberate trade: a fixture outside the roster directory cannot be mistaken for a persona and gives the invariant an independent source of truth, and the test fails the moment they diverge.
- **Cutting a body can cut domain surface.** Every cut named above is a style line, a duplicate of the tail, or a runbook step that no longer applies at the worker gate; the checklists that name a pattern and a consequence were kept.
- **The over-building axis can produce false blocks.** A plan step that genuinely needs a new dependency now draws a blocking finding and costs a revision round. The axis is scoped to a dependency or module no acceptance criterion needs, and the critic's cap of two revisions bounds the cost.
- **`agent-docs-reviewer`'s `when:` is a judgment call.** A design doc that reads like a protocol could draw it in, and a rule file phrased like a README could slip past. A miss falls through to the general reviewer by the protocol sentence above, so the failure mode is a weaker review, not an empty batch.
- **The lean-code gap outside the user's machines stays open** until a lean rule lives in the shipped protocol rather than user-level configuration. Part 2a states it rather than papering over it.
