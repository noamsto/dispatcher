# Retro notes — Phase 2 + 3: read path and dispatcher synthesis

**Issue:** #21 · **Spec:** `docs/superpowers/specs/2026-08-05-retro-notes-design.md` · **Prior plan:** `docs/superpowers/plans/2026-08-05-retro-notes-phase1-worker-capture.md` (worker capture, merged)

Phase 1 landed `notes[]` in the worker's stopping-path snapshot and the
mid-execute `retro:<crew>` sink. This covers the two phases the spec deferred:
**Phase 2**, a read-only `crew retro` fold over the bus, and **Phase 3**, the
dispatcher's own synthesis at a drained roster. Both are implemented and
merged; this doc records what shipped, including three points where the
implementation departs from the design's letter.

## What phases 2 and 3 cover

- `crew retro` / `crew retro --report [--json]` — a read-only jq fold over
  this repo's `.git/crew/events.jsonl`, grouping worker and dispatcher retro
  notes by tag.
- The dispatcher's own writer: one synthesis pass at a DRAINED roster, before
  the long park re-arms, emitting `misrouted` / `fanout_binder` /
  `spec_too_thin` / one `session_summary` per the spec's anti-slop rules.

## What they explicitly do not cover (non-goals, unchanged from the spec)

- Auto-routing off retro data — that is #86.
- Any derived store, sweep, lock, or daemon. `crew retro` folds the raw bus
  on every invocation.
- Re-instrumenting what the ledger, snapshot, or PR body already record.
- Worker self-assessment of its own quality — every tag is a protocol branch,
  never an opinion.

## Three deviations from the design

The design specified DuckDB and a cross-repo glob. Neither shipped. Each
deviation is deliberate, not a shortcut, and a future reader comparing the
design to `crew.sh` should read the reasoning here rather than conclude the
implementation is wrong.

### 1. jq, not DuckDB

The design's "Read path" section calls for DuckDB SQL, windowed with
`LEAD(ts) OVER (PARTITION BY branch ORDER BY ts)`. The implementation folds
the bus in jq instead. Two reasons, both load-bearing:

- **There is no `duckdb` anywhere in this repo** — not in `flake.nix`, not in
  any `runtimeInputs` list, not vendored. Adding it would be new surface,
  not a rewrite of existing surface.
- **`crew rate` already folds this same bus with jq**, including the same
  dispatch-boundary windowing `retro` needs. Keeping `retro` on jq means the
  two commands that share run attribution share one fold idiom, rather than
  one being DuckDB and the other jq for no reason but which issue proposed
  them.

The design's own "Storage decision (spiked)" section supports this
independently of the consistency argument: it measured reads as not the
constraint (jq folds 5,000 events in 66 ms — "over a year of runs at this
fan-out"). The DuckDB `LEAD(ts) OVER (PARTITION BY branch ORDER BY ts)`
window is expressed in `crew.sh` as the same dispatch-boundary fold `rate`'s
sweep already uses: for branch `$b`, run `i` owns
`[runs[i].ts, runs[i+1].ts)` (`adapters/core/crew.sh:1678-1691`).

### 2. No `duckdb` in `runtimeInputs`, so no flake change — but a rebuild is still required

The design's "Deployment" section says phase 2 needs a `dispatcher`
flake-input bump in nix-config plus `nh home switch`, because it expected a
new binary dependency. Since no new binary was added, `flake.nix` is
untouched.

That does **not** mean phase 2 deploys the way phase 1 did. Phase 1 was
protocol-only — protocols are read by path off the default branch, so it
deployed on merge alone, no rebuild. Phase 2 and 3 change `adapters/core/crew.sh`
and `adapters/core/protocols/DISPATCHER_PROTOCOL.md`. `crew.sh` is a
`writeShellApplication`, built into the `crew` package at evaluation time —
the merged commit is not enough to put the new `retro` subcommand on PATH.
**A `nh home switch` (or equivalent rebuild consuming this flake) is still
required**, exactly as if a new dependency had been added; the only thing
that didn't change is that this particular rebuild needs no flake-input bump
in nix-config first.

### 3. Repo-scoped, not cross-repo

The design's "Read path" section specifies globbing across repos via
`read_json('…/crew/events.jsonl', filename=true)`. `crew retro` reads only
this repo's bus (`$log`, the same file `crew log`/`crew report`/`crew rate`'s
sweep already read) — no glob, no cross-repo rollup.

