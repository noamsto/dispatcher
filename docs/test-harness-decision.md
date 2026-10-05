# Test harness decision

Issue: #724. Goal: a sub-3-minute CI test suite without dropping tests.

Status: **draft — evidence still landing**. Local benchmarks and the time-split
profile are final (fixed parser, see below); CI candidate numbers from the
`harness-bench` job and the slow-family sample extension are pending and marked
[PENDING] where they gate a conclusion.

## Method

- Canonical sample: `tests/harness/manifest.tsv` — 26 cases from
  `tests/crew-id.bats` (5), `tests/refresh-models.bats` (4), `tests/pr-watch.bats`
  (17), every mapped assertion preserved (`tests/harness/assertions.tsv`).
- Candidates: bats compatibility, bats tuned (per-file `--filter`, warm
  TMPDIR), ShellSpec, Go `testing`, pytest + xdist. Adapters under
  `tests/harness/<candidate>/`; all drive the production scripts as black-box
  subprocesses through the same fixtures and PATH stubs.
- Driver: `scripts/harness-bench.sh` — 5 repetitions, `taskset 0-3` (verified
  four-core affinity, matching the CI runner), per-case wall + user/sys CPU via
  GNU time, medians with IQR.
- Time split: `scripts/harness-split.sh` + `tests/harness/profile/parse_trace.py`
  — `strace -f -ttt -T` per case, process-tree reconstruction, no
  double-counting; buckets harness / production / wait / residual sum to case
  wall time. The parser stitches `strace -f` `<unfinished ...>` / `<resumed>`
  syscall pairs (a real 17-case pr-watch trace carries 117 split lines; the
  pre-fix parser silently dropped those execs and sleeps).
- Suite weights: `tests/shard-weights.tsv` from a full-suite
  `scripts/bats-timing.sh` run on a loaded shared host (total ≈ 2522 s vs the
  ≈ 1875 s CI audit) — relative balance only, not absolute CI times.

## Where suite time goes (time split, 26 cases)

| family         | cases | wall s | harness    | production | wait        | residual   |
| -------------- | ----- | ------ | ---------- | ---------- | ----------- | ---------- |
| pr-watch       | 17    | 31.16  | 4.03 (13%) | 3.33 (11%) | 16.07 (52%) | 7.74 (25%) |
| crew-id        | 5     | 2.13   | 0.86 (40%) | 0.11 (5%)  | 0           | 1.16 (54%) |
| refresh-models | 4     | 1.82   | 0.70 (38%) | 0.14 (8%)  | 0           | 0.98 (54%) |

pr-watch waits are 100% `sleep` and 100% inside production children
(`diagnostic_production_wait_s` = `bucket_wait_s` = 16.07): the poll loop's
`--interval 1` floor and first-poll timing. No harness choice touches them.

**Residual** is time in descendant processes that are neither production-script
execs nor sleeps, plus `strace -f` overhead spread across all processes: test-body
fixture setup, stub creation and stub execution, and fork/exec of the test body
itself. It dominates fast fork-heavy files (a single fast crew-id case spawns
~43 bash processes) and is 25% even on pr-watch. Harness swaps that keep
bash fixtures and PATH stubs keep most of it.

## Suite weights: the sample is not where CI time goes

Local full-suite weights (loaded host; relative balance only):

| file                                           | weight s | share |
| ---------------------------------------------- | -------- | ----- |
| tests/dispatch.bats                            | 1012     | 40%   |
| tests/crew.bats                                | 418      | 17%   |
| tests/secret-read-guard.bats                   | 246      | 10%   |
| tests/dispatch-resume.bats                     | 187      | 7%    |
| tests/refresh-budget.bats                      | 159      | 6%    |
| tests/model-map.bats                           | 154      | 6%    |
| tests/pr-watch.bats                            | 23       | 1%    |
| tests/crew-id.bats + tests/refresh-models.bats | ~2       | ~0.1% |

The 26-case sample (≈ 25 s of ≈ 2522 s, 1%) measures harness overhead well but
under-represents the slow, wait-heavy families that dominate CI. Per owner
direction, one slow family (role-watch in dispatch.bats or a secret-read-guard
family) joins the sample before the recommendation is final [PENDING].

## Candidate benchmarks (local, 5 reps, taskset 0-3, 0 failures)

Run wall medians over the 26-case manifest, serial per-case:

| harness            | run wall median ms | run CPU (user+sys) median ms |
| ------------------ | ------------------ | ---------------------------- |
| bats compatibility | 26550              | 12160                        |
| bats tuned         | 25930              | 11580                        |
| ShellSpec          | 27230              | 10260                        |
| pytest + xdist     | 24190              | 8150                         |
| Go testing         | 21170              | 7380                         |

Per-case wall / CPU medians (ms) by family:

| harness     | crew-id   | pr-watch   | refresh-models |
| ----------- | --------- | ---------- | -------------- |
| Go          | 140 / 220 | 1220 / 300 | 160 / 240      |
| pytest      | 260 / 250 | 1330 / 320 | 260 / 250      |
| bats tuned  | 280 / 330 | 1420 / 490 | 280 / 330      |
| bats compat | 300 / 360 | 1440 / 500 | 300 / 360      |
| ShellSpec   | 360 / 320 | 1450 / 400 | 370 / 330      |

