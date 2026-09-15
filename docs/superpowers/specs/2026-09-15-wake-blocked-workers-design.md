# Wake blocked workers for a dispatcher reply (#186)

## Problem

A worker blocks, `crew await --timeout 300` expires, the worker re-stamps
`blocked` and **stops** (its engine turn ends). `crew reply` only appends to
`events.jsonl`. An idle claude/codex/cursor/pi session reads nothing until text is
submitted into its pane, so every decision that takes longer than 300s is
undeliverable. The protocols promise "you resume on next activation" and
"resumes in place", and nothing performs that activation. The dispatcher's only
workaround is raw `tmux send-keys`. That text sits unsubmitted in a busy pane and
collides with whatever a human has already typed. In the incident (crew
`1789295062-1512555`, worker rust), the pane held an unsent human line.

## Invariants (from the task)

- **W1:** a `crew reply` to a worker whose latest status is `blocked` reaches it
  and resumes it, whether it is still in `crew await` or its turn has ended.
- **W2 (safety):** never submit into a busy pane, or into a pane whose input box
  holds unsent text. Detect it and refuse (or wait) with a clear result.
- **W3:** the pane carries only a fixed wake prompt. The directive stays on the bus.
- **W4:** submission is verified by capturing the pane after sending.

## Decision

Candidates from the issue:

| option                            | verdict                                                                                                                                                                                                                                                                                       |
| --------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1. `crew nudge` + reply-side wake | **chosen**                                                                                                                                                                                                                                                                                    |
| 2. longer/held await              | rejected: still capped, still undeliverable past the cap, and burns a parked session                                                                                                                                                                                                          |
| 3. stall-watch performs the wake  | rejected: the watchdog is nohup-detached and by design never acts on a pane (liveness spec: "send-keys … is the one action that could destroy work"). A refusal would have no caller to report to, and making it one would mean posting `msg`s from the watchdog, another contract it forbids |
| 4. protocol wording               | done alongside 1                                                                                                                                                                                                                                                                              |

Chosen: a new **`crew nudge <worker>`** primitive, and **`crew reply` invokes it
automatically** when the resolved target is a worker session whose latest status
is `blocked`. The wake is synchronous, so the dispatcher gets the outcome as an
exit code plus one stderr line, in the same call that sent the reply. W1 then
holds for plain `crew reply`, with no flag for the dispatcher to remember.
`--no-wake` keeps append-only behaviour.

### `crew nudge <worker:branch[#sid]> [--since TS] [--timeout S] [--interval S]`

`--timeout` is the **whole-call deadline** (default 60s), not a per-phase wait. A
claude dispatcher's Bash tool kills a foreground call at 120s by default. codex
parks in the foreground by design (`DISPATCHER_PROTOCOL.md` codex park). The
call must return well inside that.

