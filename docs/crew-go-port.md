# Porting crew to Go

Issue: #822. `adapters/core/crew.sh` moves to Go one subcommand at a time.
Ported so far: `sessions` and `roster`. Each slice must leave every bats file
green with its tests unmodified.

## Layout

- `crew/main.go`: argv dispatch for the ported subcommands. Module
  `github.com/noamsto/dispatcher/crew`, stdlib only.
- `crew/internal/jsonv`: ordered JSON values with jq semantics (decode, total
  order, compact and pretty encoding, number and string rules, TTY colours).
- `crew/internal/bus`: bus location, crew-id resolution, typed event reads.
- `crew/internal/identity`: codename, colour and tmux pools, the cksum slot.
- `crew/internal/roster`, `crew/internal/sessions`: the folds, pure functions
  of events, args, `now` and injected tmux/worktree probes.

New subcommands get their own `internal/<sub>` package. Shared reads go through
`bus`; dash's bus reads move there once they are genuinely shared.

## Delegation

crew.sh stays the entrypoint (direction b). A ported arm is:

```bash
sessions | roster)
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
```

`flake.nix` substitutes `@crewGoBin@` with the `crew-go` package's
`bin/crew-go`. `CREW_GO_BIN` overrides it for raw-source runs. The bash
preamble (`--help`, the git-repo check) still runs first. Add the subcommand
to this arm and delete its old arm.

## Byte-identity rules

- Same stdout, stderr and exit status as the bash arm on every bus where it
  exits 0. Emit through `jsonv` only, never `encoding/json`.
- Port each jq program op for op: stable sorts, `//` treats `false` as absent,
  `max_by` ties to last, `from_entries` last wins, `detail` cut by codepoint.
- Keep external calls (`git`, `tmux`) at the same argv, count and order.
- A bus on which jq fails exits 5 with empty stdout and one `crew: <sub>: ...`
  stderr line. Two sanctioned divergences: that wording differs from jq's, and
  the `JQ_COLORS` warning prints once rather than once per jq process.
- Before deleting a bash arm, run a differential of the old arm against Go over
  a generated corpus of buses (mask `age_s`) and keep the evidence.

## Bash helpers that stay

`_sessions`, `_identity*`, `_crew_id`, `_is_engine_cmd` and `_pane_is_engine_at`
keep other callers (`reply`, `nudge`, `reap`). Delete each only with its last
caller. While two copies exist, guard drift:

- a crew.bats test compares the bash `_sessions` helper with `crew sessions` on
  a shared fixture bus;
- Go tests parse crew.sh's name, colour and tmux pools and engine table, and
  assert the Go copies match (skipped when crew.sh is absent, as in the Nix
  sandbox).

## Running the suite

- `bats tests/...`: `tests/setup_suite.bash` builds `crew-go` from the working
  tree once per run into `$BATS_SUITE_TMPDIR/bin` and exports `CREW_GO_BIN`, so
  the suite never runs a stale binary. A failed build fails the run.
- `cd crew && go test ./... && go vet ./... && golangci-lint run ./...`.
  `golangci-lint` enables `exhaustive`; keep `switch` on `Kind` and `State`
  complete.
- `scripts/bats-affected.sh` maps `crew/*` to the crew bats files.

## Flipping to direction (a)

1. The Go binary becomes the `crew` package. It runs ported subcommands and
   `syscall.Exec`s crew.sh, packaged as an internal `crew-sh` whose path is
   baked in via ldflags, for everything else.
2. The delegating arms in crew.sh disappear.
3. crew.sh's `$0` self-re-execs (`roster`, `dash`, `reap`, `hold`, `watch`,
   `rate`) switch to a `CREW_SELF`-style env the Go front exports.
4. `run_crew` and `CREW_REAL` in the bats files switch to the Go binary, one
   mechanical edit per file.
