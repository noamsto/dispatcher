# Dispatcher handoff: a fresh session per batch (#701)

## Problem

A dispatcher session's context only grows: median 107k at turn 1, 212k at turn
100, 307k at turn 200, peak 704k; 85.8% of turns run above 150k. Every turn
re-reads all of it. Durable crew state (roster, statuses, messages, holds,
cursors) already lives on the bus, so a fresh session could rebuild most of
it. What the bus does not hold has to travel in a handoff note:

- the human's in-chat instructions and standing decisions;
- pending approvals and questions to the human;
- the queue of not-yet-dispatched tasks, its order and dependencies, and the
  spec files in flight;
- triage reasoning for live workers (why this tier/engine/model);
- open stacked layers, `pr-watch` arms, pending follow-up batches.

Live failure mode to design against: during a context reset the `crew stream`
lane died unnoticed and a worker's in-band question went unanswered until its
~2h budget ran out. Root cause: the old session's lane already consumed the
wake (the per-crew watch cursor moved past it), so nothing wakes the new
session for it. That holds for every wake state, not only `blocked`: a
`pr_open`, `done`, `failed` or `exited` delivered to the old session just
before it stopped is gone too. A fresh session must (1) re-arm its lane and
(2) reconcile the whole roster on startup, `blocked` first (it has a ~2h
budget), without waiting for a wake.

Scope: claude and pi dispatchers. codex and cursor must not break.

## Design 1: launcher-owned handoff (`dispatcher --fresh`)

- **Where the note lives:** a file, `<git-common-dir>/crew/handoff/<crew_id>.md`
  (dir mode 0700, file 0600), one per crew, overwritten by each handoff. The
  launcher owns the location; the dispatcher never computes it.
- **Who writes it:** the outgoing dispatcher writes the note to any file it can
  write, then runs `dispatcher --fresh --note <file>` from its own shell tool.
- **How the restart happens:** the launcher that started the session exports
  `DISPATCHER_LAUNCHER_PID=$$` and `DISPATCHER_HANDOFF_DIR` into the engine's
  environment. The inner `dispatcher --fresh` call verifies that pid is its own
  ancestor (so it runs inside that launcher's session; a promoted session, a
  worker or any other process fails the check), copies the note into place
  atomically, writes a restart marker holding the launcher pid, and sends
  SIGTERM to the launcher's direct child on its own ancestor chain: the engine
  (the launch execs it; probed: claude exits 143 and pi exits 0 on SIGTERM). It
  then waits up to 10 s; an engine still alive means the handoff failed: the
  inner call deletes the marker and tells the session so (exit 1). The
  inner call branches out right after argument parsing: it never runs the
  re-attach pass, the tmux stamps, `crew new` or `crew register`, so the crew
  stays registered to the outer launcher's pid. The outer launcher now runs the
  engine in a loop: when the engine exits and a marker naming this launcher
  was written within the last 120 s, it consumes the marker and relaunches the
  same engine, model, effort and flags with a handoff first prompt; the
  original task words are not repeated (the first session handled them). A
  marker that is older, or names another pid, is deleted and the launcher
  exits as today (an engine that ignored SIGTERM and was quit by hand later
  must not relaunch). Same launcher pid, so the crew registration, tmux window
  stamps and pane stay put.
- **Engines:** `--fresh` is claude and pi only. codex and cursor carry the
  protocol itself as their first prompt, so a relaunch would need to rebuild
  it; they get no new behaviour: `--fresh --agent codex|cursor` is refused and
  their launch path is unchanged.
- **How the fresh session loads it:** the first prompt names the note path and
  its age and orders the startup steps: re-arm the lane, answer `blocked`
  workers, read the note, reconcile the other rows against it, resume. The
  launcher computes the roster
  summary itself from `crew roster <crew> --live`: each `blocked` worker by
  codename + branch (charset-filtered) and a count per other wake state. It
  never puts a worker-written detail or question into the prompt, where it
  would land in a user turn. The terminal print, which only the human reads,
  also carries each blocked worker's latest question to the dispatcher (from
  `crew inbox dispatcher:<crew>`, which marks nothing delivered for a
  dispatcher reader), control characters stripped and truncated.
- **Manual path:** with no live launcher ancestor (old session gone, promoted
  session, crash), the human runs `CREW_ID=<id> dispatcher --fresh` (optionally
  `--note <file>`); it launches in place with the same handoff prompt. It
  refuses without a stored note or crew, and when the crew's dispatcher is live
  in another pane. After a crash the stored note may be an old handoff's: the
  prompt and terminal print give its age, and the protocol has the session
  confirm the queue and standing instructions with the human when the note is
  older than a day.
