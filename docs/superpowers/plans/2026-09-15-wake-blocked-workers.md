# Plan: wake blocked workers for a dispatcher reply (#186)

Spec: `docs/superpowers/specs/2026-09-15-wake-blocked-workers-design.md`, which
the spec-critic accepted in round 2. Its round-2 notes are folded in below as
**R2-1…R2-4**. **P-1…P-3** come from a live probe on 2026-09-15, run in a
throwaway `claude --model haiku` session on a private tmux server (`-L probe186`).

## Evidence carried into the plan

- **P-1:** a typed, unsent box renders at normal intensity:
  `\e[39m❯ crew wake: read your crew inbox and continue`. Only the suggestion
  ghost text is SGR 2 (`%336`). An empty box is `\e[39m❯ ` with no span.
- **P-2:** submitted, +1s. The box is empty, the transcript line above the box
  reads `❯ crew wake: read your crew inbox and continue`, and the spinner shows
  `· Smooshing…`. At +2s it shows `· Determining… (2s · thinking)`, and at +4s
  `✢ Determining… (7s · ↓ 425 tokens · thought for 4s)`. **The early spinner
  forms do not match `re_meter`.** The wake classifier therefore needs a broader
  `re_spinner`: a non-alnum glyph, a space, a capitalised word, then `…`, on a
  line within the frame above the box's top rule. `re_meter` stays as it is for
  stall-watch.
- **P-3:** this build's trust prompt is **un-numbered** (`❯ No, exit` /
  `  Yes, I trust this folder` / `Enter to confirm · Esc to cancel`), and the
  cursor defaults to "No". `_is_prompt` requires a numbered option, so it would
  not flag it. The wake classifier treats **any** last non-empty line containing
  `Enter to confirm` or `Enter to select` as `prompt`, whether or not it has an
  option line.
- **R2-1:** `mkdir -p "$dir/wake"` runs before `_lock_acquire`. A failed acquire
  is `in-progress` only when `$ld/pid` is readable, non-empty and `kill -0`-live.
  Otherwise retry the acquire up to 2 times (0.2s apart), then refuse transient
  (exit 3, `lock`).
- **R2-2:** `unknown` (no box found) is polled like `busy` and becomes exit 5
  `unknown-frame` only when it persists to the deadline. A mix of busy and
  unknown samples ends as `busy` (3).
- **R2-3:** a dim span ends at `\e[0m`, `\e[22m` or `\e[m`. An **unterminated**
  SGR-2 span inside the box classifies as `unsent` (fail-safe).
- **R2-4:** the typing floor is **35s**: 5s type-verify, 15s submit-verify, 15s
  for one Enter retry.
