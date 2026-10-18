# crew: shared system-call interfaces

`crew` packages reach the outside world (tmux, gh, git, processes, the clock,
the environment) through structs of function fields, one struct per package,
each with its own test fakes. This doc groups those fields by concern and
replaces each concern with one small interface, one real implementation and
one shared fake. Each concern lands as its own PR, behaviour unchanged (exact
exit codes, agent-facing text, value-equal JSON, the same argv to every child)
and net-negative in Go lines. No `util`/`common` package: each interface lives
in a package named for its concern.

## Inventory (main, 2026-10-18)

| struct                                           | tmux                                                     | gh                         | git               | proc                               | clock             | env / other                               |
| ------------------------------------------------ | -------------------------------------------------------- | -------------------------- | ----------------- | ---------------------------------- | ----------------- | ----------------------------------------- |
| `where.Probes`                                   | Windows, Panes                                           |                            |                   |                                    |                   |                                           |
| `identity.Probes`                                | Windows                                                  |                            |                   |                                    | Now¹              | Getenv¹                                   |
| `sender.Probes`                                  | Panes                                                    |                            |                   | Parent, PID                        |                   | Getenv                                    |
| `status.Options`                                 | Tmux                                                     |                            | Toplevel          |                                    | NowMS             | Getenv, Fold (jq seam)                    |
| `roster.Probes`                                  | Panes, LiveBranches                                      |                            | Worktrees         |                                    |                   |                                           |
| `fleet.Options`                                  | Panes, Windows                                           |                            | Worktrees, Origin |                                    | Now               | Env                                       |
| `rosterrender.Probes`                            | RolePanes, Windows, PanePID                              |                            |                   | SpawnDetached, Exec, InstalledCrew | Sleep             | AeyeHelp, AeyePublish                     |
| `adopt.Options`                                  | Windows, SelfWindow                                      | Gh, GhQuiet                | Worktrees         | Args                               | Now               | LookupEnv                                 |
| `nudge.Deps` / `Options`                         | Tmux, Capture                                            |                            | Toplevel          |                                    | Sleep, Clock      | Getenv, Exists; bus: Events, Append, Rows |
| `reap.Probes`                                    | Tmux, TmuxRun, Capture                                   | Gh, GhAll, GhQuiet, HaveGh |                   | Procs, Ancestors, Kill             | Sleep, Now        | Gtrash; bus: Append                       |
| `reap.Deps`                                      | — (wraps `reap.Probes`)                                  |                            |                   |                                    |                   | Say, Note                                 |
| `stall/probe.Probes`                             | Sample, SampleColored, PaneCmd, PaneModel, SetPaneOption |                            |                   | Load, Top                          |                   | RefreshBudget, Sh (bash libs), BusRows    |
| `stall/probe.Env`                                | Pane                                                     |                            |                   |                                    |                   | Getenv, Bash, libs, Dir, Stderr           |
| `stack.Probes`                                   |                                                          | Gh                         |                   |                                    | Now               | bus: Append                               |
| `crews.Probes`                                   |                                                          |                            |                   | Alive, Elapsed, Parent, Comm       |                   | Mtime (fs)                                |
| `entry.SelfProbes`                               |                                                          |                            |                   | LookPath, Abs                      |                   | Getenv, Setenv                            |
| `rate.Options`                                   |                                                          | Run²                       | Git               |                                    | Now               | LookupEnv                                 |
| `register.Options`                               |                                                          |                            |                   | Args                               | Now               | Getenv                                    |
| `remote` exec                                    |                                                          |                            |                   | LookPath, Self                     |                   |                                           |
| `stream`, `watch`, `hold`, `report`, `entry.new` |                                                          |                            |                   |                                    | Now, Sleep, Timer | Env                                       |

¹ fields on `identity.Options`. ² `rate.Run` runs gh and git alike.
Not a seam field: `panestate.Publish(set func(option, value string), …)` takes
the pane-option setter as a parameter; its callers pass a tmux closure.

Real calls behind them: tmux wrappers in `main.go` (`tmuxOutput`), `adopt/commands.go`,
`nudge/wire.go`, `reap/probes.go`, `rosterrender/probe.go`, `stall/probe` (own
process-group runner); gh in `reap`, `stack`, `adopt`; git in `main.go`,
`adopt`, `nudge`, `reap`, `rate`, `bus`, `wtgit`; ps and `/proc` reads in `crews`,
`reap`, `adopt`; `time.Now`/`time.Sleep` and `clock.Clock` everywhere.

## Interfaces

Each package takes only the interface it uses, as a field named for the
concern (`Tmux`, `Gh`, `Git`, `Proc`, `Clock`). The single-method interfaces are
shared, not redeclared consumer-side: one declaration is shorter than one per
consumer. Fakes live in a `<concern>test` package imported only from tests.

