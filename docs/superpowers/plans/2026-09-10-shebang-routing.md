# Plan — `shebang:` routing key (#119)

Revision 2. Implements the accepted `SPEC.md` rev 3. The spec-critic's nine notes and the
plan-critic's two blocking findings plus notes are folded into the steps that own them.

Base: `git merge-base HEAD extract` = `7d7f237`. Baseline `bats tests/adapters.bats` is
58/58 green — verified before any edit.

## The regeneration rule (plan-critic B1)

`scripts/gen-adapters.sh` copies `adapters/core/reviewers/` and `adapters/core/protocols/`
into three trees, and two tests `cmp -s` source against copy: `every canonical protocol
exactly matches both shipped protocol trees` (`tests/adapters.bats:162`) and `the reviewer
roster ships verbatim into every adapter` (`:497`).

**Therefore: every edit under `adapters/core/` is followed by `./scripts/gen-adapters.sh`
before bats is run at all.** Otherwise those two drift tests go red for a reason that has
nothing to do with the change, and a real regression hides behind the noise.

## The clause to be written (drafted here so Steps 4 and 7 can name it)

Into the `**The reviewers themselves ship with the harness.**` bullet
(`adapters/core/protocols/WORKER_PROTOCOL.md:155`), the match sentence becomes:

> …and, where a pattern cannot express the trigger, a `when:` line. A reviewer may also
> carry `shebang:`, interpreter names that route an **extensionless** changed file by its
> first line. Match your changed paths against every `globs:`, then probe every
> extensionless changed file against every `shebang:`, then honour each matched
> reviewer's `when:`, and that set is the batch below. Nothing matched: …

and a continuation paragraph, in the shape of the existing line 156, states the probe:

> **The shebang probe.** A changed file is extensionless when its basename has no `.`
> after its first character. Read its first line as it stands in the worktree after the
> change — not from the diff hunks, since a modified script's shebang is usually unchanged
> and therefore absent from them — and skip a file the change deleted. The line must start
> with `#!` or nothing matches. Drop the `#!`, split on whitespace, and if the first
> token's last path segment is `env`, drop it and every following token that begins with
> `-` or has the form `NAME=value`; an option carrying the command inline (`-Sbash`,
> `--split-string=python3 -u`) supplies it as the first word of its value. The interpreter
> word is the first token left, or nothing matches. Reduce it to its last path segment: it
> matches a `shebang:` entry when it equals that entry, or equals that entry followed only
> by a version suffix — an optional `-` or `.`, then digits, then any further
> `.`-separated digits.

Two ordering choices are deliberate: the probe sits **before** the `when:` narrowing, so a
future reviewer pairing `shebang:` with `when:` inherits the right order `[n7]`; and
`Nothing matched:` keeps its bytes while its meaning widens to "neither a glob nor a
shebang matched" `[n6]`.

## Step 1: declare the interpreters

- [ ] `adapters/core/reviewers/shell-reviewer.md` — add `shebang: ["sh", "bash"]` after
      `globs:`. Frontmatter only; the body is untouched, so the shared-tail and
      severity-ladder pins are unaffected.
- [ ] `adapters/core/reviewers/python-reviewer.md` — add `shebang: ["python"]`.
- [ ] No other reviewer gets a `shebang:`.
- [ ] Run `./scripts/gen-adapters.sh`.

Verify: `yq` parses both frontmatters; `bats tests/adapters.bats` is 58/58 green — the
existing suite does not yet know about `shebang:`, so nothing should move.

## Step 2: state the rule once, in the routing bullet

- [ ] Apply the clause drafted above to `WORKER_PROTOCOL.md:155`.
- [ ] Line 165 — "the roster entries the changed files' `globs:` matched" becomes
      "the roster entries the changed files matched", so the rule stays stated once.
- [ ] **Six** `grep -F` pins are substrings of this bullet and of the engine-table rows:
      `tests/adapters.bats:391` and the five at `:543-547` `[plan-critic note]`. Their
      bytes must not change. The two sentences actually being rewritten — the `Match your
changed paths…` sentence and line 165 — are pinned by **nothing**, so the rewrite is
      unconstrained apart from leaving those six substrings intact.
