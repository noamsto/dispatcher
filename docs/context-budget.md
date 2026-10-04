# Session context budget

Measured report for issue #691: where does a Claude Code session's context go,
and which changes would reduce it? The study covers every Claude Code
transcript on this machine touched in the 7 days to 2026-10-04 (about 1,378
transcripts). The unit is **context tokens per turn, summed over turns**: each
turn re-reads the whole conversation so far, so a token that enters context at
turn _t_ costs one token for every later turn. All figures are weekly fleet
totals unless stated otherwise. The fleet total is 6.72B context tokens.

All numbers come from `scripts/context-budget.py` (read-only) and a set of
`claude -p` ablation runs; see [Method](#method) to reproduce them.

## Summary

- **Where the tokens go.** Worker leads use 3.30B (49.0%), dispatchers 1.21B
  (18.0%), subagents 1.96B (29.1%), role panes 0.21B (3.2%), interactive
  sessions about 0.05B. 74.6% of worker-lead turns and 85.8% of dispatcher turns
  run above 150k context.
- **A worker lead starts at 96.8k and ends near 199k.** The median run is 52
  turns, with a median integral of 8.6M. The starting context (the "floor") is
  42.6% of the worker-lead integral, because it is re-read on every turn.
- **`WORKER_PROTOCOL.md` is the largest piece of the floor: 41.8k tokens**
  (43%), not the ~29k previously estimated. The earlier figure assumed 4
  chars/token; the measured ratio is 2.73.
- **The floor is not the biggest lever; accumulated context is.** Restarting the lead
  fresh at the execute and review seams removes up to 28.5% of completed-run
  integral (864M/week). A fresh dispatcher every ~100 turns removes up to 41%
  of dispatcher integral, and 34% of it is idle wakes.
- **Gate output is not a problem.** Capping test and gate output removes about
  1% of worker-lead integral. Growth comes from many medium reads and searches
  plus the conversation itself, compounding over a large floor.

Ranked recommendations (savings are upper bounds from the models below; the
fleet total is 6.72B):

| Rank | Change                                                                                             | Saving per run/session                                      | Fleet saving per week                                      | Risk                                                                          | Effort                                        | Issue |
| ---- | -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------- | ---------------------------------------------------------- | ----------------------------------------------------------------------------- | --------------------------------------------- | ----- |
| 1    | Relaunch the worker lead fresh at the execute and review seams                                     | ~5.6M median per completed run (~4.5M with a 30k re-orient) | up to 864M (12.9%)                                         | Medium: lost tacit context, more re-reads; needs a dispatcher or harness hook | 1-2 days                                      | TBD   |
| 2    | Fresh dispatcher per batch (about every 100 turns)                                                 | 40.8% of a session's integral                               | up to 494M (7.4%)                                          | Low: state lives on the bus                                                   | Hours to 1 day (protocol plus a handoff note) | TBD   |
| 3    | Suppress idle dispatcher wakes (filter no-action notifications; do not re-arm on a drained roster) | 33.8% of a session's integral                               | up to 409M (6.1%); overlaps rank 2                         | Low                                                                           | Hours                                         | TBD   |
| 4    | Load protocols by phase (worker core plus seam files; dispatcher rare reference on demand)         | ~1.1M per worker run; turn-1 floor down by up to ~27k       | ~210M worker plus ~46M dispatcher = ~256M (3.8%)           | Medium: a lead that skips a seam read; both adapter copies and tests change   | 1-2 days                                      | TBD   |
| 5    | Lean launch profile: no claude.ai connectors, no unused plugins                                    | -11.3k tokens per turn                                      | ~157M workers; ~230M with role panes and dispatcher (3.4%) | Low                                                                           | In progress                                   | #690  |
| 6    | Narrow file reads and search output in workers                                                     | Not measured per run                                        | ~160M (2.4%), low confidence                               | Low to medium                                                                 | Hours                                         | TBD   |

Savings in rows 1-3 overlap with each other and with row 4 (a smaller floor makes
a restart cheaper), so the rows do not sum. Row 2 and row 3 overlap most.

**Not recommended:** capping gate output on its own (32M/week, 0.5% of the
fleet).

**No prototype in this PR.** The top items need harness behavior or
dispatcher-protocol changes across both adapter copies, and row 4 rewrites
`WORKER_PROTOCOL.md` in both adapter copies plus the launch carrier in
`dispatch.sh` and its tests. None is small enough to finish inside this run, so
this PR ships only the measurement script and this report.

## Method

**Window.** Claude transcripts with mtime in the last 7 days (default
`--since 7`), ending 2026-10-04: about 1,380 transcripts. The figures are one
snapshot; sessions were still being written, so a rerun moves counts by a few
(for example 174 or 175 completed runs, 906 or 908 dispatcher wakes). Codex, Cursor, and pi sessions are not included.

**Definitions.**

- **Turn context**: the input tokens of one assistant turn (fresh input plus
  cache read plus cache write), taken from the transcript's usage record.
- **Integral**: the sum of turn context over all turns of a session. It is the
  quantity that drives quota use, since nearly all of it is cache reads.
- **Carry cost** of a piece of content: its token size times the number of
  later turns in which it is still in context.
- **Floor**: first-turn context times number of turns. It is what a session
  would cost if it never grew.

**Session classes.** Each transcript is classified by launch prompt and by
working directory against the crew bus logs:

| Class                | Meaning                                                                       | Sessions |
| -------------------- | ----------------------------------------------------------------------------- | -------- |
| worker-lead          | Dispatcher-launched worker session driving one task                           | 191      |
| role-pane            | A grid-mode role pane (spec-critic, plan-critic, reviewer, refuter) of a lead | 83       |
| subagent             | Agent-tool sidechain inside another session                                   | 1,067    |
| dispatcher           | Long-lived dispatcher session                                                 | 22       |
| interactive          | A human-driven session                                                        | 15       |
| interactive-worktree | A human-driven session inside a worktree                                      | 1        |

Role panes are separate sessions, not subagents; they start smaller (median
first turn 59k).

**Phase markers.** A worker lead's phase comes from the in-transcript
`crew status ... working "<stage>"` heartbeats, plus the review and deslop
seam messages. For 62 of the 174 completed runs no usable heartbeat was found
and the phase falls back to tool heuristics (critic spawns mark spec and plan,
reviewer spawns mark review, the `deslop` skill marks pr). In those runs the `start` bucket absorbs spec and plan. Phase
medians below therefore blur spec, plan, and start slightly.

**Ratio calibration.** Chars-per-token is 2.7, measured from first-turn usage
deltas when a file is appended to the prompt: the protocol gives 2.73 (115,038
bytes, +41,848 tokens) and the CLAUDE.md import chain gives 2.60. The
`sections` output and Q3 use bytes/2.75 for the same file; both round to the
same figures. The older 4 chars/token estimate understated the protocol by a
third.

**Ablation design (Q1).** Variants of one `claude -p` first turn, with flags
mirroring the worker-lead launch (`--effort high --permission-mode auto`, four
`--add-dir`, a worktree cwd). Each variant adds or removes one component and
the difference in first-turn tokens is that component's size. Sonnet was
measured; an Opus baseline matched within 50 tokens. The `-p` run omits the
interactive-only pieces (hook text, connector listing), so a residual is
computed against the real first turn.

**Restart model (Q4, Q6).** After a seam at turn _s_, the fresh session's
context is `F + R + growth since the seam`, where `F` is the run's first-turn
context and `R` is a re-orient allowance (default 10k tokens). The saving is
the sum over later turns of the old context minus the new one. It assumes
growth after the seam is unchanged, so it is an upper bound.

**Reproduce.**

```sh
python3 scripts/context-budget.py fleet
python3 scripts/context-budget.py growth
python3 scripts/context-budget.py tools
python3 scripts/context-budget.py sections adapters/core/protocols/WORKER_PROTOCOL.md
python3 scripts/context-budget.py dispatcher
```

Global flags: `--since DAYS` (window, default 7), `--json`, `--ratio R`
(default 2.7), `--root DIR` (transcript root), `--reorient R` (restart-model
re-orient tokens, default 10000), and `--restart-every N,N` (dispatcher
restart intervals, default 50,100). Results depend on whichever transcripts
are still on disk, so a later run will differ slightly.

**Privacy.** Transcripts can contain secrets. The script classifies text in
memory only and prints aggregates: counts, token sums, percentiles, sizes, and
class names. No transcript text is copied into this report or the repo.

## Q1 - Composition of the ~96k floor

The fleet's median worker-lead first turn is 96.7k tokens (p90 111.8k). The
ablation splits it into non-overlapping parts:

| Component                                                                                       | Tokens | How measured                                                   |
| ----------------------------------------------------------------------------------------------- | ------ | -------------------------------------------------------------- |
| `WORKER_PROTOCOL.md`                                                                            | 41.8k  | +41,848 for 115,038 bytes                                      |
| Core built-in tool schemas (Bash, Read, Edit, Write, Grep, Glob, Agent, Skill), excl. listings  | ~10.5k | 8-tool run minus no-tool run, less the two listings            |
| Interactive-only: SessionStart hook text, connector listing and MCP instructions, launch prompt | ~10.3k | Residual: this session's first turn 96.0k vs `-p` analog 85.7k |
| CLAUDE.md import chain plus `MEMORY.md` index                                                   | 8.3k   | Appended a copy of the chain: +8,324 (2.60 chars/token)        |
| MCP servers (plugin MCPs: context7, playwright, firefox-devtools)                               | 6.3k   | `--strict-mcp-config`: -6,294                                  |
| Claude Code base prompt plus environment and git status                                         | ~6.1k  | No-MCP, no-tools run (14.5k) minus the CLAUDE.md chain         |
| Agent tool's agent-type listing                                                                 | ~4.3k  | `--disallowedTools Agent`: -4,302 (includes the Agent schema)  |
| Other built-in tools plus deferred-tool listing                                                 | ~4.2k  | Full tool set minus the 8-tool run                             |
| Skills listing                                                                                  | 4.0k   | `--disable-slash-commands`: -3,999                             |
| **Total**                                                                                       | ~96k   | Fleet median 96.7k                                             |

Other measurements:

- Turning off all 7 enabled plugins removes 5.0k, which overlaps the skills
  listing, the agent listing, and hook text.
- For comparison, appending `DISPATCHER_PROTOCOL.md` adds 48.4k and
  `GRID_PROTOCOL.md` adds 5.2k.
- Restricting tools with `--tools Bash` is an anomaly: it disables deferred MCP
  loading and inlines every MCP schema (105.7k). Do not use it as a slimming
  tactic.
- A lean variant (no MCP, no plugins) with the protocol is 74.3k against 85.7k,
  so #690 saves 11.3k per turn. After #690 the interactive floor is about
  80-85k and the worker protocol is about half of it.

**Answer.** The protocol is the largest single item at 43% of the floor; the
rest is spread across tool schemas, listings, MCP, and instructions, none above
11%. #690 trims the connector and plugin share (~157M/week for workers, ~230M
including role panes and the dispatcher). Anything larger has to shrink the
protocol itself (Q3).

