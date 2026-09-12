# Spec — route extensionless shebang scripts to a language reviewer (#119)

Revision 3. Revision 2 closed B2 and B3; this closes the remaining B1/B4 gaps and the
three blocking findings from the second critic pass.

## Problem

The review gate in `adapters/core/protocols/WORKER_PROTOCOL.md` routes a diff by
matching changed paths against each reviewer's `globs:`, then honouring the matched
reviewer's `when:`. `when:` can only _narrow_ a glob match; it can never _add_ a file
that no glob matched. The one additive shape — `globs: []` plus a `when:` — is reserved
for `security-reviewer`.

Consequence: a changed script with no filename extension (`bin/foo`, `scripts/deploy`)
whose first line is `#!/usr/bin/env bash` matches no `globs:` entry, so no language
reviewer ever reads it.

The fallback does **not** rescue it. The protocol falls back to the general `find-bugs`
reviewer only when _nothing_ in the diff matched. In the motivating case — a Nix repo
changing `flake.nix` **and** `bin/foo` — `nix-reviewer` matches, the batch is non-empty,
and the shell script is reviewed by **nobody**. The bug is a silent hole, not a
downgrade to a generic reviewer, and it is widest exactly where extensionless scripts
are most common.

## Ground truth established before designing

- **No shipped/runtime code parses reviewer frontmatter.** `grep` over
  `adapters/**/*.sh`, `nix/`, and `scripts/` finds zero readers of `globs:`. Routing is
  prose in `WORKER_PROTOCOL.md` interpreted by the worker, over declarative frontmatter.
  (`tests/adapters.bats` _does_ parse frontmatter with `yq`; that is test code, and it is
  the enforcement mechanism this spec relies on.)
- **The roster ships byte-identical to all three engines** (`gen-adapters.sh` copies
  `adapters/core/reviewers/` verbatim; `tests/adapters.bats` pins it with `cmp -s`).
- **`WORKER_PROTOCOL.md` ships identically** to the claude-code and codex plugin trees,
  also `cmp -s`-pinned. Cursor resolves it through `DISPATCHER_PROTOCOL_DIR`.

Therefore "a step on the spawn path on three engines" is **one clause in one shared
sentence**, not three edits. That collapses the main cost the task doc argues against
design 2.

## Decision

**Adopt design 2: a `shebang:` frontmatter key beside `globs:`, honoured by the routing
sentence.**

### Why, over design 1 (content probe hardcoded in the routing sentence)

1. **Design 1 puts a language inside a language-agnostic rule.** "…counts as matching
   `*.sh`" makes the universal routing rule reach into one reviewer's private glob list.
   It must be re-edited for every future interpreter, and it couples routing correctness
   to `shell-reviewer` continuing to own `*.sh`.
2. **`shebang:` is the same shape as `globs:`.** Both are per-reviewer declarative match
   criteria in frontmatter. The roster schema is what every persona body shares after
   #116; extending it in its own idiom beats a special case in prose.
3. **The generality is real, not speculative.** `#!/usr/bin/env python3` in `bin/foo` is
   the identical hole for `python-reviewer`, so that reviewer opts in **in the same
   change** and the key has two consumers on day one.
4. **The cost difference is one frontmatter line per opting-in reviewer.**

Design 1 is rejected on (1): it is the cheaper _diff_ but the wrong-shaped _contract_,
and this issue exists precisely because the previous routing shortcut (`when:`) was the
wrong shape.

## The probe, defined exactly

The routing sentence must carry a predicate precise enough that three different workers
on three engines reach the same answer. The comparison is **anchored on the declared
entry**, never a transform applied to an unknown word — transforming first would mangle
names whose digits are load-bearing (`m4` → `m`, `runghc-9.4` → `runghc-`).

- **Which files** — a changed file whose basename has **no extension**: no `.` after its
  first character. `bin/foo` and `scripts/deploy` qualify; `foo.sh` does not.
