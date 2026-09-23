# Review notes (local)

## #268 review ledger (#270)

| invariant/family                            | finding or thread IDs                                                                                                        | observed head | fix commit | proof                                                                                                                                                                     | disposition | rounds used |
| ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- | ------------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------- | ----------- |
| lazy Cursor effort override                 | local shell review: explicit `--spawn-role --agent cursor --effort` was accepted although Cursor ignores the value           | uncommitted   | pending    | fresh re-review approved; `shellcheck adapters/core/dispatch.sh`; `bats tests/dispatch.bats` (243/243)                                                                    | fixed       | 2           |
| pace-rule override documentation            | agent-docs-reviewer: stale `credits_cover` model-only escape                                                                 | 20b87aa       | 10e5f6d    | targeted fresh re-review approved; four protocol copies match                                                                                                             | fixed       | 2           |
| protocol-dir guard test parametrization     | shell-reviewer (reviewer role): the engine-spec filter loop passes vacuously on a renamed engine or mistyped filter          | c1d281b       | 51f977d    | `env -u DISPATCH_ENGINES bats tests/dispatch.bats tests/dispatch-resume.bats` 367/0 on 51f977d; helper with a mistyped filter yields n=0, so the added count assert fails | fixed       | 1           |
| stacked base on re-dispatch                 | plan-critic (plan seam): a re-dispatch onto an existing branch regenerates the header and drops a previously stamped `base:` | 5056d63       | c0d03a4    | `bats tests/dispatch.bats` "resume: a re-dispatch preserves a previously stamped base:"                                                                                   | fixed       | 0           |
| stacked-base consumer gap                   | plan-critic (plan seam): `adapters/core/commands/autopilot.md` computes base from origin/HEAD; out of scope for #268         | 5056d63       | n/a        | follow-up issue                                                                                                                                                           | deferred    | 0           |
| AC4 resume test vacuity                     | plan-critic (plan seam): the `--print` AC4 test exits before `_hdr_set`, so it never exercises the rewrite it guards         | 5056d63       | c0d03a4    | `bats tests/dispatch-resume.bats` "resume preserves a stacked base: through the header rewrite"                                                                           | fixed       | 0           |
| code review (shell + agent-docs + security) | reviewer role (review seam): routed roster batch, no findings                                                                | c0d03a4       | n/a        | verdict accept; shellcheck clean; bats 305/69/106; gen-adapters idempotent                                                                                                | accepted    | 1           |

| grid hint contract v1 (publisher side) | reviewer role (pi deepseek-v4-pro; roster: agent-docs + shell + security): no findings | f5ca4fe | n/a | reviewer verdict accept; `shellcheck adapters/core/*.sh`; `bats tests/` 1055/1055; `scripts/gen-adapters.sh` idempotent | clean | 1 |

recurrence_escalation: unused

## Round 1 — agent-docs-reviewer + shell-reviewer (standard tier, review_mode: full)

| invariant/family                                                  | finding or thread IDs         | observed head | fix commit | proof                                                    | disposition | rounds used                   |
| ----------------------------------------------------------------- | ----------------------------- | ------------- | ---------- | -------------------------------------------------------- | ----------- | ----------------------------- |
| retro-note cross-reference from review-gate diagnostics paragraph | agent-docs-reviewer MEDIUM #1 | 4474d0f       | b176588    | `nix develop -c bats tests/adapters.bats` green post-fix | fixed       | 0 (mechanical, small-fix bar) |

shell-reviewer: no findings, Approve.

## Harness diagnostics (not reviewer-facing, recorded here per the new PR body contract)

None generated during this task's own review gate — `reviewer-roster --base` resolved cleanly with no `.dispatcher/reviewers` in this repo, so no overrides/rejections/ignored-`when:`/discovery-skipped diagnostics occurred.

## Rebase review over #270 (standard tier, review_mode: full)

| invariant/family                                       | finding or thread IDs         | observed head | fix commit | proof                                                                                                                                          | disposition | rounds used |
| ------------------------------------------------------ | ----------------------------- | ------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------- | ----------- | ----------- |
| collapsed ledger narrowed to a recurrence ledger alone | agent-docs-reviewer MEDIUM #1 | 1c75fe8       | 0ac17a2    | four autopilot command copies now hold both ledgers; `bats --tap tests/adapters.bats` 107/0; gen-adapters in sync; targeted re-review accepted | fixed       | 1           |
| thin test coverage for the collapsed-block phrasing    | agent-docs-reviewer LOW #2    | 1c75fe8       | 0ac17a2    | new both-ledgers positive + old-qualifier negative over the four command copies                                                                | fixed       | 1           |
| PR body `## Summary` rebase-over-#270 line             | agent-docs-reviewer LOW #4    | 1c75fe8       | n/a        | push-time body edit (not a diff defect)                                                                                                        | fixed       | 0           |
| `## Testing` evidence count (107, not 131)             | agent-docs-reviewer LOW #3    | 1c75fe8       | n/a        | corrected in the PR body at push time                                                                                                          | fixed       | 0           |

recurrence_escalation: unused