## Q2 - Context growth per phase

174 completed worker-lead runs; median 52 turns, first turn 96.8k, end and peak
198.8k. Delta is the median context added during the phase; integral share is
the phase's slice of the worker-lead integral.

| Phase   | Runs | Median turns | Median context added | Share of integral |
| ------- | ---- | ------------ | -------------------- | ----------------- |
| start   | 174  | 18           | 49k                  | 20.0%             |
| spec    | 27   | 12           | 23k                  | 2.6%              |
| plan    | 67   | 9            | 19k                  | 7.1%              |
| execute | 90   | 14           | 27k                  | 20.1%             |
| gate    | 64   | 13.5         | 19k                  | 8.8%              |
| review  | 137  | 14           | 23k                  | 25.2%             |
| pr      | 174  | 7            | 4k                   | 16.2%             |

Runs counts the completed runs that entered the phase; standard-tier runs have
no spec phase, and many runs post no gate heartbeat. `start` is the turns
before the first heartbeat (orientation); in fallback runs
it absorbs spec and plan. Review holds the largest integral share because it
runs at the highest context. `pr` is short but expensive for the same reason:
7 turns at about 194k.

Composition of the worker-lead integral, by carry cost:

| Source                                                  | Share of integral |
| ------------------------------------------------------- | ----------------- |
| Floor (first-turn context times turns)                  | 42.6%             |
| Tool results                                            | 21.7%             |
| Tool inputs                                             | 8.6%              |
| Assistant text                                          | 0.4%              |
| Thinking (transcripts store it nearly empty; uncertain) | 0.1%              |
| Unattributed (user-role content)                        | 26.6%             |