- [ ] Run `./scripts/gen-adapters.sh`.

Verify: each of the six pinned strings still `grep -F` hits; `bats tests/adapters.bats`
58/58 green.

## Step 3: the fixture tree

- [ ] `tests/fixtures/shebang-routing/bin/foo` — `#!/usr/bin/env bash`, a trivially
      shellcheck-clean body, **mode 755**. `[n5]` Executable is deliberate: pre-commit's
      shellcheck hook is `types: ["shell"]`, and identify classifies an extensionless file
      as shell only via shebang **and** the executable bit.
- [ ] `tests/fixtures/shebang-routing/bin/bar` — `#!/usr/bin/env python3`, valid Python,
      mode 755. No Python hook is enabled; prettier runs `--ignore-unknown` and skips both;
      treefmt's shfmt matches by path glob, so neither is reformatted.
- [ ] Keep both bodies free of any auth, crypto, input-parsing, SQL or network surface, so
      Step 8's reader has no honest reason to add `security-reviewer` `[plan-critic note]`.
- [ ] Verify: `shellcheck tests/fixtures/shebang-routing/bin/foo` clean; no trailing
      whitespace; and `nix flake check` — the command that actually runs the hooks
      `--all-files` and would catch a dirty fixture, since CI's explicit
      `shellcheck adapters/core/*.sh scripts/*.sh` never sees it.

## Step 4: the tests

All in `tests/adapters.bats`.

- [ ] **Pin the protocol clause** — `grep -F` a distinctive fragment of the probe
      paragraph, e.g. `as it stands in the worktree after the change`. Name this test
      `the routing rule probes an extensionless file's shebang`.
- [ ] **Pin the reworded line 165** — `grep -F` the new neutral wording, and assert the
      old `globs:`-only phrasing is gone.
- [ ] **Once-ness** — count with `grep -o … | wc -l`, not `grep -c`: this file is one line
      per paragraph, so `grep -c` would count a restatement inside the same paragraph as
      one `[plan-critic note]`. Assert exactly 1.
- [ ] **Pin the declarations** — `shell-reviewer` declares `sh` and `bash`,
      `python-reviewer` declares `python`, read through `yq`.
- [ ] **Extend `the routing table is coherent`** — build a second TSV in the same per-file
      loop at `:672-709`, and put the new assertions **outside** the `count -ge 2` branch
      at `:725`, with a comment in the style of its #116 neighbours explaining why
      interpreters are disjoint where globs may be arbitrated by `when:`: - the interpreter map is **non-empty** — otherwise deleting every `shebang:` key
      passes vacuously, the #116 self-skipping shape; - every declared interpreter is claimed by exactly one reviewer; - **no declared entry is a version-suffix match of another** `[n1]`, pairwise:
      `[[ "$b" =~ ^${a}([-.]?[0-9]+(\.[0-9]+)*)?$ ]]`. The separator is **optional** —
      writing it as required would miss `python`/`python3`, which is the whole point of
      this assertion and the case Step 7's fourth demonstration exercises. - Two entries on the _same_ reviewer may version-collide; only cross-reviewer
      collisions double-dispatch. State that in the comment. - Read the map line by line, never `for x in $(...)`, per the existing comment at
      `:722-732`.
- [ ] **`every reviewer carries a routable frontmatter`** — count `shebang:` as a routing
      key so a future `shebang:`-only reviewer is not failed as unreachable, and update the
      test's comment, which still says routing is globs-then-when `[n9]`.
- [ ] **Fixture premise** — assert each fixture file is extensionless, is **executable**
      (`[ -x … ]`, so a lost mode bit that silently un-shellchecks it goes red
      `[plan-critic note]`), and has the stated first line.
- [ ] Run `./scripts/gen-adapters.sh`; `bats tests/adapters.bats` green with the new tests.