Read: on fast fork-heavy cases Go is ~2× bats-tuned on wall and ~1.5× on CPU;
on wait-dominated pr-watch cases every harness is within ~15% because the wall
is sleeps, not harness. CI candidate numbers [PENDING — `harness-bench` job on
this branch's PR].

ShellSpec micro-benchmarks (`tests/harness/shellspec/measure.sh`): noop example
overhead ≈ 15 ms/example vs bats ≈ 23 ms/test, but ShellSpec function mocks cost
12.4 ms/call vs 3.6 ms/call for PATH-stub execs — mocks are 3.4× **slower** than
the stubs they would replace, and all three production entry points need
`When run script` (no `Include`/`When call` seam), so ShellSpec stays a
subprocess runner with worse ergonomics. Eliminated.

pytest validity note: the dispatcher relayed failing pytest counts
(10/15, 11/12, 51/51) that do not match the current 26-case suite. Local
evidence on this head: `run.sh` (serial) 26 passed, `run.sh xdist` 26 passed,
and 5 benchmark repetitions with 0 failures. If a failing environment
reproduces, treat pytest timings as invalid until explained [PENDING — CI run
is the environment-independent check].

## External evidence

- #721 (closed, no PR): cutting 575 s of summed test time did **not** change the
  ~15 min CI step — the suite was CPU/scheduler-bound on 4 cores, not
  test-time-bound. Unvalidated externally, consistent with everything below.
- #727 (merged, e6a2553): 4-way bats matrix + count-based sharding cut the CI
  job to 7m48s (shards 5m46s–7m40s). This branch rebalances the shards by
  measured weights (631.0/631.0/631.1/631.0 s locally) and adds
  `bats-shard.sh --check` coverage proof to lint.
- #712: stall-watch's virtual clock (`CREW_STALL_CLOCK`, `_sw_now`/`_sw_sleep`)
  cut that family 622 s → 49 s (92%).

## Clock seams first (owner direction)

Harness choice moves per-case overhead by tens to hundreds of ms; the suite's
top files are dominated by real-time waits. Ranked seams, savings estimated
from the split data and weights:

1. **pr-watch virtual clock** — 16.07 s of 31.16 s profiled wall is
   production-internal sleep. File weight 23.3 s → est. 7–9 s (save ~15 s).
   Small in absolute terms; also the proof case the pattern ports cleanly.
2. **role-watch / dispatch.bats wait seams** — dispatch.bats is 1012 s (40% of
   the suite): `*` 443 s, role-watch 188 s, add-dir 75 s, claim 60 s, grid 56 s.
   Unprofiled as yet [PENDING — slow-family sample addition measures this]. If
   its poll/hold-timer waits profile like pr-watch's, a virtual clock plus
   event-waits is the single largest lever in the repo (est. 300–500 s
   suite-wide at 50–80% wait share — to be replaced by measurement).
3. **crew await + hold timers** — crew.bats await families ≈ 49 s, pr_open 36 s;
   event-waits replacing test-side fixed sleeps.
4. **secret-read-guard** — 246 s, timing-tagged and serial by fence; event-waits
   rather than a clock, since its guards are timing-sensitive.

Serial floors that bound any projection: module.bats ≈ 1m50 per shard runner
(cache warming), the timing job ≈ 2m28. A sub-3-minute sharded suite requires
the seams above; no candidate harness gets there alone (best case, Go at the
observed per-case deltas, projects to roughly −20% on fast files and ~nothing
on wait-heavy ones).

## Recommendation [PENDING]

Preliminary: **keep bats as the runner; land clock seams first; revisit a Go
port only if post-seam profiles show harness+residual dominating the remaining
fast files.** Go leads every local benchmark and its port is already proven
(26/26 assertions preserved), so it stays the designated successor if a port is
justified after the seams land. Final call waits on: CI `harness-bench`
numbers, the slow-family sample extension, and the dispatch.bats wait profile.

## Migration slices (ordered)

1. Weight-aware shard rebalance + `--check` lint gate (this branch).
2. pr-watch virtual clock (smallest wait-heavy family; proves the pattern
   outside stall-watch).
3. Slow-family sample addition + dispatch.bats wait profile (evidence, no
   behavior change).
4. role-watch / dispatch.bats clock seams, then crew await event-waits.
5. Harness decision finalization; if Go: devshell wiring, runner, CI step,
   affected-selection, then manifest-case ports with bats originals removed
   only after parity (assertion map + coverage + 3 mutation checks per slice).

## Regenerating this evidence

```bash
nix develop -c bash scripts/harness-bench.sh --repetitions 5 --output bench-out
nix develop -c bash scripts/harness-split.sh --output split-out
nix develop -c bash scripts/bats-timing.sh   # full-suite weights input
nix-shell tests/harness/shellspec/shell.nix --run 'bash tests/harness/shellspec/measure.sh'
```

Raw run artifacts are reproducible outputs, not checked in; the tables above
are the record.