1. **Resolve the session.** Branch-only ids go through the `reply` resolver
   (`_resolve_worker`, extracted verbatim from `reply`): the newest session, and
   a terminal session is refused. For an explicit sid, nudge looks up that
   session's row in `_sessions` and refuses when the row is absent or terminal.
   `reply` does **not** gain that refusal: it keeps delivering an explicit sid
   verbatim (pinned by `tests/crew.bats` "an explicit session id is honoured
   verbatim"), and it only **skips the wake** when the sid's latest status is not
   `blocked`, which includes the case where the sid is unknown.
2. **Resolve the engine.** Read the `engine` field of that session's newest
   `dispatch`/`resume` row. If it is absent, the engine is `unknown`.
3. **Resolve the pane.** Find the worktree path for the branch
   (`git worktree list --porcelain`). Take the `@crew_name` worker window rooted
   there (the `_occupants` keying). Inside it, keep the panes whose command passes
   `_is_engine_cmd` **and** carry no pane-level `@crew_state`, which role-grid panes
   do carry (`decorate_pane`, `dispatch.sh:72`). Read that option pane-only, with
   `tmux show-options -pqv -t <pane> @crew_state`. A `#{@crew_state}` format
   string would fall back to a window or session value. Exactly one pane must
   remain. Zero is refused ("no engine pane") and more than one is refused
   ("ambiguous").
4. **Engine gate (refuse-when-unknown).**
   - `claude`: idle, busy and unsent-input frames were captured live on
     2026-09-15 and are pinned as fixtures. Supported.
   - `pi`: idle (two adjacent `────` rules) and busy (`── ⠦ Working ──`) frames
     were observed, but no unsent-input frame. Unsupported: **refuse**.
   - `codex`, `cursor`, `unknown`: no captured frames. **Refuse.**

   A refusal names the engine and tells the dispatcher to capture the pane and
   use the manual last-resort path.

5. **Classify each sample** with the stall-watch classifiers, hoisted to top
   level so both callers share one copy (`re_meter`, `re_subrow`, `re_option`,
   `_is_prompt`, `_meter_line`, `_has_subrow`), plus one new helper,
   `_input_box` (claude box extractor):
   - `prompt`: `_is_prompt`. **Refuse immediately**, never typing into an
     option-select.
   - `busy`: a meter line or a live subagent row.
   - `vimmode`: a `-- NORMAL --` (or any non-INSERT vim mode) marker, because typed
     keys there would run as editor commands. **Refuse immediately.**
   - `unsent`: the box content is non-empty after the stripping below.
     **Refuse immediately.** It is someone's text and does not clear on its own
     schedule.
   - `idle`: a box was found, it is empty, and none of the above apply.
   - `unknown`: no box could be located. **Refuse.**

   The box is the content between the **last two rule lines** (`(─)+`, optionally
   carrying a ` name ─` label), and its first line starts with `❯`.

   **Ghost text (measured 2026-09-15).** Claude Code paints a _suggested_ next
   prompt inside an idle box as dim text (SGR 2). Pane `%336` captured with `-e`
   showed `❯ \e[2mgo ahead\e[0m`, while a plain capture shows `❯ go ahead`,
   which cannot be told apart from typed text. So nudge samples with
   `capture-pane -e -p`. The box extractor deletes every SGR-2 span (up to the next
   reset) **before** judging emptiness, then strips the remaining escape sequences,
   `❯`, NBSP and whitespace. The detectors that run on escape-free text
   (`_is_prompt`, `_meter_line`, `_has_subrow`) receive the escape-stripped
   capture. Typed text rendered at normal intensity is an assumption, since nothing
   here captured it. Execute verifies it once in a throwaway claude session the
   worker owns, and pins that frame as a fixture. If typed text also renders dim,
   the design stops and escalates instead of guessing.

6. **Consumption check** on every tick, before classifying: if the session posted
   a status with `ts > since` (default: nudge start; `reply` passes its own
   line's ts) whose state is not `blocked`, the reply was consumed in-band by
   `crew await`. Exit 0 with `consumed`. A newer terminal state instead fails with
   "worker stopped before reading; re-dispatch".
7. **Serialize.** Before step 6's first tick, take a per-session lock,
   `$dir/wake/<sid>` via `_lock_acquire <dir> $$`, released on every exit path by
   an `EXIT` trap. A caller that finds the lock held by a live pid exits 0 with
   `in-progress`: the bus already holds its reply, and the running wake makes the
   worker read its whole inbox. This covers two replies in quick succession and a
   `crew nudge` retry that races the wake `reply` started.
8. **Wait.** While the pane is `busy`, poll every `--interval` (default 2s)
   until the deadline. A blocked worker still inside `crew await` paints a meter,
   so this loop is what lets the in-band path win without typing. `idle` must be
   seen on **two consecutive samples** before sending. **Typing never starts
   unless at least 20s of the deadline remain** (5s type-verify plus 15s
   submit-verify), otherwise refuse `busy`, which is the same outcome as a
   deadline hit while busy. `INT`/`TERM` exit through the trap. If one lands
   between typing and Enter, stderr names the pane that may hold the unsent wake
   text.
9. **Submit and verify (W3/W4):**
   1. Type the fixed prompt `crew wake: read your crew inbox and continue` with
      `send-keys -l`.
   2. Poll up to 5s until the box content, whitespace-stripped, **equals** the
      whitespace-stripped wake text. Any other content means someone typed
      concurrently: do **not** press Enter, and fail `unverified` naming the pane
      so a human can clear it.
   3. Send `Enter`, then poll up to 15s for: an empty box **and** (a meter present
      **or** the wake text visible above the box).
   4. If the box still holds exactly the wake text, send one more `Enter` and
      re-verify. Otherwise fail `unverified`.
10. **Result contract** (stdout: nothing; stderr: one `crew: nudge: …` line). Every
    refusal leaves the pane untouched (W2). The split between a transient and a
    permanent refusal is what tells the dispatcher whether a retry can help.

    | exit | outcome                                                                                                          | meaning                                          | stderr hint                                                  |
    | ---- | ---------------------------------------------------------------------------------------------------------------- | ------------------------------------------------ | ------------------------------------------------------------ |
    | 0    | `delivered`                                                                                                      | wake prompt submitted and verified               | —                                                            |
    | 0    | `consumed`                                                                                                       | the worker read the reply in-band; nothing typed | —                                                            |
    | 0    | `in-progress`                                                                                                    | another wake for this session holds the lock     | —                                                            |
    | 1    | usage/resolution                                                                                                 | bad args, or no/terminal session                 | re-dispatch or fix the address                               |
    | 3    | `refused (transient): busy\|unsent\|vimmode\|lock`                                                               | the condition clears on its own or by a human    | `retry: crew nudge <id>` after capturing the pane            |
    | 5    | `refused (permanent): engine <e>\|prompt\|quota\|unknown-frame\|no engine pane\|ambiguous panes\|worker stopped` | no automated wake can succeed now                | do not retry; capture the pane and act by hand (last resort) |
    | 4    | `unverified: <reason>`                                                                                           | keys were sent but acceptance was not verified   | capture the pane before anything else                        |

    `prompt` is permanent for nudge's purposes. A pane parked on an option-select
    is the watchdog's `prompt:` recovery path (answer it by hand), not something a
    retry fixes.

### `crew reply` changes

- Append exactly as today; it is durable first.
- Then, only for a `worker:` target whose resolved session's latest status is
  `blocked`, run the nudge with `--since <reply ts>`. Any other state, and any
  non-worker target, returns as today.
- On a non-zero wake result, `reply` exits with the same code. stderr always says
  `the reply is on the bus — do not resend`, so a retrying dispatcher never
  duplicates the directive, and then adds the nudge's own hint: retry only on 3,
  never on 5.
- A pi/codex/cursor worker therefore gets exit 5 `refused (permanent): engine`
  from every reply to a blocked session. That is intentional: a wake that cannot
  happen must not look like one that did. `DISPATCHER_PROTOCOL.md` says so
  explicitly: for those engines, answer inside the ~300s await window, or
  capture the pane and use the verified manual last resort.
- Flags come after the positionals: `--no-wake` and `--wake-timeout S` (default
  60, passed as nudge's `--timeout`).

### Worker contract

`WORKER_PROTOCOL.md` "Report to the bus", timeout bullet: still stop. The resume
mechanism is now concrete. When the dispatcher answers, `crew reply` submits
`crew wake: read your crew inbox and continue` into the worker's pane. On that
prompt the worker re-stamps `working`, runs the straggler fold
(`crew inbox "$CREW_WORKER_ID" --since <seen>`), handles the reply, and resumes
from the paused step. The block→await cap is unchanged. The "resumes you in place"
sentence is corrected the same way.

### Dispatcher contract

`DISPATCHER_PROTOCOL.md`: `crew reply` wakes a blocked worker in both cases,
in-band during the await and by a verified wake prompt after the await ends.
Document the exit codes and what to do on each non-zero one. On 3, capture the
pane and retry `crew nudge`. On 5, do not retry; act by hand, or answer within the
await window for non-claude engines. On 4, capture the pane before anything else.
Never re-send the reply. Raw `tmux send-keys` is demoted to a last resort, and
it requires a capture before and after.

## Consumer map

| edge                       | symbol / file                                                                         | disposition                                                                         |
| -------------------------- | ------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| reply producer             | dispatcher per `DISPATCHER_PROTOCOL.md` L520; hint text `dispatch.sh:1213`            | changed: exit codes/wake documented; hint text still valid                          |
| reply resolution           | `crew.sh` `reply)` + `_sessions`                                                      | changed only by extraction into `_resolve_worker`; existing `reply:` tests guard it |
| reply msg readers          | `crew await`, `crew inbox`; `watch`/`stream` filter `to==dispatcher` (verify in plan) | compatible: the line is unchanged                                                   |
| role targets               | `dispatch.sh --role-watch` injects `to==role:` msgs                                   | compatible: no wake for non-`worker:` targets                                       |
| await                      | `crew.sh` `await)`                                                                    | unchanged                                                                           |
| stall-watch sampling       | classifiers inside `stall-watch)`                                                     | changed: hoisted verbatim; all stall-watch tests are the guard                      |
| stall-watch suppression    | self-`blocked` suppresses D0–D3                                                       | compatible: a verified wake makes the worker post `working`                         |
| dispatch resume            | new session and `resume` row on the same pane                                         | compatible: newest session resolves; the pane is found by window/path               |
| send-keys: engine launch   | `dispatch.sh` launch lines, role panes                                                | unaffected (launch, not wake)                                                       |
| send-keys: role assignment | `dispatch.sh:177` (unverified)                                                        | out of scope, noted                                                                 |
| manual injection           | `DISPATCHER_PROTOCOL.md`                                                              | demoted to a last resort with verify                                                |
| bus schema                 | no new event kind                                                                     | absent                                                                              |

## Tests (separate block in `tests/crew.bats`)

A stateful stub `tmux` on PATH. `capture-pane` prints the current frame.
`send-keys -l` moves to a typed frame. `send-keys Enter` moves to a busy frame and
appends the worker's `working` status. The frames are pinned from today's live
captures.

- **Red on main:** a reply to a blocked session whose await has ended, on an idle
  pane, leads to a newer `working` row (the simulated worker consumed it). Current
  `main` only appends, so there is no send-keys and no `working`, and the test
  fails.
- refusal on a busy pane: deadline → exit 3, no send-keys.
- refusal on unsent input → exit 3, immediate, no send-keys.
- refusal on NORMAL vim mode → exit 3; refusal on a prompt frame → exit 5.
- refusal on a non-claude engine → exit 5, and `reply` still appended.
- a dim ghost suggestion in the box reads as idle and gets a verified submit.
- the lock is held by a live pid → exit 0 `in-progress`, no send-keys.
- typing is not started when less than 20s of the deadline remains.
- an explicit unknown sid: `reply` delivers verbatim and skips the wake; `nudge`
  refuses (exit 1).
- verified submit → exit 0, send-keys `-l <wake>` then `Enter`, `delivered`.
- unverified: the box never clears → second Enter, then exit 4.
- in-band: the pane is busy and the worker posts `working` mid-wait → exit 0
  `consumed`, no send-keys.
- reply to a `working` session → no wake, and no tmux capture at all.
- `--no-wake` → append only.

## Round-2 amendments and live probe (2026-09-15)

The spec-critic accepted round 2 with four notes. A throwaway claude session on
a private tmux server then settled the typed-text assumption. Where these
amendments differ from the sections above, the amendments win. Details are in
the plan.

- The lock's parent dir is created first. `in-progress` requires a readable,
  live pid; otherwise the acquire is retried, then refused as transient.
- An `unknown` frame is polled to the deadline, and only a persistent one exits 5.
- A dim span ends at `\e[0m`, `\e[22m` or `\e[m`. An unterminated dim span is
  `unsent`.
- The typing floor is 35s, which covers the Enter retry.
- **Measured:** typed text renders at normal intensity, so the ghost-text rule
  holds. The early-turn spinner forms (`· Smooshing…`,
  `· Determining… (2s · thinking)`) do not match `re_meter`, so the wake
  classifier adds `re_spinner`. The current trust prompt is un-numbered, so a
  last line of `Enter to confirm` / `Enter to select` alone classifies as
  `prompt`. Submission is verified by the transcript line `❯ <wake prompt>`
  together with an empty box.
- Inside `reply`, a pane or engine resolution failure is exit 5, never 1. Exit 1
  keeps meaning "nothing was written".

## Out of scope

- pi/codex/cursor wake support (needs captured unsent-input frames).
- Verifying the role-watch assignment injection.
- stall-watch auto-wake.