The cross-repo glob was a DuckDB affordance (`read_json` with a wildcard
path); jq has no equivalent without a registry of other repos' bus paths to
fold over, which does not exist. This is a deliberate scope reduction, not an
oversight: every other `crew` read subcommand is already repo-scoped, so
`retro` matches its siblings rather than being the one exception. The
ratings store (`crew rate`'s output) already carries a `repo` field, so a
cross-repo retro rollup — if wanted later — has a field to key on without
`retro` itself needing to change.

## Smaller resolved judgment calls

- **A note missing its `tag` key surfaces as the unknown tag `"null"`**,
  not silently reclassified as `other`. `tag` is coerced with `tostring`
  before grouping (`adapters/core/crew.sh:1654-1663,1723`), so a malformed
  note is visible in the `unknown` bucket rather than absorbed into a
  catch-all — the same "surface it, don't swallow it" principle the design's
  "Tag validation is reader-side" section states for a mistyped tag.
- **`--report`'s `sample` column truncates to 60 chars with newlines
  flattened** (`adapters/core/crew.sh:1665`); **`--json`'s `details` carry
  the full, untruncated text.** This keeps the human table scannable while
  leaving the `gate_thrash` ledger payload — the design's "highest-value
  payload" — reachable through `--json` even when it runs long.

## Invariants that must not break

- **No new event kind, no new write subcommand.** `retro` is read-only; both
  writers (worker mid-execute, dispatcher at DRAINED) reuse the existing
  `crew msg` synthetic-sink pattern, exactly like `metrics:`.
- **The `retro:`/`metrics:` sinks provably never wake a dispatcher.**
  `watch`/`inbox` filter on `to == dispatcher:<crew>` or `*`
  (`adapters/core/crew.sh`); a `to` of `retro:<crew>` or `metrics:<crew>`
  matches neither. Exercised directly by "the retro sink does not wake the
  dispatcher" (`tests/retro.bats:297`).
- **Snapshot-inherited supersede semantics.** A note learned mid-execute is
  emitted immediately to `retro:<crew>` _and_ carried in the final metrics
  snapshot; a resumed run's later snapshot supersedes the earlier one (`crew
rate`'s latest-snapshot-wins rule), and `retro`'s dedupe on
  `{seam, tag, detail}` means the same note reaching the reader by both
  paths counts once, not twice.
- **Silence on a clean run.** A run that takes no trouble branch, and a
  drained roster with nothing to say, both produce zero notes and zero
  rows — asserted, not assumed.

## Tasks as carried out

1. **`crew retro` case arm** (`adapters/core/crew.sh:1611-1776`) — flag
   parsing (`--report`, `--json`, reject anything else and `--json` without
   `--report`), the dispatch-boundary fold shared with `rate`, worker-row and
   dispatcher-row assembly, tag grouping with vocabulary-then-alphabetical
   ordering, and the three render modes. Extended the `usage:` string to
   `retro [--report [--json]]`.
2. **`tests/retro.bats`** — 15 tests covering fold/windowing, vocabulary
   drift, silence/dispatcher-notes/robustness, and flags (see "Testing"
   below).
3. **Dispatcher synthesis** — the "Retro synthesis" section in
   `adapters/core/protocols/DISPATCHER_PROTOCOL.md` (search `Retro
synthesis`), regenerated into `adapters/claude-code/` and `adapters/codex/`
   via `scripts/gen-adapters.sh`. Defines the DRAINED trigger, the three
   emission rules (`misrouted` only on contradiction, `fanout_binder` /
   `spec_too_thin` on a concrete cause, one `session_summary` naming every
   worker plus any tag on ≥2 workers), and the two anti-slop rules (every
   note quotes a specific observable; a clean drained roster writes nothing).
4. **`tests/adapters.bats`** — one added test, "dispatcher protocol
   synthesizes retro notes at a drained roster", asserting the four
   dispatcher tag names, the exact `crew msg` invocation, and both anti-slop
   sentences verbatim against `DISPATCHER_PROTOCOL.md`.

Note: the worker vocabulary also gained a seventh tag, `review_unavailable`
(`WORKER_PROTOCOL.md:172,192`), from the unrelated fresh-context-reviewer-gate
work that landed between phase 1 and phase 2. It is not part of this feature's
scope but is part of `crew retro`'s live `$vocab` list
(`adapters/core/crew.sh:1673-1675`) and is called out here so its presence
there isn't mistaken for scope creep.

## Output modes — real examples

Generated by seeding a throwaway bus in a scratch directory outside this repo
and running the shipped binary against it; not seeded into this repo's own
bus.

**`crew retro`** — one row per run/dispatcher-episode with notes, tab-separated:

```
branch	engine	model	tier	outcome	tags
feat/widget	claude	sonnet	standard	failed	gate_thrash
dispatcher:c1	—	—	—	—	misrouted,session_summary
```

**`crew retro --report`** — grouped by tag, padded in jq:

```
tag              hits  runs  engines  nondone  sample
gate_thrash         1     1  claude       100  gate=build target=pkg/widget old_plan="use interface X" amen…
misrouted           1     0  —              —  trivial, but the diff touched an auth path -- no review gate…
session_summary     1     0  —              —  feat/widget (standard/claude/sonnet) failed on gate_thrash; …
```

**`crew retro --report --json`** — same grouping, machine-readable, `details`
untruncated:

```json
{
  "tags": [
    {
      "tag": "gate_thrash",
      "known": true,
      "hits": 1,
      "runs": 1,
      "engines": ["claude"],
      "nondone_pct": 100,
      "details": [
        "gate=build target=pkg/widget old_plan=\"use interface X\" amendment=narrowed"
      ]
    },
    {
      "tag": "misrouted",
      "known": true,
      "hits": 1,
      "runs": 0,
      "engines": [],
      "nondone_pct": null,
      "details": [
        "trivial, but the diff touched an auth path -- no review gate ran"
      ]
    }
  ],
  "unknown": []
}
```

`misrouted` and `session_summary` are dispatcher tags: `runs: 0`, no engines,
`nondone_pct: null` — they carry no run attribution, per the spec's
"Dispatcher notes are crew-scoped, not run-scoped" rule.

## Testing

Maps to the design's "Testing" list:

- **Fold + windowing** — "windowing: two dispatches on one branch keep their
  own notes" (`tests/retro.bats:53`).
- **Killed worker** — "killed worker: seam notes survive a run with no
  terminal status" (`:79`): outcome renders `—` (no terminal status) or
  `working`, with notes retained either way.
- **Snapshot supersede** — "snapshot supersede: the later metrics snapshot
  wins and does not duplicate" (`:99`) and "cross-path dedupe: a note in both
  the seam msg and the snapshot counts once" (`:117`).
- **Unknown tag** — "unknown tag: reported with its value, listed in
  --json's unknown, never dropped" (`:155`) and "no unknown tags leaves the
  --report footer off and --json's unknown empty" (`:181`).
- **Silence on a clean run** — "a clean run is silent in all three modes"
  (`:201`).
- **Oversized ledger detail** — "oversized gate_thrash detail: one parseable
  line through the real crew msg" (`:264`), exercising the #20 guard through
  the retro path and asserting the report-truncates/json-keeps-whole split.

Plus: a repeated tag inside one run counts rather than deduping (`:133`);
dispatcher notes get their own row with em-dash columns and no run
attribution (`:222`); an unparseable note body doesn't abort the fold
(`:250`); the sink doesn't wake the dispatcher (`:297`); and the four flag/
error-path tests (`:314,320,330`).

`tests/adapters.bats` adds "dispatcher protocol synthesizes retro notes at a
drained roster" (line 308) for the Phase 3 protocol prose.

Baseline was 413 tests; it is now 429 (16 new: 15 in `tests/retro.bats`, 1 in
`tests/adapters.bats`).

## Verification

- `shellcheck adapters/core/*.sh scripts/*.sh` clean.
- `./scripts/gen-adapters.sh` followed by `git diff --exit-code` — no drift
  between `adapters/core/` and the generated `adapters/claude-code/` /
  `adapters/codex/` trees.
- `bats tests/` green, 429 tests.
- `nix flake check`.

All four are the repo's CI gate (`.github/workflows/ci.yml`).