Within tool inputs, Edit and Write payloads are 28.7% and Agent prompts 24.1%.
The unattributed share is user-role content the script does not split: system
reminders (including file-change notices), hook output, task notifications, and
subagent hand-back messages.

**Answer.** Growth is broad rather than driven by a few huge outputs. About 49k
of orientation, then 20-27k per working phase, on top of a floor that is
re-read every turn. Median context at the start of execute is 187k and at the
start of review 193k, so the late phases run at roughly twice the floor.

## Q3 - Protocol by phase

### Worker protocol

`WORKER_PROTOCOL.md` is 115,038 bytes (about 41.8k tokens, bytes/2.75). Each
`##` section was classified by when a worker lead needs it:

| Group                                          | Sections                                                                                                                                                                                       | Size                  |
| ---------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------- |
| Core (always on)                               | Title 0.3K, Process authority 1.7K, First action 3.2K, Task kind 1.2K, Pipeline by tier 1.4K, Gating verdicts 1.8K, Checkpoint-peek 3.8K, Report to the bus 11.8K, Rules 11.8K, When done 4.0K | ~41 KB, ~14.9k tokens |
| Plan seam (plan phase only, consult deep only) | Plan of record 3.6K, Orchestration consult 4.7K, Cross-engine one-shots 3.5K                                                                                                                   | ~11.8 KB, ~4.3k       |
| Execute and gate seam                          | Fast deterministic gate 3.1K (1.1k tokens); Bounded plan-shaped recovery 6.0K (2.2k tokens, only on a gate failure episode)                                                                    | ~9.1 KB, ~3.3k        |
| Review seam                                    | Code review gate 19.5K, PR body contract 2.0K, Deferred findings 4.4K, Acceptance ledger 1.9K                                                                                                  | ~27.8 KB, ~10.1k      |
| Conditional (only when stamped or triggered)   | Grid mode 11.9K (`roles:`), Base ref 5.5K (`base:`, at gate and push), Resuming 1.7K (`resume: true`), Retro notes 4.5K (failure branch), Deliberate load 0.6K                                 | ~24.2 KB, ~8.8k       |

