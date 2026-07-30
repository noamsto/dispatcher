# `crew watch` — dispatcher-side await-any

**Issue:** noamsto/nix-config#53
**Date:** 2026-06-30
**Status:** approved design

## Problem

The dispatcher→worker monitoring direction has no real wakeup. Workers got a
zero-token blocking primitive (`crew await`, #41), but the dispatcher still
eyeballs `crew roster` at LLM-attention cadence. The worker's finish ping
(`tmux display-message`, WORKER*PROTOCOL.md) is cosmetic: it flashes text in the
dispatcher pane's status line but injects nothing into the dispatcher's Claude
session, so it never \_wakes* the LLM. DISPATCHER_PROTOCOL.md only says to check
the bus "when a worker pings your pane and before dispatching the next task" — an
idle LLM session does not loop on its own, so the effective cadence is "whenever
the dispatcher next happens to take a turn and remembers to poll." That is
human-paced and irregular.

The asymmetry is the bug: workers block efficiently on the dispatcher
(`crew await`); the dispatcher cannot block efficiently on the workers.

## Fix

A symmetric `crew watch` — a held bash long-poll that blocks at zero token cost
until any worker in the crew produces an actionable event, returns the batch,
and lets the dispatcher re-arm. The monitor loop turns from "remember to poll"
into "block until event."

**Success criterion:** worker events surface to the dispatcher within `--interval`
(default ≤2s) of landing, at zero token cost while blocked — replacing the prior
human-paced, irregular cadence. This bound is the observable proof the fix works
and is pinned by the latency case in the throwaway smoke harness (below).

## The command

```
crew watch [--since <ts>] [--states blocked,pr_open,done,failed] [--timeout S] [--interval S]
```

- Defaults: `--since 0`, `--states blocked,pr_open,done,failed`, `--timeout 110`
  (safely under the Bash tool's 120s default), `--interval 2`.
- Resolves `crew_id` the same way every other subcommand does (`CREW_ID` env, else
  `WORKER_TASK.md` — though the dispatcher always has `CREW_ID` in its shell).
- Reuses the bus dir / log resolution already in `crew.sh`
  (`git rev-parse --path-format=absolute --git-common-dir`).

### Flag parsing

Follows the `await` parser (crew.sh): each flag requires a value, unknown flags
exit 1. `--states` is a comma-separated list (e.g. `blocked,done`) split into a
jq array and matched by membership; an empty `--states` exits 1, while an unknown
state token is allowed but simply never matches (states are free-form strings in
the log). `--since` is an integer ms timestamp; a non-integer exits 1.

## What it matches

An event qualifies when `ts > since` AND it is either:

- a `status` whose `body.state` is in the watched set (default
  `blocked`/`pr_open`/`done`/`failed` — **not** `working`, which is heartbeat
  noise), or
- a `msg` addressed to the dispatcher — `to == "dispatcher:" + crew` **or**
  `to == "*"` (broadcast), matching `inbox`'s filter in `crew.sh` so a broadcast
  question can't fall through the wakeup. The dispatcher's own `crew reply` writes
  `to == "worker:<branch>"`, which matches neither — so replies are never echoed
  back to the dispatcher.

So a single `watch` return hands the dispatcher everything actionable — a state
change _or_ a question — in one batch.

## Output and the cursor contract

On a hit, print one JSON object to stdout and exit 0:

```json
{"cursor": 1782803292582, "events": [ {…event…}, {…event…} ]}
```

- `events` = **all** unseen matches with `ts > since` (batched: if three workers
  finished during the dispatcher's think time, it gets all three at once),
  ordered by `ts`.
- `cursor` = max `ts` among the returned events. The dispatcher threads it back as
  `--since` on the next call.

**Cursor invariant — a cursor is emitted only on exit 0.** On timeout, `watch`
prints nothing to stdout, writes a one-line notice to stderr, and exits 3; it
never emits or advances a cursor. The dispatcher therefore re-arms with the
**same** cursor it last held (which stays `0` until the first hit). This is the
one place the `await` mental model misleads: `watch` must never capture
`start=now` on timeout, or events arriving during a post-hit timeout window are
silently dropped — the exact failure the cursor exists to prevent.

### Why an explicit cursor (not `start=now` like `await`)

`await` is one-shot: it waits for exactly one reply then proceeds, so capturing
`start=now` per call is safe. `watch` runs _continuously_; events that land in the
gap between one `watch` returning and the next starting (while the dispatcher is
acting) would be missed under `start=now`. Threading the cursor closes that gap:
the next call picks up anything with `ts > cursor` regardless of when in the
think-window it arrived.

### Known edge (accepted)

The filter is strict `>`, matching `await`. Two events sharing the same
millisecond (`ts` is `now*1000|floor`) that straddle a `watch` return could drop
one. Wall-clock think time is effectively always ≫1ms, so this is the same
negligible risk class `await` already accepts. Documented in a code comment;
no dedup machinery.

## The dispatcher loop (DISPATCHER_PROTOCOL.md change)

This protocol rewrite is the actual behavioral fix.

```
cur=0
loop:
  out = crew watch --since $cur --timeout 110     # run with tool timeout ≥ 120000ms
  exit 0 → cur = out.cursor; act on each event (reply / dispatch next / intervene); re-loop
  exit 3 → re-loop with the same cur
```

While parked in `watch` the dispatcher's turn is busy; the human can still
interrupt (Esc), and there are brief windows between re-arms. For a fan-out that
is monitoring anyway, "block until event" is the right default. `crew roster`
stays available as the at-a-glance dashboard; `watch` is the wakeup.

### Coexistence with the scheduler role

DISPATCHER*PROTOCOL also makes the dispatcher a scheduler: dispatch up to the
fan-out budget, let the roster drain, then dispatch more. `watch` does not fight
this — the events that free budget (`done`/`pr_open`/`failed`) are exactly the
ones in the default watched set, so a terminal worker event wakes `watch` the
instant budget frees; the dispatcher then dispatches the next queued task and
re-arms. Responsiveness to worker events is bounded by `--interval` (≈2s), not by
`--timeout`; the 110s timeout only governs the idle re-arm cadence when nothing
happens. The one case `watch` cannot service mid-block is a \_brand-new* task the
human injects while the dispatcher is parked — the human interrupts (Esc) and
hands it over. `watch` is entered once the dispatcher has dispatched everything
it can within budget and is otherwise idle.

## What does NOT change

- **Worker side:** untouched. Workers already post `crew status
done/pr_open/blocked` — exactly what `watch` keys on. The cosmetic
  `tmux display-message` ping stays as the human-visible nicety.
- **`await` / `reply` / `roster` / `inbox` / `log`:** untouched.

## Testing

A throwaway smoke harness (run from the scratchpad, not committed — same as the
`await` test), mirroring the `await` test, covering:

1. An event with `ts ≤ since` (before the watch's cursor) is ignored → exit 3.
2. Single qualifying event → exit 0, returned in `events`, `cursor` set.
3. Batched multi-worker: several qualifying events during one window → all
   returned in one `events` array.
4. Cursor advance across two re-armed calls: an event landing between calls is
   delivered on the next call (no miss), and no event is delivered twice.
5. `working` status is filtered out (does not wake watch).
6. A worker question (`msg` to `dispatcher:<crew>`) is surfaced; a broadcast
   (`to == "*"`) is too.
7. Cursor survives a timeout: hit → timeout (exit 3, no cursor emitted) → event
   lands during the timeout window → next call delivers it with the pre-timeout
   cursor unchanged.
8. The dispatcher's own `crew reply` (to a worker) does **not** wake `watch`.
9. Wakeup latency: a qualifying event is returned within `--interval` of landing
   (pins the success criterion).

## Deploy

Read-by-path symlink → merge to main checkout → `nh home switch` (the standard
crew.sh deploy gotcha — workers/dispatcher read the CLI from the nix profile).
