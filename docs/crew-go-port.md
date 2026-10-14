# Porting crew to Go

Issue: #822. `adapters/core/crew.sh` moved to Go one subcommand at a time.
Ported so far: `log`, `report`, `sessions`, `roster`, `crews`, `inbox`, `hold`,
`await`, `retro`, `rate` (both modes: the per-repo sweep of #895 and the
`--report` rollup of #890), `reply`, `watch`, `resolve-target`, `where`,
`identity` (#981),
`stall-watch` (#832), `stream`, `adopt` (#920), `pr-watch` (#934), `roster-render`
(#935), `fleet` (#926), `status`, `msg`, `register` and `deregister` (#945), `nudge`
(#939), `git-baseline` (#993), which brings the git-config guard
(`worktree-git.sh`'s `_wt_cfg_*`) to Go as one shared implementation, and `reap` and
`occupants` (#997), and the entry section — `help`, `id`, `new`, `engine-cmd` and `dash`
(#1007) — and `pi-agent-dir` (#1016). #821 finished the direction flip: the Go
binary is what `crew` runs, and the script the arms left behind — the bash helpers
the drift guards still read, with no entry logic of its own — is deleted, its
helpers that are still live already carried by the scripts that call them (see
"What the bash scripts still carry").
Each slice must leave every bats file green;
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
  `roster` also owns the liveness rule of #999 — `Stale`, the flag `crew fleet`
  already stamped its rows with and hid unless `--all`, and `Drawn`, what the
  diagram keeps — because the two views have to agree on who is alive while each
  reads its own probes.
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
- `crew/internal/lock`: the lock under `ratings.lock.d`, `ratings.sweep.lock.d`,
  `reap.lock.d`, `watch.lock.d`, `stream.lock.d`, `roster-render.lock.d`, the `nudge --wait` dir and the budget refresh, as one package
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
  dispatch.sh is absent, as in the Nix sandbox. Since #821 the block it extracts
  is dispatch.sh's `--role-watch` one, the only bash copy that ships; the
  predicates with no bash copy (`_is_quota_prompt`, `_is_quota_session_limit`,
  `_pane_idle_reason`) are frozen in `testdata/golden-oracle.tsv`, and
  `_is_bg_wait`, which shipped nowhere else either, keeps its pre-port body in
  `testdata/is_bg_wait.bash` as the oracle.

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
  unchanged frame for `--release` with no prompt, and `reap.ReleaseWindows` verdict 3,
  or an error from it (the release row could not be written), keeps watching while any
  other verdict exits 0. Every bus write is
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
  release call (`releaseCall`, the same signal contract as `nudgeCall`) and the watchdog's
  own nudge and continue runs run on through a signal (for at most 2 minutes), as the
  arm's `$(…)` child outlived it, so a typed nudge or continue line or killed window
  still gets its bus row. D6 and D9 call
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
  `reap`, whose arm is now the delegation one-liner, so the re-entry runs Go. That keeps
  the arm's fault-injection rows honest and `crew reap` single-implemented, and it is why
  the port is a subprocess
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
  What reaches the renderer is the fold cut to `roster.Liveness` (#999): a running
  row that is not stale, and a finished row only while its worktree still waits
  for `crew reap` to take it — a crew that lives for days was stacking its whole
  history beside the one or two workers that were running. The cut runs on the
  folded rows before the model merges the pending dispatches, so a session
  launched and not yet posting stays drawn as `dispatched`, and the live count,
  the legend and the boxes all speak of the same rows. Fold and rule share one
  worktree probe per pass, anchored at the common dir rather than the launch
  directory, which a reap can take away under a running daemon.
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
- `crew/internal/gitguard`: the git-config guard of #539/#557/#678/#944 and the #993
  commondir pin, once, for every caller. It holds the exec and redirect key tables
  (`ExecKeys`, `RedirectKeys`) and their glob match (only `*`, matching any byte
  including `.` and `/`); the URL, key and pair masking; bash `%q` quoting (`quote.go`,
  C locale); the `#covers` marks; the local/worktree-scope pair listing from
  `git config --list --show-scope -z` and the union over the main checkout and every
  linked admin context; the legacy `remotes/`/`branches/` records; baseline init and its
  migrations; the guard (`Check`) and the cwd guard (`CheckCwd`, including the relative
  `core.hooksPath` equivalence); the commondir pin (`Pin`, `PinCwd`, `PinAdmin`); and the
  `git-baseline` listing and `--accept`. It is reached two ways: the hidden
  `crew-go git-guard <op>` that `worktree-git.sh`'s wrappers exec, and the public
  `crew git-baseline`. See "Shared guard" under Delegation.
- `crew/internal/wtgit`: the anchored git invocation of `worktree-git.sh` (#539/#557/#578/
  #633/#677) for Go callers: `Command` (`_wt_git`) and `CommonCommand` (`_wt_git_common`)
  return the unstarted `*exec.Cmd`, argv-identical to the bash helpers (`NeutralCfg` and
  `CommonExtra` in `_wt_neutral_cfg`'s order, `--attr-source` on the empty tree, `-C`,
  `--git-dir`, `--work-tree`), after the refusals and the in-process `gitguard.Check`;
  `Status`, `GitlinkOK`, `AdminDir` and `AnchorPath` are `_wt_status`, `_wt_gitlink_ok`,
  `_wt_admin_dir` and `_worktree_anchor_path`; `TrustedCwd` is `_wt_trusted_cwd`, returning
  the directory to run cwd-discovered git from instead of `cd`-ing (same refusal text, same
  `gitguard.PinCwd` calls before and after). One place the invocation is not the helper's:
  the command's environment is scrubbed of the variables that would re-point or extend an
  anchored git (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_COMMON_DIR`, `GIT_INDEX_FILE`, `GIT_CONFIG`,
  `GIT_CONFIG_PARAMETERS`, `GIT_CONFIG_COUNT` and the `GIT_CONFIG_KEY_*`/`_VALUE_*` pairs,
  `GIT_ATTR_SOURCE`, `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`), where bash
  inherits it, and `GIT_INDEX_FILE` is set only through `Opts.IndexFile` (the `--discard`
  scratch index); `GIT_CONFIG_GLOBAL`, `GIT_CONFIG_NOSYSTEM` and `HOME` pass through, since
  the suite isolates with them. The bash copy stays for dispatch, so a
  drift test parses `_wt_neutral_cfg=( … )` and the common extras from `worktree-git.sh` and
  asserts equality with the Go lists (skipped via `internal/drift` when the file is absent).
- `crew/internal/reap`: the `reap` arm (#997) and `occupants`, plus `_release_windows`. The
  arm runs op-for-op in the arm's order, with its jq folds (`latest.jq`, `idle.jq`,
  `claimed.jq`, `posted.jq`, `findpr.jq`, the forget-pairs fold and `human.jq`, the
  transcript filter) on jqrun, each patch documented in its header. Every side effect is a
  seam of `Probes` (tmux, gh, gtrash, process enumeration through `CREW_REAP_PROC_CMD`,
  ancestors, kill, bus append, sleep and clock), so the arm is testable without tmux; the
  release path's pane captures go through `stall/probe`'s `CREW_STALL_SAMPLE_CMD` and
  `_COLOR_CMD` seams and its inter-sample sleep through `CREW_RELEASE_GAP`, while the
  candidate loop's idle check calls `tmux capture-pane` directly, as the arm did. `reap`
  records the caller's cwd (it still decides "this is the current worktree"), resolves
  `wtgit.TrustedCwd` and `chdir`s there once, where the arm `cd`'d, so every cwd-discovered
  child (`git config`, `git worktree list`, every `gh`, `gtrash`, the autosweep) runs from
  the trusted dir. `ReleaseWindows` is the one release implementation, shared in process
  with `stall-watch` (see Delegation); `Occupants` is the one `_occupants`, used by the
  release, the arm and `crew occupants`. The ratings autosweep is `CREW_RATE_AUTOSWEEP`'s
  `sync` (take `ratings.sweep.lock.d`, run `rate` in process) or the default async: a hidden
  `crew-go reap-autosweep` child (not on the public usage, like `git-guard`) with stdin
  `/dev/null`, output appended to `autosweep.log`, SIGHUP ignored, and the log trimmed in
  place to its last 200 lines. `reap` takes `reap.lock.d` through `internal/lock`. SIGTERM,
  SIGINT and SIGHUP, each only when not inherited ignored, remove the `--discard` temp dir
  and release the locks, then exit 128+signo, as the arm's EXIT trap did.
- `crew/internal/stack`: `crew stack cut` and `crew stack align` (#1004). One argv
  parser keeps the arm's usage lines, refusals and exit codes, and one record fold
  (`newest.jq`, the arm's own program, patched as its header documents) answers both of
  its questions about the bus: `cut` reads the newest complete record for a branch, and
  `align` folds every branch in the stack in a single pass (`record.jq`, keyed by branch).
  `align`'s walk is the arm's: gh resolves each PR, the recorded parent overrides the PR
  base only where the arm let it, and each layer comes back `ff`, `onto` or `unknown` with
  the directive a worker runs verbatim. Every outside call is a `Probes` seam (git through
  `internal/wtgit`'s anchored invocation, gh, the clock, the bus append), so the tests
  drive the whole command against a fixture repo and a gh stub — including the row-level
  assertions the bats rows make, which the port keeps as the contract.
- `crew/internal/entry`: the arms that ran ahead of the repo check (#1007) —
  `help`, `id`, `new`, `engine-cmd` and `dash`. One package because they are one
  thing in the script: the entry section, every arm of which execs the binary
  before `bus.Locate` is called, so none of them may read the bus. `help` is the
  651 lines of `_crew_help`'s heredocs, extracted out of the script mechanically
  and embedded one file per screen (`texts/`, `texts/hold/`); the routing that
  decides _when_ help answers — `--help` as the command, as the first argument of
  one, `hold <action> --help`, `fleet stream --help`, and never a third position
  (#812) — stays in crew.sh, because that precedence is the preamble's own
  contract. `id` is `bus.CrewID` behind the arm's refusal; `new` is
  `date +%s`-`$$`, and the pid is this process's because the arm execs it; `engine-cmd`
  is `roster.IsEngineCmd`, the table `crew roster` already read; `dash` resolves
  `CREW_SELF` into `CREW_BIN` and `syscall.Exec`s `crew-dash` from PATH.
  Byte identity is `scripts/help-parity.sh`: it reads the pre-change crew.sh out
  of git, runs every help invocation AC1 of #1007 names against both sides, and
  diffs stdout, stderr and the exit status. No in-tree drift guard can hold that
  link, because this is the change that deleted the script's copy.
- `crew/internal/piagentdir`: `crew pi-agent-dir` (#1016), the seeder of the shared
  `PI_CODING_AGENT_DIR` every pi worker runs with — never-trust `settings.json`, the
  hookyard bridge and its `extensions` entry, the `auth.json` link (or `{}`), the
  `models-store.json` copy and the dispatcher-owned `models.json`. It is trust-boundary
  code, so every refusal (symlinked or non-directory worker dir, a worker dir and an
  ambient dir that resolve inside one another, a non-array `extensions`, an `auth.json`
  that is not a reachable regular file or not a JSON object, a directory where the link
  goes, a `localModels` provider with a stored credential) keeps the arm's text and
  exit 1, and each file keeps its mode (`settings.json`/`models.json`/copies 0644, a
  seeded `auth.json` 0600, the dir 0700). Writes are the arm's same-dir `.seed.XXXXXX`
  temp plus rename, skipped when the content already matches, so a re-seed rewrites
  nothing. The two jq programs run on jqrun: the clash check is the arm's, and
  `models.jq` is `_local_pi_models_json` from `local-models.sh` verbatim, which a drift
  test pins because dispatch and refresh-budget still source that lib. Like the entry
  arms it execs ahead of the repo check. Not mirrored: the arm sourced
  `local-models.sh` first, so a missing lib failed the seed; Go embeds the program and
  does not read the lib. `scripts/pi-agent-dir-parity.sh` runs the pre-port arm and Go
  over the same fixture homes and compares stdout, stderr, exit status and the trees.
- `crew/internal/testjson`: test-only value-equal JSON comparison.
- `crew/internal/drift`: test-only drift-guard skip, fatal under `CREW_DRIFT_REQUIRED`.

New subcommands get an `internal/<sub>` package; shared reads go through `bus`;
folds that outgrew hand-translation run on jqrun.

## Entrypoint

The `crew` command is the Go binary (#821): `flake.nix` builds `crewGo` and installs
its `bin/crew-go` as `crew` behind a PATH wrapper, so the tools the old
`writeShellApplication` preamble prepended for crew.sh — gh, gtrash, crew-dash,
dispatch-config, refresh-budget, pr-watch — are still there for the subcommands that
shell out. `--inherit-argv0` is load-bearing: argv[0] is what `CREW_SELF` is derived
from, and `CREW_SELF` must name the entry a switch repoints, not the store path the
wrapper execs. `crew-go` remains its own package (`worktree-git.sh` bakes it, the
tests build it).

Routing lives in `main.go`: `routeHelp` for `--help` anywhere in the argv and
`crew help <sub> [<action>]`, then the kong grammar (`main.go`'s `subcommands`
literal) that selects the subcommand, and an input kong does not select prints the
grouped list to **stderr** and exits 1 — the script's `*)` arm, byte for byte
(`scripts/help-parity.sh` covers the bare and unknown cases). There is no bash
`case` left to add an arm to; a new subcommand is a name in that literal plus its
help entry, and `tests/crew.bats`'s `_crew_cli_subs` reads the list from `main.go`.

### kong, the root parser (#862)

`main.go` builds one kong parser over `os.Args[1:]`. The conversion is complete:
every subcommand except `dash` is a typed command. The first slice took `id`,
`new`, `engine-cmd`, `occupants` (argv words); the second `where`, `identity`,
`resolve-target`, `crews`, `register`, `deregister`, `log`, `sessions`, `inbox`
(typed flags and positionals, in `crew/commands.go`); the third `roster`,
`report`, `retro`, `rate`, `fleet`, `stack`, `roster-render` and `pi-agent-dir`;
the fourth `msg`, `status`, `reply`, `hold`, `nudge`, `await`, `watch` and
`stream` (with `fleet stream`); the fifth `reap`, `reap-autosweep`, `adopt`,
`pr-watch`, `stall-watch`, `git-baseline`, `git-guard` and `help`. `dash` is the
one dynamic command, with a `passthrough:""` capture, so kong accepts the word
and stops; it stays that way for good (see below). The first slice converted only
the subcommands whose whole argv is "a list of words, the first of which may be
data", and the rules below are what the rest of #862 converted against.

**Help stays embedded.** `routeHelp` runs first, and `kong.NoDefaultHelp()`,
`kong.Writers(io.Discard, io.Discard)` and a do-nothing `kong.Exit` mean kong can
neither print a screen nor end the process. `helpTexts` is still the source of
every screen, which is what `scripts/help-parity.sh` proves.

**`dash` parses the raw tail, not kong's capture.** The capture is not verbatim:
kong splits the first word after a command name at `=`, so `crew dash --since=abc`
would arrive as `--since abc`. `dash` forwards its raw argv to `crew-dash`, which
is why it is the one passthrough left for good; `parseCrew` returns the words
after the command name for exactly that reason, and `TestParseCrewSplitsLeadingEquals`
pins the measurement on `dash`. A subcommand takes its own bound fields only once
its table shows the split cannot change its answer: `engine-cmd` matches no engine
name on either half of `--pane=x`, and `occupants` matches no worktree path.

**A parse error is the grouped list.** A name kong does not select — no words at
all, `bogus`, a flag where a name belongs — is answered by `run` with
`entry.RunHelp(nil, stderr, …)` and exit 1, and none of kong's own text reaches
the caller. The one input kong selects and the switch never admitted is a leading
`--`: kong swallows the terminator and matches the word after it, so
`crew -- status w1 working` would reach no arm and exit 0 silently. `parseCrew`
refuses any first word that is not an admitted name, which is that refusal and
exit 1.

**Flags: `--flag=value` is accepted (owner decision, 2026-10-14).** The typed
subcommands bind flags, so `--flag value` and `--flag=value`
are one spelling (`TestFlagSpellingsAreIdentical` runs both for every flag), flags
may sit in any position, and no guard refuses the `=` form. The measured
consequences against the first-slice build: `crew where --crew=x nova` is a lookup,
`crew identity --hash=x` is the hash form, `crews --mine=yes` is `--mine`, `--`
is swallowed (`crew where -- nova` succeeds) and a single-dash word (`-x`) is an
unknown flag. `kong.WithHyphenPrefixedParameters(true)` keeps `--crew --bogus` a
value as the arms had it, and binds a trailing `--crew` to `""`, so a flag whose
empty or missing value is a refusal is a `*string` checked after the parse.
Refusals keep the arms' wording and exit status (`kongRefusal` maps kong's
`unknown flag ` / `unexpected argument ` text per subcommand; pinned by
`TestKongErrorClassification`); a refusal prints after the not-in-a-repo check,
as the arms' did. Extra positionals now refuse where `identity`, `log`,
`register`, `deregister`, `crews` and `inbox` ignored or last-won them (an empty
word included: `crew crews ""` is a refusal, exit 64, where it printed the table). The
grammar's first error wins, not the arms' leftmost one.

The third slice (`roster`, `report`, `retro`, `rate`, `fleet`, `stack`,
`roster-render`, `pi-agent-dir`) follows the same rules, with these differences:

- `rate --root` is a `[]*string` with `sep:"none"` (a comma stays in the path, an
  empty value binds and is refused). A trailing `--root` binds nothing, so
  `kongRefusal` maps kong's `--root: missing value` to the arm's
  `rate --root needs a directory`. The mode combinations (`--json` without
  `--report`, `--root` without `--sweep-all`) stay in `rate.Run`, shared with
  `reap`'s in-process sweep.
- `fleet` binds `Rest` with `passthrough:"partial"`: `fleet stream …` reached
  `stream.RunFleet` untouched, because the lane's flags were `stream`'s shared
  parser; the fourth slice types that lane (below). Any other word, or `stream`
  behind a `fleet` flag, is the fleet usage line.
- `stack cut|align` are kong subcommands; every grammar failure is the two-line
  stack usage, and `--record` may sit anywhere after `cut`.
- `roster-render` keeps `--interval`/`--quiet` as the caller's bytes (they are
  handed to the detached daemon); its digit, crew-required, `--once`/`--detach`
  and crew-id checks stay in `rosterrender.check`. Its refusals exit 64.
- `dash` stays a passthrough: it forwards raw argv to `crew-dash`, and kong's
  split of the first word at `=` would alter it.
- The refusals of `fleet` and `rate --sweep-all` print before the repo check, as
  those modes run outside one. A kong parse error carries no typed value, so
  `parseCrew` keeps the raw tail and `run` treats a refused rate line with a
  `--sweep-all…` word as the mode. `rate --root --sweep-all` is such a line (kong
  does not take the flag as `--root`'s value): it prints the rate usage line,
  where the arm said `--root needs --sweep-all`.

The fourth slice (`msg`, `status`, `reply`, `hold`, `nudge`, `await`, `watch`,
`stream`, `fleet stream`) follows the same rules, with these differences:

- `msg` binds its words with `passthrough:"all"`: from, to and body are data,
  flag-shaped included (`msg a b -x` posts `-x`); words past the third are ignored
  as before; only a leading `--a=b` from is split by kong.
- `status` binds `<from> <state> [detail] [pr]` positionally beside `--restamp`: a
  detail that starts with any dash, single included, now needs `--` (was: only
  `--`-led ones); a fifth word refuses; the unknown-flag line keeps the `use --`
  hint. `reply` binds `<to> <body>` and `--crew` anywhere (it is documented after
  the body), so a dash-led body needs `--` too (was: posted); a third word refuses (was
  ignored) and the unknown-flag line carries the `use --` hint.
- `hold`'s actions are kong subcommands; each action's grammar fault prints that
  action's usage line, anything else `crew: hold add|list|due|park|release`.
  `hold add`'s title is `passthrough:"partial"`: from the first title word on,
  every word is title, as the arm's `break` had it; a leading `--` is still
  refused. `hold park -5` and `hold park abc --bogus` now print the park usage
  line (kong reads `-5` as a flag) where the arm printed `must be a positive
integer number of seconds`.
- `nudge --wait` is a `*string`: a bare trailing `--wait` is 1800 s, a non-digit
  value with no target becomes the target (the arm's `--wait nova`), with a target
  bound it is the `one target only` refusal. `--wait --crew c nova` binds `--crew`
  as the value (was: default wait).
- `stream` and `fleet stream` share one embedded flag struct; `fleet`'s table flags
  moved onto a hidden default subcommand named `-table` (a dash-led name no word
  can select; on the parent they would clash with the lane's `--crew`), and a lane
  selected behind a table flag is the fleet usage line. `fleet stream`'s refusals
  still print before the repo check; `--reap-every` is a hidden flag whose
  presence is the arm's refusal.
- `kongRefusal`/`validate` are keyed by `hold <action>` / `fleet stream`
  (`refusalKey`), so each action keeps its own usage line.
- Grammar refusals print in `run` before the package runs, so they now precede the
  checks the arms ran first: `begin`'s crew check (`status`, `msg`), `await`'s crew
  check, `watch`'s bus mkdir, and `nudge`'s dispatcher-only refusal (exit 2) for a
  worker passing a bad flag. No write happens on any of those paths; the sender
  check itself did not move.

The fifth slice (`reap`, `reap-autosweep`, `adopt`, `pr-watch`, `stall-watch`,
`git-baseline`, `git-guard`, `help`) follows the same rules, with these differences:

- `git-guard`'s ops are kong subcommands; every grammar failure is the guard usage
  line, exit 64, printed before `bus.Locate` and before any git call. A dash-led
  value (`show-key --x=y`, `show-pair k -v`) is now that refusal, where it was
  printed.
- `git-baseline --accept` is a bool: `=true|yes|1` reach the same tty and typed-`yes`
  gate in `RunBaseline`, and `=false` lists. Its refusal prints before `bus.Locate`.
- `reap`'s refusals (needs-a-value, digits, unknown/extra as `reap takes … (got
'…')`) print after `bus.Locate` and before the config guard; `--idle=5` is
  accepted, and `--bogus=x` is named `--bogus`. `reap-autosweep` binds its words
  and drops them.
- `adopt` binds `--force`, the id and the pid; a dash-led word is an unknown flag
  (it was read as the crew id) and a third word is refused (it was ignored).
- `pr-watch` binds `--repo`, `--timeout`, `--interval` and the PR, and rebuilds the
  child argv PR-first, so `--repo` may precede the PR. An empty or trailing value is
  forwarded and the child's own refusal answers; unknown and extra words use the
  child's `pr-watch: unknown arg` wording, now printed by crew before the child
  runs and before the missing-crew refusal.
- `stall-watch`: the sender/pane check (`sender.Watch` in `stall.Run`) still compares
  the same id and pane at the same point, and flags may now precede the id. Kong's
  unknown and extra refusals print before the missing-crew refusal (still before any
  bus write); an empty or trailing `--engine`, `--grace`, `--interval` is `--<flag>
needs a value`, checked in `stall.parseArgs` after the crew.
  `TestStallWatchSenderCheckSurvivesTyping` and the #908 bats rows pin it.
- A bad bool value (`reap --quiet=maybe`, `adopt --force=maybe`, `stall-watch
--no-budget=maybe`) now gets kong's bool line under the sub's name, as
  `status --restamp=nope` did in slice 4.

`fleet` and `rate --sweep-all` need no repo, so the preamble's not-in-a-repo
refusal lets them through — and both ask the same question twice: the script with
its `case "$sub $*"` glob, the binary with the typed `SweepAll` / the `fleet` arm,
so a direct `crew-go` call decides the same way. Both read repo discovery from
`crew/internal/repos`, shared between them; the hidden `discover-repos` entry the
bash `rate --sweep-all` loop used to shell out to is gone now that the loop is Go.
Candidates are byte-sorted (Go `sort.Strings`) where bash `sort -u` used the
caller's locale collation, so `rate --sweep-all` may list repos in a different
order; the set is identical (behaviour, not bytes).

`crew stack cut|align` is Go (#1004) and joins the one-liner arm after the repo check;
its `--help` reaches Go as `help stack`, as every ported subcommand's does since #1007.
`cut` is the one
of the two that writes: one `stack_cut` row through `bus.Append`, the record of a parent
oid a child is based on. Its reader is the same fold that reads the `dispatch` row
`dispatch` appends when it creates the child — the newest of the two wins — and nothing
else in the repo reads `stack_cut`, so the row is compatible in _value_ (jsonv parses it)
rather than in bytes. What is read verbatim is the other side: the oid `cut` prints and
the refusal text, which the dispatcher quotes into a rebase directive.

`crew occupants <worktree-path>` needs no repo either, and `reap` is ported, so the
script's pre-repo preamble branch for `occupants` is now `exec "${CREW_GO_BIN:-@crewGoBin@}"
occupants "$@"` and the bash `_occupants` is gone. `occupants` is dispatched before
`bus.Locate`, like `git-guard`; `reap` joins the one-liner arm after the repo check, and
`crew reap --help` reaches Go as `help reap`.

The four arms of #1007 sit where they always did, ahead of the list arm, and each is
the one-liner form: `exec "${CREW_GO_BIN:-@crewGoBin@}" <sub> …`. `id` and `new` pass
no arguments, because the bash arms read none; `engine-cmd` passes `${1:-}`, which is how
the empty-argument refusal survives the exec; `dash` prefixes `CREW_SELF="$0"`, and the Go side readlinks it into `CREW_BIN` — the one
place the entry path is resolved, because `crew-dash` execs it or runs it through bash and
needs a real file, not a symlink a switch has since repointed.

`CREW_SELF` names the entry — the path the arms used to re-enter as `$0`. Since
#821 nothing exports it on the way in: `entry.EnsureSelf` derives it once at start
from `argv[0]` when the caller left it unset (the shell hands a PATH-resolved
command its own full path, so that is the profile path a home-manager switch
repoints) and otherwise keeps the inherited value exactly as it is — a caller that
names the entry it wants outranks anything derived here, which is what the arm's
`CREW_SELF="$0" exec` meant for the process it replaced. Two read it. `stream`
reads it for its inner `watch`, its `hold due` pre-check and `crew reap`, all three
of which it re-enters through `entry.EntryArgv`, which names the entry directly —
`children.bash` and `Options.Bash` went with the script, and so did the bash prefix
the script put in front of a `self` without an exec bit: running `crew.sh` that way
would exit 0 having done nothing, where naming it leaves the child's 126.
`roster-render` reads it as
its own build identity: the daemon compares it against the installed `crew` and
`syscall.Exec`s that in place, so a home-manager switch reaches a daemon that
outlived the dispatch that started it. The entry's path and not the binary's under
it — the `crew` package is that binary behind a `makeWrapper --inherit-argv0`
wrapper, and the wrapper's path is what a switch replaces while the binary it execs
is read fresh each time. And resolved **once**, at start — `CREW_SELF` names the entry, which is the
very symlink a switch repoints, so re-resolving it per tick follows the new build
and reads as "no hop needed" forever.
`TestDaemonFollowsTheInstalledEntryNotItsOwnStart` repoints a fixture entry under
a running loop to hold that.

`crew-go` is the binary the tests and the harnesses run; `CREW_GO_BIN` names it for
raw-source runs, and it is no longer an indirection the entry consults — the entry
is that file. The git-repo check is Go's (`bus.Locate`, `crew:
not in a git repo`, exit 1, with `fleet` and `rate --sweep-all` exempt). `crew hold`'s
per-action `--help` screens are Go like everything else (#1007): the routing that
turns `crew hold due --help` into `help hold due` is the entry's, and
`help: hold due --help prints that action's screen, not the flag's` pins that it
does. `rate` reaches it like every other subcommand: its flag loop, its
refusals and its `--sweep-all` loop moved to Go with #966, so the arm passes the
argv through and the package decides the mode.

`stall-watch` joined that switch like the rest: the `--sh` branch and `CREW_SH` are
gone, and with #821 `crew.sh` holds no `case` arm at all — what remains is the
oracle helper functions the drift guards and adapters.bats read, and nothing that
dispatches a subcommand.

The helpers the loop still needs (budget-gate.sh, local-models.sh) have other bash
callers, so Go sources them instead of copying them. The finished-worker release is not
among them: `reap.ReleaseWindows` runs in process (`Options.Release`, wired in `main.go`
with silent `say`/`note` and no dry run).
A call-out is `probe.Probes.Sh(op, args…)`, run as `<bash> -euo pipefail -c <script> _
<op> <lib> <args…>`: `bashBin` is the Nix build's pinned bash (`-X main.bashBin`, `bash`
on PATH otherwise), and `<lib>` is `budgetGateLib` for `budget` and `localModelsLib` for
`local-model`. The flake bakes both with `-X`; `BUDGET_GATE_LIB` and `LOCAL_MODELS_LIB`
override them, as for `dispatch.sh`. The child's cwd is the repo's git common dir, as the
roster renderer `cd`s there: the worktree the watchdog started in may be reaped while it
lives. Each op is the arm's old call site, errexit-suppressed as the arm suppressed it,
then exits its verdict + 10, so the child's own failures — an unreadable lib (exit 1),
127, a signal (-1 to Go) — never read as a verdict: Go decodes 10–125 as verdict rc−10 and
anything else as a helper that failed to run. The environment is inherited; Go strips
trailing newlines from stdout. An unknown op or budget predicate is one `crew:
stall-watch: unknown --sh …` line, exit 1.

| op            | argv after `<op>`                         | stdout                           | verdict (exit − 10)          | helper failed  |
| ------------- | ----------------------------------------- | -------------------------------- | ---------------------------- | -------------- |
| `budget`      | `<windows\|limit> <cache> <engine> <now>` | `_budget_*`'s TSV                | 0 hit, 1 clear, 2 can't tell | can't tell (2) |
| `local-model` | `<model>`                                 | non-empty iff the model is local | 0                            | D8 stays on    |

`local-model` also runs `${DISPATCH_CONFIG_BIN:-dispatch-config}`.

`nudge` execs Go the same way; since #968 the one helper it called back for,
`_unread_scan`, is in process (`internal/unread`), which is what tells the nudge
which directive it is waking the lead for.

### Shared guard (`worktree-git.sh` to `crew-go git-guard`)

`git-baseline` lists drift through the guard (`_wt_cfg_guard` and its helpers in
`worktree-git.sh`), which also runs in `dispatch.sh`, `dispatch-resume.sh` and
`crew reap` (which calls `internal/gitguard` in process now). So the guard moved whole, and the lib calls it instead of keeping a copy. `worktree-git.sh` keeps four one-line wrappers,
each `"${CREW_GO_BIN:-@crewGoBin@}" git-guard <op> …`:

| wrapper                 | op                          | meaning                                                                  |
| ----------------------- | --------------------------- | ------------------------------------------------------------------------ |
| `_wt_cfg_guard`         | `check <common> [<gitdir>]` | refuse exec/redirect config or legacy-file drift (empty gitdir = common) |
| `_wt_cfg_guard_cwd`     | `check-cwd <common>`        | `check` for `<common>` and the git dir the process cwd resolves to       |
| `_wt_cfg_baseline_init` | `init <common>`             | record the baseline once; only add coverage classes                      |
| `_wt_cfg_commondir`     | `commondir <common>`        | the #993 pin from the process cwd                                        |

Those four are the production ops. The rest are test inspection only, driven by
test-local shims in `tests/worktree-git.bats` that stand in for the removed bash
helpers: `pairs <gitdir>`, `union <common>` (NUL-separated records), `show-url <s> [proxy]`,
`show-key <k>`, `show-pair <k> <v> [alias…]` and `canon <R> <rec>`. `git-guard` is
dispatched before `bus.Locate`: it takes explicit dirs and must run from any cwd.

- `flake.nix` substitutes `@crewGoBin@` into the lib (`worktreeGitLib`), so each of its
  Nix-built consumers (`dispatch`, `dispatch-resume`) carries the pinned binary;
  `crew.sh` stopped substituting it when its arms exec'd nothing left to exec (#821).
  Raw-source runs export `CREW_GO_BIN`
  (`tests/setup_suite.bash`, `tests/harness/ensure-crew-go.sh`).
- A missing or unrunnable binary makes the wrapper exit non-zero (127 or 126), which
  every caller already treats as a refusal: the guard fails closed. Any internal Go error
  in a guard path exits 1, never 0, with the error on stderr.
- The #993 pin from the process cwd (`commondir`, and `check-cwd` before its checks)
  also refuses a `<common>` that is not the git dir the cwd resolves to, when that git
  dir has no `commondir`: callers read `<common>` by discovery earlier, and a
  `commondir` removed since leaves it naming the old redirect target.
- The anchored git invocation stays bash for the callers still in bash (see "Bash helpers
  that stay"): it runs arbitrary git subcommands in the caller's process (background pids,
  `GIT_INDEX_FILE`, captured stdout), which a Go exec cannot hand back, and it holds no
  guard logic. It calls the Go check first, exactly where it called `_wt_cfg_guard` before.
  Go callers (`reap`, `stack`) use `internal/wtgit`, which builds the same argv
  and calls `gitguard.Check` in process; `crew.sh` stopped sourcing the lib when
  #1004 ported the arm that sourced it.
- Startup cost: one `crew-go` exec per guard call replaces the bash guard's own forks
  (each call ran `git config --list` one to three times, `sort`, `sha256sum` and
  subshells); Go runs the same git calls and nothing else. Dispatch makes on the order of
  ten to twenty guard calls, expected to stay well under a second in total.

**Single implementation (#993).** Two designs were weighed. (A), chosen: one Go
implementation behind the wrappers above. (B), rejected: keep the bash guard, add a Go
copy for `git-baseline` and `reap`, and pin them with a cross-implementation drift suite
that runs every guard fixture through both and compares verdict and refusal text. The
suite would have to enumerate the guard's behaviour (the masking grammar, trust on first use,
the migrations, the cwd hooks equivalence, the legacy entry kinds), so any case it misses is
an undetected divergence in a security check, and every later guard fix would land twice.
(A) costs one exec per call and a hard dependency on the binary, both bounded above. The
guard's key tables, masking and baseline format therefore have one copy, and no drift guard.

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
- Specific to `reap`, and not mirrored:
  - the anchored git commands run with an environment scrubbed of `GIT_DIR`,
    `GIT_WORK_TREE`, `GIT_CONFIG_*` and the other re-pointing variables (`internal/wtgit`);
    bash inherited it. The arm's callers never set them, so no bats row can see it;
  - a non-positive pid from `CREW_REAP_PROC_CMD` is never signalled (`kill 0` and `kill -1`
    reach a whole process group or every process the user may signal);
  - a non-numeric tmux `window_activity` or `session_attached` field reads as no human;
    bash evaluated the activity field with `$((…))`, where a bare word is a variable name
    and an expression counts as its value;
  - the idle-window kill order is byte order, where `sort -u` used the caller's locale
    collation; the set is identical;
  - the `kind:"reap"` row is best-effort: a bus write failure prints
    `crew reap: could not record the reap row for <branch>: <err>` on stderr and the
    `reaped` status row, record cleanup, branch delete, label release and the next
    candidate still run, where the arm's `set -e` aborted the sweep (#997). The release
    row in idle release is not: its write failure still exits 1;
  - `stream: a failing reap surfaces as one stream error line` broke reap by pointing
    `WORKTREE_GIT_LIB` at a missing file, which the Go arm never sources; the row now fails
    reap through a `TrustedCwd` refusal (a planted anchor record naming another crew dir).
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
- Specific to `git-guard` and `git-baseline`, and not mirrored; each refuses at least as
  often as the bash guard:
  - `%q` is bash's under `LC_ALL=C` everywhere: bytes at or above 0x80 print as `$'\ooo'`.
    Bash used the ambient locale outside URL masking, so a UTF-8 locale printed non-ASCII
    raw (bidi controls included). Which values are masked, and how, is unchanged for ASCII;
  - legacy `unsafe-name` also covers invalid UTF-8 and Unicode space and control runes
    (glibc's `[[:space:][:cntrl:]]` depends on the locale), so such names always refuse;
  - legacy refusal and listing lines come out in byte order (`os.ReadDir`), not bash glob
    collation;
  - origin and key dedupe use exact sets; bash's joined-string substring test could drop a
    line that merely looked like a duplicate;
  - a legacy file swapped for a fifo or symlink between the `lstat` and the open reads as
    `unreadable`;
  - shell and `realpath` error text that bash printed before a silent refusal is not
    printed; the refusal and the exit status stay.
- Specific to `entry`, and not mirrored:
  - `dash` with no `crew-dash` on PATH prints Go's own lookup line under
    `crew: dash: ` where bash printed `crew-dash: command not found`; the 127 stays (the
    `pr-watch` precedent). A target that is found but will not exec is 126, which bash
    reached with its own `cannot exec` line;
  - a help name that is no case label is refused the same way whatever it looks like:
    `crew help hold/add` and `crew help ../top` name no screen, and so do the two file
    names this package invented (`top`, `default`), never subcommands themselves;
  - the `*)` arm of the top-level case runs the screen as a child rather than `exec`-ing
    it, because the arm's exit stays 1 and the screen itself exits 0.
- Before deleting a bash arm, diff it against Go over a generated corpus and
  keep the evidence: compare exit status, human/agent text and JSON values
  (`jq -S`) — nothing else.

## What the bash scripts still carry

#821 deleted `adapters/core/crew.sh`. Every helper still in it had either no caller
left or a live copy in the script that calls it, so deleting it moved no behaviour;
what is left of the drift-guard set is what the table says. Where a live copy
exists, that copy is what the guard now reads — the library was never the only
implementation of those, only the one they were all named after. Where the library
held the only copy of a rule Go already implements, the expectation is frozen
instead of compared, because there is nothing left to compare against.

| helper                                                                                | what ships                                                                    | guard                                                                                                                           |
| ------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `_bus_append`                                                                         | dispatch.sh, dispatch-notify.sh, dispatch-resume.sh (three standalone builds) | adapters.bats byte-compares the three, dispatch.sh first                                                                        |
| `_is_engine_cmd`                                                                      | local-models.sh (the lib dispatch.sh sources), dispatch-notify.sh             | adapters.bats byte-compares the pair; `TestEngineCommandsMatchLocalModels` parses the case pattern into `roster.EngineCommands` |
| `_pid_alive`, `_file_mtime_s`, `_ps_elapsed_s`, `_pid_recycled`, `_recorded_pid_live` | dispatch.sh, dispatch-resume.sh                                               | adapters.bats byte-compares all five                                                                                            |
| the `--role-watch` frame predicates (`re_option` … `_pi_idle_box`, 20 of them)        | dispatch.sh — the only bash copy that ships                                   | `TestDriftAgainstBash` extracts the block and compares Go against it                                                            |
| `_pane_idle_reason`, `_is_quota_prompt`, `_is_quota_session_limit`                    | nothing: no bash script carries them                                          | `TestGoldenOracle` against `testdata/golden-oracle.tsv`                                                                         |
| `_names`, `_colors`, `_tmuxc`                                                         | nothing                                                                       | `TestPoolsAreGolden` against the frozen 32-entry lists                                                                          |
| `_await_state`, `_await_marks`, `_await_record`                                       | nothing: every reader and writer is Go                                        | `marks.TestPath` / `TestPathMatchesShell` (real `tr`/`cksum`), `TestRecord` / `TestRecordMatchesJq`                             |
| `_clock_now`, `_clock_now_f`                                                          | phase-status.sh and the `--sh` ops, each in its own script                    | unchanged: both sides read and write one `CREW_CLOCK` file                                                                      |
| `_crew_id`                                                                            | nothing; directive-delivery.sh and phase-status.sh each restate the rule      | `bus.CrewID`'s tests, and the bats rows that resolve a crew id                                                                  |
| `_gh_json`                                                                            | nothing                                                                       | nothing to guard: `jqrun` is the implementation, its tests the guard                                                            |

The golden rows were generated, not retyped: a throwaway Go test drove the then-live
crew.sh oracle over `testdata/frames` and over every `rows()` input, and wrote one
`<input>\t<key>\t<value>` row per case; the generator was deleted once the file
landed, so nothing in the tree reads the deleted script. `TestGoldenOracle` fails on
an unknown row, a duplicate, or a corpus frame with no row, so the file cannot
quietly stop covering the package.

What follows is where each helper went, kept because the guards above still name them.

`_is_engine_cmd` keeps two callers, and each copy is now the lib's or the standalone
build's (`_pane_is_engine_at` went with `reap`). While two copies exist, guard drift.

The `_identity*` helpers lost their last caller with the `identity` arm (#981) and
are gone; `crew/internal/identity` is now the only picker of a codename. Its three
pools are the only copy of the palette: `identity_test.go`'s `TestPoolsAreGolden`
freezes all three against the lists the bash arrays held, which is what keeps a slot
from being reordered under a recorded identity, and crew.bats's `_rr_palette`
enumerates the Go array to compile every palette stroke through d2.

`_sessions` and `_resolve_target` lost their last caller with the `nudge` arm and
are gone, and their drift guards went with them: `crew sessions` and
`crew resolve-target` are now the only implementations, and `nudge` reaches both
through them (`sessions.Fold` for the newest-session gate, `resolve.Rows` for its
"ambiguous target … pass a branch" refusal, which `crew resolve-target` cannot
print because it exits 2 with an empty stdout there).

`_await_state`, `_await_marks` and `_await_record` went with the script: every
reader and writer of the marks file is Go now, `internal/marks` owning the format the
helpers defined. The guards moved into the package — `TestPath` and
`TestPathMatchesShell` pin the sanitized-key-plus-cksum name against real `tr` and
`cksum`, `TestRecord` and `TestRecordMatchesJq` pin the merge program — and the two
bats rows that replayed the helpers compute the path from the same formula
(`marks_path` in tests/helpers.bash) instead of sourcing a helper.

`_clock_now` and `_clock_now_f` stay for `roster-render` and the `--sh` ops, and
`internal/clock` is their Go copy for `hold`, `await`, `stall-watch` and `nudge`;
`_clock_now_ms` and `_clock_sleep` lost their last caller with the `nudge` arm.
The guard is that both sides read and write one file: the suite
exports `CREW_CLOCK`, so the clock a Go `await` or `stall-watch` seeds is the
file a Go `nudge --wait` advances, and `crew clock: unset, await and hold still use
real time` pins the no-`CREW_CLOCK` branch. `watch` is deliberately outside this
pair: its park runs on `internal/clock`'s real half only, because the arm's
`jq -nc 'now*1000|floor'` and its `sleep` never consulted the clock file.

`_lock_acquire`, `_lock_release` and `_lock_publish_pid` are gone with `reap` (#997), and
so are the crew.bats rows that `eval`ed them; `internal/lock` holds a `flock(2)` and reads
a `*.d` only as the record it publishes (or, for an old build's, as the old format), and its
tests cover the old-format holders those rows planted.
Which lock paths either language takes, from `grep -rn 'lock\.d\|refresh\.d\|nudge-wait'`
over the non-test code:

| lock path                                  | bash | Go                           |
| ------------------------------------------ | ---- | ---------------------------- |
| `$XDG_DATA_HOME/crew/ratings.sweep.lock.d` | none | `reap` autosweep             |
| `<crew dir>/reap.lock.d`                   | none | `reap`                       |
| `$XDG_DATA_HOME/crew/ratings.lock.d`       | none | `rate` sweep                 |
| `<crew dir>/watch.lock.d`                  | none | `watch`                      |
| `<crew dir or lane>/stream.lock.d`         | none | `stream`, fleet lane         |
| `<crew dir>/roster-render.lock.d`          | none | `roster-render`              |
| `<bus dir>/nudge-wait/<sid>.<cksum>.d`     | none | `nudge --wait`               |
| `<budget file>.refresh.d`                  | none | `stall-watch` budget refresh |

No lock is taken by bash any more, so neither a `crew lock` helper nor `flock(1)` (absent on
macOS by default) is needed. The sharing that remains is across builds, not languages: old-build
processes still running after a switch (an old `stream` re-entering its old
`CREW_SELF` for `watch`, old `stall-watch`es, an older installed `crew` whose `rate`
and `nudge` are bash arms) take the same paths with the mkdir protocol, and
`internal/lock`'s old-format branch handles them. Its own tests are the drift
guard: live, dead, empty, non-numeric, `0`, out-of-range and trailing-newline
holders against that branch, plus the `stream` rows that TERM a parked Go watch and
replace a `--force`'d holder, and the reap and autosweep bats rows that plant a mkdir lock
with a live pid.

The frame classifiers (`_frame_classifier`, `_pane_idle_reason`, `_claude_idle_box`,
`_box_rows`, the `_pi_*` helpers) leave dispatch.sh's copies as the only bash ones:
`internal/frame` is the Go implementation `stall-watch` and `reap` run, and its drift
test extracts dispatch.sh's `--role-watch` block and compares Go against it, which is
the same comparison adapters.bats used to make with the library as the middleman.
`_is_quota_cursor_limit` ships only in dispatch.sh, and is inside that extracted
block. `_is_quota_prompt`, `_is_quota_session_limit` and `_pane_idle_reason` ship in
no bash script, so their cases are the golden table above. `_is_bg_wait` had no bash
caller left and no pinned copy, so it moved to Go and its pre-port body is the
vendored oracle.

The rest of what `stall-watch` reaches stays bash and is delegated, not copied
(one copy needs no guard): budget-gate.sh and local-models.sh, reached
through `--sh budget` and `--sh local-model`. `_release_windows`, `_human_present`,
`_frames_busy`, `_pane_capture`, `_occupants` and `_pane_is_engine_at` went with `reap`
(#997): `reap.ReleaseWindows` and `reap.Occupants` are the one implementation, reached in
process by `stall-watch`, `reap` and `crew occupants`. `_unread_scan` was the last one
with two Go callers (`stall-watch` every 4th tick and `crew nudge` per call);
both read `internal/unread` since #968 and it is gone from crew.sh. `release_grace` and its
drift guard are gone too: `reap.ReleaseGrace` is the one constant, and stall's `releaseGrace`
(`--release`'s default) is that value. `_publish_pane_state` is gone with its guard: `panestate` is the one copy, shared by
`stall` and `status`.

`_hold_outstanding` lost its last bash caller when `roster-render` moved (#935)
and is gone: `hold.Outstanding` is the one implementation, reached in process by
the renderer's model and by `hold list` alike, so the guard that compared the two
copies went with the copy. `_hold_crew` and `_hold_render` were already gone.
`_fit_line`, `_shrink`, `_LINE_MAX`/`_ELIDED` and
their drift guards are gone with the `status`/`msg` port (#945): `bus` is the one
copy. `_bus_append` had no library caller since `crew stack` moved (#1004); the three
standalone builds that call it each carry a copy, and adapters.bats byte-compares them
with dispatch.sh as the one it names (the same arrangement as `_pid_alive`), while
`internal/bus.Append` is the Go copy.
`_owner_pid`, `_pidfile_log` and `_has_c0` are gone (`crews.Probes.OwnerPID`,
`crews.PidfileLog`, and `msg`'s own control-byte test). `_pid_alive`,
`_file_mtime_s`, `_ps_elapsed_s`, `_pid_recycled` and `_recorded_pid_live` are carried
by dispatch.sh and dispatch-resume.sh, two standalone builds, and adapters.bats
byte-compares all five between them (the same arrangement as `_is_quota_cursor_limit`).
`_crew_id` lost its last library caller with the `id` arm (#1007) and went with the
script: `directive-delivery.sh` and `phase-status.sh` each resolve the crew id by
naming its rule (task doc first, then the environment) as the thing they copy, and
`bus.CrewID` is the Go copy of that rule.

The guard helpers are gone from `worktree-git.sh` (#993): the key tables and their masking,
the `#covers` marks, the pair and union listing, the legacy records, `_wt_cfg_canon`, the
baseline init and migrations, and the guard and cwd guard bodies are all
`crew/internal/gitguard`, so there is no drift guard to keep. What stays in the lib, and why:

- the four wrappers (`_wt_cfg_guard`, `_wt_cfg_guard_cwd`, `_wt_cfg_baseline_init`,
  `_wt_cfg_commondir`), which exec Go;
- the anchored git invocation: `_wt_git`, `_wt_git_common`, `_wt_common_argv`,
  `_wt_neutral`, `_wt_neutral_cfg` and `_wt_empty_tree`. They run arbitrary git in the
  caller's process (pids a caller backgrounds, `GIT_INDEX_FILE`, stdout capture) and hold
  no guard logic;
- `_wt_trusted_cwd`, which `cd`s the caller, so it cannot be a child process (it calls
  `_wt_cfg_commondir` first and again after the `cd`);
- `_wt_admin_dir`, `_wt_gitlink_ok`, `_wt_status`, `_wt_plain_branch_name`,
  `_wt_fetch_origin_branch` and `_worktree_anchor_path`: bash callers' helpers over git
  and the dispatcher records, with no config check in them. `internal/wtgit` carries Go
  copies of `_wt_plain_branch_name` (`PlainBranchName`) and `_wt_fetch_origin_branch`
  (`FetchOriginBranch`) for `crew stack`; the bash copies stay for dispatch, whose
  branch-name and fetch checks are load-bearing.

`crew reap` is Go (#997) and uses `internal/wtgit`, the Go copy of the anchored invocation,
`_wt_trusted_cwd` (as a returned directory), `_wt_admin_dir`, `_wt_gitlink_ok`, `_wt_status`
and `_worktree_anchor_path`, plus `PlainBranchName` and `FetchOriginBranch` (`crew stack`
has run the Go copies since #1004). The bash copies stay for dispatch until its callers
are ported, and `wtgit`'s drift test pins `_wt_neutral_cfg` and the common extras against
`worktree-git.sh`, so a change to either list fails until both agree. The rest of the
invocation (the refusals, `--attr-source`, anchoring) has no second guard: it is covered by
`wtgit`'s argv tests and by the bats rows that exercise the bash copy.

## Running the suite

- `bats tests/...`: `tests/setup_suite.bash` builds `crew-go` from the working
  tree once per run and exports `CREW_GO_BIN`; a failed build fails the run.
- The bench harnesses run the same binary from the checkout, so their entries
  export it too. `tests/harness/ensure-crew-go.sh` builds unless
  `CREW_GO_BIN` is already set: a bench adapter builds once and its per-case
  children inherit, keeping the build out of the measured per-case window.
- `cd crew && go test ./... && go vet ./... && golangci-lint run ./...`.
  `exhaustive` is on: keep `switch` on `Kind` and `State` complete.
- CI runs that same `go test ./...` from the checked-out repo with
  `CREW_DRIFT_REQUIRED=1`, so a drift guard that cannot read its bash file fails
  there instead of skipping (`nix build .#crew-go` still skips them: its sandbox
  holds only `./crew`).
- `scripts/help-parity.sh` is the help screens' evidence run (#1007): it reads the
  pre-change `crew.sh` out of git, runs every help invocation against both sides
  and diffs stdout, stderr and the exit status. `--base <ref>` points it at the
  script as it was before any later change to the screens.
- `scripts/bats-affected.sh` maps `crew/*` to the crew bats files.

## Flipping to direction (a)

Done in #821, and not the way this list first sketched it: nothing execs crew.sh
any more, because nothing was left for it to run.

1. The Go binary became the `crew` package — `makeWrapper` of `crew-go`, with
   `--inherit-argv0` so the entry still sees the profile path. No internal
   `crew-sh` fallback: an unknown subcommand is the help screen and exit 1, which
   is what the script's `*)` arm already did.
2. The delegating arms in crew.sh are gone, with the help routing and the `*)` arm
   that followed them.
3. crew.sh's `$0` self-re-execs (`roster`, `dash`, `hold`, `rate`) read the
   `CREW_SELF` the entry exports; `stream` re-enters the entry through
   `entry.EntryArgv` instead of a bash script of its own.
4. `run_crew` and `CREW_REAL` in the bats files are the Go binary (`tests/helpers.bash`
   defines `CREW` from `CREW_GO_BIN`).
5. The library itself is deleted. The helpers still live in the scripts that call
   them, the guards that compared Go against a library copy read those copies
   instead, and the comparisons that had no copy left became golden tables — see
   "What the bash scripts still carry".
