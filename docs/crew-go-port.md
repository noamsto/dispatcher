# Porting crew to Go

Issue: #822. `adapters/core/crew.sh` moves to Go one subcommand at a time.
Ported so far: `sessions` and `roster`. Each slice must leave every bats file
green with its tests unmodified.

## Layout

- `crew/main.go`: argv dispatch. Module `github.com/noamsto/dispatcher/crew`,
  stdlib only.
- `crew/internal/jsonv`: ordered JSON values with jq semantics (decode, total
  order, compact and pretty encoding, number and string rules, TTY colours).
- `crew/internal/bus`: bus location, crew-id resolution, typed event reads.
- `crew/internal/identity`: codename, colour and tmux pools, the cksum slot.
- `crew/internal/roster`, `crew/internal/sessions`: the folds, pure functions
  of events, args, `now` and injected tmux/worktree probes.

New subcommands get an `internal/<sub>` package; shared reads go through `bus`.

## Delegation

crew.sh stays the entrypoint (direction b). A ported arm is:

```bash
sessions | roster)
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
```

`flake.nix` substitutes `@crewGoBin@` with the `crew-go` package's
`bin/crew-go`; `CREW_GO_BIN` overrides it for raw-source runs. The bash
preamble (`--help`, the git-repo check) runs first. Add a subcommand to this
arm and delete its old arm.

## Byte-identity rules

- Same stdout, stderr and exit status as the bash arm on every bus where it
  exits 0. Emit through `jsonv` only, never `encoding/json`.
- Port each jq program op for op: stable sorts, `//` treats `false` as absent,
  `max_by` ties to last, `from_entries` last wins, `detail` cut by codepoint.
- External calls: `tmux` and `git worktree list` keep argv, count and order.
  Equivalent git queries may differ: Go runs `git -C <cwd>` and repeats the
  preamble's common-dir lookup.
- A bus on which jq fails exits 5 with empty stdout and one `crew: <sub>: <log>:
...` stderr line. Sanctioned divergences: that wording differs from jq's; the
  `JQ_COLORS` warning prints once, not once per jq process; and, unreachable
  with real git branches, the shell rewrites the arm applies to bus-supplied
  branch strings (glob expansion of `$(...)` words, NUL bytes dropped by
  `$(...)`, awk `-v` escape processing) are not mirrored.
- Before deleting a bash arm, diff it against Go over a generated bus corpus
  (mask `age_s`) and keep the evidence.

## Bash helpers that stay

`_sessions`, `_identity*`, `_crew_id`, `_is_engine_cmd` and `_pane_is_engine_at`
keep other callers (`reply`, `nudge`, `reap`). Delete each only with its last
caller. While two copies exist, guard drift:

- a crew.bats test compares the bash `_sessions` helper with `crew sessions` on
  a shared fixture bus;
- Go tests parse crew.sh's pools and engine table and assert the Go copies
  match (skipped when crew.sh is absent, as in the Nix sandbox).

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
