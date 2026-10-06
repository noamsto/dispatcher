# Test harness decision

Issue: #724. Goal: a sub-3-minute CI test suite without dropping tests.

Status: **final**. Local benchmarks (31 cases), the time-split profile
(fixed parser, see below), the slow-family extension, and CI candidate
numbers from the `harness-bench` job (run 37304440476) are all in.

## Method

- Canonical sample: `tests/harness/manifest.tsv` — 31 cases from
  `tests/crew-id.bats` (5), `tests/refresh-models.bats` (4), `tests/pr-watch.bats`
  (17), and the role-watch family of `tests/dispatch.bats` (5, the slow
  wait/CPU-heavy family added per owner direction), every mapped assertion
  preserved (`tests/harness/assertions.tsv`).
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
- Suite weights: `tests/shard-weights.tsv` currently comes from a full-suite
  `scripts/bats-timing.sh` run on a loaded shared host (total ≈ 2376 s
  post-rebase vs the ≈ 1875 s CI audit) — relative balance only, not absolute
  CI times. The CI artifact path below supersedes that input after this PR has
  produced its first successful main run; it must not be claimed as regenerated
  before then.

## Where suite time goes (time split, 31 cases)

| family         | cases | wall s | harness    | production | wait        | residual    |
| -------------- | ----- | ------ | ---------- | ---------- | ----------- | ----------- |
| pr-watch       | 17    | 31.16  | 4.03 (13%) | 3.33 (11%) | 16.07 (52%) | 7.74 (25%)  |
| role-watch     | 5     | 78.47  | 3.95 (5%)  | 0.54 (1%)  | 16.65 (21%) | 57.33 (73%) |
| crew-id        | 5     | 2.13   | 0.86 (40%) | 0.11 (5%)  | 0           | 1.16 (54%)  |
| refresh-models | 4     | 1.82   | 0.70 (38%) | 0.14 (8%)  | 0           | 0.98 (54%)  |

pr-watch waits are 100% `sleep` and 100% inside production children
(`diagnostic_production_wait_s` = `bucket_wait_s` = 16.07): the poll loop's
`--interval 1` floor and first-poll timing. No harness choice touches them.

role-watch waits are likewise 100% production-internal `sleep` (16.65 s =
`diagnostic_production_wait_s`): the watcher's fixed 0.6 s start sleep, 0.8–1.5 s
post-delivery lingers, and 0.2 s poll interval. Its 73% residual is the
watcher's poll-loop fork storm (tmux capture/send, jq, git per 0.2 s tick per
pane) magnified by `strace -f`; uninstrumented, the cases run 3–5 s wall and
the fixed sleeps put the real wait share at an estimated 60–80%. Harness
overhead is 5% — no harness choice moves this family; only a clock seam does.

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
| tests/dispatch.bats                            | 1025     | 42%   |
| tests/crew.bats                                | 373      | 15%   |
| tests/secret-read-guard.bats                   | 246      | 10%   |
| tests/model-map.bats                           | 154      | 6%    |
| tests/refresh-budget.bats                      | 149      | 6%    |
| tests/dispatch-resume.bats                     | 123      | 5%    |
| tests/pr-watch.bats                            | 23       | 1%    |
| tests/crew-id.bats + tests/refresh-models.bats | ~2       | ~0.1% |

The original 26-case sample (≈ 25 s of ≈ 2455 s, 1%) measured harness overhead
well but under-represented the slow, wait-heavy families that dominate CI. Per
owner direction the role-watch family of dispatch.bats (5 cases, family weight
188 s — the suite's slowest wait/CPU-heavy family that still runs in the
sharded suite) joined the sample, taking it to 31 cases (≈ 60 s of run wall).

## Candidate benchmarks (local, 5 reps, taskset 0-3, 0 failures)

Run wall medians over the 31-case manifest, serial per-case:

| harness            | run wall median ms | vs bats compat | run CPU (user+sys) median ms |
| ------------------ | ------------------ | -------------- | ---------------------------- |
| bats compatibility | 60410              | —              | 35180                        |
| bats tuned         | 60590              | +0.3%          | 35350                        |
| ShellSpec          | 43140              | −29%           | 14870                        |
| pytest + xdist     | 38640              | −36%           | 11300                        |
| Go testing         | 35570              | −41%           | 10710                        |

The earlier 26-case run (before the slow family joined) on the same host:
bats compat 26550, bats tuned 25930, ShellSpec 27230, pytest 24190, Go 21170 —
Go's wall advantage widened from −20% to −41% once wait/fork-heavy role-watch
cases were in the mix, because its per-case overhead stays flat where bats
pays bash interpreter + tempfile costs per test on top of the same waits.

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
is sleeps, not harness.