- **Which content** — the file's first line **as it exists in the worktree after the
  change**. Not the diff hunks: a modified script's shebang is usually unchanged and so
  absent from the diff, which is the common case and would otherwise silently fail. A
  deleted file is not probed.
- **The interpreter word** — drop the leading `#!` and split the rest on whitespace
  (`#! /bin/sh` and `#!/bin/sh` tokenise identically). If the first token's last path
  segment is `env`, drop it, then drop each following token that is an option (`-S`,
  `-u`, `--split-string`) or a `NAME=value` assignment — except that an option carrying
  the command inline (`-Sbash`, `--split-string=python3 -u`) supplies it: its value's
  first whitespace-separated word is the interpreter word. Otherwise the interpreter
  word is the first token left. If none is left, the file does not match.
- **The match** — reduce the interpreter word to its last path segment. It matches a
  declared entry `E` when the segment is exactly `E`, or `E` followed only by a version
  suffix: an optional `-` or `.`, then digits, then any further `.`-separated digits.
  Case-sensitive.

Worked: `#!/bin/sh` → `sh`. `#! /bin/sh` → `sh`. `#!/usr/bin/env bash` → `bash`.
`#!/bin/bash -e` → `bash`. `#!/usr/bin/env -S PYTHONPATH=lib python3` → `python3`,
matching `python`. `#!/usr/bin/env --split-string=python3 -u` → `python3`.
`#!/usr/bin/python3.11` → `python3.11`, matching `python`. `#!/usr/bin/m4` → `m4`,
matching nothing, because no reviewer declares `m` or `m4`. Note the separator is
**optional**, so a declared `m` would match `m4` — anchoring on the declared entry
removes the mangling bug, not every collision. That is what the coherence check in
R7 exists to catch, and it is why `python` and `python3` must not be declared by two
different reviewers.

Because the suffix is matched rather than stripped, declared lists stay minimal:
`python`, not `python`/`python3`.

## Requirements

- **R1** — `shell-reviewer` declares `shebang: ["sh", "bash"]`.
- **R2** — `python-reviewer` declares `shebang: ["python"]`.
- **R3** — The routing bullet in `adapters/core/protocols/WORKER_PROTOCOL.md` states the
  rule **once**, engine-neutrally, carrying the file-selection, content-source,
  interpreter-word and anchored-match clauses above. The clause is **additive around the
  existing verbatim-pinned strings** in that bullet (`tests/adapters.bats:391` pins
  `Nothing matched: one general reviewer running the `find-bugs` skill.`, and `:543-547`
  pins four more) — those bytes must not change. `Nothing matched` now means neither a
  glob nor a shebang matched; its meaning widens, its bytes do not.
- **R4** — `WORKER_PROTOCOL.md:165` currently reads "the roster entries the changed
  files' `globs:` matched", which becomes false. It changes to "the roster entries the
  changed files matched" — neutral, so the rule stays stated once.
- **R5** — `tests/adapters.bats` pins, each red when removed: the new protocol clause;
  the reworded line 165; the `shebang:` declarations of R1 and R2.
- **R6** — A once-ness assertion: the distinctive shebang-rule phrase occurs exactly
  once in `WORKER_PROTOCOL.md`, so a later edit cannot restate the rule elsewhere.
- **R7** — The routing-coherence test learns `shebang:` with an **unconditional**
  assertion: the interpreter map is non-empty, and every declared interpreter is claimed
  by exactly one reviewer. It must not hide inside an `if count >= 2` branch — with
  today's disjoint lists that branch would never execute, reproducing the dead-check
  shape #116 caught and the task doc forbids. It must also not pass vacuously when every
  `shebang:` key is deleted, which is why non-emptiness is asserted. The test carries an
  explanatory comment in the style of its #116 neighbours, saying why interpreters are
  disjoint where globs may be shared.
- **R8** — `every reviewer carries a routable frontmatter` counts `shebang:` as a routing
  key, so a future `shebang:`-only reviewer is not failed as unreachable.
