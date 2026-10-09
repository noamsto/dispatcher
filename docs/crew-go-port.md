# Porting crew to Go

Issue: #822. `adapters/core/crew.sh` moves to Go one subcommand at a time.
Ported so far: `log`, `report`, `sessions`, `roster`, `crews` and `inbox`. Each
slice must leave every bats file green; tests may be adapted only where the Go
design changes what they can observe (the value-identity contract below), with
each edit justified.

## Layout

- `crew/main.go`: argv dispatch.
- `crew/internal/jqrun`: runs the original jq programs on gojq
  (`github.com/itchyny/gojq`): jsonv values in, jsonv values out, `now` and
  `$vars` injected, type errors surfaced as exit 5. The only gojq caller.
- `crew/internal/jsonv`: ordered JSON values with jq semantics — decode
  (number-literal text kept, NaN/Infinity extensions), encode (compact/pretty,
  `$JQ_COLORS`), and the two number-text accessors jqrun needs. The fold
  comparators are gone (#861): gojq owns ordering and grouping now.
- `crew/internal/bus`: bus location, crew-id resolution, typed event reads.
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
- `crew/internal/marks`: the delivered-marks file of #290 — `_await_state`'s
  path, `_await_marks`' read and `_await_record`'s atomic merge, the two jq
  programs embedded verbatim. `await`, `nudge` and `stall-watch` still read and
  write the same file from bash, so the name and the content are the contract.
- `crew/internal/testjson`: test-only value-equal JSON comparison.

New subcommands get an `internal/<sub>` package; shared reads go through `bus`;
folds that outgrew hand-translation run on jqrun.

## Delegation

crew.sh stays the entrypoint (direction b). A ported arm is:

```bash
crews | log | report | sessions | roster | inbox)
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
```

`flake.nix` substitutes `@crewGoBin@` with the `crew-go` package's
`bin/crew-go`; `CREW_GO_BIN` overrides it for raw-source runs. The bash
preamble (`--help`, the git-repo check) runs first. Add a subcommand to this
arm and delete its old arm.

## Output-identity rules

- Value identity with the bash arm: same JSON values, same exit status, on
  every bus where it exits 0. Object **key order is free** (gojq hands objects
  back as Go maps); bats guards compare with `jq -S`. Emit through `jsonv`
  only, never `encoding/json`.
- Run the arm's jq program itself, embedded and verbatim apart from
  documented patches (e.g. the sessions `capture` anchor: Go regexp `$` is
  end-of-text, Oniguruma's also matches before a trailing newline, so the
  embedded copy uses `\n?\z`). Editing a `.jq` file edits the fold.
- Ported-by-hand remainders must keep jq op for op semantics: stable sorts,
  `//` treats `false` as absent, word-split guards, `from_entries` last wins.
- Known gojq divergences (spike #861): NaN sorts below every value in
  `sort_by` where jq treats it as equal (unreachable: the bus writes
  `now*1000` ts); everything else the spike found value- and literal-exact,
  number literals included.
- External calls: `tmux` and `git worktree list` keep argv, count and order.
  Equivalent git queries may differ: Go runs `git -C <cwd>` and repeats the
  preamble's common-dir lookup.
- A bus on which jq fails exits 5 (corrupt) or 2 (unreadable; `sessions` still
  prints `[]`) with one `crew: <sub>: <log>: ...` stderr line and otherwise
  empty stdout. Sanctioned divergences: that wording differs from jq's; the
  `JQ_COLORS` warning prints once, not once per jq process; and, unreachable
  with real git branches, the shell rewrites the arm applies to bus-supplied
  branch strings (glob expansion of `$(...)` words, NUL bytes dropped by
  `$(...)`, awk `-v` escape processing) are not mirrored. `crew crews` also
  sorts its id union and `--mine` scan in byte order, not the caller's
  `sort -u`/glob locale collation — observable only for id sets whose C
  order differs from the run locale's, among no-stats rows or `last` ties;
  a non-string `crew_id` contributes no id (the arm's `jq -r` printed
  numbers and JSON fragments as id lines, an object spanning several); and
  when the final pass fails (a stats row with a null `ts`), Go prints no
  rows where the arm's `jq -r` had already streamed the rows before the
  failing one — exit 5 and the one stderr line match.
- `crew log` mirrors jq's _last-input_ exit rule, not a sticky one: a row it
  cannot index (`5`, `[]`, `"s"`) prints one `crew: log: <log>: cannot index
<type> with "crew_id"` line, and only such a row at the end of the stream
  makes the command exit 5 — the arm's `jq` did the same, and its
  `jq: error (at <log>:<line>): Cannot index … with string ("crew_id")` wording
  is the sanctioned difference. A torn tail prints the prefix, then the parse
  error, then 5, exactly like `jq -c`.
- `crew report`'s fold yields one joined string (the `join("\n")` patch for
  jqrun's one-value rule), so when a _later_ dispatch row makes the fold fail,
  the arm's `jq -r` had already streamed the rows before it while Go prints the
  header only — exit 5 and the one stderr line match, as with `crews` above.
  The joined string is empty only when there are no rows at all, since a
  6-column `@tsv` row always carries its five tabs.
- `crew inbox`'s delivered marks are the one file another component still reads
  while this port writes it, so the contract is the file, not stdout: same path
  (`_await_state`'s sanitized key plus its cksum), same value, and the same
  "any failure means redeliver" rule. Two byte-level differences are sanctioned
  because every reader parses it with jq: gojq returns object keys sorted where
  the arm's reduce keeps insertion order, and a msg whose ts is the literal
  Infinity lands as 1.7976931348623157e+308 where jq writes the same value with
  an uppercase exponent. Value-identical either way, pinned by a crew.bats row
  against `_await_state`/`_await_record` and by `internal/marks` running the
  helpers' own programs through jqrun.
- Before deleting a bash arm, diff it against Go over a generated bus corpus
  (mask `age_s`) and keep the evidence.

## Bash helpers that stay

`_sessions`, `_identity*`, `_crew_id`, `_is_engine_cmd` and `_pane_is_engine_at`
keep other callers (`reply`, `nudge`, `reap`). Delete each only with its last
caller. While two copies exist, guard drift:

- a crew.bats test compares the bash `_sessions` helper with `crew sessions` on
  a shared fixture bus (value compare, `jq -S`: key order is engine-internal);
- Go tests parse crew.sh's pools and engine table and assert the Go copies
  match (skipped when crew.sh is absent, as in the Nix sandbox).

`_await_state`, `_await_marks` and `_await_record` stay for `await`, `nudge` and
`stall-watch --unread` even though `crew inbox` no longer calls them: it writes
the same file from `internal/marks`. Two of the three drift guards above apply —
crew.bats compares the Go marks file with `_await_state`'s path and
`_await_record`'s content on the same msgs, and `internal/marks` runs the
helpers' own two programs through jqrun.

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