- **Consumer impact found while planning:** `tests/crew.bats` "a session-less
  watchdog post … (#173)" (L993) and "a session-less row after a resume row"
  (L1044) seed a `blocked` latest state and call `run_crew reply` without `run`.
  With auto-wake, those calls now exit 5 (engine unknown, no pane). Those two
  calls get `--no-wake`, because they pin resolution, not waking. Inside `reply`,
  every wake failure is **≥3**: exit 1 keeps meaning "nothing was written", so a
  pane or engine problem there is exit 5, never 1.

## Decomposition conformance

`DECOMPOSITION.md` (worktree root) is a hard constraint. This plan maps onto it
as follows:

| component               | steps    |
| ----------------------- | -------- |
| hoist-stall-classifiers | S1       |
| extract-resolve-worker  | S2       |
| claude-wake-classifier  | S3       |
| crew-nudge              | S4       |
| reply-wake-integration  | S5       |
| wake-bats-block         | S6       |
| protocol-text           | S7       |
| red-on-main-evidence    | S8       |
| live-verification       | S9       |
| gates-and-regen         | S10, S11 |

It adopts the decomposition's names (`_input_box`, `_wake_class`, `_wake_pane`),
its interface literals, and its consumed rule verbatim.

Two ordering deviations are deliberate:

- The decomposition marks `extract-resolve-worker ∥ hoist-stall-classifiers` as
  parallel. Here they run serially (S1 → S2), because both edit `crew.sh` and
  concurrent subagent edits to one file collide.
- Trust-prompt footer: the decomposition says the last line is _exactly_
  `Enter to confirm`. The measured footer is
  `Enter to confirm · Esc to cancel` (P-3), so the match is _contains_.
- Boundary: S7 also edits `WORKER_PROTOCOL.md` L326, just outside
  protocol-text's L308–319. L326 repeats the same stale claim ("resumes you in
  place") in the same "Report to the bus" section, and neither #179 section is
  involved.
- Boundary: S5 edits the `await)` **comment** (`crew.sh` L641–643, comment-only
  stale "next activation" text). The acceptance `rg` requires it. No `await)`
  code changes.
- Boundary: S4 adds `nudge` to the `crew.sh` L2 header comment, which mirrors the
  L3799 usage line that crew-nudge may touch.
- Ordering: S8 runs after S5/S6 against an `origin/main` worktree rather than
  before reply-wake-integration. That satisfies the edge's intent: the red run is
  of main's code with the new test.
- Interface: `_wake_class` adds a seventh class word, `quota`, for the
  session-limit frame (normal box, `_is_quota_session_limit`). It maps to exit 5
  `quota`, and folding it into `prompt` would lose that distinction.

## Interfaces (stable across steps)

```
crew nudge <worker:branch[#sid]> [--since TS] [--timeout S] [--interval S]
  exit 0 delivered|consumed|in-progress · 1 usage/session · 3 transient · 4 unverified · 5 permanent
crew reply <to> <body> [--no-wake] [--wake-timeout S]
  exit 1 only when nothing was appended; otherwise 0, or the wake's 3/4/5
WAKE_PROMPT='crew wake: read your crew inbox and continue'
```

Top-level shell functions in `crew.sh`. They are hoisted verbatim unless marked
new:

- `re_option`, `re_meter`, `re_subrow`, `_is_prompt`, `_meter_line` and
  `_has_subrow` are hoisted.
- `re_spinner`, `_input_box`, `_wake_class` and `_wake_pane` are new:
  - `_input_box <escaped-capture>` prints the box text (dim spans removed, then
    escapes, `❯`, NBSP and all whitespace stripped; empty for an empty box) and
    returns 0. It returns 1 when no box is found, and 2 for an unterminated dim
    span.
  - `_wake_class <escaped-capture>` prints **exactly one word**: `prompt`,
    `quota`, `busy`, `vimmode`, `unsent`, `idle` or `unknown`. It calls
    `_input_box`. Nudge calls `_input_box` itself when it needs the text (type
    verification).
  - `_wake_pane <branch>` prints one pane id and returns 0. On failure it returns
    1 with stderr `no engine pane` or `ambiguous panes`.
- `_resolve_worker <to> <crew>` is extracted verbatim from `reply`. It prints the
  resolved `to` and returns 1 with the existing stderr messages.
- `_nudge <worker-id> <crew> <since> <timeout> <interval>` is new and returns the
  exit code.

## Steps

Order: S1 → S2 → S3 → S4 → S5, serially, because all five edit `crew.sh`. S6
(tests) runs **in parallel with S3–S5** once S2 lands (per the decomposition,
the fixtures are the classifier's spec). S6 is the only step that edits
`tests/crew.bats` (PC-4), and it writes against the Interfaces block. S7
(protocols) runs in parallel with S3–S6 and copies its literals from the
Interfaces block, never from memory. S8 (red evidence) needs S6 plus a `main` checkout. S9 (live run), S10
(gates) and S11 (gen-adapters) come last.

### S1 — hoist stall-watch classifiers (mechanical) · implement: sonnet

- `adapters/core/crew.sh`: move `re_option`, `re_meter`, `re_subrow`,
  `_is_prompt`, `_is_quota_prompt`, `_is_quota_session_limit`, `_meter_line` and
  `_has_subrow` (with their comments) from inside `stall-watch)` to top level
  after `_lock_release`. The stall-watch call sites stay byte-identical. Keep the bodies byte-identical.
  `_is_prompt` reads `$re_option` from the global scope.
- Validate: `bats -f 'stall-watch' tests/crew.bats`, all green, and
  `shellcheck adapters/core/crew.sh` clean.

### S2 — extract `_resolve_worker` · implement: sonnet

- `crew.sh` `reply)`: replace the inline `case "$to" in worker:*)` block with
  `to=$(_resolve_worker "${1:-}" "$crew") || exit 1`. The function keeps the
  explicit-sid-verbatim branch and every stderr message unchanged.
- Validate: `bats -f '^reply:' tests/crew.bats` green.

### S3 — `re_spinner` + `_wake_class` · implement: opus (types-into-panes safety core)

- New top-level code in `crew.sh`, placed after the hoisted classifiers.
- `_wake_class` works on the `capture-pane -e -p` text:
  1. `plain` = the capture with all CSI escapes (`\e\[[0-9;]*[A-Za-z]`) removed.
  2. **prompt** if the last non-empty line of `plain` contains `Enter to confirm`
     or `Enter to select` (P-3), if `_is_prompt "$plain"` succeeds, or if
     `_is_quota_session_limit "$plain"` succeeds, in which case the class is
     **quota**, not prompt. That frame has a normal box, and nudge maps it to
     exit 5 `quota`. The box itself is extracted by
     `_input_box <escaped-capture>`, defined in this step per the Interfaces
     block (steps 3 and 6 below are its body).
  3. Locate the box on `plain`: the last two lines matching
     `^(─)+( [^─]+ (─)+)?[[:space:]]*$`, where the first line between them starts
     with `❯`. If none is found, the result is **unknown**.
  4. **vimmode** if any line below the box matches `-- [A-Z]+ --` other than
     `-- INSERT --`.
  5. **busy**, with each signal scanned only inside a bounded window so a
     meter-shaped line in old transcript cannot pin `busy` (PC-6). Live
     subagent rows paint in **both** places: under the meter, above the box
     (foreground batch, pinned `fx_subbatch`, pane `%129`), and under the status
     bar, below the box (background agents, pane `%249`) (PC2-1).
     - `below` = every line after the bottom rule.
     - `above` = the last 6 non-empty lines above the top rule, **counted after
       skipping** lines that match `re_subrow`, lines starting with `⎿`, and
       todo-list rows (`^[[:space:]]*[☐☒✔◻◼]`). A parallel batch or a todo list
       therefore cannot push the meter out of the window.
     - Busy if `_meter_line "$above"` is non-empty, or a line in `above` matches
       `re_spinner` (`^[^[:alnum:][:space:]]+[[:space:]]+[A-Z][a-z]+…`,
       multibyte-safe via grouping, per the stall-watch note).
     - Busy if `_has_subrow` matches **any** line in the skipped block
       immediately above the top rule, or in `below`.
  6. On the **escaped** box lines: delete every SGR-2 span (`\e\[2m` through the
     next `\e\[0m`, `\e\[22m` or `\e\[m`). A `\e[2m` with no terminator is
     **unsent** (R2-3). Then strip the remaining escapes, `❯`, NBSP and
     whitespace. Empty → **idle**, otherwise **unsent**, plus the text.
- Precedence: prompt > unknown > vimmode > busy > unsent > idle.
- Validate: unit tests in S6 ("wake-frame:") over pinned fixtures.

### S4 — `crew nudge` · implement: opus

- New `_nudge` function plus a `nudge)` case in the subcommand switch. Add usage
  to the header comment (L2) and the final usage line.
- Flow:
  1. `_is_session_id` or `_resolve_worker` gives the id.
  2. Find the `_sessions` row for the id, then refuse when it is absent or
     terminal (exit 1).
  3. The engine is the `.engine` of the newest `dispatch`/`resume` row whose
     `.session` matches. A non-`claude` engine is exit 5 `engine <e>`.
  4. `_wake_pane <branch>` (a new top-level function, per the Interfaces
     block), with nudge mapping its return 1 to exit 5. The worktree comes from
     `git worktree list --porcelain`, where `branch
refs/heads/<b>` matches. Take the windows from `tmux list-windows -a -F
'#{window_id}\t#{@crew_name}\t#{pane_current_path}'` (the same keying as
     `_occupants`, dispatcher excluded). The candidate panes come from `tmux
list-panes -a -F '#{window_id}\t#{pane_id}\t#{pane_current_command}'`,
     filtered by `_is_engine_cmd` and an empty `tmux show-options -pqv -t <pane>
@crew_state`. Zero or more than one pane is exit 5 (`no engine pane` /
     `ambiguous panes`).
  5. Lock (R2-1), with `trap` on EXIT/INT/TERM releasing it.
     5a. **Quota gate (PC-2)** runs **immediately after step 2 and before the
     engine and pane resolution** (PC2-3). If the session's latest status is
     `blocked` with `source:"watchdog"` and a `detail` starting `quota:`, exit 5
     `quota`. The hint reads "worker is quota-parked — do not answer or wake it
     (see DISPATCHER_PROTOCOL quota:)". Its position under 5 in this list is
     for readability only; execution order is 1, 2, 5a, 3, 4, 5, 6.
  6. Loop until the deadline:
     - consumed check: a `status` from the session with `ts > since`,
       `state != blocked` **and `(.body.source // "") != "watchdog"`** → exit 0
       `consumed`, or exit 5 `worker stopped` if the state is terminal. A
       watchdog `working "<prefix> cleared"` row is not the worker reading its
       inbox (`_post_clear`, `crew.sh:3132`), so it must never count (PC-1).
       A watchdog terminal row (`failed dead:`) still exits 5 `worker stopped`.
     - sample `tmux capture-pane -e -p -t <pane>` and classify:
       - prompt → 5.
       - vimmode or unsent → 3.
       - busy or unknown → reset the idle count and sleep.
       - idle → count it. On 2 in a row, if `remaining >= 35` go to submit;
         otherwise refuse 3 `busy` (floor).
  7. Deadline: exit 5 `unknown-frame` if every sample was unknown, otherwise
     exit 3 `busy`.
  8. Submit:
     - `tmux send-keys -t <pane> -l "$WAKE_PROMPT"`.
     - Poll up to 5s for `_wake_class` = unsent with text = the stripped wake
       prompt. Any other text → exit 4 `input collided`, naming the pane, with no
       Enter.
     - `tmux send-keys -t <pane> Enter`.
     - Poll up to 15s for **idle or busy with an empty box**, and `plain`
       containing a line `❯ crew wake: read your crew inbox and continue` above
       the top rule (P-2) → exit 0 `delivered`.
     - If the box still holds exactly the wake text → one more Enter, then poll
       another 15s. Otherwise exit 4.
     - An INT/TERM between typing and the verified Enter prints the pane name.
- stderr is one line per outcome: `crew: nudge: <outcome>: <reason> (pane %N)`,
  plus the hint column from the spec table.
- Validate: the S6 "nudge:" tests.

### S5 — `crew reply` wakes a blocked worker · implement: sonnet

- Parse trailing flags after `<to> <body>`: `--no-wake` and `--wake-timeout S`
  (default 60).
- After `_bus_append`, if `to` is `worker:*` and wake is not disabled:
  - look up the latest state of the resolved session (`_sessions` on its branch,
    matching session);
  - if it is `blocked`, run `_nudge "$to" "$crew" <line ts> <wake-timeout> 2`.
  - Read the line ts back with `jq -r .ts` on the built line.
- Map a nudge exit 1 to 5 inside `reply` (R-consumer).
- On non-zero, prefix stderr with `crew: reply: the reply is on the bus — do not
resend`, then exit with the nudge code.
- `crew.sh` `await)` comment (L641–643): replace "stays in the durable log for
  the next activation" with "stays in the durable log; `crew reply` wakes an
  ended turn (see `nudge`)" (PC-5).
- The test edits at L996/L1050 are **not** here; they moved to S6 (PC-4).
- Literal-drift check, run after S5 and S7:
  `rg -F 'crew wake: read your crew inbox and continue' adapters/core/crew.sh
adapters/core/protocols/WORKER_PROTOCOL.md
adapters/core/protocols/DISPATCHER_PROTOCOL.md` must hit all three files.
  The exit-code table in `DISPATCHER_PROTOCOL.md` must list exactly 0/1/3/4/5,
  with the outcome words from the Interfaces block.
- Validate: `bats -f '^(reply|nudge|wake)' tests/crew.bats` (after S6 lands).

### S6 — tests · implement: sonnet

- It is the only step that edits `tests/crew.bats` (PC-4). It first appends
  `--no-wake` to the `run_crew reply` calls at L996 and L1050, which pin
  resolution, not waking.
- **New block appended at the END of `tests/crew.bats`** under
  `# ---- wake (#186) ----`. Leave the stall-watch block (#185) untouched.
- `wake_tmux` is a stateful stub on PATH. It records argv to `$STUB_LOG`.
  - State files hold `frame` (the current frame name), `panes.txt` and `wins.txt`.
  - `capture-pane` prints `$WAKE_DIR/frames/<state>`.
  - `send-keys -l <t>` switches the state `idle` → `typed`.
  - `send-keys Enter` switches `typed` → `submitted`, unless `WAKE_ENTER_NOOP=1`,
    and appends a `working` status for the session (the simulated worker).
  - `show-options -pqv` prints `$WAKE_DIR/crew_state.<pane>`, or nothing.
  - `list-windows` and `list-panes` read the fixed files.
  - `WAKE_CONSUME_AFTER=N` appends `working` after N captures, which covers the
    in-band await case.
- The worktree comes from a real `git worktree add` of branch `feat/x` inside the
  test repo. That lets `git worktree list` resolve it, and its `pwd -P` path goes
  into `wins.txt`.
- The session is seeded with a `dispatch` row (`engine:"claude"`, `session
s1-1`) and a `blocked` status.
- Frame fixtures, pinned from the P-1/P-2/`%336` captures, with escapes kept:
  `idle`, `idle_ghost` (`\e[2mgo ahead\e[0m`), `ghost_unterminated`, `typed`,
  `submitted` (`· Smooshing…` plus the transcript wake line), `busy_meter`
  (`* Canoodling… (2m 39s · ↓ 10.7k tokens)`), `busy_early` (`· Determining…
(2s · thinking)`), `unsent` (typed human text), `trust_unnumbered` (P-3),
  `normal_mode` (`-- NORMAL --`), and `garbage` (no rules).
- Tests:
  1. **RED on main:** `reply` to a blocked session, idle pane → a `working` status
     after the reply ts exists, and `send-keys -l crew wake: …` was logged.
  2. wake-frame unit tests, one per fixture: the classification and the stripped
     text.
  3. nudge: `busy_meter` → exit 3, no `send-keys`. `busy_early` → exit 3.
  4. nudge: `unsent` → exit 3 immediately (the log shows one capture), no
     `send-keys`.
  5. nudge: `trust_unnumbered` → 5. `normal_mode` → 3. `garbage` persisting → 5
     `unknown-frame`.
  6. nudge: `idle_ghost` → delivered (exit 0), `send-keys -l` then `Enter`.
  7. nudge: `WAKE_ENTER_NOOP=1` → two Enters, exit 4.
  8. nudge: `WAKE_CONSUME_AFTER=2` on `busy_meter` → exit 0 `consumed`, no
     `send-keys`.
  9. nudge: engine `codex` → exit 5, no `capture-pane`.
  10. nudge: a live-pid lock present → exit 0 `in-progress`. A lock dir with a
      dead pid → acquired and delivered. A missing `$dir/wake` → still delivered
      (R2-1).
  11. nudge: `--timeout 20` on idle → exit 3 (floor), no `send-keys`.
  12. reply on a `working` session → no tmux call at all. `--no-wake` on a blocked
      session → append only, exit 0.
  13. reply on a codex blocked session → exit 5, the msg line is present, and
      stderr contains `do not resend`.
  14. reply with an explicit unknown sid → appended verbatim, no wake, exit 0.
      `nudge` on that sid → exit 1.
  15. nudge: two panes in the window, one carrying `@crew_state` → the lead is
      chosen. Two unmarked engine panes → exit 5 `ambiguous`.
  16. nudge (PC-1): the reply ts is followed by a watchdog `working` row reading
      `prompt: cleared` (`source:"watchdog"`), and the pane is idle → exit 0
      `delivered` with `send-keys`, never `consumed`.
  17. nudge/reply (PC-2): the latest status is a watchdog `blocked` with
      `quota: session limit …` → exit 5 `quota`, no `capture-pane`, no
      `send-keys`. `reply` still appends.
  18. nudge (PC-6): `busy_transcript`, an idle empty box with a meter-shaped
      line more than 6 non-empty lines above the top rule → `idle` / delivered.
      `busy_subrow`, an empty box plus a live subagent row below the status bar
      → exit 3. `busy_subbatch_many` (PC2-1): the meter, then 6 live subagent
      rows and 2 `⎿` rows, then an empty box → exit 3, no `send-keys`.
  19. nudge (PC-7): a `dispatch` row with `engine:"codex"`, then a `resume` row
      with `engine:"claude"` for the same branch and a new session that is now
      blocked → the claude engine is used (delivered). The reverse order → exit 5
      `engine codex`.
- Tests use `--interval 0` except the busy ones, which use `--interval 1
--timeout 2`.

### S7 — protocols · implement: sonnet

- `WORKER_PROTOCOL.md` "Report to the bus". Leave the Plan of record and Code
  review gate sections alone (#179).
  - L317 timeout bullet: replace "(you resume on next activation)" with the real
    mechanism. The dispatcher's `crew reply` submits the fixed prompt
    `crew wake: read your crew inbox and continue` into your pane once your turn
    has ended. On that prompt: re-stamp `working`, run the straggler fold
    (`crew inbox "$CREW_WORKER_ID" --since <seen>`), handle the reply with
    receiving-code-review discipline, and resume from the paused step. A wake
    prompt with nothing new in the inbox: re-stamp `blocked` with the same why
    and stop, **without** starting a new await cycle and without counting toward
    the cap.
  - L326: "it resumes you in place" → "the dispatcher's reply reaches you in
    place, in-band while you await and by a wake prompt after".
- `DISPATCHER_PROTOCOL.md`:
  - L520 bullet: drop "resumes in place, no tmux". `crew reply` to a `blocked`
    worker waits (≤60s) and wakes it: `consumed` in-band, or a verified wake
    prompt.
  - Add an exit-code table (0/3/4/5, plus 1 meaning nothing written) and the
    action for each. Never re-send the reply.
  - Non-claude engines always get 5 today, so answer inside the ~300s await
    window or use the manual last resort.
  - New short sub-bullet **"Manual pane injection (last resort)"**: only after
    `crew nudge` returned 5 or 4, and a human decision.
    1. `tmux capture-pane -e -p -t %N` and confirm there is no meter/spinner and
       no non-dim text in the box.
    2. `send-keys -l` the **wake prompt**, never the directive.
    3. Capture again and confirm the box holds exactly that text.
    4. `Enter`.
    5. Capture and confirm the box is empty and the transcript shows it.
  - L521–524, the watchdog `blocked`: `crew reply` now also wakes it when the pane
    is idle. The note that nobody is in `crew await` stays; if the wake refuses,
    go to the pane.
- Quota (PC-2): in the L520 bullet, state that `crew reply` never wakes a
  watchdog `quota:`-blocked worker (exit 5 `quota`), consistent with the quota
  section's "stop, don't answer".
- The exit-code table follows this plan's **Interfaces** block: no or ambiguous
  pane is 5, and 1 means nothing was written. It does not follow the spec's
  original §10 row (PC-8, since corrected in the spec).
- Validate: `rg -n 'next activation|resumes in place' adapters/core` returns
  nothing stale, including the `crew.sh` `await)` comment (PC-5).

### S8 — red evidence · worker

- `git worktree add <scratch>/main-red origin/main`, then copy the new
  `tests/crew.bats` block (and only that) into it.
- Run `bats -f 'RED on main' tests/crew.bats` there. Expect an assertion failure,
  not a setup error: no `working` row and no `send-keys`. Then run the same test
  in this worktree and expect green.
- Record the commands, revisions and assertion output for `## Evidence`. Remove
  the scratch worktree afterwards.

### S9 — live run · worker

- A private tmux server, `tmux -L wake186`, with a window stamped
  `@crew_name probe` whose path is a scratch `git worktree` of a scratch repo on
  branch `feat/x`, running `claude --model haiku` (trust answered in my own
  session).
- Seed that scratch repo's bus with a `dispatch` row (engine claude) and a
  `blocked` status.
- **Isolation is mandatory (PC-3).** Inside tmux, `$TMUX` overrides
  `TMUX_TMPDIR`, so a bare `tmux` from crew would hit the LIVE server and could
  type into a real worker. Every live-run crew invocation therefore runs as
  `env -u TMUX -u TMUX_PANE PATH="<shimdir>:$PATH" CREW_ID=<scratch>` with **cwd =
  the scratch repo** (the bus and `git worktree list` resolve from cwd).
  `<shimdir>/tmux` is `#!/usr/bin/env bash` followed by `exec <abs real tmux> -L
wake186 "$@"`. Before any `reply`, confirm `tmux list-panes -a` through the shim
  lists only the probe pane.
- Then run `bash <worktree>/adapters/core/crew.sh reply worker:feat/x "decision"`
  under that env.
- Expect exit 0 `delivered`, and a capture showing the transcript wake line.
- Then type unsent text by hand and re-run `crew nudge`. Expect exit 3 `unsent`,
  and the typed text is still intact.
- Third case, mid-turn: submit a prompt to the probe session by hand, then
  immediately run `crew nudge` under the same env. Expect exit 3 `busy` and a
  log with no `send-keys`.
- Kill the private server afterwards, through the shim. Record all three
  outputs.

### S10 — gates · worker

- `shellcheck adapters/core/*.sh adapters/core/reviewers/*.sh scripts/*.sh`.
- `bats --jobs 16 tests/`.
- `nix fmt` (treefmt), or check the formatter touches no in-scope file.

### S11 — rebase and adapters · worker

- `git fetch`, then `git rebase origin/main`.
- `scripts/gen-adapters.sh` twice, asserting `git diff --exit-code` after the
  second run.
- If `PROTOCOL_REV` exists by then: regenerate and commit it, and never hand-merge
  it.

## Acceptance

- [ ] W1: test 1 is green here and red on `main` (S8), and the live run delivers
      (S9).
- [ ] W2: tests 3, 4, 5, 11 and 15, plus the live `unsent` refusal, leave no
      `send-keys` in the log.
- [ ] W3: every `send-keys -l` in the tests carries exactly `WAKE_PROMPT`, and the
      directive body never appears in the tmux log.
- [ ] W4: tests 6 and 7. Delivery requires the transcript line plus an empty box.
- [ ] The protocols describe the mechanism, and no stale "next activation" /
      "resumes in place" text remains.
- [ ] shellcheck and the full bats suite are green, and gen-adapters is idempotent.