**Saving model.** A section read mid-run stays in context from then on, so
loading it on demand saves its tokens times the turns before it is first read
(all turns if it is never read). Inputs, over 191 worker-lead runs: 13,920
turns in total, 7,845 turns before the first review in completed runs, 3,322
turns before execute. About 21% of runs are grid runs (83 role panes is roughly
40 grids).

| Section group                  | Computation                         | Saving per week |
| ------------------------------ | ----------------------------------- | --------------- |
| Grid mode                      | 4.3k x 13,920 turns x 0.79 non-grid | ~47M            |
| Review group                   | 10.1k x 7,845 turns                 | ~79M            |
| Resume, retro, deliberate load | 2.4k x 13,920 turns                 | ~33M            |
| Bounded recovery               | 2.2k x 13,920 turns                 | ~31M            |
| Base ref                       | 2.0k x 7,845 turns                  | ~16M            |
| Fast gate                      | 1.1k x 3,322 turns                  | ~4M             |
| Plan group                     | Counted as 0 (conservative)         | ~0              |
| **Total**                      |                                     | **~210M**       |

That is 6.4% of worker-lead integral, 3.1% of the fleet, and about 1.1M per
run. The turn-1 floor drops by up to ~27k.

**Design sketch.** Pass the core through `--append-system-prompt-file` and put
phase files (for example `WORKER_PROTOCOL.review.md`) in the same
`protocol_dir`. The core names each seam's file and tells the lead to read it
at the seam heartbeat. Enforcement already exists for the review and deslop
seams (`crew status` refuses `pr_open` without them). The risk is a lead acting
without reading a phase file; a mitigation is for `crew status working "<stage>"`
to print the path to read. Rendering per engine (a Claude worker needs no
codex, cursor, or pi text) would trim a little more: about 50 of 693 lines name
another engine, a few k tokens, not measured precisely.

