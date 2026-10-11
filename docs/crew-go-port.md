# Porting crew to Go

Issue: #822. `adapters/core/crew.sh` moves to Go one subcommand at a time.
Ported so far: `log`, `report`, `sessions`, `roster`, `crews`, `inbox`, `hold`,
`await`, `retro`, `rate` (both modes: the per-repo sweep of #895 and the
`--report` rollup of #890), `reply`, `watch`, `resolve-target`, `where`,
`identity` (#981),
`stall-watch` (#832), `stream`, `adopt` (#920), `pr-watch` (#934), `roster-render`
(#935), `fleet` (#926), `status`, `msg`, `register` and `deregister` (#945), and `nudge`
(#939). Each slice must leave every bats file green;
tests may be adapted only where the Go design changes what they can observe (the
output contract below), with each edit justified.

## Layout

- `crew/main.go`: argv dispatch.
- `crew/internal/jqrun`: runs the original jq programs on gojq
  (`github.com/itchyny/gojq`): jsonv values in, jsonv values out, `now` and
  `$vars` injected, type errors surfaced as exit 5. The only gojq caller.
- `crew/internal/jsonv`: ordered JSON values with jq semantics — decode
  (number-literal text kept, NaN/Infinity extensions), encode (compact/pretty,
  `$JQ_COLORS`), and the two number-text accessors jqrun needs. The fold
  comparators are gone (#861): gojq owns ordering and grouping now.
- `crew/internal/bus`: bus location, crew-id resolution, typed event reads, and
  the writer `hold` appends through: `Append` is `_bus_append` (create the
  directory, terminate a torn tail, `O_APPEND`), `FitLine`/`Shrink` are
  `_fit_line`/`_shrink` at `LineMax`, the one copy now that `status` and `msg`
  are Go too. `Toplevel` is `git rev-parse --show-toplevel` from the cwd, which
  the status gate reads the worker's `WORKER_TASK.md` through.
- `crew/internal/identity`: codename, colour and tmux pools, the cksum slot, and
  the assignment the arm made: `Assign` is `_identity_assign`, reusing `Recorded`
  for what dispatch stamped and folding the bus for who holds what.
- `crew/internal/roster`, `crew/internal/sessions`, `crew/internal/crews`,
  `crew/internal/report`: each embeds its jq program (`*.jq`, kept verbatim from
  crew.sh apart from documented patches) plus the Go glue: tmux/worktree
  probes, identity attachment, the crews pid-liveness and ancestor probes.
- `crew/internal/log`: the arm that runs no jq — `select(.crew_id==$crew)` is a
  member test, so it filters and re-encodes through jsonv, keeping `jq -c`'s
  torn-tail prefix via `bus.ReadEventsTolerant`.
- `crew/internal/inbox`: same shape as `log` (member tests plus one numeric
  compare, no jq), and the only writer of the delivered marks on the Go side.
  Its `.ts > $since` is a decimal-literal compare, because jq compares literals
  exactly and a double cannot tell `18446744073709551617` from
  `18446744073709551616`.
- `crew/internal/hold`: the arm of #882, and the first port that **writes**.
  Its four folds (`outstanding.jq`, `matured.jq`, `park.jq`, `render.jq`) are
  the arm's programs, run over the bus through jqrun; its two row builders are
  pure Go over jsonv, because a row's `body` is a _string_ — key order inside it
  is part of the value, so `jq -S` cannot excuse gojq's sorted keys and the
  write path has to be byte-exact. Arg parsing, validation order and the
  `_clock_now`/`_clock_now_f` pair are Go; `$CREW_CLOCK` reaches it through
  `Options.CrewClock`.
- `crew/internal/marks`: the delivered-marks file of #290 — `_await_state`'s
  path, `_await_marks`' read and `_await_record`'s atomic merge, the two jq
  programs embedded verbatim. Its readers are Go too now: the unread scan of
  `stall-watch` and `nudge` moved to `internal/unread` (#968), so the name and
  the content are the contract only against the crew.bats guards that still
  replay the bash helpers.
- `crew/internal/unread`: the unread scan of #290, `_unread_scan`, ported in
  process (#968) — `stall-watch`'s D6 runs it every 4th tick and `crew nudge`
  per call, both through this one package instead of the `--sh unread`
  call-out. It folds the log's last 2000 lines (`probe.BusRows`) over the
  marks it reads through `internal/marks`, and runs the helper's jq program
  verbatim through jqrun (a new package beside marks, not inside it: marks
  owns the marks file, while a scan is a bus read that consults it).
- `crew/internal/clock`: the `$CREW_CLOCK` pair — `_clock_now`, `_clock_now_f`,
  `_clock_now_ms` and `_clock_sleep` — shared by `hold` (which had the first two
  privately until #884), `await`, `stall-watch` and `nudge`. With no `CREW_CLOCK`
  these are `date +%s`, jq's `now`, jq's `now*1000|floor` and the real `sleep`
  (same argv, so a
  rejected interval costs coreutils' own message and status, which is what
  killed the arm under `set -e`).
- `crew/internal/lock`: the lock under `ratings.lock.d`, `watch.lock.d`, `stream.lock.d`,
  `roster-render.lock.d`, the `nudge --wait` dir and the budget refresh, as one package
  instead of a copy per caller. Exclusion is `flock(2)` (`LOCK_EX|LOCK_NB`) on
  `<dir>.flock`, which the kernel drops when the holder dies, so no acquirer judges
  staleness. The record dir `<dir>/` holds `pid` (reporting only: `Holder`,
  `stream --force` and `--status` read it) and a `flock` marker, and is published whole by
  renaming `<dir>.new` into place, so `Holder` never reads empty under a live holder. A
  record whose marker matches its `pid` and whose flock is free is replaced with no
  liveness probe, so pid reuse or a reboot cannot wedge it. Held for the two `rate` store
  windows and never across a gh call; `watch` holds it for its whole park and releases it on
  every exit path, including the SIGTERM/SIGINT/SIGHUP `stream` sends it. In-process
  re-acquire by the same owner succeeds and another owner is refused. Release acts only
  while this process holds the lock, and touches paths only while `<dir>.flock` is still
  the file it locked: it then retires the record (rename to `<dir>.stale`, re-check,
  remove) if its `pid` is still the owner's, and unlinks `<dir>.flock`; it always closes
  the fd; an acquirer that locked a file
  its holder had since unlinked sees the inode mismatch and retries. The `roster-render`
  exec hop drops the flock (`O_CLOEXEC`) and the new image adopts the record carrying its
  own pid.

  An old-format record (no matching marker) is the one-time compatibility path for
  old-build processes still on the mkdir protocol: a live `pid` by bare `kill -0` refuses,
  an empty one inside the 2s grace refuses (an old acquirer mid-write), anything else is
  retired by renaming the dir to `<dir>.stale`, re-reading the retired `pid`, and
  renaming it back if an old acquirer published meanwhile. That keeps old builds out too:
  their `mkdir` fails on the record and they read a live pid. Residual windows, all
  transitional and closing when the last old-build lock user exits: two old-build
  acquirers interleaving inside one retire can both hold (the retire renames `<dir>` away,
  one reclaims by its compare-and-swap before the rename, another `mkdir`s the gap before
  the restore); old builds can already double-hold among themselves by their own
  protocol's gaps, so no new-side rule reaches zero while they run; an old reclaimer that judged a
  dead record before a new publish can briefly make `Holder` read empty for a live new
  holder (a `roster-render` poll there exits early and the next start re-acquires); and
  old builds keep their own pid-reuse wedge for records they write. After a switch,
  re-arm running streams with `crew stream --force` and let old workers' stall-watches
  finish; nothing else needs restarting.

- `crew/internal/await`: the arm of #884, and the first port that **polls**. Its
  fold is the arm's jq through jqrun (`await.jq`), over rows the caller decoded
  one per line — the arm's `-R` + `fromjson?` skip, with the repo's own decoder.
  The clock, the marks and the deadline are Go; a timeout is exit 0.
- `crew/internal/retro`: the arm of #887, read-only like `report`. Its fold is
  the arm's jq through jqrun (`retro.jq`), and it is the only port that renders
  three shapes from one run: the bare `@tsv` rows, the `--report` table (whose
  width math the program does itself, never `column -t`), and `--report --json`,
  its one non-string output, printed pretty and plain — the palette is a
  process quirk like the `$JQ_COLORS` warning, so neither is mirrored and the
  object is compared with `jq -S`. The fold reads the whole bus with no crew
  filter: a note is cross-run evidence, which is why a clean run prints nothing
  at all.
- `crew/internal/rate`: the whole `rate` arm — #890's read-only `--report`,
  #895's sweep, and #966's `--sweep-all` loop, which is the port's first writer
  that is not the bus. The flag loop and every refusal line are Go now, so the
  arm is the shared one-liner. `--sweep-all` is the mode with no repo of its own:
  it discovers repos the way `crew fleet` does and calls the same per-repo sweep
  the hand-run command does, where bash re-entered its own arm per repo. The
  sweep is the arm op-for-op: `records.jq` folds the bus into one row
  per run, `plan.jq` decides per call (not per run) which gh queries a stored
  row still needs, `view.jq`/`actions.jq`/`threads.jq` ingest the three
  responses, and `merge.jq` merges forward against a fresh store read so a t2
  upgrade another sweep landed during the network phase is not lost. `dedupe.jq`
  is the store fold every reader shares, and every store failure folds to `[]`
  in silence — the arm's `2>/dev/null || true` — so an empty, missing or
  unparseable store renders the header alone and sweeps as new.
  `burn.go` prices models from the settings `burnClasses` table, whose globs are
  bash `case` patterns, not `path.Match` (`*` crosses `/`, and `|` is literal
  because the helper matched an expanded pattern). The `ratings.lock.d` gate is
  `internal/lock`'s flock, held for the two store windows and never across a gh
  call, and released on every exit path while held; an older installed `crew` whose
  `rate` is the bash arm is met by its old-format branch. `nudge --wait` holds
  `<dir>/nudge-wait/<sid>.<cksum>.d` for its whole park (one waiter per lead
  session), keeping the bash helper's name and `pid` file so a call on
  an older installed `crew` joins the same wait. The batch append is one
  `O_APPEND` write, the same single-write guarantee the arm's `dd bs=1048576`
  gave.
- `crew/internal/reply`: the arm of #893, a writer like `hold` but with no fold
  of its own — it resolves a branch-only `worker:<branch>` target through
  `sessions.Fold`, and with no crew named through every crew the bus carries,
  dropping the ones whose registered pid is known-dead by the same three-state
  read `crews` prints (`crews.PidAlive`). Its row is the arm's six keys in order,
  and it needs no byte-exact body: `body` is the caller's text, so `jq -S` does
  excuse the key order. `bus.IsSessionID` is what `_is_session_id` was, the one
  `inbox`, `await` and `nudge` share.
- `crew/internal/watch`: the arm of #901, the second poller. Its fold is the
  arm's jq through jqrun (`watch.jq`), and it is the port with the most
  cross-language surface: `crew stream` re-enters this command as its child, so
  the batch, the cursor file and `watch.lock.d` are all contracts across that
  boundary. Two rules follow from the arm. The clock is the wall
  clock — it stamped `start` and every deadline with jq's `now` and slept with
  the real `sleep`, so unlike `await` and `hold` it never reads `$CREW_CLOCK`
  (and the suite exports `CREW_CLOCK` for `await`'s sake, which is why this is
  worth saying). And the lock is held for the whole park and released on every
  exit path, including the SIGTERM/SIGINT/SIGHUP `stream` sends its inner watch
  when it stops or retries: bash ran its `trap … EXIT` for those three. A watch killed
  before it releases leaves only an inert record (its flock dies with it), which the next
  watch replaces at once; only an old-build watch still judges that record by `kill -0`. The batch prints before the cursor moves, as the arm's `printf`
  preceded its `mv`.
- `crew/internal/resolve`: `crew resolve-target`, and the fold `crew where` reads
  for one question. `resolve.jq` is `_resolve_target`'s program and `Rows` is its
  read — `jq -R` with `fromjson?`, so a line that will not decode is skipped and
  its neighbours still resolve — and because the helper ran jq under
  `2>/dev/null || true`, neither arm exits 2 or 5 off the bus: 2 is ambiguity
  alone. `where`'s one use of it is the difference between a window that is gone
  and a target nothing ever dispatched.
- `crew/internal/where`: `crew where`, the address a dispatcher relays to a
  human, so its line is exact text. It reads its two tmux queries through
  injected probes (the arm's argv and `-F` formats; a failed read is the arm's
  own refusal, not "the target is gone"), keeps the arm's trust rule — only the
  window's `@crew_*` stamps and the bus's `dispatch` rows, never git discovery in
  a worktree — and falls back to the pool identity (`identity`) for a window
  stamped before `@crew_name` existed.
- `crew/internal/identity` is also the `crew identity` arm of #981: the codename a
  branch works under, which `dispatch` stamps on its dispatch row and on the
  window's `@crew_name`. A branch keeps what it recorded unless another live
  worker holds that name, else its hash slot steps forward to the first free
  codename — occupancy the arm read twice, and both reads stay tolerant: no bus,
  a bus `jq -s` could not slurp, and a failed `tmux list-windows` each contribute
  nothing rather than refusing, because a branch always gets an identity. The
  window list is an injected probe (the arm's argv; a failed read names nothing),
  so no test needs tmux, and it is consulted only on the fallback path — a
  recorded name is gated by the bus alone, as the arm gated it.
- `crew/internal/adopt`: `crew adopt`, the re-attach a dispatcher runs after a
  restart lost `CREW_ID` (#29) and the release of the `dispatched` labels a crew
  that died mid-claim left behind (#73). stdout is the bare id alone — callers
  run `CREW_ID=$(crew adopt <id> $PPID)` — so every message the run has to say,
  kept claim included, is stderr. The pid file and `$dir/pidfile.log` have no bash
  writer left: `_pidfile_log` is `crews.PidfileLog` (with `crews.Caller` as the
  pid/ppid/pwd/by identity it stamps), shared with `register`, and `_owner_pid`
  is `crews.Probes.OwnerPID`; `$$`, `$PPID` and `$PWD` are this process's own,
  which is what crew.sh's `exec` leaves them. `claims.jq` is the arm's fold with
  the documented jqrun patch: its final `.[] | @tsv` is dropped and Go reads the
  rows by position. Occupancy is the one question asked of `_occupants`, and the
  window list alone answers it, so the arm's third read (`tmux list-panes`,
  whose rows fill only the advisory `engine`/`panes` fields) is not run.
- `crew/internal/prwatch`: `crew pr-watch`, the thin bus bridge over the standalone
  `pr-watch` binary — the first port that runs a child named after itself. The binary
  owns the park, the change signals and the
  per-PR cursor and needs no crew id; the arm adds the post, addressed to this crew's
  dispatcher through `_crew_id` (`bus.CrewID`), and keeps stdout the event. The row is
  the arm's six keys in order, and like `reply` it needs no byte-exact body: `body` is
  the event text as the child printed it. The child is injected (`Options.Start`), so
  no test needs the binary, and `$(…)`'s trailing-newline strip is Go's own. The stop
  signals follow `stall.NotifySignals`'s two rules — inherited-ignored signals are
  left alone, and the first one stops the handler — because this is the one port that
  parks on a child which must not outlive it.
- `crew/internal/frame`: Go copies of `_frame_classifier`'s predicates (prompt,
  permission, quota, background-wait, meter and sub-row shapes, the claude and
  pi input boxes) and `_pane_idle_reason`. Every function takes the sampler's
  stdout with trailing newlines stripped and the engine as an argument, where
  bash read the global `$engine`. `BoxText`, `Tail` and `HasSpinnerRow` joined
  them with `nudge`, on the same box scan (`scanBox`) `BoxRows` already used, so
  the box's bounds live in one place; `BoxText` differs from `BoxRows` in that an
  empty box is a result rather than a failure, which is what the caller's "is the
  box empty?" needs. The bash originals stay (see below); the translation rules
  are what keep the two equal:
  - POSIX classes follow glibc's `C.UTF-8`, the arm's runtime, not RE2's ASCII
    ones. `[:space:]` is `\t\n\v\f\r`, space, U+1680, U+2000–2006, U+2008–200A,
    U+2028, U+2029, U+205F and U+3000 — NBSP, U+2007 and U+202F are _not_ space,
    which is what lets a `❯`+NBSP draft read as unsent input. `[:alnum:]` is
    `\p{L}\p{Nd}\p{Nl}`; glibc also takes the Other_Alphabetic combining marks
    (U+093E, U+0345), which RE2 cannot name, so that gap is known and noted in
    the source.
  - The two patterns bash runs under `LC_ALL=C` as raw bytes (`_pi_working_row`,
    `_pi_working_label`: `\xe2[\xa0-\xa3][\x80-\xbf]`) become the rune range
    `[\x{2800}-\x{28FF}]` with an ASCII space class, never a Go `\xe2` byte.
  - Every extraction (`grep -o`, a `sed -E` capture: the draft prefix, pi's
    vim-mode label, `_top_consumers`' cwd tail) is POSIX leftmost-longest
    (`regexp.Longest()`); match-only predicates are unaffected.
  - Counts are runes: `${body:0:$max}`, `${#suffix}`, `${detail:0:40}`.

  The drift guard runs the extracted `_frame_classifier` and `_pane_idle_reason`
  in bash under an explicit `LC_ALL=C.UTF-8` and asserts that every predicate,
  for every engine, agrees with Go over `testdata/frames` (the crew.bats
  fixtures, a frame per engine, multibyte and NBSP edge frames). It skips when
  crew.sh is absent, as in the Nix sandbox. crew.sh dropped `_is_bg_wait`, so
  its pre-port body is kept in `testdata/is_bg_wait.bash` as the oracle.

- `crew/internal/stall`: the `stall-watch` arm of #832. Go parses the arm's
  argv in its order (the five `crew: stall-watch…` lines and the
  `CREW_ID unset…` line are exact), resolves identity (role mode, sessioned, or
  branch-only; INV-W0's own-epoch step-aside), builds the signature table with
  the role-mode override, decides D8 once (pi asks the pane's `@crew_model`,
  then `--sh local-model`; any failure to tell keeps it on) and exits early for
  a non-claude role with D8 off. One loop then samples the pane every
  `--interval` and runs D4, D8, D5 and the role-mode end-of-life check, then D1,
  D2, D1b, D7, D3, D0 and D9 on the frame (D9 types one `continue` line into a
  stopped pi lead and never escalates), then D6, then the `dead:` escalation
  (D2 and D3; D3's also needs the engine gone; an open `prompt:`, `quota:`,
  `runaway:`, `truncated:` or `provider-error:` episode is exempt), with the pane-gone quorum of 3,
  the 4-tick bus-read cadence and `--max-life` checked every tick. A `done` or
  `failed` bus state hands over to the release loop: claude must show
  `frame.PaneIdleReason` idle on two consecutive ticks, other engines an
  unchanged frame for `--release` with no prompt, and `--sh release` verdict 3,
  or a helper that failed to run, keeps watching while any other verdict
  exits 0. Every bus write is
  `post`/`postBlocked`/`postClear` (INV-W1 pre-write refresh and terminal-state
  abort, INV-W3 same-prefix suppression with sticky `prompt:`/`quota:`, INV-W2
  own-prefix clearance); a row is one `bus.Append` of the arm's
  `{ts,crew_id,from,to,kind,body:{state,detail,source}}`, followed in worker
  mode by the three `tmux set-option -p` calls of `_publish_pane_state` (detail
  cut to 40 runes). The two jq programs are the arm's, through jqrun:
  `refresh.jq` (`_bus_refresh`) and `nudged.jq` (the already-nudged check over
  the same 2000-line tail). Their patches, documented in each header: the
  per-row filter is wrapped in `[.[] | …]` because jqrun returns one value and
  takes the decoded rows as `.` in place of `inputs | fromjson?`; `refresh.jq`
  runs each row under `try`, so a malformed row drops out alone as jq skips it;
  rows are arrays, not `@tsv` text, so Go reads fields by position; and the
  `capture` anchor is `\n?\z`, as in `sessions`. Signals: SIGTERM (exit 143)
  and SIGINT (exit 130) cancel the context with a cause and the process exits
  128+signo, as a shell reports the killed bash arm; sleeps and children are
  context-aware, and the budget-refresh lock is released on every exit path
  while held. SIGINT is handled only when not inherited ignored: dispatch starts
  the watchdog as a non-interactive bash `&` job, which ignores it, so a Ctrl-C
  to dispatch's group never reached the arm. SIGHUP is never handled — the
  watchdog is `nohup`-launched, and `signal.Notify(SIGHUP)` would undo the
  ignore it inherits. A probe that returns into a cancelled context was cut
  short, and its failure value (empty text, a dead pane) is no evidence: the
  watch exits 128+signo without posting, as the arm died on the signal. The
  `--sh release` op and the watchdog's own nudge and continue runs run on through a
  signal (for at most 2 minutes), as the arm's `$(…)` child outlived it, so a typed
  nudge or continue line or killed window still gets its bus row. D6 and D9 call
  `nudge.Pane` through `Options.Nudge` on a `context.WithoutCancel` copy with that
  same bound: the watchdog and `crew nudge` are one implementation of the decision,
  and a line a nudge is mid-way through typing is not cancelled by the tick ending.
  Every append is one `O_APPEND` write,
  so a kill cannot leave a torn row; the `refresh-at` stamp and the clock file
  keep tmp+rename. A failed bus write exits 1 where the arm's `set -e` did (a
  `failed` or clearance post); `_post_blocked` ran only in `if` context, so
  there it means not posted and the detector retries next tick. The budget
  refresh's `mkdir` and stamp writes are unchecked, as in the arm: a failed
  `mkdir` only fails the lock.
- `crew/internal/stall/probe`: the side effects as a struct of seams
  (`Probes`), so the loop is testable without tmux. The `CREW_STALL_SAMPLE_CMD`,
  `_COLOR_CMD`, `_PROC_CMD`, `_LOAD_CMD`, `_TOP_CMD` and `CREW_BUDGET_REFRESH_CMD`
  seams keep their meaning: the text runs under `bash -c` (the arm `eval`ed it),
  stderr is discarded, stdout loses trailing newlines as `$(…)` does, and the
  sampler's exit status is pane liveness. The default probes keep the arm's
  argv: `tmux capture-pane [-e] -p`, `list-panes -a -F`, `show-options -pqv`,
  `set-option -p`, `ps -eo …`, `nproc` once (`1` on failure) and
  `env -u CREW_WORKER_ID -u CREW_ID timeout 120 refresh-budget`. Every child runs
  in its own process group, killed whole on cancel, so a `sleep` under a seam
  dies with its parent. `Sh` is the `--sh` runner (see Delegation), and
  `ShOffset`/`ShVerdict` are its rc−10 contract, shared with `nudge`'s one call.
- `crew/internal/nudge`: the `crew nudge` arm of #939 — resolve a dispatcher's
  target to one live lead pane and type the package's own fixed line into it,
  once, or wait for the lead to come idle. `Pane` is the whole decision — the
  frame, anchor and session gates, the keystroke, the confirm, and the one row —
  and it is the only implementation of it: `stall-watch`'s D6 auto-nudge and D9
  auto-continue both call it through `Options.Nudge`, and two copies could
  disagree about whether a pane may be typed into, where the second opinion is
  the expensive one. D9 types the package's other constant, `ContinueLine`, with
  a `msg_ts` of 0 so an auto-continue never reads as covering a directive; both
  bash entry points it used to have (`_nudge_pane`, `stall-watch --sh continue`)
  are gone with the port. Nothing about the target is ever typed: the text is a
  constant, and the pane comes only from the window's `@crew_*` stamps.
  The outcome is four answers, not a boolean — accepted (0), refused before
  typing (2), typed-but-not-accepted (3, in three flavours: `held`,
  `unconfirmed`, `unknown`) and a failure (1) — and the three typed flavours all
  say _never re-typed, a human must clear it (crew where)_, because a second
  Enter would submit whatever the lead is holding. Only `send-keys -l` and one
  `send-keys Enter` touch the pane, and `Enter` is never sent when the box never
  showed the line.
  The clock split is the row contract: the `nudge` row's `ts` is jq's
  `now*1000|floor` (`Clock.RealMS`), the `nudge_wait` rows' is `_clock_now_ms`
  (`Clock.NowMS`, so `$CREW_CLOCK` moves them and the loop's waits), and `rc`
  exists only once it is a number.
  `already.jq` and `joined.jq` are the wait loop's two folds, verbatim but for the
  documented jqrun patches; the engine/window/pane/codename filtering is Go, the
  two numeric compares being the only reason a fold is a fold. The pane's captures
  go through `stall/probe`'s seams (`CREW_STALL_SAMPLE_CMD`, `_COLOR_CMD`), which
  are `_pane_capture`'s here: a bats row scripts one capture per call, for the
  watchdog and the command alike, and tmux stays a subprocess with the arm's argv
  because that stub counts them. Its unread scan is `internal/unread`'s, in
  process (see Delegation).
- `crew/internal/stream`: the arm of #914, the port that owns a process group.
  It is the only one that never exits on its own, so it writes its protocol lines
  straight to stdout — a buffered line would wait for a batch that may never come.
  Three children are the **crew script re-entered** (`bash -euo pipefail <self> …`,
  `self` from the `CREW_SELF` the delegation arm exports): `watch`, `hold due` and
  `reap`, which stayed there. That keeps the arm's fault-injection rows honest and
  keeps `crew reap` single-implemented, and it is why the port is a subprocess
  runner rather than three function calls: `watch` is stopped with TERM and waited
  so it can release `watch.lock.d` itself, and `reap` is never awaited nor signalled
  (an interrupted removal strands a worktree) and writes a per-child pair of files,
  so a reap that outlives its stream cannot clobber the next one's.
  `stream.lock.d` is taken through `internal/lock` before any signal is caught, so the
  refusal path is signal-free and a handler armed earlier has nothing to release.
  `lock.pidAlive` reads a holder past
  `lock.MaxPID` free, which is what bash's own refusal (`kill -0 4294967295` →
  "not a pid or valid job spec") gave the arm: such an old-format lock is replaced, never
  signalled — Go's `syscall.Kill` would have truncated that holder to `-1`, every
  process this user may signal. One place the port is not the arm: `--force`
  refuses the all-zero spellings (`00`, `000`) that bash's `kill` accepted as pid
  `0`, TERMed this whole process group with, and then waited five seconds to
  report had not cleared.
- `crew/internal/rosterrender`: the `roster-render` arm of #935, the port that
  **replaces its own process**. It is a daemon, a writer outside the bus, and a renderer
  whose output is compared byte for byte, and those three pull in different directions.
  Rendering stays jq: `d2.jq` is `_rr_d2` verbatim (patched only to take its input as
  jqrun's array and to collect the arm's `jq -r` lines into the one value jqrun
  returns), because the diagram is text a `cmp -s` gates and every label is quoted
  against injection. Row folding is the one part that stops being a subprocess: the arm
  re-entered the script for `crew roster`, and Go calls `roster.Fold` in process on the
  same wall clock. The upgrade is a `syscall.Exec`, at the top of the sleep and nowhere
  else, so the pid never changes hands; the flock does not survive the exec (`O_CLOEXEC`),
  and the new image re-acquires it and adopts the record carrying its own pid; the
  entry is followed whenever it names a build other than the running one, a rollback
  included, and only the running build is refused. PATH is restored to the value the
  daemon started with on every hop (`CREW_RR_START_PATH`, exported once), so N hops
  carry one wrapper prefix instead of growing it. The clock is split the way the arm
  split it: the render timers read `_clock_now_f`, so the suite drives them through
  `CREW_CLOCK`, while the poll sleeps on the wall clock and `crew roster`'s ages stay
  real. A daemon that slept on the virtual clock would let a test retire a live crew.
  Losing the lock ends the loop, and the owner check on release lives in `internal/lock`
  (Release is a no-op unless this process holds the lock), so a renderer whose crew dir
  was removed and re-created cannot delete the new owner's record. The exec hop drops
  the flock (`O_CLOEXEC`), and the new image's re-acquire adopts the record carrying its
  own pid; if another renderer took the flock in that instant it replaced the record and
  the hopping one's re-acquire refuses, so it exits quietly.
- `crew/internal/ledger`: the pure acceptance-ledger gate of `crew status <from>
pr_open` for an implement task: the ledger's form, the empty-detail rule, the
  CI-evidence rule and the dispatcher-waiver rule. It is hand-parsed, not run on
  jqrun, because the arm's form test and item scan are Oniguruma regexes with
  recursion (`\g<b>`) and look-ahead, which RE2 cannot express and gojq (built on
  RE2) cannot run. Matching the arm means matching Oniguruma's classes too: `\s` is
  Unicode, the `i` flag is full case folding (so `ß`/`ẞ` match `ss` and back), and
  the `\b` the arm tests uses Oniguruma's word class, not RE2's ASCII one. The
  arm's three jq programs sit verbatim in `testdata/` (`ledger.jq`, `items.jq`,
  `waiver.jq`) as the oracle: the differential test runs them under the real `jq`
  over a generated corpus and skips when `jq` is absent (the Nix sandbox). The
  package header lists the known differences; each one refuses more often than
  the arm or is unreachable.
- `crew/internal/status`: `crew status` and `crew msg`, the two writers of a row
  onto the bus. `status` is a write behind gates (`gate.go`): a worker posting
  `pr_open` or `done` is refused unless the ledger conforms and, for a standard or
  deep implement session, the log holds the lead's review seam and deslop seam.
  The two seam folds are the arm's own jq (`seam.jq`, `deslop.jq`) on jqrun with
  two patches each: the arm ran `jq -R -n` and read the log with `inputs`, here `.`
  is the array of log lines and `inputs` is `.[]`; and every `fromjson` is
  `_jqfromjson` (`jqrun.WithJQFromJSON`), jq 1.8.2's `fromjson` over the repo's
  own decoder (`jsonv.ParseOne`), because gojq's accepts a lone high surrogate
  escape (`"\ud83d"`) that jq refuses and refuses `-nan` that jq accepts. That
  patch closes the gojq-vs-jq `fromjson` differences, so they are not listed as
  not mirrored. `diff_test.go` pins it: it runs both programs on the real
  `jq -Rn` and on gojq over one corpus of line shapes (torn, non-object,
  trailing-garbage, surrogate-escape and NaN/Infinity lines and bodies) and
  compares.
  The row's `ts` is real time and never `CREW_CLOCK`: the arm stamped with a bare
  `jq -n 'now*1000|floor'`. The dedupe read (`lastPosted`) is the arm's
  `jq -r … | tail -1` for the sender's last `state<TAB>pr_url`, so an identical
  repeat post is swallowed; like jq it stops at the first unparseable line and
  the well-formed prefix decides. The pane publish after a successful append goes through `panestate`.
  `msg` carries its own recipient checks (empty id after the colon, a control
  byte in a `role:` body), then fits the line through `bus`.
- `crew/internal/register`: `crew register` and `crew deregister`, a dispatcher
  claiming and releasing its crew directory. The pid and pane files and
  `$dir/pidfile.log` are read by `crews`, `adopt` and dispatch, so their text is a
  contract: the pid is written verbatim, whatever the caller passed. Liveness,
  the owner-pid walk and the recycle check run through `crews.Probes`; the log
  line is `crews.PidfileLog`.
- `crew/internal/panestate`: the three `@crew_*` pane options
  `_publish_pane_state` set (state, detail cut to 40 characters, source), in that
  order. One copy shared by `stall` and `status`, so there is nothing to guard.
- `crew/internal/testjson`: test-only value-equal JSON comparison.
- `crew/internal/drift`: test-only drift-guard skip, fatal under `CREW_DRIFT_REQUIRED`.

New subcommands get an `internal/<sub>` package; shared reads go through `bus`;
folds that outgrew hand-translation run on jqrun.

## Delegation

crew.sh stays the entrypoint (direction b). A ported arm is:

```bash
crews | log | report | sessions | roster | identity | inbox | hold | await | watch | retro | reply | resolve-target | pr-watch | where | stream | roster-render | adopt | status | msg | register | deregister | fleet | rate)
  export CREW_SELF="$0"
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
```

`fleet` and `rate --sweep-all` need no repo, so the preamble's not-in-a-repo
refusal lets them through — and both ask the same question twice: the script with
its `case "$sub $*"` glob, the binary with `rate.WalksEveryRepo` / the `fleet` arm,
so a direct `crew-go` call decides the same way. Both read repo discovery from
`crew/internal/repos`, shared between them; the hidden `discover-repos` entry the
bash `rate --sweep-all` loop used to shell out to is gone now that the loop is Go.
Candidates are byte-sorted (Go `sort.Strings`) where bash `sort -u` used the
caller's locale collation, so `rate --sweep-all` may list repos in a different
order; the set is identical (behaviour, not bytes).

`CREW_SELF` is this script's own path, the `$0` the bash arms re-entered. Two
read it. `stream` reads it for its inner `watch`, its `hold due` pre-check and
`crew reap`, all three of which stayed in the script. `roster-render` reads it as
its own build identity: the daemon compares it against the installed `crew` and
`syscall.Exec`s that in place, so a home-manager switch reaches a daemon that
outlived the dispatch that started it. The script's path and not the Go binary's,
because a switch replaces the script and the binary it execs is read fresh each
time. And resolved **once**, at start — `CREW_SELF` names the entry, which is the
very symlink a switch repoints, so re-resolving it per tick follows the new build
and reads as "no hop needed" forever.
`TestDaemonFollowsTheInstalledEntryNotItsOwnStart` repoints a fixture entry under
a running loop to hold that.

`flake.nix` substitutes `@crewGoBin@` with the `crew-go` package's
`bin/crew-go`; `CREW_GO_BIN` overrides it for raw-source runs. The bash
preamble (`--help`, the git-repo check) runs first. Add a subcommand to this
arm and delete its old arm. `crew hold`'s per-action `--help` text lives only in
that preamble — it never reaches Go, and `help: crew hold <action> --help` pins
it. `rate` reaches it like every other ported subcommand: its flag loop, its
refusals and its `--sweep-all` loop moved to Go with #966, so the arm passes the
argv through and the package decides the mode.

`stall-watch` is a second shape: every invocation but the hidden `--sh` branch
execs Go, and that branch stays in bash.

```bash
stall-watch)
  if [ "${1:-}" = --sh ]; then …; exit $((rc + 10)); fi
  CREW_SH="$(readlink -f "$0")" exec "${CREW_GO_BIN:-@crewGoBin@}" stall-watch "$@"
  ;;
```

The helpers the loop needs (`_release_windows`,
budget-gate.sh, local-models.sh) have other bash callers, so Go calls back
instead of copying them: `CREW_SH stall-watch --sh <op> …` run directly when
`$CREW_SH` is executable (the Nix-built `crew`, which carries its pinned bash
and `set -euo pipefail`), otherwise `bash -euo pipefail "$CREW_SH" stall-watch
--sh <op> …` (a raw-source run) — the self-re-exec form crew.sh already uses
(`bash -euo pipefail "$0" reap …`), which runs the helpers under the arm's own
preamble. The child's cwd is the repo's git common dir, as the roster renderer
`cd`s there: the worktree the watchdog started in may be reaped while it lives,
and the preamble would then refuse. Each op is the arm's old call site,
errexit-suppressed as the arm suppressed it, then exits its verdict + 10, so
the child's own failures — the preamble's exit 1, 127, a signal (-1 to Go) —
never read as a verdict: Go decodes 10–125 as verdict rc−10 and anything else
as a helper that failed to run. The environment is inherited; Go strips
trailing newlines from stdout. An unknown op or budget predicate is one `crew:
stall-watch: unknown --sh …` line, exit 1.

| op            | argv after `--sh <op>`                          | stdout                           | verdict (exit − 10)          | helper failed       |
| ------------- | ----------------------------------------------- | -------------------------------- | ---------------------------- | ------------------- |
| `release`     | `<branch> <session\|-> <state> <ts_ms> <grace>` | none                             | 3 keep watching; else exit 0 | keep watching, as 3 |
| `budget`      | `<windows\|limit> <cache> <engine> <now>`       | `_budget_*`'s TSV                | 0 hit, 1 clear, 2 can't tell | can't tell (2)      |
| `local-model` | `<model>`                                       | non-empty iff the model is local | 0                            | D8 stays on         |

The op supplies what the arm supplied itself: `release` appends the empty
dry-run argument to `_release_windows` (after no-op `say`/`note`). `budget` and
`local-model` source `${BUDGET_GATE_LIB:-@budgetGateLib@}` and
`${LOCAL_MODELS_LIB:-@localModelsLib@}`; `local-model` also runs
`${DISPATCH_CONFIG_BIN:-dispatch-config}`.

`nudge` is the `stall-watch` shape without the `--sh` branch: every invocation
execs Go, and since #968 the arm exports nothing — the one helper it called
back for, `_unread_scan`, moved in process to `internal/unread`, which is what
tells the nudge which directive it is waking the lead for.

## Output contract

Idiomatic Go, behaviour not bytes (#821). **Exact:** exit codes, usage and
refusal lines, and the text people or agents read (the tables, the one-line
errors). **Value-equal only:** JSON — key order, indentation and colour are
free, compare with `jq -S`; emit through `jsonv`, never `encoding/json`.
**Not mirrored, not documented:** incidental jq/bash quirks — the
`$JQ_COLORS` warning count, TTY colouring, jq's own error wording, and the
rewrites `$(...)`/`cut`/`awk` apply to strings.

Rules that still bind a porter:

- Run the arm's jq program verbatim through jqrun, documenting each patch in
  the file header (the sessions `capture` anchor: Go regexp `$` is end-of-text,
  Oniguruma's also matches before a trailing newline, so the embedded copy uses
  `\n?\z`). Editing a `.jq` file edits the fold.
- Hand-ported remainders keep jq op-for-op semantics: stable sorts, `//` treats
  `false` as absent, `from_entries` last wins, word-split guards.
- A bus on which the fold fails exits 5 (corrupt) or 2 (unreadable) with one
  `crew: <sub>: <log>: …` stderr line and otherwise empty stdout.
- gojq's one known divergence (spike #861): NaN sorts below every value in
  `sort_by` where jq treats it as equal — unreachable, the bus writes
  `now*1000` ts. Everything else is value- and literal-exact.
- External calls: `tmux` and `git worktree list` keep argv, count and order.
  Equivalent git queries may differ: Go runs `git -C <cwd>` and repeats the
  preamble's common-dir lookup.
- `crew hold`'s appended rows are the exception to value identity: a row's
  `body` is a string, so its key order is part of the value a later reader
  compares, and those writes stay byte-exact in the arm's construction order.
- Specific to `stall-watch`, and not mirrored:
  - bash's `IFS=$'\t' read` collapsed an empty `.body.source`, so the row's
    `detail` slid into `bus_source`; Go reads the fields by position;
  - the `bash -x` trace: the one bats row that counted `_bus_refresh` lines under
    it is a Go test counting bus reads through the injected reader;
  - arithmetic on non-integer numeric flags, which the arm fed to `$((…))` and
    crashed on; Go refuses the value with one line, the refusal `--release` and
    `--budget-refresh` already had;
  - `cksum`: Go compares the frame text, and equal text is equal hash for every
    use the arm made of it;
  - `--release 00`: bash tests `[ "$release" = 0 ]` as a string, so `00` kept the
    release loop on with a zero grace; Go parses the integer, so `00` is off like
    `0`.
- Specific to `adopt`, and not mirrored: `_occupants`' third read,
  `tmux list-panes -a -F '#{window_id}\t#{pane_id}\t#{pane_current_command}'`. Its rows
  fill only the advisory `engine`/`panes` fields of the JSON the helper prints, and
  `adopt` asks only whether that array is empty — which the window list alone
  answers — so the read would run for nothing. The two reads that decide it keep
  their argv, count and order.
- Specific to `pr-watch`, and not mirrored: two places the port is not the arm.
  A `SIGTERM`/`SIGINT`/`SIGHUP` sent to `crew` is forwarded to the child and the wait
  goes on until the child is gone, exiting with the child's status and posting nothing
  — the arm inherited the signal and died around `pr-watch`, leaving it polling
  GitHub. A signal this process inherited as ignored stays ignored (a park under
  `nohup`, or a non-interactive bash `&` job, survived those for the arm), and the
  first forwarded signal unregisters the handler, so a second one takes the default
  disposition instead of vanishing into a channel nobody reads. And a `pr-watch`
  missing from `PATH` ends on Go's own exec error line where bash printed
  `pr-watch: command not found`; the 127 stays.
- Specific to `status`, `msg`, `register` and `deregister`, and not mirrored:
  - Oniguruma's retry limit: a pathological but well-formed ledger makes jq fail
    the match and the arm refuse, where the hand-parser has no limit and accepts.
    The one stated exception that refuses less; unreachable at bus line sizes;
  - `could not check the acceptance ledger (jq exit N)` and `could not parse the
acceptance ledger items`: no jq is left to fail, so the lines are unreachable;
  - `sed`/`grep`/`awk`'s own `Permission denied` lines before the refusal on an
    unreadable task doc; Go prints the refusal alone (the exit and the refusal
    line stay);
  - `mkdir`'s own error text when the bus dir cannot be created; Go prints
    `crew: <sub>: <error>` and the same status;
  - an id run through `awk -v` that unescapes to invalid UTF-8 (`\xff`) is compared
    as U+FFFD runes, and awk's warning about an unknown escape is not printed;
  - `[:alnum:]` follows each engine's Unicode tables (bash's `=~` on glibc 2.42 is
    Unicode 16, Go is 15, gawk's regex lags both): the CI test and the
    run-evidence boundary take the side that refuses more, and the id lookup
    reads an id followed by a character gawk may not call alnum both ways (the
    item is CI if the entry or its number or position fallback names CI), so it
    refuses at least as often as the arm.

  gojq-vs-jq `fromjson` differences in the seam folds are not on this list: the
  jq-compatible `_jqfromjson` patch closes them and the differential test in
  `internal/status` pins that, rather than documenting them.

- Specific to `nudge`, and not mirrored: two places bash's own mechanics set the
  behaviour. The arm called `_unread_scan` in its own shell, so a scan that could
  not run died under `set -e` with no message; Go used to answer it with `crew: nudge: cannot scan
the bus for unread msgs (unread helper failed)` at exit 1, because the empty-scan
  refusal (exit 2, "post the directive first") is a different answer and a
  dispatcher acts on it differently; since #968 the scan runs in process and
  cannot fail to run, so that exit went with the call-out. And bash suppresses errexit inside a `$(…)` on
  the left of `||`, so `out=$(_nudge_pane …) || rc=$?` swallowed a `nudge` row the
  bus would not take and printed the outcome anyway; Go keeps that — the pane really
  was typed, and the `held`/`unconfirmed` line telling a human to clear it is the
  point of exit 3 — and adds a stderr line naming the row it could not write,
  which stdout cannot carry without breaking the outcome line. A `nudge_wait` row is a
  plain statement in the arm, so its write failure does reach `set -e`: exit 1,
  nothing printed.
- Before deleting a bash arm, diff it against Go over a generated corpus and
  keep the evidence: compare exit status, human/agent text and JSON values
  (`jq -S`) — nothing else.

## Bash helpers that stay

`_crew_id`, `_is_engine_cmd` and `_pane_is_engine_at` keep other callers (`reap`).
Delete each only with its last caller. While two copies exist, guard drift:

- Go tests parse crew.sh's pools and engine table and assert the Go copies
  match (skipped when crew.sh is absent, as in the Nix sandbox; CI runs them
  required — see Running the suite).

The `_identity*` helpers lost their last caller with the `identity` arm (#981) and
are gone; `crew/internal/identity` is now the only picker of a codename. The three
pools stay: no bash caller reads them either, but they are the canonical copy —
crew.bats's `_rr_palette` enumerates `_colors` from here to compile every palette
stroke through d2, and `identity_test.go` asserts the Go pools against all three,
which is what keeps a slot from being reordered under a recorded identity.

`_sessions` and `_resolve_target` lost their last caller with the `nudge` arm and
are gone, and their drift guards went with them: `crew sessions` and
`crew resolve-target` are now the only implementations, and `nudge` reaches both
through them (`sessions.Fold` for the newest-session gate, `resolve.Rows` for its
"ambiguous target … pass a branch" refusal, which `crew resolve-target` cannot
print because it exits 2 with an empty stdout there).

`_await_state`, `_await_marks` and `_await_record` stay for the crew.bats
guards that replay Go's writes through them, even
though neither `crew await` nor `crew inbox` calls them any more: both write it
from `internal/marks`, and `internal/unread` has read it since #968. Two of the three drift guards above apply — crew.bats
compares the Go marks file with `_await_state`'s path and `_await_record`'s
content on the same msgs, and `internal/marks` runs the helpers' own two programs
through jqrun — and one more read the other way: crew.bats hands the marks a Go
`await` raised to `crew nudge`, whose scan is `internal/unread`'s (the bash
`_unread_scan` replay guards moved into that package's tests with #968).

`_clock_now` and `_clock_now_f` stay for `roster-render` and the `--sh` ops, and
`internal/clock` is their Go copy for `hold`, `await`, `stall-watch` and `nudge`;
`_clock_now_ms` and `_clock_sleep` lost their last caller with the `nudge` arm.
The guard is that both sides read and write one file: the suite
exports `CREW_CLOCK`, so the clock a Go `await` or `stall-watch` seeds is the
file a Go `nudge --wait` advances, and `crew clock: unset, await and hold still use
real time` pins the no-`CREW_CLOCK` branch. `watch` is deliberately outside this
pair: its park runs on `internal/clock`'s real half only, because the arm's
`jq -nc 'now*1000|floor'` and its `sleep` never consulted the clock file.

`_lock_acquire` and `_lock_release` stay for the two bash-only locks until `reap` is
ported, and `internal/lock` no longer copies them: it holds a `flock(2)` and reads
a `*.d` only as the record it publishes (or, for an old build's, as the old format).
Which lock paths either language takes, from `grep -rn '_lock_acquire\|lock\.d\|refresh\.d\|nudge-wait'`
over the non-test code:

| lock path                                  | bash (`adapters/core/crew.sh`) | Go                           |
| ------------------------------------------ | ------------------------------ | ---------------------------- |
| `$XDG_DATA_HOME/crew/ratings.sweep.lock.d` | `_rate_autosweep` (in `reap`)  | none                         |
| `<crew dir>/reap.lock.d`                   | `reap`                         | none                         |
| `$XDG_DATA_HOME/crew/ratings.lock.d`       | none                           | `rate` sweep                 |
| `<crew dir>/watch.lock.d`                  | none                           | `watch`                      |
| `<crew dir or lane>/stream.lock.d`         | none                           | `stream`, fleet lane         |
| `<crew dir>/roster-render.lock.d`          | none                           | `roster-render`              |
| `<bus dir>/nudge-wait/<sid>.<cksum>.d`     | none                           | `nudge --wait`               |
| `<budget file>.refresh.d`                  | none                           | `stall-watch` budget refresh |

No lock is taken by both bash and Go, so bash keeps its mkdir helper until `reap` is
ported, and neither a `crew lock` helper nor `flock(1)` (absent on macOS by default)
is needed. The sharing that remains is across builds, not languages: old-build
processes still running after a switch (an old `stream` re-entering its old
`CREW_SELF` for `watch`, old `stall-watch`es, an older installed `crew` whose `rate`
and `nudge` are bash arms) take the same paths with the mkdir protocol, and
`internal/lock`'s old-format branch handles them. Its own tests are the drift
guard: live, dead, empty, non-numeric, `0`, out-of-range and trailing-newline
holders against that branch, plus the `stream` rows that TERM a parked Go watch and
replace a `--force`'d holder.

The frame classifiers (`_frame_classifier`, `_pane_idle_reason`, `_claude_idle_box`,
`_box_rows`, the `_pi_*` helpers) stay in crew.sh for `reap`
and `_release_windows`; dispatch.sh's byte-identical copies are pinned by adapters.bats
and untouched. `internal/frame` is the Go copy `stall-watch` runs every tick, and
the `LC_ALL=C.UTF-8` drift test described under Layout is the guard.
`_is_quota_cursor_limit` has no bash caller left but stays, as the canonical copy
adapters.bats compares dispatch.sh's against. `_is_bg_wait` had no bash caller
left and no pinned copy, so it moved to Go and is gone from crew.sh.

The rest of what `stall-watch` reaches stays bash and is delegated, not copied
(one copy needs no guard):
`_release_windows` (with `reap`), and budget-gate.sh and local-models.sh, reached
through `--sh budget` and `--sh local-model`. `_unread_scan` was the last one
with two Go callers (`stall-watch` every 4th tick and `crew nudge` per call);
both read `internal/unread` since #968 and it is gone from crew.sh. One helper still has a Go copy and a
guard: `release_grace` stays for `reap`, and a Go test parses `^release_grace=` from
crew.sh and asserts it equals `--release`'s default (skipped when crew.sh is absent).
`_publish_pane_state` is gone with its guard: `panestate` is the one copy, shared by
`stall` and `status`.

`_hold_outstanding` lost its last bash caller when `roster-render` moved (#935)
and is gone: `hold.Outstanding` is the one implementation, reached in process by
the renderer's model and by `hold list` alike, so the guard that compared the two
copies went with the copy. `_hold_crew` and `_hold_render` were already gone.
`_fit_line`, `_shrink`, `_LINE_MAX`/`_ELIDED` and
their drift guards are gone with the `status`/`msg` port (#945): `bus` is the one
copy. `_bus_append` stays for `reap` and `_release_windows`, and `internal/bus.Append` is its Go copy.
`_owner_pid`, `_pidfile_log` and `_has_c0` are gone (`crews.Probes.OwnerPID`,
`crews.PidfileLog`, and `msg`'s own control-byte test). `_pid_alive`,
`_file_mtime_s`, `_ps_elapsed_s`, `_pid_recycled` and `_recorded_pid_live` have no
crew.sh caller left but stay, as the canonical copies adapters.bats pins
dispatch.sh and dispatch-resume.sh against (the same arrangement as
`_is_quota_cursor_limit`). `_crew_id` stays for the bash arms that resolve a
crew id.

## Running the suite

- `bats tests/...`: `tests/setup_suite.bash` builds `crew-go` from the working
  tree once per run and exports `CREW_GO_BIN`; a failed build fails the run.
- The bench harnesses shell into crew.sh from the checkout as well, so their
  entries export it too. `tests/harness/ensure-crew-go.sh` builds unless
  `CREW_GO_BIN` is already set: a bench adapter builds once and its per-case
  children inherit, keeping the build out of the measured per-case window.
- `cd crew && go test ./... && go vet ./... && golangci-lint run ./...`.
  `exhaustive` is on: keep `switch` on `Kind` and `State` complete.
- CI runs that same `go test ./...` from the checked-out repo with
  `CREW_DRIFT_REQUIRED=1`, so a drift guard that cannot read its bash file fails
  there instead of skipping (`nix build .#crew-go` still skips them: its sandbox
  holds only `./crew`).
- `scripts/bats-affected.sh` maps `crew/*` to the crew bats files.

## Flipping to direction (a)

1. The Go binary becomes the `crew` package: it runs ported subcommands and
   `syscall.Exec`s crew.sh (an internal `crew-sh`, path baked in via ldflags)
   for the rest.
2. The delegating arms in crew.sh disappear.
3. crew.sh's `$0` self-re-execs (`roster`, `dash`, `reap`, `hold`, `rate`) switch
   to the `CREW_SELF` env the Go front exports. `stream` already reads that env
   rather than a `$0` of its own — it re-enters the script for its inner park, so
   once the front exports `CREW_SELF` itself the port needs no further change.
4. `run_crew` and `CREW_REAL` in the bats files switch to the Go binary.
