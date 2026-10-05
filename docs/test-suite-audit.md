# Bats suite audit (#712)

Measured on 2026-10-04 at `4bd0199` (main). Reproduce with `scripts/bats-timing.sh`
(per-test timing) and `scripts/bats-classify.sh` (`--summary`, `--near-dups`).

## Summary

- **2,857 tests**, 42,395 lines of `tests/*.bats`. All pass.
- **Test time is dominated by real wall-clock waits, not by doc tests.** 211 tests
  that take at least 2s account for 58% of summed test time. The 102 `stall-watch`
  tests in `crew.bats` alone account for 30%.
- **Doc-pinning is small and cheap at runtime.** The classifier finds 76 tests
  (74 of them in `adapters.bats`), not ~280. Most hits of the rough grep in
  `dispatch.bats` and `dispatch-resume.bats` only _name_ a protocol file in a
  launch argv or a fixture `touch`, which is behaviour. Together, the 76 tests
  take 5.8s of summed test time (0.3%). Cutting them is about maintenance and
  surviving the #703 protocol restructure, not CI time.
- **Exact duplicates: 3** (the classifier flagged 4; one was a false positive).
- CI time is won by speeding up slow families (S1 and S3 below; S2 turned out to have nothing to change). Worker time is
  won by running only the affected bats files while iterating (`scripts/bats-affected.sh`).

## Runtime

### CI baseline (main, three most recent green runs)

| step                                    | run 37221841753 | run 37221838262 | run 37209428403 |
| --------------------------------------- | --------------: | --------------: | --------------: |
| bats (module.bats, serial)              |           1m48s |           1m55s |           1m56s |
| bats (`--jobs 16`, `!timing`)           |          13m49s |          14m35s |          14m35s |
| bats (secret-read-guard timing, serial) |           1m58s |           2m28s |           2m28s |

### Local measurement

`scripts/bats-timing.sh` runs one file per job (`parallel -j16`, 32-core host),
then module.bats and the `timing`-tagged tests serially. Wall times: 856s for the
parallel phase, which is bounded by `crew.bats` running as a single job; 4s for
module.bats (warm nix cache); 54s for the timing step. Summed per-test time is
**1,928s**. Per-test times include `-j16` contention from sibling files, so read
them as relative.

### Slowest files

| file                           | tests | seconds | share |
| ------------------------------ | ----: | ------: | ----: |
| crew.bats                      |   521 |   830.5 | 43.1% |
| dispatch.bats                  |   792 |   480.8 | 24.9% |
| refresh-budget.bats            |   100 |   140.5 |  7.3% |
| model-map.bats                 |     7 |    98.0 |  5.1% |
| dispatch-resume.bats           |   151 |    88.9 |  4.6% |
| secret-read-guard.bats         |   447 |    84.1 |  4.4% |
| secret-read-guard.bats[timing] |    28 |    51.3 |  2.7% |
| adapters.bats                  |   190 |    21.4 |  1.1% |
| worktree-git.bats              |    59 |    20.8 |  1.1% |
| pr-watch.bats                  |    17 |    18.6 |  1.0% |
| permission-check.bats          |   147 |    16.3 |  0.8% |
| rate.bats                      |    44 |    10.3 |  0.5% |
| crews.bats                     |    64 |     9.0 |  0.5% |
| rate-autosweep.bats            |    21 |     8.3 |  0.4% |
| reap-race.bats                 |     6 |     7.8 |  0.4% |
| dispatch-comment.bats          |    10 |     6.7 |  0.3% |
| dispatcher.bats                |    39 |     5.8 |  0.3% |
| hold.bats                      |    24 |     5.2 |  0.3% |
| retro.bats                     |    29 |     4.9 |  0.3% |
| dispatch-config.bats           |    40 |     4.1 |  0.2% |
| rate-sweep-all.bats            |    11 |     3.4 |  0.2% |
| crew-dash.bats                 |    12 |     3.3 |  0.2% |
| dispatch-notify.bats           |    16 |     3.2 |  0.2% |
| module.bats                    |    39 |     1.5 |  0.1% |
| public-leak-guard.bats         |    16 |     1.4 |  0.1% |
| refresh-scores.bats            |     6 |     1.0 |  0.0% |
| model-map-doc.bats             |     9 |     0.6 |  0.0% |
| crew-id.bats                   |     5 |     0.3 |  0.0% |
| smoke.bats                     |     3 |     0.2 |  0.0% |
| refresh-models.bats            |     4 |     0.2 |  0.0% |

### Slowest families (test-name prefix, ≥15s summed)

| file                            | family               | tests | seconds | share |
| ------------------------------- | -------------------- | ----: | ------: | ----: |
| crew.bats                       | `stall-watch:`       |   102 |   584.5 | 30.3% |
| dispatch.bats                   | `role-watch:`        |    64 |   173.6 |  9.0% |
| model-map.bats                  | `model map:`         |     7 |    98.0 |  5.1% |
| secret-read-guard.bats          | `secret-read-guard:` |   281 |    75.8 |  3.9% |
| crew.bats                       | `stream:`            |    26 |    72.0 |  3.7% |
| secret-read-guard.bats (timing) | `secret-read-guard:` |    28 |    51.3 |  2.7% |
| crew.bats                       | `reap:`              |    80 |    50.9 |  2.6% |
| dispatch.bats                   | `claim:`             |    50 |    43.3 |  2.2% |
| crew.bats                       | `await:`             |    18 |    34.4 |  1.8% |

### Slowest 30 tests