### Dispatcher protocol

`DISPATCHER_PROTOCOL.md` is 136,052 bytes (about 48.4k tokens measured):

| Section                        | Size    | Note                                                      |
| ------------------------------ | ------- | --------------------------------------------------------- |
| Decide tier and model (levers) | 29.3 KB | Triage reference                                          |
| Scaffold one worker per task   | 58.1 KB | Dispatch reference                                        |
| Read the bus                   | 36.9 KB | Of which "Relaying a pane to the human" is ~22.9 KB, rare |
| Roster diagram                 | 5.2 KB  |                                                           |
| Rules                          | 4.4 KB  |                                                           |
| Retitle                        | 0.9 KB  |                                                           |

An always-on core would be about 20 KB (~7k tokens). A long-lived dispatcher
reads the triage and scaffold reference at its first batch and keeps it, so
the split pays mainly for the rarely used parts: relaying a pane plus the
roster diagram is about 10k tokens times 4,485 turns, roughly 46M/week (3.8%
of dispatcher integral). It pays more when combined with fresh sessions per
batch (Q6).

**Answer.** About 6.4% of worker-lead integral is recoverable by loading the
protocol by phase, with the review group and grid mode the two big pieces.
The yield is real but modest next to restarts (Q4), and it carries a
compliance risk that restarts do not.

## Q4 - Phase-seam restarts

Restart the lead fresh at a seam (the harness relaunches it with a re-orient
note) and compare against letting context keep growing. Model: after the seam,
`ctx' = F + R + growth since the seam`, with `F` the run's first-turn context
(about 97k) and `R` the re-orient allowance. Totals are over the completed
runs, whose combined integral is about 3.0B.

| Seam             | R = 10k: saving | R = 10k: share of integral | R = 30k: share of integral |
| ---------------- | --------------- | -------------------------- | -------------------------- |
| Plan             | 558M            | 18.4%                      | 14.8% (445M)               |
| Execute          | 672M            | 22.2%                      | 18.2%                      |
| Review           | 632M            | 20.8%                      | 18.1%                      |
| Execute + review | 864M            | 28.5%                      | 24.7% (744M)               |

For execute plus review at R = 10k the median saving is 5.6M per run (about
4.5M at R = 30k). The median context at the start of execute is 187k and at the
start of review 193k, against a fresh-session floor of about 97k.

**Cost note.** Each restart is one uncached write of about `F + R` tokens
(107-127k). A cache write costs 12.5 times a cache read, so a restart costs
about 1.3-1.6M read-equivalent tokens. Two restarts cost 2.6-3.2M against a
median saving of 4.5-5.6M per run, so the change is net positive even in
dollar terms.

**Non-token costs.** The fresh lead loses tacit context (why a fix was chosen)
and re-reads files. Much of the mechanism exists: `dispatch resume --fresh`
relaunches with a reorient note built from `SPEC.md`, `PLAN.md`, and git;
reviewers are already fresh-context subagents; and the review ledger lives in
the PR body. `/compact` is an alternative, but a lead cannot invoke it on
itself; a dispatcher could type it into the pane.

**Answer.** Restarting at execute and review is the largest measured saving
(up to 864M/week, 12.9% of the fleet). It is an upper bound because post-seam
growth is held unchanged. The plan seam models a similar saving (558M), but
plan-phase boundaries are the least reliable (see Method).

## Q5 - Gate output hygiene

Gate commands (bats, flake check, go test, shellcheck, other nix) carry about
68M of the 3.29B worker-lead integral (2.1%). Capping each result at 2,000
characters:

| Class       | Carry cost | After cap | Saving | Share of worker-lead integral |
| ----------- | ---------- | --------- | ------ | ----------------------------- |
| bats        | 56.9M      | 27.5M     | 29.5M  | 0.89%                         |
| flake check | 1.9M       | 1.4M      | 0.5M   | 0.02%                         |
| go test     | 3.2M       | 2.7M      | 0.5M   | 0.02%                         |
| shellcheck  | 0.4M       | 0.3M      | 0.1M   | 0.00%                         |
| other nix   | 5.4M       | 4.2M      | 1.1M   | 0.03%                         |
| **Total**   | 67.8M      | 36.1M     | 31.7M  | 0.96%                         |

