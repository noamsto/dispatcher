# Porting crew to Go

Issue: #822. `adapters/core/crew.sh` moves to Go one subcommand at a time.
Ported so far: `log`, `report`, `sessions`, `roster`, `crews`, `inbox`, `hold`,
`await`, `retro`, `rate --report` (report mode only; the sweep path stays in
bash) and `reply`. Each slice must leave every bats file green; tests may be
adapted only where the Go design changes what they can observe (the
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
  programs embedded verbatim. `nudge` and `stall-watch` still read the same file
  from bash, so the name and the content are the contract.
- `crew/internal/clock`: the `$CREW_CLOCK` pair — `_clock_now`, `_clock_now_f`,
  `_clock_now_ms` and `_clock_sleep` — shared by `hold` (which had the first two
  privately until #884) and `await`. With no `CREW_CLOCK` these are `date +%s`,
  jq's `now`, jq's `now*1000|floor` and the real `sleep` (same argv, so a
  rejected interval costs coreutils' own message and status, which is what
  killed the arm under `set -e`).
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
- `crew/internal/rate`: the arm of #890, read-only like `report` and its first
  partial delegation — bash keeps `rate`'s flag loop, refusals and sweep path
  and execs Go only for `--report`. Its two folds (`dedupe.jq`, `report.jq`)
  read the global ratings store, not the bus, and every store failure folds to
  `[]` in silence — the arm's `2>/dev/null || true` — so an empty, missing or
  unparseable store renders the header alone.
- `crew/internal/reply`: the arm of #893, a writer like `hold` but with no fold
  of its own — it resolves a branch-only `worker:<branch>` target through
  `sessions.Fold`, and with no crew named through every crew the bus carries,
  dropping the ones whose registered pid is known-dead by the same three-state
  read `crews` prints (`crews.PidAlive`). Its row is the arm's six keys in order,
  and it needs no byte-exact body: `body` is the caller's text, so `jq -S` does
  excuse the key order. `bus.IsSessionID` is `_is_session_id`'s Go twin, the one
  `inbox` and `await` now share.
- `crew/internal/testjson`: test-only value-equal JSON comparison.

New subcommands get an `internal/<sub>` package; shared reads go through `bus`;
folds that outgrew hand-translation run on jqrun.

## Delegation

crew.sh stays the entrypoint (direction b). A ported arm is:

```bash
crews | log | report | sessions | roster | inbox | hold | await | retro | reply)
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
```

`flake.nix` substitutes `@crewGoBin@` with the `crew-go` package's
`bin/crew-go`; `CREW_GO_BIN` overrides it for raw-source runs. The bash
preamble (`--help`, the git-repo check) runs first. Add a subcommand to this
arm and delete its old arm. `crew hold`'s per-action `--help` text lives only in
that preamble — it never reaches Go, and `help: crew hold <action> --help` pins
it. `rate` is the partial case: only its `--report` block execs Go (plus
`--json`/`--pooled` as parsed), and the Go side refuses every other mode with
one usage line.

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

`_await_state`, `_await_marks` and `_await_record` stay for `_unread_scan` (and
through it `nudge` and `stall-watch --unread`), which read the marks file even
though neither `crew await` nor `crew inbox` calls them any more: both write it
from `internal/marks`. Two of the three drift guards above apply — crew.bats
compares the Go marks file with `_await_state`'s path and `_await_record`'s
content on the same msgs, and `internal/marks` runs the helpers' own two programs
through jqrun — and two more read the other way: crew.bats hands the marks a Go
`await` raised to the extracted `_unread_scan`, and to `crew nudge` itself.

`_clock_now`, `_clock_now_f`, `_clock_now_ms` and `_clock_sleep` stay for `nudge`,
`stall-watch` and `roster-render`, and `internal/clock` is their Go copy for
`hold` and `await`. The guard is that both sides read and write one file: the
suite exports `CREW_CLOCK`, so a Go `await`'s seeded clock is the file a bash
`stall-watch` advances, and `crew clock: unset, await and hold still use real
time` pins the no-`CREW_CLOCK` branch.

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
3. crew.sh's `$0` self-re-execs (`roster`, `dash`, `reap`, `hold`, `watch`,
   `rate`) switch to a `CREW_SELF`-style env the Go front exports.
4. `run_crew` and `CREW_REAL` in the bats files switch to the Go binary.
