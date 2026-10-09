# Porting crew to Go

Issue: #822. `adapters/core/crew.sh` moves to Go one subcommand at a time.
Ported so far: `log`, `report`, `sessions`, `roster`, `crews`, `inbox`, `hold`,
`await`, `retro`, `rate` (both modes: the per-repo sweep of #895 and the
`--report` rollup of #890), `reply`, `watch`, `resolve-target`, `where` and
`stall-watch` (#832). Each slice must leave every bats file green; tests may be
adapted only where the Go design changes what they can observe (the output
contract below), with each edit justified.

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
  `_fit_line`/`_shrink` at `LineMax`. `status` and `msg` still call the
  bash originals, so both copies are guarded from each side.
- `crew/internal/identity`: codename, colour and tmux pools, the cksum slot.
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
  programs embedded verbatim. `nudge` and, through `--sh unread`, `stall-watch`
  still read the same file from bash, so the name and the content are the
  contract.
- `crew/internal/clock`: the `$CREW_CLOCK` pair — `_clock_now`, `_clock_now_f`,
  `_clock_now_ms` and `_clock_sleep` — shared by `hold` (which had the first two
  privately until #884) and `await`. With no `CREW_CLOCK` these are `date +%s`,
  jq's `now`, jq's `now*1000|floor` and the real `sleep` (same argv, so a
  rejected interval costs coreutils' own message and status, which is what
  killed the arm under `set -e`).
- `crew/internal/lock`: crew.sh's `_lock_acquire`/`_lock_release` protocol —
  the `mkdir` gate, the `pid` owner file, the bare `kill -0` liveness probe and
  the dead-owner reclaim — as one package instead of a copy per caller, because
  two of its lock dirs have bash holders on the other side: `ratings.lock.d`
  (the autosweep spawner in `reap`, older installed `crew` binaries) and
  `watch.lock.d` (bash `stream`, which takes it through the `crew watch` it
  re-enters). Held for the two `rate` store windows and never across a gh call;
  `watch` holds it for its whole park and releases it on every exit path,
  including the SIGTERM/SIGINT/SIGHUP `stream` sends it.
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
- `crew/internal/rate`: the whole `rate` arm — #890's read-only `--report` and
  #895's sweep, which is the port's first writer that is not the bus. Bash keeps
  the flag loop, the refusals and the `--sweep-all` loop (it discovers repos
  from the registry and re-enters this arm per repo), and execs Go for both
  modes. The sweep is the arm op-for-op: `records.jq` folds the bus into one row
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
  `internal/lock` (#901): mkdir, `pid` file, bare `kill -0` liveness, kept
  byte-compatible so an older installed `crew` and the autosweep spawner
  interoperate; it is held for the two store windows and never across a gh
  call, and released on every exit path while held. The batch append is one
  `O_APPEND` write, the same single-write guarantee the arm's `dd bs=1048576`
  gave.
- `crew/internal/reply`: the arm of #893, a writer like `hold` but with no fold
  of its own — it resolves a branch-only `worker:<branch>` target through
  `sessions.Fold`, and with no crew named through every crew the bus carries,
  dropping the ones whose registered pid is known-dead by the same three-state
  read `crews` prints (`crews.PidAlive`). Its row is the arm's six keys in order,
  and it needs no byte-exact body: `body` is the caller's text, so `jq -S` does
  excuse the key order. `bus.IsSessionID` is `_is_session_id`'s Go twin, the one
  `inbox` and `await` now share.
- `crew/internal/watch`: the arm of #901, the second poller. Its fold is the
  arm's jq through jqrun (`watch.jq`), and it is the port with the most
  cross-language surface: `crew stream` stayed bash and re-enters `crew watch`
  as its child, so the batch, the cursor file and `watch.lock.d` are all
  contracts with bash. Two rules follow from the arm. The clock is the wall
  clock — it stamped `start` and every deadline with jq's `now` and slept with
  the real `sleep`, so unlike `await` and `hold` it never reads `$CREW_CLOCK`
  (and the suite exports `CREW_CLOCK` for `await`'s sake, which is why this is
  worth saying). And the lock is held for the whole park and released on every
  exit path, including the SIGTERM/SIGINT/SIGHUP `stream` sends its inner watch
  when it stops or retries: bash ran its `trap … EXIT` for those three, and a
  leaked `watch.lock.d` refuses every later watch of the crew until the dead pid
  is reclaimed. The batch prints before the cursor moves, as the arm's `printf`
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
- `crew/internal/frame`: Go copies of `_frame_classifier`'s predicates (prompt,
  permission, quota, background-wait, meter and sub-row shapes, the claude and
  pi input boxes) and `_pane_idle_reason`. Every function takes the sampler's
  stdout with trailing newlines stripped and the engine as an argument, where
  bash read the global `$engine`. The bash originals stay (see below); the
  translation rules are what keep the two equal:
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
  D2, D1b, D7, D3 and D0 on the frame, then D6, then the `dead:` escalation
  (D2 and D3; D3's also needs the engine gone), with the pane-gone quorum of 3,
  the 4-tick bus-read cadence and `--max-life` checked every tick. A `done` or
  `failed` bus state hands over to the release loop: claude must show
  `frame.PaneIdleReason` idle on two consecutive ticks, other engines an
  unchanged frame for `--release` with no prompt, and `--sh release` rc 3 keeps
  watching while any other status exits 0. Every bus write is
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
  `capture` anchor is `\n?\z`, as in `sessions`. Signals: SIGTERM and SIGINT
  cancel the context with a cause and the process exits 128+signo, as a shell
  reports the killed bash arm; sleeps and children are context-aware, and the
  budget-refresh lock is released on every exit path while held. SIGHUP is left
  untouched — the watchdog is `nohup`-launched, and `signal.Notify(SIGHUP)`
  would undo the ignore it inherits. Every append is one `O_APPEND` write, so a
  kill cannot leave a torn row; the `refresh-at` stamp and the clock file keep
  tmp+rename.
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
  dies with its parent. `Sh` is the `--sh` runner (see Delegation).
- `crew/internal/testjson`: test-only value-equal JSON comparison.

New subcommands get an `internal/<sub>` package; shared reads go through `bus`;
folds that outgrew hand-translation run on jqrun.

## Delegation

crew.sh stays the entrypoint (direction b). A ported arm is:

```bash
crews | log | report | sessions | roster | inbox | hold | await | watch | retro | reply | resolve-target | where)
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
```

`flake.nix` substitutes `@crewGoBin@` with the `crew-go` package's
`bin/crew-go`; `CREW_GO_BIN` overrides it for raw-source runs. The bash
preamble (`--help`, the git-repo check) runs first. Add a subcommand to this
arm and delete its old arm. `crew hold`'s per-action `--help` text lives only in
that preamble — it never reaches Go, and `help: crew hold <action> --help` pins
it. `rate` execs Go for both of its modes — no flags for the sweep, `--report`
for the rollup — with `--sweep-all`/`--root` looping in bash and re-entering the
arm; the Go side refuses anything else with one usage line.

`stall-watch` is a second shape: every invocation but the hidden `--sh` branch
execs Go, and that branch stays in bash.

```bash
stall-watch)
  if [ "${1:-}" = --sh ]; then …; exit "$rc"; fi
  CREW_SH="$(readlink -f "$0")" exec "${CREW_GO_BIN:-@crewGoBin@}" stall-watch "$@"
  ;;
```

The helpers the loop needs (`_release_windows`, `_nudge_pane`, `_unread_scan`,
budget-gate.sh, local-models.sh) have other bash callers, so Go calls back
instead of copying them: `bash -euo pipefail "$CREW_SH" stall-watch --sh <op> …`,
the self-re-exec form crew.sh already uses (`bash -euo pipefail "$0" reap …`),
which runs the helpers under the arm's own preamble. Each op is the arm's old
call site, errexit-suppressed as the arm suppressed it, then `exit "$rc"`. The
environment is inherited; Go strips trailing newlines from stdout and reads the
status (-1 if the child was killed). An unknown op or budget predicate is one
`crew: stall-watch: unknown --sh …` line, exit 1.

| op            | argv after `--sh <op>`                                        | stdout                                            | rc to Go                                                             |
| ------------- | ------------------------------------------------------------- | ------------------------------------------------- | -------------------------------------------------------------------- |
| `release`     | `<branch> <session\|-> <state> <ts_ms> <grace>`               | none                                              | 3 keep watching; else exit 0                                         |
| `nudge`       | `<pane> <engine> <sid> <crew> <msg_ts>`                       | one line                                          | 0 ok; 2 refused (`anchor:` turns nudging off); 3 typed, not accepted |
| `unread`      | `<crew> <branch> <me> <from_id> <t0_ms> <oldest\|dispatcher>` | `<ts> <dispatcher\|role>`, `<min> <max>` or empty | always 0                                                             |
| `budget`      | `<windows\|limit> <cache> <engine> <now>`                     | `_budget_*`'s TSV                                 | 0 hit, 1 clear, 2 can't tell                                         |
| `local-model` | `<model>`                                                     | non-empty iff the model is local                  | ignored; a failure keeps D8 on                                       |

The op supplies what the arm supplied itself: `release` appends the empty
dry-run argument to `_release_windows` (after no-op `say`/`note`), and `nudge`
inserts the literal `watchdog` actor before the message timestamp. `budget` and
`local-model` source `${BUDGET_GATE_LIB:-@budgetGateLib@}` and
`${LOCAL_MODELS_LIB:-@localModelsLib@}`; `local-model` also runs
`${DISPATCH_CONFIG_BIN:-dispatch-config}`.

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
- Before deleting a bash arm, diff it against Go over a generated corpus and
  keep the evidence: compare exit status, human/agent text and JSON values
  (`jq -S`) — nothing else.

## Bash helpers that stay

`_sessions`, `_identity*`, `_crew_id`, `_is_engine_cmd` and `_pane_is_engine_at`
keep other callers (`nudge`, `reap`). Delete each only with its last caller.
While two copies exist, guard drift:

- a crew.bats test compares the bash `_sessions` helper with `crew sessions` on
  a shared fixture bus (value compare, `jq -S`: key order is engine-internal);
- Go tests parse crew.sh's pools and engine table and assert the Go copies
  match (skipped when crew.sh is absent, as in the Nix sandbox).

`_resolve_target` stays for `nudge`, which reads its rows to phrase its own
"ambiguous target … pass a branch" refusal — `crew resolve-target` exits 2 and
prints nothing on stdout there, so `nudge` cannot be routed through the arm, and
the recovery watcher would spawn the Go binary per nudge. Its Go copy is
`internal/resolve`, and the guard is `_sessions'` shape: a crew.bats row replays
the extracted helper against `crew resolve-target` over the target shapes on one
fixture bus, deriving the arm's status and text from the helper's own rows —
`@tsv` both sides, so a byte compare with no `jq -S` excuse.

`_await_state`, `_await_marks` and `_await_record` stay for `_unread_scan` (and
through it `nudge` and `stall-watch --sh unread`), which read the marks file even
though neither `crew await` nor `crew inbox` calls them any more: both write it
from `internal/marks`. Two of the three drift guards above apply — crew.bats
compares the Go marks file with `_await_state`'s path and `_await_record`'s
content on the same msgs, and `internal/marks` runs the helpers' own two programs
through jqrun — and two more read the other way: crew.bats hands the marks a Go
`await` raised to the extracted `_unread_scan`, and to `crew nudge` itself.

`_clock_now`, `_clock_now_f`, `_clock_now_ms` and `_clock_sleep` stay for `nudge`
(the arm and `_nudge_pane`, which `stall-watch --sh nudge` also runs) and
`roster-render`, and `internal/clock` is their Go copy for `hold`, `await` and
`stall-watch`. The guard is that both sides read and write one file: the suite
exports `CREW_CLOCK`, so the clock a Go `await` or `stall-watch` seeds is the
file a bash `nudge` advances, and `crew clock: unset, await and hold still use
real time` pins the no-`CREW_CLOCK` branch. `watch` is deliberately outside this
pair: its park runs on `internal/clock`'s real half only, because the arm's
`jq -nc 'now*1000|floor'` and its `sleep` never consulted the clock file.

`_lock_acquire` and `_lock_release` stay for `stream`, `nudge` and `roster-render`,
and `internal/lock` is their Go copy for `rate`, `watch` and `stall-watch`'s
budget refresh (`engine-budget.json.refresh.d`). Same guard as the clock — one protocol, two languages, one lock dir:
bash `stream` writes `watch.lock.d/pid` with `$$` through the `crew watch` it
re-enters, and an older installed `crew` holds `ratings.lock.d`. The drift test
is `internal/lock`'s own table (live, dead, empty and non-numeric owners, a `0`
holder, trailing newlines) and the `stream` rows that TERM a parked Go watch.

The frame classifiers (`_frame_classifier`, `_pane_idle_reason`, `_claude_idle_box`,
`_box_rows`, the `_pi_*` helpers) stay in crew.sh for `reap`, `nudge` and
`_release_windows`; dispatch.sh's byte-identical copies are pinned by adapters.bats
and untouched. `internal/frame` is the Go copy `stall-watch` runs every tick, and
the `LC_ALL=C.UTF-8` drift test described under Layout is the guard.
`_is_quota_cursor_limit` has no bash caller left but stays, as the canonical copy
adapters.bats compares dispatch.sh's against. `_is_bg_wait` had no bash caller
left and no pinned copy, so it moved to Go and is gone from crew.sh.

The rest of what `stall-watch` reaches stays bash and is delegated, not copied
(one copy needs no guard): `_unread_scan` (it keeps `nudge` as a caller, reads the
marks through the `_await_*` trio, and runs every 4th tick), `_release_windows`
(with `reap`), `_nudge_pane` (with `nudge`), and budget-gate.sh and local-models.sh,
reached through `--sh budget` and `--sh local-model`. Two helpers do have a Go copy
and a guard: `_publish_pane_state` stays for `status`, and a Go test runs the
extracted bash function and `publishPaneState` against one stub `tmux` and
compares the argv logs, including a multibyte detail over 40 runes; `release_grace`
stays for `reap`, and a Go test parses `^release_grace=` from crew.sh and asserts
it equals `--release`'s default. Both tests skip when crew.sh is absent.

`_hold_outstanding` stays for `roster-render`'s `_rr_model`, which reads the same
bus `crew hold list` reads; `_hold_crew` and `_hold_render` lost their last
caller with the arm and are gone. `_fit_line`, `_shrink` and `_bus_append` stay
for `status` and `msg`, and Go copies all three — so crew.bats replays the
appended row through the extracted `_fit_line`/`_shrink`, compares
`_hold_outstanding` with `crew hold list --json` on one fixture bus, and
`internal/bus` parses `_LINE_MAX`/`_ELIDED` out of crew.sh against its own
constants.

## Running the suite

- `bats tests/...`: `tests/setup_suite.bash` builds `crew-go` from the working
  tree once per run and exports `CREW_GO_BIN`; a failed build fails the run.
- `cd crew && go test ./... && go vet ./... && golangci-lint run ./...`.
  `exhaustive` is on: keep `switch` on `Kind` and `State` complete.
- `scripts/bats-affected.sh` maps `crew/*` to the crew bats files.

## Flipping to direction (a)

1. The Go binary becomes the `crew` package: it runs ported subcommands and
   `syscall.Exec`s crew.sh (an internal `crew-sh`, path baked in via ldflags)
   for the rest.
2. The delegating arms in crew.sh disappear.
3. crew.sh's `$0` self-re-execs (`roster`, `dash`, `reap`, `hold`, `stream`,
   `rate`) switch to a `CREW_SELF`-style env the Go front exports. `stream` is
   the one that matters for `watch`: it re-enters `$0` for its inner park, and
   that arm is now the delegation exec.
4. `run_crew` and `CREW_REAL` in the bats files switch to the Go binary.