Gates are already scoped and their output is small: the median bats result is
about 220 tokens (p90 about 1.9k).

The material tool-output classes are elsewhere. Share of tool-result carry:

| Class                                              | Share of tool-result carry |
| -------------------------------------------------- | -------------------------- |
| Shell file reads (`sed -n`, `cat`, `head`, `tail`) | 32.5%                      |
| `rg` / `grep`                                      | 22.7%                      |
| `git diff`, `log`, `show`                          | 8.9%                       |
| bats                                               | 8.0%                       |
| Read tool, other                                   | 7.2%                       |
| `crew`                                             | 4.8%                       |
| Read tool, markdown                                | 3.7%                       |
| Agent results                                      | 3.2%                       |
| `gh`                                               | 3.2%                       |

Reads of `crew.sh` and `dispatch.sh` through the Read tool are tiny (0.1% each)
because the skeleton-read guard pushes them to `sed -n` slices, which land in
the shell-read class.

**Answer.** Do not cap gate output on its own: 32M/week is 0.96% of
worker-lead integral and 0.5% of the fleet. File reads and searches are about
14.6% of worker-lead integral including the Read tool. Guidance to read narrow
slices, delegate broad surveys to an Explore subagent, and cap `rg` with
`--max-count` could plausibly cut a third of that, about 160M/week (2.4% of the
fleet), at low confidence.

## Q6 - Dispatcher

22 dispatcher sessions, 4,485 turns, 1.21B tokens (18% of the fleet). The median
session is 164 turns; the peak context is 704k.

| Point    | Median context |
| -------- | -------------- |
| Turn 1   | 107k           |
| Turn 50  | 172k           |
| Turn 100 | 212k           |
| Turn 200 | 307k           |

Wakes: 906 notification-driven wakes (nearly all task notifications, a handful
of peer messages). Of these, 640 were idle: none of the following turns
dispatched, merged, messaged, replied, spawned an agent, or edited a file. The
1,406 idle-wake turns run at a median context of 269k and account for 409M, or
33.8% of dispatcher integral (6.1% of the fleet). All wake turns together are
61.4% of dispatcher integral.

Restart model, a fresh dispatcher every N turns:

| Interval  | R = 10k saving | R = 30k saving |
| --------- | -------------- | -------------- |
| 50 turns  | 586M (48.4%)   | 42.7%          |
| 100 turns | 494M (40.8%)   | 36.4%          |

The bus (crew roster and log) holds the dispatcher's state, so per-batch
restarts are cheap. Restart and idle-wake savings overlap, since restarts make
the idle turns cheaper; do not add them.

**Answer.** The dispatcher is the most skewed consumer: it runs above 150k for
85.8% of turns and a third of its cost is waking up to do nothing. A fresh
dispatcher per batch (up to 494M) and filtering no-action notifications (up to
409M) attack the same cost from two sides.

## Not measured

- **Other engines.** Codex, Cursor, and pi sessions use different transcript
  stores and are excluded. pi has its own `scripts/cache-report.sh`.
- **Dollar cost.** Nearly all context tokens are cache reads, so the savings
  here are quota and limit savings, not proportional dollar savings. Restart
  rewrites are priced in the cost note under Q4 only.
- **Unattributed content.** 26.6% of the worker-lead integral is user-role
  content (system reminders, hook output, task notifications, subagent
  hand-backs) that the script does not split further.
- **Thinking blocks.** Transcripts store them nearly empty and whether prior
  thinking stays in context is uncertain; the 0.1% figure is a lower bound.
- **Behavioral effects.** The restart and phased-loading savings are upper
  bounds that assume post-seam growth is unchanged; quality, extra re-reads, and
  a lead skipping a phase file are not measured.
- **Per-engine rendering and read-narrowing.** Both are estimates with low
  confidence.
- **Sample size.** One 7-day window, 191 worker-lead runs and 22 dispatcher
  sessions; phase medians for spec and plan rest on small samples and on the
  heuristic fallback for 62 of 174 runs.