|   # | file                   | test                                                                                           | seconds | share |
| --: | ---------------------- | ---------------------------------------------------------------------------------------------- | ------: | ----: |
|   1 | model-map.bats         | model map: dispatch escalation admits, refuses and records as the old code did                 |    55.4 | 2.88% |
|   2 | model-map.bats         | model map: resume escalation admits as the old dispatch-resume.sh code did                     |    29.7 | 1.54% |
|   3 | crew.bats              | stall-watch: D1's rate-limit quota: transitions cleanly to D1b's session-limit quota: and back |    18.0 | 0.93% |
|   4 | dispatch.bats          | pace decisions hold across every claude/codex/cursor model and effort (#605)                   |    17.4 | 0.90% |
|   5 | crew.bats              | stall-watch: a quiet: episode with a live engine process does NOT escalate                     |    15.7 | 0.82% |
|   6 | secret-read-guard.bats | secret-read-guard: rule 3 agrees under busybox awk                                             |    15.6 | 0.81% |
|   7 | crew.bats              | stall-watch: D4 runs engine-independent (codex and no --engine)                                |    15.5 | 0.80% |
|   8 | crew.bats              | stall-watch: D2 reads the reconstructed 1h meter and ignores #31's transcription               |    14.8 | 0.77% |
|   9 | crew.bats              | stall-watch: D4 load: is supersedable by quiet: and does not re-post stale                     |    13.5 | 0.70% |
|  10 | secret-read-guard.bats | secret-read-guard: rule 3 agrees under mawk                                                    |    13.4 | 0.69% |
|  11 | secret-read-guard.bats | secret-read-guard: rule 3 agrees under nawk                                                    |    13.1 | 0.68% |
|  12 | dispatch.bats          | role-watch: C0-bearing assignments are never typed and plain assignments still deliver for e…  |    12.9 | 0.67% |
|  13 | crew.bats              | stall-watch: D4 clears itself when the load drops back to the cores                            |    12.6 | 0.65% |
|  14 | refresh-budget.bats    | a hanging pi auth call is bounded and leaves pi unknown                                        |    11.1 | 0.58% |
|  15 | model-map.bats         | model map: the tier gate admits and describes each row as the old case arms did                |    11.0 | 0.57% |
|  16 | crew.bats              | stall-watch: a quota: episode NEVER escalates (C-1 quota variant)                              |    10.6 | 0.55% |
|  17 | crew.bats              | stall-watch: a prompt: episode NEVER escalates (C-1)                                           |    10.6 | 0.55% |
|  18 | crew.bats              | stall-watch: a session-limit quota: episode NEVER escalates                                    |    10.5 | 0.55% |
|  19 | crew.bats              | stall-watch: a cursor monthly-limit quota: episode NEVER escalates                             |    10.4 | 0.54% |
|  20 | dispatch.bats          | role-watch: real codex and cursor non-idle captures never receive keys                         |    10.2 | 0.53% |
|  21 | refresh-budget.bats    | a pi auth store without a usable key leaves pi unknown                                         |     9.7 | 0.50% |
|  22 | crew.bats              | stall-watch: D6 clears itself once the verdict is delivered                                    |     9.6 | 0.50% |
|  23 | crew.bats              | stall-watch: one post per episode, then a working clearance that re-arms                       |     9.5 | 0.50% |
|  24 | crew.bats              | stall-watch: a finished turn waiting on a background shell posts nothing                       |     9.5 | 0.49% |
|  25 | crew.bats              | stall-watch: session-3 regression — a static trust prompt is prompt:, never failed or stalled: |     8.6 | 0.44% |
|  26 | crew.bats              | stall-watch: a prompt: episode held past --idle and --dead still never escalates or gets sup…  |     8.6 | 0.44% |
|  27 | crew.bats              | stall-watch: a session-limit quota: episode held past --idle and --dead still never escalate…  |     8.5 | 0.44% |
|  28 | crew.bats              | stall-watch: D5 clears itself when the engine appears late                                     |     8.5 | 0.44% |
|  29 | crew.bats              | stall-watch: a static permission frame is prompt:, never stalled:                              |     8.5 | 0.44% |
|  30 | crew.bats              | stall-watch: a live spinner under a stale done line is not a background-shell wait             |     8.5 | 0.44% |

## Classification

Every `@test` gets exactly one class (`scripts/bats-classify.sh`):

- **behaviour**: runs a production entry point (a core script, a generated
  script, a sourced lib's functions, `nix build`/`eval`) and asserts on what it does.
  Security regressions are in this class.
- **doc-pinning**: asserts on protocol/README/command/skill wording.
- **structure**: adapter copies are byte-identical, a required file or frontmatter
  is present, or a helper copy matches its source.
- **duplicate**: the normalized body (whitespace collapsed, comments and name
  dropped) is identical to an earlier test.

The rules were spot-checked against at least 10 tests per class, and every
doc-pinning test was then triaged by hand (see the cut list).

| file                   | behaviour | doc-pinning | structure | duplicate |    total |
| ---------------------- | --------: | ----------: | --------: | --------: | -------: |
| adapters.bats          |        69 |          74 |        47 |         0 |      190 |
| crew.bats              |       520 |           1 |         0 |         0 |      521 |
| crew-dash.bats         |        12 |           0 |         0 |         0 |       12 |
| crew-id.bats           |         5 |           0 |         0 |         0 |        5 |
| crews.bats             |        63 |           0 |         0 |         1 |       64 |
| dispatch.bats          |       776 |           1 |        14 |         1 |      792 |
| dispatch-comment.bats  |        10 |           0 |         0 |         0 |       10 |
| dispatch-config.bats   |        40 |           0 |         0 |         0 |       40 |
| dispatcher.bats        |        39 |           0 |         0 |         0 |       39 |
| dispatch-notify.bats   |        15 |           0 |         0 |         1 |       16 |
| dispatch-resume.bats   |       147 |           0 |         4 |         0 |      151 |
| hold.bats              |        24 |           0 |         0 |         0 |       24 |
| model-map.bats         |         6 |           0 |         1 |         0 |        7 |
| model-map-doc.bats     |         9 |           0 |         0 |         0 |        9 |
| module.bats            |        37 |           0 |         2 |         0 |       39 |
| permission-check.bats  |       146 |           0 |         1 |         0 |      147 |
| pr-watch.bats          |        17 |           0 |         0 |         0 |       17 |
| public-leak-guard.bats |        16 |           0 |         0 |         0 |       16 |
| rate-autosweep.bats    |        20 |           0 |         1 |         0 |       21 |
| rate.bats              |        44 |           0 |         0 |         0 |       44 |
| rate-sweep-all.bats    |        11 |           0 |         0 |         0 |       11 |
| reap-race.bats         |         6 |           0 |         0 |         0 |        6 |
| refresh-budget.bats    |        99 |           0 |         0 |         1 |      100 |
| refresh-models.bats    |         4 |           0 |         0 |         0 |        4 |
| refresh-scores.bats    |         6 |           0 |         0 |         0 |        6 |
| retro.bats             |        29 |           0 |         0 |         0 |       29 |
| secret-read-guard.bats |       475 |           0 |         0 |         0 |      475 |
| smoke.bats             |         3 |           0 |         0 |         0 |        3 |
| worktree-git.bats      |        59 |           0 |         0 |         0 |       59 |
| **total**              |  **2707** |      **76** |    **70** |     **4** | **2857** |

| class       | tests | summed seconds |
| ----------- | ----: | -------------: |
| behaviour   |  2707 |         1914.2 |
| doc-pinning |    76 |            5.8 |
| structure   |    70 |            4.4 |
| duplicate   |     4 |            0.4 |

The near-duplicate pass (bodies identical once string literals and numbers are
placeholders) finds 114 same-file pairs, mostly parametrised families such as
codex limit rows or `msg: rejects X: with empty id`. Each pair exercises a
different input, so they are behaviour cases, not duplicates. None are on the
cut list, and folding them into loops would save no runtime.

## Source → test map

Derived from what each test file actually sources or runs: path literals,
`$BATS_TEST_DIRNAME/..` paths, the libs `tests/helpers.bash` exports
(`DISPATCH_CONFIG_BIN`, `GRANT_CHECK_LIB`, `WORKTREE_GIT_LIB` via `setup_repo`),
`@…Lib@` substitutions inside core scripts, and directory copies (tests that
copy `adapters/core/protocols`, `reviewers`, `critics` or `skills` wholesale).
It is a static over-approximation, which is the safe direction for test selection.

| source                                              | #tests | shared | tests                                                                                                                                                                                                                                                                                                                                                    |
| --------------------------------------------------- | -----: | :----: | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `adapters/core/crew.sh`                             |     19 |  yes   | adapters, crew-dash, crew-id, crew, crews, dispatch-comment, dispatch-notify, dispatch-resume, dispatch, dispatcher, hold, model-map, module, pr-watch, rate-autosweep, rate-sweep-all, rate, reap-race, retro                                                                                                                                           |
| `adapters/core/cross-repo-hint.sh`                  |      7 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, model-map, module                                                                                                                                                                                                                                                                          |
| `adapters/core/dispatch-config.sh`                  |     28 |  yes   | adapters, crew-dash, crew-id, crew, crews, dispatch-comment, dispatch-config, dispatch-notify, dispatch-resume, dispatch, dispatcher, hold, model-map-doc, model-map, module, permission-check, pr-watch, rate-autosweep, rate-sweep-all, rate, reap-race, refresh-budget, refresh-models, refresh-scores, retro, secret-read-guard, smoke, worktree-git |
| `adapters/core/dispatch-notify.sh`                  |      3 |        | adapters, dispatch-notify, dispatch                                                                                                                                                                                                                                                                                                                      |
| `adapters/core/dispatch-resume.sh`                  |      7 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, model-map, module                                                                                                                                                                                                                                                                          |
| `adapters/core/dispatch.sh`                         |      7 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, model-map, module                                                                                                                                                                                                                                                                          |
| `adapters/core/dispatcher.sh`                       |      5 |        | adapters, crews, dispatch, dispatcher, module                                                                                                                                                                                                                                                                                                            |
| `adapters/core/grant-check.sh`                      |     28 |  yes   | adapters, crew-dash, crew-id, crew, crews, dispatch-comment, dispatch-config, dispatch-notify, dispatch-resume, dispatch, dispatcher, hold, model-map-doc, model-map, module, permission-check, pr-watch, rate-autosweep, rate-sweep-all, rate, reap-race, refresh-budget, refresh-models, refresh-scores, retro, secret-read-guard, smoke, worktree-git |
| `adapters/core/permission-check.sh`                 |      5 |        | adapters, dispatch-resume, dispatch, module, permission-check                                                                                                                                                                                                                                                                                            |
| `adapters/core/pr-watch.sh`                         |      4 |        | adapters, dispatch, module, pr-watch                                                                                                                                                                                                                                                                                                                     |
| `adapters/core/public-leak-guard.sh`                |      8 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, model-map, module, public-leak-guard                                                                                                                                                                                                                                                       |
| `adapters/core/refresh-budget.sh`                   |      5 |        | adapters, crew-dash, dispatch, module, refresh-budget                                                                                                                                                                                                                                                                                                    |
| `adapters/core/refresh-models.sh`                   |      4 |        | adapters, dispatch, module, refresh-models                                                                                                                                                                                                                                                                                                               |
| `adapters/core/refresh-scores.sh`                   |      4 |        | adapters, dispatch, module, refresh-scores                                                                                                                                                                                                                                                                                                               |
| `adapters/core/secret-read-guard.sh`                |      3 |        | adapters, dispatch, secret-read-guard                                                                                                                                                                                                                                                                                                                    |
| `adapters/core/worktree-git.sh`                     |     21 |  yes   | adapters, crew-dash, crew-id, crew, crews, dispatch-comment, dispatch-notify, dispatch-resume, dispatch, dispatcher, hold, model-map, module, pr-watch, rate-autosweep, rate-sweep-all, rate, reap-race, retro, smoke, worktree-git                                                                                                                      |
| `adapters/core/commands/autopilot.md`               |      2 |        | adapters, dispatch                                                                                                                                                                                                                                                                                                                                       |
| `adapters/core/commands/dispatcher.md`              |      2 |        | adapters, dispatch                                                                                                                                                                                                                                                                                                                                       |
| `adapters/core/commands/finish-prs.md`              |      2 |        | adapters, dispatch                                                                                                                                                                                                                                                                                                                                       |
| `adapters/core/commands/project-autopilot.md`       |      2 |        | adapters, dispatch                                                                                                                                                                                                                                                                                                                                       |
| `adapters/core/critics/plan-critic.md`              |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/critics/spec-critic.md`              |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/defaults.json`                       |     28 |  yes   | adapters, crew-dash, crew-id, crew, crews, dispatch-comment, dispatch-config, dispatch-notify, dispatch-resume, dispatch, dispatcher, hold, model-map-doc, model-map, module, permission-check, pr-watch, rate-autosweep, rate-sweep-all, rate, reap-race, refresh-budget, refresh-models, refresh-scores, retro, secret-read-guard, smoke, worktree-git |
| `adapters/core/protocols/DISPATCHER_PROTOCOL.md`    |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/protocols/EVIDENCE_REVIEW.md`        |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/protocols/GRID_PROTOCOL.md`          |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/protocols/REVIEW_TASK.md`            |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/protocols/WORKER_PROTOCOL.md`        |     10 |        | adapters, crew, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                      |
| `adapters/core/protocols/dispatch-orchestration.md` |     11 |        | adapters, crew, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map-doc, model-map, module, permission-check                                                                                                                                                                                                                       |
| `adapters/core/reviewers/agent-docs-reviewer.md`    |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/charm-tui-reviewer.md`     |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/general-reviewer.md`       |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/go-reviewer.md`            |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/nix-reviewer.md`           |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/postgres-reviewer.md`      |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/python-reviewer.md`        |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/resolve-roster.sh`         |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/security-reviewer.md`      |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/shell-reviewer.md`         |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/sqlite-reviewer.md`        |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/terraform-reviewer.md`     |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/typescript-reviewer.md`    |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/reviewers/yaml-reviewer.md`          |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/skills/deslop/SKILL.md`              |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/core/skills/spec-plan-critic/SKILL.md`    |      9 |        | adapters, crews, dispatch-comment, dispatch-resume, dispatch, dispatcher, model-map, module, permission-check                                                                                                                                                                                                                                            |
| `adapters/claude-code/**`                           |      2 |        | adapters, module                                                                                                                                                                                                                                                                                                                                         |
| `adapters/codex/**`                                 |      2 |        | adapters, module                                                                                                                                                                                                                                                                                                                                         |
| `adapters/cursor/**`                                |      2 |        | adapters, module                                                                                                                                                                                                                                                                                                                                         |
| `scripts/cache-report.sh`                           |      1 |        | adapters                                                                                                                                                                                                                                                                                                                                                 |
| `scripts/gen-adapters.sh`                           |      1 |        | adapters                                                                                                                                                                                                                                                                                                                                                 |
| `scripts/render-engine.sh`                          |      1 |        | adapters                                                                                                                                                                                                                                                                                                                                                 |
| `scripts/gen-model-map-doc.sh`                      |      2 |        | adapters, model-map-doc                                                                                                                                                                                                                                                                                                                                  |
| `README.md`                                         |      3 |        | adapters, dispatch, permission-check                                                                                                                                                                                                                                                                                                                     |
| `dash/**`                                           |      2 |        | crew-dash, module                                                                                                                                                                                                                                                                                                                                        |
| `flake.nix`                                         |      1 |        | module                                                                                                                                                                                                                                                                                                                                                   |
| `nix/hm-module.nix`                                 |      2 |        | adapters, module                                                                                                                                                                                                                                                                                                                                         |
| `tests/helpers.bash`                                |     27 |  yes   | adapters, crew-dash, crew-id, crew, crews, dispatch-comment, dispatch-config, dispatch-notify, dispatch-resume, dispatch, dispatcher, hold, model-map-doc, model-map, permission-check, pr-watch, rate-autosweep, rate-sweep-all, rate, reap-race, refresh-budget, refresh-models, refresh-scores, retro, secret-read-guard, smoke, worktree-git         |

**Shared sources.** These reach at least half of the 29 files, so a change to
any of them runs the full suite: `tests/helpers.bash`, `adapters/core/dispatch-config.sh`,
`adapters/core/grant-check.sh`, `adapters/core/defaults.json` and
`adapters/core/worktree-git.sh`. `adapters/core/crew.sh` reaches 19 files and is
selected by its own map row.

## Cut list (approved 2026-10-04, applied)

The owner approved every item below on 2026-10-04. The **Applied** section says
where the applied change differs from the proposal. Behaviour and security tests
are not on this list, except the exact duplicates in C2 and S3's redundant re-run.

### C1. Doc-pinning: delete 44, collapse 16 into one anchor test, keep 16

All of these are in `adapters.bats`. That brings `adapters.bats` from 190 to
about 131 tests and removes about 1,135 lines. Generated copies are already
covered twice: CI runs `scripts/gen-adapters.sh` and fails on any diff, and
`adapters.bats` "every canonical protocol exactly matches both shipped protocol
trees" checks byte identity. Re-pinning the same sentences in four copies adds
nothing.

**C1a. Delete (44).** These tests pin verbatim prose that a reword or a section
move breaks, and they carry no invariant beyond "this sentence exists".

| line | test                                                                                                   | what it pins                                                                  |
| ---: | ------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------- |
|  339 | a rewritten parent is rebased --onto its recorded head, plain rebase only when it is an ancestor       | exact git rebase/merge-base strings in 8 copies; copies covered by drift gate |
|  434 | worker protocol defines bounded plan-shaped gate recovery                                              | ten verbatim sentences of plan-shaped recovery prose                          |
|  484 | owner-auth cross-engine resume cannot carry the prompt (#459)                                          | pins three sentences of owner-auth chain prose plus an absent over-claim      |
|  515 | dispatcher protocol claim bullet pins the resume exemption and adopt release                           | two verbatim sentences of claim-bullet wording                                |
|  689 | worker protocol carries retro notes in the metrics snapshot                                            | snapshot notes field wording; sentences only                                  |
|  700 | worker protocol emits mid-execute retro notes immediately                                              | four verbatim sentences on mid-execute retro emission                         |
|  712 | worker protocol points each branch at its retro tag                                                    | per-branch retro-tag sentences; pure guidance text                            |
|  798 | review workers follow one base rule: the stamped header base:                                          | two sentences about review-worker base rule                                   |
|  821 | grid roles reply to the current worker session after resume                                            | pins a GRID lead_id shell snippet and absence of worker:$branch               |
|  831 | worker protocol pins the fresh-context reviewer contract                                               | fresh-context reviewer contract sentences                                     |
|  846 | worker protocol makes an unspawnable reviewer terminal and loud                                        | unspawnable-reviewer prose; eleven verbatim sentences                         |
|  867 | worker protocol glosses review_mode none by the gate-reached condition                                 | one review_mode none gloss sentence plus its old wording                      |
|  877 | the metrics snapshot carves no engine out of either gate                                               | metrics-snapshot carve-out sentences and absent stale phrases                 |
|  903 | worker protocol pins the bounded blocked→await cycle on every copy                                     | bounded blocked-await sentences x4 copies; copies covered by drift gate       |
|  931 | dispatcher protocol pins in-band delivery and terminal re-dispatch on every copy                       | dispatcher in-band delivery sentences x4 copies                               |
|  952 | dispatcher protocol pins the permission-check policy on every copy                                     | permission-check policy sentences x4 copies                                   |
|  986 | every review-task copy mirrors the bounded blocked→await cadence                                       | review-task cadence sentence x4 copies                                        |
| 1777 | the review gate resolves repo-local reviewers under the harness contract                               | repo-local reviewer resolver contract prose x4; behaviour in roster tests     |
| 1808 | the PR body contract keeps agent-state ledgers in the collapsed block, not a visible heading           | PR-body ledger wording in 8 files                                             |
| 1833 | repo when: is scoped to new entries versus overrides on every copy, never the old blanket sentence     | one exact long sentence counted once in 12 files plus stale phrases           |
| 1879 | grid protocols pin the sender-filtered, bounded verdict wait on every copy                             | grid sender-filtered wait sentences x4 copies                                 |
| 1901 | spec-plan-critic pins a synchronous critic spawn on every copy                                         | single sentence 'The spawn is synchronous' x4 copies                          |
| 1912 | grid and autopilot route over the resolved roster                                                      | grid/autopilot roster-routing sentences; overlaps 1777 and 601                |
| 1951 | autopilot targets the stacked parent branch, not the default branch                                    | autopilot stacked-parent snippets pinned verbatim, count==1                   |
| 2080 | autopilot Step 4: the branch-exists split replaces the false idempotency claim                         | autopilot Step 4 branch-exists snippet wording                                |
| 2152 | autopilot parent branch: the branch-exists split replaces the unguarded --create                       | autopilot parent-branch snippet wording                                       |
| 2222 | finish-prs Setup: the branch-exists split replaces the false idempotency claim                         | finish-prs setup snippet wording                                              |
| 2705 | the routing rule probes an extensionless file's shebang                                                | shebang probe clause sentences                                                |
| 2721 | the shebang probe is stated exactly once                                                               | counts '**The shebang probe.**' occurs once; formatting-bound                 |
| 2730 | the language reviewer bullet does not route by globs alone                                             | one bullet phrase and absence of old phrase                                   |
| 2778 | both protocols state the severity mapping                                                              | two severity-mapping sentences                                                |
| 2896 | the claude lane's hold_due wake lives inside its own slice                                             | hold_due sentence in claude lane slice                                        |
| 2915 | the cursor lane's overshoot and park primitive live below its heading                                  | two phrases in cursor lane slice                                              |
| 2948 | the Tracker bullet states both branch forms and the three-way duplicate guard                          | Tracker bullet sentences with stale dispatch.sh:NNN line refs                 |
| 2961 | the dispatcher protocol tells the human at all three ends of a hold                                    | three human-notification sentences for holds                                  |
| 2984 | every review-task copy pins the P1 and P2 completion peeks                                             | review-task P1/P2 peek sentences x4 copies                                    |
| 3005 | every review-task copy decides APPROVE once and bars a follow-up PR write                              | review-task APPROVE-once sentences x4 copies                                  |
| 3183 | the cursor Task-spawn slug pointers reach every seam and every generated copy                          | ~25 pointer sentences across 4 copies x 4 docs                                |
| 3244 | claude-only skill names are defined in-repo, with the superpowers name only as an optional convenience | claude-only skill name wording; sentence and absent-phrase pins               |
| 3281 | the dispatcher command stops when its crew does not read alive (#399)                                  | two sentences in dispatcher command                                           |
| 3289 | the dispatcher protocol's activation names every in-place engine (#399)                                | Activation line wording                                                       |
| 3296 | autopilot's reviewer spawn names each engine's mechanism (#399)                                        | autopilot reviewer-spawn line wording                                         |
| 3305 | every Argument line covers an engine that does not substitute $ARGUMENTS (#399)                        | Argument line wording                                                         |
| 3312 | the evidence contract names its techniques engine-neutrally (#399)                                     | evidence contract wording/head -12 position pin                               |

**C1b. Collapse into one table-driven anchor test (16).** The new test checks,
in the canonical copy only, that each required heading or bold anchor is present.
It survives section moves and rewording inside a section, which is what #703
needs. Each row below becomes one row of that table.

| line | test                                                                   | anchor it requires                                                                                                 |
| ---: | ---------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
|  454 | permission denials await in-band and relaunch with --owner-auth (#454) | WORKER_PROTOCOL: crew await permission stop; DISPATCHER_PROTOCOL: **Owner authorization.**, **Permission blocks.** |
|  498 | worker protocol pins the resume-a-killed-run contract                  | WORKER_PROTOCOL: **Resuming a killed run**                                                                         |
|  660 | every engine runs /deslop and posts the deslop seam                    | WORKER_PROTOCOL: deslop seam msg {"seam":"deslop"} and dispatcher:deslop                                           |
|  672 | worker protocol defines the retro-note vocabulary                      | WORKER_PROTOCOL: ## Retro notes (all tiers) (tags verified against code elsewhere)                                 |
|  727 | dispatcher protocol synthesizes retro notes at a drained roster        | DISPATCHER_PROTOCOL: retro: synthesis at drained roster (tags misrouted, fanout_binder, spec_too_thin)             |
|  743 | worker protocol binds the review gate to every engine                  | WORKER_PROTOCOL: Code review gate binds every engine (claude/codex/cursor rows)                                    |
|  763 | worker protocol gives every engine a runnable consult and diverse-revi | WORKER_PROTOCOL: ## Cross-engine one-shots (consult and diverse reviewer)                                          |
|  999 | the cursor rule runs both gates and names where the bodies are         | cursor/rules/dispatcher.mdc (hand-maintained): DISPATCHER_CRITICS_DIR and both-gates statement                     |
| 1763 | the review gate routes the batch over the roster                       | WORKER_PROTOCOL: **The reviewers themselves ship with the harness.** / reviewer-roster --base                      |
| 1860 | worker protocol pins plan: required as binding and gating verdicts as  | WORKER_PROTOCOL: ## Gating verdicts are awaited (all engines)                                                      |
| 2818 | the critic gate routes over the roster on every engine                 | spec-plan-critic SKILL: **The critics themselves ship with the harness.** / $DISPATCHER_CRITICS_DIR                |
| 2836 | the worker protocol points at the critic roster too                    | WORKER_PROTOCOL: **Critics are independent, on every engine.** / $DISPATCHER_CRITICS_DIR                           |
| 2847 | the claude lane carries its Monitor-stream contract                    | DISPATCHER_PROTOCOL: **claude — streaming monitor.** lane (crew stream, Monitor)                                   |
| 2932 | the pi lane carries its blocking-park contract                         | DISPATCHER_PROTOCOL: **codex / pi — blocking park.** and pi exception                                              |
| 3023 | worker protocol pins the deferred-findings contract                    | WORKER_PROTOCOL: ## Deferred findings (standard/deep); tracker: github / linear lines                              |
| 3095 | cursor Task-spawn slugs are distinguished from launch slugs, with a su | dispatch-orchestration: ### Cursor Task-spawn slugs (between ### Tier map and ## Orchestrator engines)             |

**C1c. Keep (15 lint, 1 reclassified to behaviour).** These enforce cross-file
invariants or doc↔code conformance, such as un-namespaced command references,
roster↔code tables, a predicate copied verbatim from `dispatch.sh`, or the
README roster count. They survive section moves.

| test               | action     | name                                                                             | reason                                                                            |
| ------------------ | ---------- | -------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- | ------------------------------------- |
| adapters.bats:225  | KEEP-LINT  | codex skill bodies drop the source frontmatter                                   | generated codex skills carry no source argument-hint frontmatter                  |
| adapters.bats:393  | RECLASSIFY | protocol PRs editing different files merge in either order (#193)                | runs real git merges of two protocol branches; behaviour                          |
| adapters.bats:560  | KEEP-LINT  | no command body references a plugin command without its namespace                | no un-namespaced /autopilot                                                       | /finish-prs refs across shipped trees |
| adapters.bats:586  | KEEP-LINT  | project-autopilot points teammates at the namespaced autopilot                   | project-autopilot uses /dispatcher:autopilot, never bare                          |
| adapters.bats:601  | KEEP-LINT  | autopilot routes reviewers through the roster, not a private table               | reviewer tokens in autopilot must exist in the roster dir                         |
| adapters.bats:632  | KEEP-LINT  | the dispatcher command resolves its protocol via the env var                     | dispatcher command references $DISPATCHER_PROTOCOL_DIR path (doc to code env var) |
| adapters.bats:2506 | KEEP-LINT  | no critic body names a model, so no engine reads a rung it cannot spawn          | no model: in shared critic bodies (engine neutrality)                             |
| adapters.bats:2556 | KEEP-LINT  | no roster body carries an engine-specific or repo-specific idiom                 | no engine-specific idiom in roster/critic bodies                                  |
| adapters.bats:2764 | KEEP-LINT  | the README counts the roster                                                     | README roster count and every reviewer domain named                               |
| adapters.bats:2867 | KEEP-LINT  | the claude lane carries none of the cursor lane's park scaffolding               | slice invariant: cursor park scaffolding absent from claude lane                  |
| adapters.bats:2972 | KEEP-LINT  | the release predicate matches the gate's own, verbatim                           | release predicate copied verbatim from dispatch.sh gate                           |
| adapters.bats:3155 | KEEP-LINT  | every Task-spawn substitution candidate is on the recorded roster and never burn | Task-spawn substitution candidates are on roster and never lighter (computed)     |
| adapters.bats:3270 | KEEP-LINT  | the dispatcher command is engine-neutral (#399)                                  | no claude-specific idiom in engine-neutral dispatcher command                     |
| adapters.bats:3320 | KEEP-LINT  | the worker protocol lists no roster reviewer as a skill (#399)                   | worker protocol lists no roster reviewer as a skill                               |
| crew.bats:6445     | KEEP-LINT  | burn map conformance: the doc slice names every classed rung                     | burn-map doc slice names every classed rung (conformance with code)               |
| dispatch.bats:3263 | KEEP-LINT  | tier map conformance: dispatch.sh matches the documented rule                    | tier map in doc and dispatch.sh/defaults.json agree                               |

### C2. Exact duplicates: delete 3

| delete                                                                                  | duplicate of               | evidence                                                                                 |
| --------------------------------------------------------------------------------------- | -------------------------- | ---------------------------------------------------------------------------------------- |
| `crews.bats` "id: honours CREW_ID when set"                                             | `crew.bats:244`            | identical commands and assertions; crew.bats' extra setup does not touch `crew id`       |
| `dispatch.bats:2673` "the grammar floor holds with no codex cache"                      | `dispatch.bats:2598`       | same file and setup, same `run_dispatch deep gpt-5.6 --agent codex`, same two assertions |
| `dispatch-notify.bats:263` "a SessionEnd with the lead process gone still posts exited" | `dispatch-notify.bats:107` | same file and setup; only the name and comment differ                                    |

`refresh-budget.bats:1488` was flagged by the classifier but is **not** a duplicate
(`--json` vs plain `--report`), so it stays.

### C3. Structure tests: no cuts

The 70 structure tests take 4.4s together and guard copy sync, which is the very
check that lets C1 drop its per-copy pins. Keep all 70.

### Speed-ups

| id  | target                                                                               | change                                                                                                                                                                                                                                                                                                                                                                                 | est. saved (summed) |
| --- | ------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------: |
| S1  | 102 `stall-watch:` tests (584s)                                                      | Add a test-only clock seam to `crew stall-watch`: route its `date +%s` reads and `sleep "$interval"` through two small helpers. When a test sets `CREW_STALL_CLOCK=<file>`, they read and advance a virtual clock instead of waiting. Tests drop from ~6s each (`--interval 1`, `--max-life 5..15`) to well under 1s. **Touches production `crew.sh`; the default path is unchanged.** |               ~500s |
| S2  | 64 `role-watch:` tests (174s)                                                        | Where a test drives `dispatch --role-watch --interval 1` under `timeout 10`, use `--interval 0.2`, which other role-watch tests already use. Test-only change; each edited test is timed before and after.                                                                                                                                                                             |            ~80–100s |
| S3  | `model map: resume escalation admits as the old dispatch-resume.sh code did` (29.7s) | It re-runs the 5,590-row escalation table against `dispatch-resume.sh`'s copy of `_glob_match`/`_escalation_hop`. Replace that with a byte-identity check of those two functions against `dispatch.sh`, as `model-map.bats` already does for the settings helpers. The table run against `dispatch.sh` stays.                                                                          |                ~29s |

Not proposed: the busybox/mawk/nawk `secret-read-guard` agreement tests (42s)
and the timing-tagged step are security regressions. `module.bats` is slow only
on a cold nix cache.

### Estimated effect

- **Summed test time:** about 1,928s → about 1,290s (−33%). S1 is ~26%, S2 ~5%,
  S3 ~1.5%; C1 and C2 add up to under 0.5%.
- **CI `bats` step:** on the 4-core runner this step is limited by slot
  occupancy, so wall time should fall roughly in proportion, from ~14m to ~9–10m.
  This is an estimate; the measured before/after goes in the PR.
- **Worker gate:** with `scripts/bats-affected.sh`, a change to one script runs only
  its mapped files. For example, a `secret-read-guard.sh` change runs 3 files
  (~2.5 min summed, under 30s wall) instead of all 29. A change to a shared
  source still runs everything.

## Applied

| item | result                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| ---- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| C1   | 44 tests deleted and 16 collapsed into `protocol and command anchors other docs and code rely on are present` (one table row per anchor, canonical copy only); `adapters.bats` went from 190 to 131 tests and lost ~1,200 lines. The 15 lint tests and `adapters.bats:393` are kept.                                                                                                                                                                                                          |
| C2   | 3 exact duplicates deleted (`crews.bats`, `dispatch.bats`, `dispatch-notify.bats`).                                                                                                                                                                                                                                                                                                                                                                                                           |
| C3   | No change.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| S1   | `crew stall-watch` reads time through `_sw_now`/`_sw_sleep`. When `CREW_STALL_CLOCK` is unset these are exactly `date +%s` and `sleep`. When a test sets it to a file, the clock starts at real time and only moves forward, so bus rows written in real time still sort at or before the watchdog's `now`. Two tests that waited in real time now key off the sampler's call count. Stall-watch subset at `--jobs 16`: **622.6s → 49.0s summed, 54.4s → 10.3s wall**, three runs, no flakes. |
| S2   | **Dropped, no change.** The audit's premise was wrong: every slow `role-watch:` test already polls at `--interval 0.2`. Their time is fixed test-side waits (`_rw_start`'s `sleep 0.6`, closing `sleep 0.8`/`1.2`), which this task did not rework; that is a follow-up.                                                                                                                                                                                                                      |
| S3   | **Deleted rather than rewritten.** `model map: the settings helpers (including _exact_match) are byte-identical between dispatch.sh and dispatch-resume.sh` already compares `_glob_match` and `_escalation_hop`, so a rewritten test would have been an exact duplicate of it. The 5,590-row table still runs against `dispatch.sh`. `model-map.bats` serial: 92s → 55s.                                                                                                                     |

Net effect, counted against the rebased base `d499e7f`: 2,893 → 2,850 tests (−64 cut, +1 anchor test, +1 virtual-clock test, +19 `bats-affected.bats` tests), with the summed time of the slowest family cut by about 575s (30% of the measured total). The CI `bats` step before and after is reported in the PR.

## CI wall time (#721)

The `check` run 37234504956 took 21 minutes; its non-timing bats step took
15m17s. A four-core local probe used 3.76 cores with `bats --jobs 16`, so the
workflow now spreads that suite across four balanced file shards. Each shard
keeps `--jobs 16`; `module.bats`, timing tests, lint, and `nix flake check` run
alongside it, and the final `check` job preserves the previous status name.

`scripts/bats-shard.sh` weights files by `bats --count`, assigns them with a
deterministic greedy bin-pack, and `tests/bats-shard.bats` verifies that the
four shards are exhaustive and disjoint.

## Affected-test selection (phase 3)

`scripts/bats-affected.sh [--base REF] [--files FILE...]` takes the changed files
(vs the merge-base with `--base`, plus staged, unstaged and untracked changes)
and prints the bats files to run, one per line.

- A changed `tests/*.bats` file selects itself.
- A source with a map row selects that row.
- The shared sources above, any other `tests/` helper or fixture, `flake.nix`, or
  any changed file with no row selects the full suite, and the reason goes to stderr.
- A docs-only change outside the protocol tree selects nothing.

Workers use it at every gate, including the last one before push, and never run
the full suite locally. CI runs the full suite and is where a shared-code
regression surfaces; workers fix CI failures from its log.

## Unique coverage (#714)

Measured at `d321f8f` (pre-conversion). Reproduce with `scripts/bats-coverage.sh --out <dir>`.

Each test is line-traced with bash xtrace from `tests/coverage.bash`, which stays
inert unless `BATS_COVERAGE_DIR` is set. Child bash processes inherit the trace
through `BASH_ENV` (`tests/coverage-env.bash`), which opens the trace file by
path, so no file descriptor is inherited. `scripts/bats-coverage.sh` runs each
file once untraced (the pass/fail baseline, and the number→name map) and once
traced, and reduces a test's trace when its TAP line lands. Peak disk is
therefore bounded by parallelism, not by suite size. The reducer keeps only
lines that map to a tracked production source (`adapters/core/`, `scripts/`,
`dash/`); bats-library lines, tool internals, and store paths are discarded
before anything is counted.

Validity: **0 outcome mismatches** between the traced and untraced runs (the
traced run reproduced the untraced pass/fail set). Peak disk **2.06 GB**
(2,058,961,963 bytes), wall **2266s** at `JOBS=16`. Raw trace sizes over 2,861
tests: min 129,213 bytes, median 1,478,071 bytes (1.48 MB), p90 11,695,391
bytes (11.7 MB), max 1,951,065,614 bytes (1.95 GB).

Blind spots:

- Lines executed are not behaviours asserted.
- Granularity is per command, not per branch.
- A partial `env -i` scrub inside an otherwise-traced test is invisible (for example `secret-read-guard.bats:909`).
- Detached `nohup` children (`dispatch.sh` role-watch and stall-watch) can append after the janitor has reduced the trace.
- Tests that exec the built `writeShellApplication` wrappers (`module.bats`'s `$OUT_CREW/bin/crew` and friends) measure as `no-lines`: a `/nix/store/…` path does not suffix-match a tracked source, so the reducer discards every line. The wrapper's own PATH setup is likewise discarded, not counted.
- `smoke.bats` is `no-lines` for the same reason: it drives git/XDG/stub binaries, never a tracked production script. `module.bats`'s `nix build` in `setup_file` is also untraced.
- The measurement indexes only the stamped commit.

| file                   | tests | measured | unmeasured | zero-unique share |
| ---------------------- | ----: | -------: | ---------: | ----------------: |
| adapters.bats          |   131 |       39 |         92 |            0.8462 |
| bats-affected.bats     |    21 |       21 |          0 |            0.9048 |
| crew.bats              |   539 |      539 |          0 |            0.8479 |
| crew-dash.bats         |    12 |       11 |          1 |            0.9091 |
| crew-id.bats           |     5 |        5 |          0 |            1.0000 |
| crews.bats             |    63 |       63 |          0 |            0.8889 |
| dispatch.bats          |   800 |      780 |         20 |            0.8167 |
| dispatch-comment.bats  |    10 |       10 |          0 |            1.0000 |
| dispatch-config.bats   |    40 |       37 |          3 |            0.9189 |
| dispatcher.bats        |    39 |       39 |          0 |            0.8205 |
| dispatch-notify.bats   |    15 |       15 |          0 |            0.9333 |
| dispatch-resume.bats   |   162 |      157 |          5 |            0.8471 |
| hold.bats              |    24 |       24 |          0 |            0.7083 |
| model-map.bats         |     6 |        6 |          0 |            1.0000 |
| model-map-doc.bats     |     9 |        9 |          0 |            0.8889 |
| module.bats            |    39 |        0 |         39 |                na |
| permission-check.bats  |   147 |      146 |          1 |            0.9384 |
| pr-watch.bats          |    17 |       17 |          0 |            0.6471 |
| public-leak-guard.bats |    16 |       16 |          0 |            0.7500 |
| rate-autosweep.bats    |    21 |       20 |          1 |            0.9500 |
| rate.bats              |    44 |       44 |          0 |            0.8864 |
| rate-sweep-all.bats    |    11 |       11 |          0 |            0.6364 |
| reap-race.bats         |     6 |        6 |          0 |            1.0000 |
| refresh-budget.bats    |   100 |      100 |          0 |            0.9300 |
| refresh-models.bats    |     4 |        4 |          0 |            0.5000 |
| refresh-scores.bats    |     6 |        6 |          0 |            0.6667 |
| retro.bats             |    29 |       29 |          0 |            0.9310 |
| secret-read-guard.bats |   483 |      479 |          4 |            0.9958 |
| smoke.bats             |     3 |        0 |          3 |                na |
| worktree-git.bats      |    59 |       59 |          0 |            0.8983 |

With ~2,850 tests sharing entry paths, zero-unique reads high everywhere
(0.65–1.00 on 25 of 28 measured files; `pr-watch.bats` is 0.6471,
`rate-sweep-all.bats` is 0.6364, `refresh-models.bats` is 0.5000). The table
is descriptive: it finds tests with large unique sets worth protecting and
feeds the candidate list. It is not a cut signal by itself.

Unmeasured: **169 tests, all `no-lines`** (zero `outcome` mismatches). Dominated
by `adapters.bats` structure and doc tests (92) that grep or `cmp` files
without executing production bash, and by `module.bats` (39, store-path
executables the reducer discards, plus an untraced `nix build`).
These 169 are excluded from the zero-unique share and from the cut candidates.

## Cut candidates (#714)

A candidate needs zero unique lines and a sibling test with the same input
class and the same assertion. Security regressions (`secret-read-guard`,
`permission-check`, the git-config guard, grant checks) stay even when
line-redundant: each pins a specific bypass input.

Cross-referencing the 2,358 measured zero-unique tests at `d321f8f` with the
converted families finds **no applied cuts**. #712 already removed the exact
duplicates. The conversions removed 127 standalone `@test` titles and added
49 table-driven tests (net 2,861 → 2,783); near-duplicate family members
differ in input, so they became table rows, not cuts. The rows
below were read in full; none is the same input class and the same assertion
as its sibling.

| candidate                                                                                    | decision | reason                                                                                                                                                                                             |
| -------------------------------------------------------------------------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pr_open: a pi accept then one non-verdict message is refused` (five rows)                   | kept     | Same assertion (`_refused "no review seam"`) after one planted accept, but each row's bus body is a different bypass (unparseable, fenced JSON, JSON array, a `tag` key, `final` plus a question). |
| `msg: rejects a recipient prefix with an empty id` (`dispatcher:` / `worker:` / `retro:`)    | kept     | Same status-1 assertion and the same "missing an id" fragment; the recipient prefix is the input under test.                                                                                       |
| `claim: '#42' canonicalises to issue 42` and `claim: a bare '42' stays issue 42`             | kept     | Both end at issue 42. The inputs are `#42` and `42`; only the hash form asserts the `claim-issue` bus row.                                                                                         |
| `secret-read-guard: denies cat .env` / `denies grep KEY .env` / `denies rg TOKEN .env.local` | kept     | File zero-unique share is 0.9958. Each row is a different command. Security regression.                                                                                                            |
| `budget rung and exhaustion gates refuse codex sol by 7d percent` (70 / 84 / 95)             | kept     | Same dispatch invocation shape; the percent and the expected refusal fragment change (70 and 84 name `gpt-5.6-terra` and forbid "quota exhausted"; 95 is the exhaustion row).                      |

## Table-driven families (#714)

Conversion rules: each row isolates its fixture to the degree the family
needs. `crew.bats` rows unset `STUB_DIR`/`STUB_LOG`, take a fresh
`XDG_DATA_HOME` under `$BATS_TEST_TMPDIR`, restore `PATH`, and truncate the
bus log. `dispatch.bats` refusal families reuse the setup-time stubs and
truncate the events log per row. `secret-read-guard.bats` rows are
stateless. Row loops read the table on fd 3, so no command in a row body
can swallow the remaining rows from stdin. Every row runs; failures are
accumulated and named by row. No assertion was weakened.

49 families folded (17 in `crew.bats`, 11 in `dispatch.bats`, 21 elsewhere).
Line count rose by 905: per-row isolation and failure-accumulation scaffolding
costs more lines than the folded duplicates saved. The win is growth shape (a
new case is one row, not one test) and per-case failure naming, not line count.

| file                   | tests before | tests after | lines before | lines after |
| ---------------------- | -----------: | ----------: | -----------: | ----------: |
| crew.bats              |          539 |         508 |         8999 |        9361 |
| dispatch.bats          |          800 |         785 |        13301 |       13448 |
| secret-read-guard.bats |          483 |         475 |         3676 |        3742 |
| refresh-budget.bats    |          100 |          92 |         2014 |        2042 |
| permission-check.bats  |          147 |         144 |         1340 |        1382 |
| dispatch-config.bats   |           40 |          36 |          470 |         519 |
| dispatcher.bats        |           39 |          37 |          391 |         454 |
| model-map-doc.bats     |            9 |           7 |           93 |         137 |
| module.bats            |           39 |          36 |          725 |         768 |
| adapters.bats          |          131 |         129 |         2163 |        2224 |
| **total**              |     **2861** |    **2783** |    **42090** |   **42995** |

Before counts are `d321f8f`. After counts are `grep -c '^@test'` and `wc -l`
on `tests/*.bats` at this tree. Unconverted files are unchanged and included
in the total only.

### Mutation and contamination proofs

Each proof broke one row's expected fragment (or planted a contamination
assertion) and re-ran that test. The suite named the broken row and failed;
a passing neighbour did not hide it.

| family                                                                | break                                                       | result                                    |
| --------------------------------------------------------------------- | ----------------------------------------------------------- | ----------------------------------------- |
| F04 `msg: rejects a recipient prefix with an empty id`                | expected fragment replaced so the `retro:` row cannot match | `not ok`; the failure names the retro row |
| F12 `pr_open: a pi accept then one non-verdict message is refused`    | expected fragment broken on the JSON-array row              | failure names the JSON-array row          |
| F12 contamination                                                     | a row greps for a previous row's bus marker                 | failed (no leak)                          |
| F12 contamination                                                     | a row expects `no deslop seam` without planting an accept   | failed (no leak)                          |
| F33 `codex absolute limit refuses each absolute-limit signal`         | expected fragment broken on one signal row                  | failure names that row                    |
| F33 contamination                                                     | a row expects a previous row's refusal text                 | failed                                    |
| F32 `budget rung and exhaustion gates refuse codex sol by 7d percent` | expected fragment broken on one percent row                 | failure names that row                    |
| F32 contamination                                                     | a row expects a previous row's refusal text                 | failed                                    |
| `secret-read-guard.bats` direnv family                                | one deny row flipped to `allow_cmd`                         | failure names `direnv-dot-env`            |

### Examined, not converted

| family                                     | reason                                                                                  |
| ------------------------------------------ | --------------------------------------------------------------------------------------- |
| dispatch.bats F30, F31, F38, F39, F40, F43 | Shared success-path worktree names: a second row would resume the first row's worktree. |
| dispatch.bats F46                          | Fenced: the tests drive `dispatch --role-watch`.                                        |
| permission-check.bats F60–F67              | Append-only stub logs contaminate row 2.                                                |
| refresh-budget.bats F72                    | Classifier false pair: bodies truncated at the heredoc `}}`.                            |
