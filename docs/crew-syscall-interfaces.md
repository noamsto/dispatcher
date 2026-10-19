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
| `stream`, `watch`, `hold`, `report`, `entry.new` |                                                          |                            | Origin (stream)   |                                    | Now, Sleep, Timer | Env                                       |
| `entry` dash, `repos`, `reap.Options`            |                                                          |                            |                   | Exec, LookPath, Resolve            |                   | Getenv, Environ, Env; RateRun             |
| `crews.Options`, `where`/`nudge.Options`         |                                                          |                            |                   |                                    | Now               | Remote (ssh)                              |

¹ fields on `identity.Options`. ² `rate.Run` runs gh and git alike.
Holding a `clock.Clock` value already: `await`, `nudge`, `prwatch`,
`reply`, `rosterrender.Options`, `stall.Options` (`hold` builds one from its `Now` and `CrewClock` fields).
Not a seam field: `panestate.Publish(set func(option, value string), …)` takes
the pane-option setter as a parameter; its callers pass a tmux closure.

Real calls behind them: tmux wrappers in `main.go` (`tmuxOutput`), `adopt/commands.go`,
`nudge/wire.go`, `reap/probes.go`, `rosterrender/probe.go`, `stall/probe` (own
process-group runner); gh in `reap`, `stack`, `adopt`; git in `main.go`,
`adopt`, `nudge`, `reap` (six sites), `rate`, `bus`, `claimindex`, `gitguard`,
`remote`, `wtgit`; ps and `/proc` reads in `crews`,
`reap`, `adopt`; `time.Now`/`time.Sleep` and `clock.Clock` everywhere.

## Interfaces

Each package takes only the interface it uses, as a field named for the
concern (`Tmux`, `Gh`, `Git`, `Proc`, `Clock`). The single-method interfaces are
shared, not redeclared consumer-side: one declaration is shorter than one per
consumer. Fakes live in a `<concern>test` package imported only from tests.

| concern     | interface                                                                                                                                                                                                                                                                                                       | real                                                             | fake                                                           |
| ----------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- | -------------------------------------------------------------- |
| tmux (done) | `tmux.Client{ Run(ctx, args...) (string, error) }` + `tmux.Output` (failure = "")                                                                                                                                                                                                                               | `tmux.Exec{}`                                                    | `tmuxtest.Fake{Out, Func}` + `Calls()`                         |
| gh          | `gh.Client{ Run(ctx, args...) (string, error); Combined(ctx, args...) (string, error) }`; quiet = `Run` with the output dropped; `HaveGh` = `errors.Is(err, exec.ErrNotFound)`                                                                                                                                  | `gh.Exec{}`                                                      | `ghtest.Fake` (same shape as `tmuxtest`)                       |
| git         | `git.Client{ Run(ctx, dir string, args...) (string, error) }` + funcs `Output`, `Worktrees`, `Toplevel`, `Origin`, `RepoSlug` over it; a non-zero exit is a `*git.ExitError`; `wtgit` and `gitguard` keep their own runners                                                                                     | `git.Exec{}`                                                     | `gittest.Fake` keyed by dir + argv                             |
| proc        | `proc.Table{ Alive(pid) bool; Elapsed(pid) (int64, bool); Parent(pid) (int, bool); Comm(pid) (string, bool); Args(pid) string; Cwds(ctx) []Cwd; Kill(pid) }`, plus `IsAncestor`/`OwnerPID` as funcs over a `Table`                                                                                              | `proc.OS{}` (from `crews.DefaultProbes`, `reap`, `adopt.PsArgs`) | `proctest.Table{Procs}` (map pid → proc) + `Killed()`          |
| clock       | packages take the existing `clock.Clock` value instead of bare `Now` funcs; its `Sleep`/`SleepCtx` already advance a `CrewClock` file instead of sleeping. The wall-clock `time.Duration` sleeps (`reap`, `rosterrender`, which must never follow `CREW_CLOCK`) and `stream`'s timers are decided in that slice | new `clock.FromEnv()` (what `main.go` builds by hand today)      | `clock.Clock{Now: fixed, CrewClock: tmpfile}` (no new package) |
| env         | no interface: `Getenv func(string) string` is already the stdlib shape; rename `Env`/`LookupEnv` fields to `Getenv` and drop duplicates                                                                                                                                                                         | `os.Getenv`                                                      | a map's lookup                                                 |

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
- Shape changes: `probe.Probes` embeds `Env`; `roster.Probes` lost `Panes` and
  `LiveBranches` for `Tmux` and `Live bool`; `fleet` memoises each argv once
  per run, as its `Panes`/`Windows` closures did.
- One execution change: `stall/probe`'s tmux calls ran in their own process
  group, killed as a group on cancel; they now share `Exec`'s plain kill plus
  `WaitDelay`, as every other site already did. A tmux client forks no
  children, so only a forking PATH stub could tell.
- Lines (crew/, `wc -l` over `*.go`): non-test 27415 → 27404 (−11), test 44252 → 44242 (−10), with the new
  `tmux` and `tmuxtest` packages counted (`tmuxtest` is not a `_test.go` file).

## The proc slice

- `internal/proc`: `Table`, `OS`, and `IsAncestor`/`OwnerPID` over a `Table`
  (they moved out of `crews`). `Cwds` replaces reap's `Procs`/`Ancestors` pair
  (typed rows; `CREW_REAP_PROC_CMD` moved with it); reap's ancestor set became
  `Ancestors`, so ancestry reads through `Parent` (`ps`) rather
  than `/proc/<pid>/status`.