| concern     | interface                                                                                                                                                                                   | real                                                             | fake                                                        |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- | ----------------------------------------------------------- |
| tmux (done) | `tmux.Client{ Run(ctx, args...) (string, error) }` + `tmux.Output` (failure = "")                                                                                                           | `tmux.Exec{}`                                                    | `tmuxtest.Fake{Out, Func}` + `Calls()`                      |
| gh          | `gh.Client{ Run(ctx, args...) (string, error); Combined(ctx, args...) (string, error) }`; quiet = `Run` with the output dropped; `HaveGh` = `errors.Is(err, exec.ErrNotFound)`              | `gh.Exec{}`                                                      | `ghtest.Fake` (same shape as `tmuxtest`)                    |
| git         | `git.Client{ Run(ctx, dir string, args...) (string, error) }` + funcs `Worktrees`, `Toplevel`, `Origin` over it; `wtgit` keeps the porcelain layer                                          | `git.Exec{}`                                                     | `gittest.Fake` keyed by dir + argv                          |
| proc        | `proc.Table{ Alive(pid) bool; Elapsed(pid) (int64, bool); Parent(pid) (int, bool); Comm(pid) (string, bool); Args(pid) string; Kill(pid) }`, plus `Procs`/`Ancestors` as funcs over `/proc` | `proc.OS{}` (from `crews.DefaultProbes`, `reap`, `adopt.PsArgs`) | `proctest.Table` (map pid → proc)                           |
| clock       | `clock.Clock` gains `Sleep(ctx, d)`; packages take a `clock.Clock` instead of bare `Now`/`Sleep` funcs                                                                                      | `clock.Real()`                                                   | `clock.Clock{Now: fixed}` (existing struct; no new package) |
| env         | no interface: `Getenv func(string) string` is already the stdlib shape; rename `Env`/`LookupEnv` fields to `Getenv` and drop duplicates                                                     | `os.Getenv`                                                      | a map's lookup                                              |

Left as they are: bus reads/appends (in-repo, not a system call), `stall/probe.Sh`
and the `CREW_STALL_*_CMD` seams (bash libs and test seams under `bash -c` with
their own group-kill runner), aeye, gtrash, `rosterrender`'s exec hop.

## The tmux slice (this PR)

- `internal/tmux`: `Client`, `Exec` (stdout trailing-newline-trimmed and kept
  on failure, as `$(…)` keeps it; a `WaitDelay` bounds Wait), `Output` for the
  sites that blanked a failed read.
- `internal/tmux/tmuxtest`: `Fake` answers by the longest word prefix of the
  argv in `Out`, or `Func`; an unmatched call fails like tmux with no server.
- `stall/probe.Capture` is `_pane_capture` once (seam or `capture-pane`),
  shared by stall, nudge and reap; `Sample`, `SampleColored`, `PaneCmd`,
  `PaneModel`, `SetPaneOption` are methods on `probe.Probes`.
- Every package above holds one `Tmux tmux.Client`; the argv moved from
  `main.go` into the package that reads it. Entry points that now read tmux
  take `ctx` first (`where`, `identity`, `adopt`, `fleet`, `roster.Fold`,
  `sender.Check`/`Watch`, `status`, `hold`, `reply`).
- Kept: `roster.Liveness.Windows` (a memo of a parse, built from `roster.Windows`),
  `stall.Options.NewProbes` (constructor taking a pane id).
- Lines (crew/, `wc -l` over `*.go`): non-test 27415 → 27401 (−14), test 44252 → 44240 (−12), with the new
  `tmux` and `tmuxtest` packages counted (`tmuxtest` is not a `_test.go` file).

## Slice order

One PR each, largest duplication first. Savings are estimates from the field
and fake counts above (net Go lines, non-test + test).

| #   | concern | packages                                                | fakes replaced                                                                             | est. net lines |
| --- | ------- | ------------------------------------------------------- | ------------------------------------------------------------------------------------------ | -------------- |
| 1   | tmux    | 13 + main                                               | ~19 test files                                                                             | measured above |
| 2   | proc    | crews, reap, sender, adopt, register, stall/probe, main | ~12 test files (`crews_test`, `register_test`, `adopt_test`, `reply_test`, `main_test`, …) | −60 to −120    |
| 3   | git     | main, roster, fleet, adopt, nudge, status, rate, stream | ~6 test files                                                                              | −40 to −90     |
| 4   | gh      | reap, stack, adopt, rate                                | ~4 test files                                                                              | −30 to −60     |
| 5   | clock   | ~15 `Now` and 4 `Sleep` fields                          | most test files set `Now`                                                                  | −20 to −50     |
| 6   | env     | ~17 `Getenv`/`LookupEnv`/`Env` fields                   | —                                                                                          | −10 to −30     |

Each slice follows the tmux slice's shape: add the package and fake, convert
one dependency cluster per commit (consumers that share an entry point move
together), wire `main.go` last, then check that only the new package execs the
binary (`rg 'exec\.(Command|CommandContext|LookPath)\([^)]*"<bin>"'`).