## Step 5: the prose that describes routing

- [ ] `README.md:158` — "whose `globs:` frontmatter routes a diff" becomes wording that
      covers both keys. **Introduce no blank line** inside the `**Two rosters, spawned
three ways.**` paragraph and keep all twelve domain words: `tests/adapters.bats:740`
      slices that paragraph to the first blank line and requires every one, plus
      `twelve engine-neutral`.
- [ ] `README.md:345` — "engine-neutral reviewer roster, glob-routed" becomes accurate,
      keeping the tree column alignment.

## Step 6: regenerate and run the full gate

- [ ] `./scripts/gen-adapters.sh`, then run it again and `git diff --exit-code` to prove
      idempotence — CI's drift gate.
- [ ] `bats tests/` in full, not just `adapters.bats`.
- [ ] `shellcheck adapters/core/*.sh scripts/*.sh` — CI's exact invocation.

## Step 7: demonstrate red — drift-neutral, and named (plan-critic B2)

Step 6 has regenerated, so deleting from `adapters/core/…` alone would desynchronise the
generated trees and turn `:162` / `:497` red **whatever** Step 4 wrote. That failure proves
nothing about the new pins. So each demonstration:

1. makes the removal in `adapters/core/…`,
2. **runs `./scripts/gen-adapters.sh`** so the trees stay in sync and the drift tests stay
   green,
3. runs `bats tests/adapters.bats`,
4. records **the named test** that failed — not "suite red",
5. reverts with `git checkout -- adapters/` and re-runs to confirm green again.

| #   | removal                                              | the test that must fail                                    |
| --- | ---------------------------------------------------- | ---------------------------------------------------------- |
| a   | delete the probe paragraph from `WORKER_PROTOCOL.md` | `the routing rule probes an extensionless file's shebang`  |
| b   | delete `shebang:` from `shell-reviewer.md`           | the declaration pin                                        |
| c   | add `bash` to a second reviewer                      | `the routing table is coherent` (exactly-one-claimant)     |
| d   | add `python3` to a second reviewer                   | `the routing table is coherent` (version-suffix collision) |

- [ ] Demo (d) is the one exact-string uniqueness would have passed; if it does not fail,
      the regex separator was written as required rather than optional.
- [ ] `git status` clean afterwards, so no demonstration leaks into the branch.

## Step 8: the evidence run

- [ ] Spawn one fresh-context reader — **claude, sonnet**, named in the PR body so the run
      is reproducible `[n8]` — given only the routing bullet, the full roster frontmatter,
      read access to `tests/fixtures/shebang-routing/`, and the changed-file list
      `tests/fixtures/shebang-routing/bin/foo`, `tests/fixtures/shebang-routing/bin/bar`,
      `flake.nix`.
- [ ] Pass condition: the batch is exactly `{shell-reviewer, python-reviewer,
nix-reviewer}`. `security-reviewer` is **out of scope for the pass condition** — its
      `when:` is a judgement call about the diff's surface, not about shebang routing, so
      its presence or absence neither passes nor fails the run `[plan-critic note]`.
- [ ] On a different batch: the prose is unclear, not the reader wrong. Reword and rerun
      **once**; record both runs. A second failure is escalated in the PR body rather than
      reworded again `[n8]`.

## Step 9: residual scope, stated honestly

- [ ] The PR body records what stays broken: an undeclared interpreter
      (`#!/usr/bin/env ruby`) alongside a matching file is still reviewed by nobody, and
      the `#!/usr/bin/env nix-shell` / `#! nix-shell -i bash` two-line form resolves to
      `nix-shell` and does not match `[n2]` — notable because that is the dominant
      extensionless-script shape in the Nix repos the Problem section calls the widest
      case. This change narrows the hole to shell and Python; it does not close it.
- [ ] `docs/superpowers/specs/2026-09-10-persona-roster-gaps-design.md:137` narrates the
      hole as open. It is a dated design record — leave it.
- [ ] The PR body names design 1 and argues it down, and carries `Closes #119`.