- **R9** — A committed fixture tree `tests/fixtures/shebang-routing/` holds `bin/foo`
  (`#!/usr/bin/env bash`) and `bin/bar` (`#!/usr/bin/env python3`), both extensionless,
  both valid in their language. A test asserts the premise the evidence run depends on:
  each fixture file is extensionless and its first line is the stated shebang.
- **R10** — Prose describing routing as glob-only is corrected: `README.md:158` ("whose
  `globs:` frontmatter routes a diff") and `README.md:345` ("engine-neutral reviewer
  roster, glob-routed"). The `README.md:158` edit must not introduce a blank line inside
  the `**Two rosters, spawned three ways.**` paragraph and must keep all twelve domain
  words — `tests/adapters.bats:740-746` slices that paragraph to the first blank line.
- **R11** — Existing invariants keep passing: verbatim roster ship, shared tail,
  engine-neutral idiom, severity ladder, `twelve engine-neutral`.
- **R12** — Generated adapter trees regenerated by `scripts/gen-adapters.sh`; CI drift
  gate clean; `shellcheck` clean.

## What is and is not testable

This repo has **no automated routing oracle**, and cannot have an honest one: the router
is an LLM worker reading prose. A bats test that re-implemented the predicate and matched
it against a fixture would be asserting on its own reimplementation, not on the rule —
the #116 self-asserting-test shape.

So the R5–R7 pins are the achievable mechanical ceiling: they prove the rule is _stated_
and _declared_, and go red the moment either is removed. Acceptance 1 is closed by an
**observed evidence run** with a fixed input and a stated pass condition:

- **Input** — a fresh-context reader receives the routing bullet, the full roster
  frontmatter, read access to `tests/fixtures/shebang-routing/`, and the changed-file
  list `tests/fixtures/shebang-routing/bin/foo`,
  `tests/fixtures/shebang-routing/bin/bar`, `flake.nix`.
- **Pass condition** — the chosen batch is exactly
  `{shell-reviewer, python-reviewer, nix-reviewer}`.
- **Failure branch** — any other batch means the prose is unclear, not that the reader
  is wrong: reword the clause and rerun, and record **both** runs in the PR body.

## Non-requirements

- No new reviewer persona.
- No change to `security-reviewer`'s reserved `globs: []` + `when:` shape.
- **No synthetic routing simulator** in the test suite — see above.
- No shebang opt-in for reviewers whose languages have no shebang convention (Nix, YAML,
  Terraform, Go, TypeScript, SQL, Bubble Tea, agent-docs).
- **No `zsh`/`fish`/`perl`/`ruby` interpreters.** `shell-reviewer`'s body is
  bash/POSIX-scoped and its globs already exclude `*.fish`; declaring interpreters it
  does not review would route diffs to a persona with nothing to say about them.
- **The hole stays open for undeclared interpreters.** `bin/deploy` with
  `#!/usr/bin/env ruby`, alongside a matching `flake.nix`, is still reviewed by nobody.
  This change narrows the hole to shell and Python, the two cases that motivated it; it
  does not close it, and the Problem section should not be read as claiming otherwise.
- No arbitration mechanism for a shared interpreter. R7 forbids sharing outright; if a
  future reviewer needs it, that edit adds the mechanism then.
- No dotfile special case. `.envrc` is extensionless under the rule above, so
  `shell-reviewer` matches it by both glob and shebang. That is the same reviewer twice,
  which changes no batch, and is left alone deliberately.

## Acceptance

1. The evidence run above returns exactly `{shell-reviewer, python-reviewer,
nix-reviewer}`, quoted in the PR body.
2. The rule is stated once, in the routing bullet, engine-neutrally — pinned by R6.
3. **Demonstrated red, not asserted**, each reverted after: delete the protocol clause →
   suite fails; delete `shebang:` from `shell-reviewer.md` → suite fails; add a duplicate
   `bash` to a second reviewer → the R7 assertion fails.
4. Full `bats tests/` green; `scripts/gen-adapters.sh` idempotent; `shellcheck` clean.
5. PR body names design 1 and argues it down. `Closes #119`.