- `internal/proc/proctest`: `Table{Procs map[int]Proc}` (a pid is alive when it
  is a key), `New(live, parents)` and `Killed()`.
- `crews.Probes` is gone: `crews.Options.Proc`, and `crews.RecordedLive`/
  `PidAlive` take the `Table`. `Mtime` was a seam only to fake "unreadable"; the
  recycle check stats the pidfile itself.
- `sender.Probes.Proc` replaces `Parent` and `PID`: the caller's pid is
  `os.Getpid()`, so tests key their ancestry on it. `adopt`/`register` lost
  `Args` for `Proc.Args`; `reply`, `stream` and `rosterrender` hold a `Proc`.
- Left as they are: `stall/probe.Load`/`Top` (host load and CPU, behind the
  `CREW_STALL_*_CMD` bash seams, not the process table), `lock`'s holder probe,
  and the `syscall.Kill` of `stream --force` (a TERM then a delivery poll) and
  of the relay/stall process-group kills.
- Lines (crew/, `wc -l` over `*.go`): non-test 27404 → 27403 (−1), test 44242 →
  44223 (−19), with the new `proc` and `proctest` packages counted.

## The git slice

- `internal/git`: `Client`, `Exec` (cmd.Dir = dir; stdout trailing-newline-trimmed
  and kept on failure; a non-zero exit is `*ExitError{Code, Stderr}` with git's
  stderr captured), and `Output`, `Worktrees`, `Toplevel`, `Origin` over a
  `Client`. `RepoSlug` is the `_origin_repo` sed rate and reap each carried.
  `git.ExitError` replaces `roster.ExitError`.
- `internal/git/gittest`: `Fake` answers by the exact call key (argv, prefixed
  `<dir>: ` when dir is set) in `Out`, or `Func`; `Calls()` records the keys.
- Every site passes the argv it passed before (`-C dir` stays in argv where it
  was; `cmd.Dir` becomes dir). `rate`, `status`, `nudge`, `adopt`, `reap`,
  `fleet`, `rosterrender` and `stream`'s fleet lane hold one `Git git.Client`;
  `claimindex.Path`/`Release` take one; `bus` and `remote`, with no test seam,
  call `git.Exec{}`. `main.go` lost `originOf`, `commonWorktrees`, `worktreeList`.
- Kept: `roster.Probes.Worktrees` and `roster.Liveness.Worktrees`, the per-pass
  memo of one `git.Worktrees` read (fleet and rosterrender share it between the
  fold and the liveness rule); `bus.Toplevel` (`git -C cwd rev-parse`: `bus.CrewID` and reap's
  autosweep log line).
- Exceptions (still exec git themselves): `wtgit` (the anchored porcelain layer:
  `--git-dir`, neutral `-c`, scrubbed env) and `gitguard`, which streams git's
  stderr live to its caller and is called from the unchanged `wtgit`. No
  converted site carried `-c` or env scrubbing; the anchored `--git-dir` sites
  (claimindex, release, fleet) have tests pinning the exact argv.
- One output change: reap's `worktree list` and `rev-parse` streamed git's
  stderr; they now write the captured stderr after a failed exit, so a warning
  on a successful run is no longer forwarded.
- Lines (crew/, `wc -l` over `*.go`): non-test 27403 → 27460 (+57), test
  44223 → 44325 (+102), with the new `git` and `gittest` packages counted. The
  rest of the diff is net negative; the new package (150 lines) and the AC2
  exact-argv tests are the cost, since each replaced site was one to three lines.

## Slice order

One PR each, largest duplication first. Savings are estimates from the field
and fake counts above (net Go lines, non-test + test).

| #   | concern | packages                                                                                                            | fakes replaced                                                                             | est. net lines |
| --- | ------- | ------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ | -------------- |
| 1   | tmux    | 14 + main                                                                                                           | 20 test files                                                                              | measured above |
| 2   | proc    | crews, reap, sender, adopt, register, reply, stream, rosterrender, main                                             | ~12 test files (`crews_test`, `register_test`, `adopt_test`, `reply_test`, `main_test`, …) | measured above |
| 3   | git     | main, roster, fleet, adopt, nudge, status, rate, reap, bus, claimindex, gitguard, remote; `stream`'s `Origin` field | ~10 test files                                                                             | measured above |
| 4   | gh      | reap, stack, adopt, rate                                                                                            | ~4 test files                                                                              | −30 to −60     |
| 5   | clock   | 15 `Now`, 5 `Sleep`/`Timer` fields and `Rediscover`, 6 `clock.Clock` holders                                        | most test files set `Now`                                                                  | −20 to −50     |
| 6   | env     | 17 `Getenv`/`LookupEnv`/`Env`/`Setenv`/`Environ` fields                                                             | —                                                                                          | −10 to −30     |

Each slice follows the tmux slice's shape: add the package and fake, convert
one dependency cluster per commit (consumers that share an entry point move
together), wire `main.go` last, then check that only the new package execs the
binary (`rg 'exec\.(Command|CommandContext|LookPath)\([^)]*"<bin>"'`; the git
slice exempts `wtgit`, the porcelain layer the new client sits beside).