CI candidate numbers (`harness-bench` job, run 37304440476, 26-case manifest,
5 reps, 0 failures in every harness; per-case wall medians ms):

| harness     | crew-id | pr-watch | refresh-models |
| ----------- | ------- | -------- | -------------- |
| Go          | 120     | 1210     | 135            |
| pytest      | 200     | 1300     | 220            |
| bats tuned  | 280     | 1440     | 270            |
| bats compat | 290     | 1450     | 280            |
| ShellSpec   | 350     | 1460     | 340            |

CI reproduces the local ranking and spread (Go −17% on pr-watch wall, −59% on
crew-id wall vs bats compat), on quieter hardware.

CI run-wall medians over the 31-case manifest (run 37312458935, 5 reps, 0
failures in every harness):

| harness            | run wall median ms | vs bats compat |
| ------------------ | ------------------ | -------------- |
| bats compatibility | 79050              | —              |
| bats tuned         | 79040              | −0.0%          |
| ShellSpec          | 48680              | −38%           |
| pytest + xdist     | 42360              | −46%           |
| Go testing         | 37880              | −52%           |

The slow family widens Go's lead on CI even more than locally (−52% vs −41%):
the runner's slower cores amplify bats' per-case interpreter cost while the
waits stay constant. Ranking is identical local and CI; pytest ran 0 failures
in both CI runs, closing the validity question for good.

Post-seam CI (run 37315774340, 32-case manifest with `PR_WATCH_CLOCK` live, 5
reps, 0 failures):

| harness            | run wall median ms | vs bats compat | vs pre-seam CI |
| ------------------ | ------------------ | -------------- | -------------- |
| bats compatibility | 64630              | —              | −14.4 s        |
| bats tuned         | 64650              | +0.0%          | −14.4 s        |
| ShellSpec          | 35540              | −45%           | −13.1 s        |
| pytest + xdist     | 29180              | −55%           | −13.2 s        |
| Go testing         | 24920              | −61%           | −13.0 s        |

The pr-watch clock cut ~14 s of run wall in every harness — the estimated 15 s,
measured. And with the waits gone, Go's lead widened again (−52% → −61%): the
seams-then-redecide ordering matters, because each landed seam makes the
harness's own overhead a larger share of what remains.

ShellSpec micro-benchmarks (`tests/harness/shellspec/measure.sh`): noop example
overhead ≈ 15 ms/example vs bats ≈ 23 ms/test, but ShellSpec function mocks cost
12.4 ms/call vs 3.6 ms/call for PATH-stub execs — mocks are 3.4× **slower** than
the stubs they would replace, and all three production entry points need
`When run script` (no `Include`/`When call` seam), so ShellSpec stays a
subprocess runner with worse ergonomics. Eliminated.

pytest validity note: the dispatcher relayed failing pytest counts
(10/15, 11/12, 51/51). Traced to a stale `summary.tsv` from the early
pre-port-fix prototype run in the local evidence dir (since labeled
`bench-pytest.STALE-early-prototype-pre-port-fixes`). Current evidence, this
head: `run.sh` (serial) 31 passed, `run.sh xdist` 31 passed, 5 local benchmark
repetitions with 0 failures, and the CI `harness-bench` run above with 0
failures. Resolved — pytest timings stand.

## External evidence

- #721 (closed, no PR): cutting 575 s of summed test time did **not** change the
  ~15 min CI step — the suite was CPU/scheduler-bound on 4 cores, not
  test-time-bound. Unvalidated externally, consistent with everything below.
- #727 (merged, e6a2553): 4-way bats matrix + count-based sharding cut the CI
  job to 7m48s (shards 5m46s–7m40s). This branch rebalances the shards by
  measured weights (594.0/593.9/594.0/593.9 s locally, post-rebase) and adds
  `bats-shard.sh --check` coverage proof to lint.
- #712: stall-watch's virtual clock (`CREW_STALL_CLOCK`, `_sw_now`/`_sw_sleep`)
  cut that family 622 s → 49 s (92%).

## Clock seams first (owner direction)

Harness choice moves per-case overhead by tens to hundreds of ms; the suite's
top files are dominated by real-time waits. Ranked seams, savings estimated
from the split data and weights:

1. **role-watch / dispatch.bats wait seams** — measured by the slow-family
   sample: 21% wait under `strace` (est. 60–80% uninstrumented), all
   production-internal fixed sleeps. Family weight 188 s → est. 40–75 s (save
   ~110–150 s). The single largest measured lever in the repo. Note: the plan
   for this issue scopes H12 away from role-watch production paths, so this
   seam is a dispatcher decision, ranked first on the numbers.