- **Trust:** the note is read with the file tool, i.e. as tool output: data, not
  instructions, never authorization (named approvals and `--owner-auth` text are
  re-collected from the human). The bus wins over the note on worker state.
- **Cost:** ~150 lines of launcher bash, bats tests, one protocol section.

## Design 2: protocol-owned handoff (bus row + startup step)

- **Where:** a synthetic-sink bus row, `crew msg dispatcher:<c> handoff:<c>
'<json>'`, read back by a new `crew handoff` Go subcommand (latest row wins).
- **Who writes:** the outgoing dispatcher, same triggers.
- **Restart:** none automated; the protocol tells the dispatcher to ask the
  human to quit and relaunch (`dispatcher`), or relies on the next crash.
- **Load:** a new first startup step in the protocol for every session
  (launcher or promoted): `crew handoff`, `crew stream --status`, `crew roster`.
- **Cost:** a Go subcommand + help/drift entries + protocol text.

## Choice: Design 1

|                   | Design 1 (launcher)                                                                                     | Design 2 (bus row)                                                                                |
| ----------------- | ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| Note size         | a file, no limit beyond a 64 KiB sanity cap                                                             | bus lines clip near 2 KB; a real note (instructions, queue, specs) does not fit                   |
| Restart           | automatic, unattended                                                                                   | needs the human at the pane; unattended dispatchers (the common case) never restart, so no saving |
| Startup checks    | deterministic: the launcher computes the blocked list and puts the lane instruction in the first prompt | relies on the model reading a startup step from a long system prompt                              |
| Blocked surfacing | launcher-computed list (questions on the terminal), plus the in-session roster read                     | in-session only                                                                                   |
| Engines           | claude/pi; codex/cursor refused, launch unchanged                                                       | engine-agnostic                                                                                   |
| New Go surface    | none                                                                                                    | a subcommand, help text, drift tests                                                              |

Design 1 wins on the two things that decide the saving and the failure mode:
it restarts without a human, and its startup surfacing is computed by code, not
remembered by a model. Design 2's bus row cannot hold the note.

## When to hand off (protocol text, claude and pi)

Triggers, both requiring a quiet point (no batch half-handled, no dispatch
half-done, the human not awaiting your answer):

1. **Batch boundary:** a batch leaves the roster DRAINED, after retro
   synthesis, provided this session has dispatched or handled a batch since it
   started (never hand off a session that has done nothing).
2. **Context pressure:** the session has auto-compacted, or the engine warns of
   context pressure; hand off at the next quiet point even with live workers.

A literal turn counter is rejected: no engine shows the model its turn count,
and compaction (300k window) is the observable proxy. 9 of 22 sessions never
reach turn 100, and the batch boundary catches them.

## Note contents (protocol text)

Required headings: standing instructions (the human's words, quoted where the
wording matters), pending human questions/approvals, queue (order,
dependencies, spec file paths, held tasks' refs), per-worker disposition (what
you already did with each live or finished worker, so the next session can
reconcile the roster against it) and triage reasoning, open stacked layers and
`pr-watch` arms, anything else in flight. Never:
secrets, anything the bus already holds, private data beyond what the human
typed for the task.

## Startup steps (protocol text and first prompt)

1. Re-arm the lane now: claude `crew stream --status`, `--force` when `alive` or
   `stale` (the previous session armed it), arm when `dead`; pi parks as usual.
2. `crew roster` and `crew inbox dispatcher:<c>`: act on every `blocked` worker
   now (reply to its question, or verify a watchdog block); its wake was spent.
3. Read the note.
4. Reconcile each `pr_open`, `done`, `failed` and `exited` row the note does
   not record as handled, as if its wake had just arrived.
5. Tell the human one line; resume the queue.

## Acceptance mapping

- AC1: this file (committed under `docs/superpowers/specs/`).
- AC2: bats end-to-end: a stub engine writes a note and runs `dispatcher
--fresh --note`; the launcher stores the note, relaunches once, and the second
  launch's first prompt carries the note path, the blocked worker and the lane
  instruction, while the terminal print carries the blocked worker's question;
  plus rows for a stale marker, the manual path and the refusals (no note,
  codex/cursor, `--new`, a non-ancestor launcher pid).
- AC3: `DISPATCHER_PROTOCOL.md` section "Hand off to a fresh session".
- AC4: CI.