2. **dispatch.bats `*` bucket (443 s)** — not wait- but fork/CPU-dominated:
   its slowest tests are table-driven tier-gate sweeps (31 s, 31 s, 19 s × 3)
   spawning one dispatch.sh per table cell. A clock does nothing here; the
   lever is fewer subprocess spawns per cell or a compiled harness (Go's −41%
   on the 31-case sample is mostly this bucket's cost shape).
3. **pr-watch virtual clock** — 16.07 s of 31.16 s profiled wall was
   production-internal sleep. **Landed on this branch** (`PR_WATCH_CLOCK`,
   mirroring `CREW_STALL_CLOCK`): post-seam split shows wait 16.07 s → 1.00 s
   (the residue is exactly the new default-path regression's intentional real
   1 s park), file weight 23.3 s → 3.8 s, `bats tests/pr-watch.bats` 23 s →
   4.1 s. Estimate said save ~15 s; measured save 19.5 s of file weight. The
   pattern ports cleanly to a second script.
4. **crew await + hold timers** — crew.bats await families ≈ 49 s, pr_open 36 s;
   event-waits replacing test-side fixed sleeps.
5. **secret-read-guard** — 246 s, timing-tagged and serial by fence; event-waits
   rather than a clock, since its guards are timing-sensitive.

Serial floors that bound any projection: module.bats ≈ 1m50 (moved off the
shard critical path by #744), the timing job ≈ 2m28. A sub-3-minute sharded
suite requires the seams above; no candidate harness gets there alone (best
case, Go at the observed 31-case delta projects to roughly −40% on
fork/CPU-bound files and ~nothing on wait-heavy ones until their seams land).

Projected CI test time, staged (shard critical path; today's main ≈ 7m48s
with module.bats repeated per shard, ≈ 6m once #744's single module job is
the only place it runs):

| stage                                           | critical path |
| ----------------------------------------------- | ------------- |
| main post-#727+#744 (measured)                  | ≈ 6m          |
| + this branch's weight-balanced shards          | ≈ 5m30–6m     |
| + pr-watch seam (landed here, −15 s suite-wide) | ≈ 5m15–5m45   |
| + role-watch seam (slice 4, −110–150 s suite)   | ≈ 4m45–5m15   |
| + secret-read-guard + crew event-waits (5, 6)   | ≈ 4m–4m30     |
| + Go port if the re-decision gate (7) says so   | ≈ 2m45–3m15   |

The last row straddles the 3-minute goal; the honest statement is that sub-3
is reachable only if the Go re-run after the seams shows the −40–60% harness
delta holding on the post-seam profile. Every stage before it is measured or
estimated from measured wait shares, not hoped for.

## Recommendation

**Keep bats as the runner. Land clock seams first, in the ranked order above.
Hold Go as the proven, designated successor: its port is assertion-complete
(31/31, plus 5 bench reps and a CI run with 0 failures) and leads every
benchmark local and CI — but −41% on the sample is mostly the fork/CPU bucket,
and the suite's top files are wait-dominated, so a port before the seams pays
full migration cost for a fraction of its eventual value. Re-run this
benchmark after seams 1 and 3 land; if harness+residual then dominates the
remaining profile, port to Go per the slices below.**

Why not the others, finally: bats-tuned is +0.3% — dead. ShellSpec is
eliminated (mock overhead 3.4× the PATH stubs it would replace, no `When call`
seam, −29% wall but worst CPU of the non-bats candidates). pytest is a
credible −36% but its validity needed a stale-evidence scare to resolve, its
port is assertion-complete yet unloved by the team's shell-native review
habits, and it loses to Go on every axis measured here — it stays the fallback
if Go's build step ever blocks the devshell.

## Parity proofs (first slice: pr-watch clock seam)

Assertion map: every manifest `assert_id` has exactly one counterpart per
candidate harness, machine-checked — `tests/harness/go/verify.sh` ("verified 32
result rows and assertion metadata"), `tests/harness/pytest/check-results.sh`,
and `tests/harness/shellspec/check-results.sh` all gate on it in CI. The
decision keeps bats, so no bats case is removed and no boundary needs an
integration replacement; the ports stand as the proven successor evidence.

Coverage (scripts/bats-coverage.sh, sample-scoped to tests/pr-watch.bats):
unique covered `adapters/core/pr-watch.sh` lines 61 (pre-seam aea199b) → 72
(HEAD). Every pre-seam covered statement has a covered counterpart post-seam
(the helper insertion shifted the loop +23); the virtual-clock path is covered
by 13 park/poll cases, the real path by the default-clock regression. No
production statement lost coverage.

Mutation checks (scratch worktrees at HEAD; bats = selected runner, Go =
designated successor; control: unmutated 18/18 bats ok, 18/18 Go pr-watch
subtests pass):

| mutation (pr-watch.sh)                          | bats 18 cases | Go 18 subtests | caught |
| ----------------------------------------------- | ------------- | -------------- | ------ |
| change-detection `!=` → `==` in changed-keys jq | 12 fail       | same 12 fail   | yes    |
| deadline `-ge` → `-lt`                          | 1 fail        | same 1 fail    | yes\*  |
| `_save` writes `{}`                             | 8 fail        | same 8 fail    | yes    |

\* the deadline flip is caught by exactly one test — the default-clock
regression added with the seam. Under the virtual clock the mutated park still
exits early with the right message; only the real-time `>= 1s` assertion sees
it. Thin but sufficient, and it is the argument for keeping that regression in
every future port.

Commands: `nix develop -c bats tests/pr-watch.bats` and
`cd tests/harness/go && go test -count=1 -run "TestManifest/^pr-watch" .` per
scratch worktree (mutation 2 under `timeout 600`; nothing hung), control in
the main worktree.

## Migration slices (ordered)

Landed on this branch:

1. Weight-aware shard rebalance + `--check` lint gate (warn-on-stale: an
   unweighted family falls back to a median-rate weight and is still
   scheduled — a PR that adds a family can never break or drop tests).
2. Slow-family sample addition + role-watch wait profile (evidence only).
3. pr-watch virtual clock (`PR_WATCH_CLOCK`; wait 16.07 s → 1.00 s, file
   23.3 s → 3.8 s; default-path regression included).

Future slices — one named family and one PR each, ordered by measured saving.
The owner accepted this recommendation and split the next slices into
parallel workers under #724:

4. **role-watch clock seam → #736** (dispatch.bats role-watch family; 188 s
   weight, measured 60–80% wait share → est. save 110–150 s, the largest
   lever). Retained coverage: the 5 role-watch manifest cases run in all four
   harnesses; per-slice mutation checks per the table above.
5. **spawn reduction in the dispatch.bats tier-gate sweeps → #737** (the
   fork/CPU-bound `*` bucket, 443 s: table-driven sweeps spawning one
   dispatch.sh per cell; the lever is fewer subprocess spawns per cell, not a
   clock).
6. **module.bats off every shard's critical path → landed as #744** (its
   multi-output nix build raced on a cold git-fetcher cache; now one
   unsharded job).
7. **secret-read-guard event-waits** (246 s, timing-tagged and serial by
   fence). Dependency: a wait-profile run first — its wait share is
   unmeasured, and its guards are timing-sensitive, so this is event-waits
   rather than a virtual clock. Unassigned as yet.
8. **crew await + hold event-waits** (crew.bats await families ≈ 49 s,
   pr_open ≈ 36 s). Dependency: none. Retained coverage: CREW_STALL_CLOCK
   already proves the pattern in this file (#712: 622 s → 49 s on
   stall-watch). Unassigned as yet.
9. **Harness re-decision gate**: re-run this benchmark with the seams landed.
   If harness+residual then dominates the remaining profile, port to Go —
   devshell wiring, runner, CI step, affected-selection, then manifest-case
   ports with bats originals removed only after parity (assertion map +
   coverage + 3 mutation checks per slice, as demonstrated above). If waits
   still dominate, keep bats and stop.

## Regenerating this evidence

### CI shard weights (staged delivery)

Each bats shard uploads one `bats-timing-shard-N` JUnit artifact, even when the
shard fails. After this PR merges and a successful `main` run has completed,
regenerate the committed weights from one or more such runs:

```bash
nix develop -c bash scripts/bats-shard-weights.sh --run <main-run-id> [--run <second-main-run-id>] > tests/shard-weights.tsv
nix develop -c bash scripts/bats-shard.sh --check
```

The importer uses `gh run download` and `yq`; it rejects a red report, a
duplicate case, or a run whose JUnit `(file, test)` inventory does not exactly
match the current non-timing suite. With two runs it averages each individual
test's milliseconds before it totals family weights. This branch intentionally
does not update `tests/shard-weights.tsv`: the preceding `main` runs have no
JUnit artifacts, so CI-derived weights do not yet exist.

For a planning-only lower bound, the current 2,406.6 weighted seconds divide
to 601.6 / 481.3 / 401.1 seconds at 4 / 5 / 6 perfectly balanced shards. That
is not a wall-clock projection: every extra shard also pays checkout plus Nix
setup, and the observed main run 37427270027 already shows 2–3 minutes of
per-job elapsed time. Keep four shards for the first artifact-backed rebalance;
compare real 4/5/6 assignments after regeneration before adding runners.

```bash
nix develop -c bash scripts/harness-bench.sh --repetitions 5 --output bench-out
nix develop -c bash scripts/harness-split.sh --output split-out
nix develop -c bash scripts/bats-timing.sh   # full-suite weights input
nix-shell tests/harness/shellspec/shell.nix --run 'bash tests/harness/shellspec/measure.sh'
```

Raw run artifacts are reproducible outputs, not checked in; the tables above
are the record.
