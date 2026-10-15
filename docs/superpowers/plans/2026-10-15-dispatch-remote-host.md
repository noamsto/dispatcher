# Remote workers (`dispatch --host`) — implementation plan

**Spec:** `docs/superpowers/specs/2026-10-15-dispatch-remote-host-design.md`.
Its section numbers are cited below as §N.

**Conventions for every step.**

- Work from the repo root and run commands in `nix develop -c …` (or an
  already-loaded direnv shell).
- Go tests run from `crew/`.
- Bats files run with `bats <file>`; a single case runs with `--filter '<regex>'`.
- Never read `.env*`, credentials or keys, and never print secret values.
- After touching a shell script, run `shellcheck` on it.
- Match the surrounding code's naming, comment density and error style.

## File list

| file                                                                                                                       | purpose                                                                                                                               |
| -------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `adapters/core/dispatch-config.sh`                                                                                         | validate the new `hosts` key (§1)                                                                                                     |
| `nix/hm-module.nix`                                                                                                        | `programs.dispatcher.hosts` option → locked layer (§1)                                                                                |
| `README.md`                                                                                                                | Settings: document `hosts`                                                                                                            |
| `tests/dispatch-config.bats`                                                                                               | `hosts` validation cases                                                                                                              |
| `tests/module.bats`                                                                                                        | `hosts` option and locked-layer emission                                                                                              |
| `crew/internal/remote/remote.go` (new)                                                                                     | settings load, validation, route lookup, request build, ssh argv, `Run` (client core)                                                 |
| `crew/internal/remote/exec.go` (new)                                                                                       | `Serve`: the `crew remote-exec` server (§2)                                                                                           |
| `crew/internal/remote/row.go` (new)                                                                                        | `DispatchRow`: the `crew dispatch-row` query (§3.6)                                                                                   |
| `crew/internal/remote/*_test.go` (new)                                                                                     | unit tests for the above                                                                                                              |
| `crew/internal/watch/watch.go`                                                                                             | export the fold; add `Relay` (`crew relay`, §6)                                                                                       |
| `crew/internal/watch/relay_test.go` (new)                                                                                  | relay tests                                                                                                                           |
| `crew/internal/identity/identity.go`                                                                                       | `CREW_IDENTITY_AVOID`, bridge names, `remote:true` 24 h occupancy, `Occupied` (§3.5)                                                  |
| `crew/internal/identity/identity_test.go`                                                                                  | tests for the above                                                                                                                   |
| `crew/internal/resolve/resolve.jq`, `resolve.go`, `resolve_test.go`                                                        | 5th TSV column `remote` (§5)                                                                                                          |
| `crew/internal/reply/reply.go`, `reply_test.go`                                                                            | route remote targets (§5)                                                                                                             |
| `crew/internal/nudge/arm.go` (+ test)                                                                                      | two-sided resolution and route (§5)                                                                                                   |
| `crew/internal/where/where.go`, `where_test.go`                                                                            | bridge-window lookup, then route (§5)                                                                                                 |
| `crew/internal/roster/roster.go`, `roster_test.go`                                                                         | `--live` field; remote merge helper (§7)                                                                                              |
| `crew/internal/stream/relay.go` (new), `stream.go`, `fleet.go`, `relay_test.go` (new)                                      | relay sources in the per-bus loop (§6)                                                                                                |
| `crew/main.go`, `crew/commands.go`, `crew/main_test.go`                                                                    | new subcommands `remote-run`, `remote-exec`, `relay`, `dispatch-row`; flags `roster --live`, `identity --occupied`; wiring the probes |
| `crew/internal/entry/texts/{remote-run,remote-exec,relay,dispatch-row}.txt` (new), `texts/top.txt`, `entry/help.go`        | help rows for the new subcommands                                                                                                     |
| `adapters/core/dispatch.sh`                                                                                                | `--host`/`--recover` local path (§3); remote-mode tmux targeting, `dispatch_row:` emit, roster-render skip (§4)                       |
| `adapters/core/dispatch-resume.sh`                                                                                         | route a `remote:true` target (§5); remote-mode tmux targeting (§4)                                                                    |
| `adapters/core/permission-check.sh`                                                                                        | route a remote branch (§5)                                                                                                            |
| `adapters/core/protocols/DISPATCHER_PROTOCOL.md`, `WORKER_PROTOCOL.md` (+ generated mirrors via `scripts/gen-adapters.sh`) | §8                                                                                                                                    |
| `tests/remote-host.bats` (new)                                                                                             | AC2/AC3 end-to-end with an ssh shim                                                                                                   |
| `tests/crew.bats`                                                                                                          | grouped-help row expectations, only if its literal-parsing test needs updating                                                        |
| `scripts/bats-affected.sh`                                                                                                 | map `remote-host` from `dispatch.sh`, `dispatch-resume.sh`, `permission-check.sh` and `crew/*`                                        |
| `tests/shard-weights.tsv`                                                                                                  | add `remote-host.bats` rows if `bats-shard.sh --check` demands it                                                                     |

## Steps

### A. Settings (§1)

- [ ] **Step 1: failing `hosts` validation tests.**
  - In `tests/dispatch-config.bats`, add cases that use the existing
    `user_settings`/`locked_settings` helpers.
  - (a) A valid `hosts` from the user layer appears in the merged output:
    `{"hosts":{"mini":{"ssh":"me@mini","repos":{"acme/widget":"/r/widget"}}}}`.
  - Each of the following makes the resolver exit non-zero, with stderr naming
    `hosts`:
    - (b) a host name `-x`;
    - (c) `ssh` set to `-oProxyCommand=x`;
    - (d) `ssh` with a space;
    - (e) a repo key `widget` (no owner);
    - (f) a relative repo path `r/widget`;
    - (g) a repo path containing a newline;
    - (h) `hosts` set to a non-object.
  - Verify: `bats tests/dispatch-config.bats --filter hosts`. Expect (a)
    passes, since an unknown key already merges through, and (b)–(h) fail.

- [ ] **Step 2: implement validation.**
  - In `adapters/core/dispatch-config.sh`, add a `host_violations` jq function,
    modelled on `lane_profile_violations`, applied to the merged tree next to
    the other violation checks.
  - Use the regexes from §1:
    - host name `^[A-Za-z0-9][A-Za-z0-9._-]*$`;
    - ssh `^[A-Za-z0-9_][A-Za-z0-9._@%+-]*$`;
    - repo key `^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`;
    - value: starts with `/` and has no control characters;
    - `hosts` must be an object of objects.
  - Do **not** add `hosts` to `strip()`.
  - Verify: `bats tests/dispatch-config.bats` is all green, and
    `shellcheck adapters/core/dispatch-config.sh` is clean.

- [ ] **Step 3: home-manager option.**
  - In `nix/hm-module.nix`, add
    `hosts = mkOption { type = nullOr (attrsOf (submodule { options = { ssh = mkOption { type = str; }; repos = mkOption { type = attrsOf str; default = {}; }; }; })); default = null; description = …; }`.
  - Add `// lib.optionalAttrs (cfg.hosts != null) { hosts = cfg.hosts; }` to
    `lockedSettings`, after the `lockFields` pattern.
  - In `tests/module.bats`, add `hosts` to `fullCfg`, and assert that
    `.full.locked.hosts.mini.ssh == "me@mini"` and the option name is listed,
    following the `localModels`/`laneProfiles` assertions.
  - In `README.md` "### Settings", add a `hosts` bullet.
  - Verify: `nix develop -c bats tests/module.bats --filter hosts` passes.

### B. Transport: `internal/remote` (§2)

- [ ] **Step 4: failing unit tests for the client core** (`crew/internal/remote/remote_test.go`).
  - **`Load`**, given a fake `DISPATCH_CONFIG_BIN` script printing JSON:
    - returns the hosts;
    - errors on a bad ssh value (defence in depth: re-validate with the same
      regexes).
  - **`Resolve(hosts, name, slug)`**:
    - unknown host → the error text from §2.3;
    - no mapping → the §2.3 text;
    - mapping lookup is case-insensitive on the slug.
  - **`SSHArgv("me@mini")`** equals exactly:
    `[-T -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o PermitLocalCommand=no -- me@mini crew remote-exec]`.
  - **`BuildRequest`**:
    - produces `{"v":1,"repo":…,"slug":…,"argv":[…],"env":{…}}`, with `spec`
      only when given;
    - rejects env keys outside the allowlist;
    - rejects values failing their regex: `CREW_ID`,
      `DISPATCH_TMUX_SESSION`, `DISPATCH_EMIT_ROW`, `DISPATCH_SESSION_ID`,
      `CREW_IDENTITY_AVOID`.
  - **`Route(rows, branch, crew)`** returns the host of the newest `dispatch`
    row for that branch and crew only when it has `remote:true`, and `""` for a
    local row.
  - **`RemoteHosts(rows, crew)`** returns the sorted distinct hosts of
    `remote:true` rows.
  - Verify: `cd crew && go test ./internal/remote/` fails (the package doesn't
    exist yet).

- [ ] **Step 5: implement `remote.go`** (implement: escalated).
  - Implement `Load`, `Resolve`, `SSHArgv`, `BuildRequest`, `Route`,
    `RemoteHosts`, and `Run(ctx, Call{Host, Cwd, Env, SpecFile, Argv}, stdout, stderr) int`.
  - `Run`:
    - loads settings;
    - derives the slug as `claimindex.Slug` of the cwd's `remote.origin.url`,
      resolved with `bus.Locate` → `git --git-dir=<common> config --get remote.origin.url`;
    - resolves, builds the request, and runs `exec.CommandContext("ssh", SSHArgv(…)...)`
      with the request on stdin and stdout/stderr passed through;
    - on exit 255, prints `crew: remote-run: ssh to host '<h>' (<ssh>) failed (exit 255)`;
    - returns ssh's status; usage and config errors return 2.
  - Settings come from `DISPATCH_CONFIG_BIN`, else `dispatch-config` on PATH.
    Copy `piagentdir`'s lookup.
  - The regexes live in one place, exported for `exec.go` to reuse.
  - Verify: `go test ./internal/remote/` passes; `go vet ./internal/remote/` is
    clean.

- [ ] **Step 6: failing tests for the server** (`crew/internal/remote/exec_test.go`).
  - `Serve(stdin, stdout, stderr, Options{LookPath, Self, TempRoot})` runs a fake
    `dispatch` script on a temp PATH, which dumps its argv NUL-separated, plus
    `$DISPATCH_SPEC` content and the allowlisted env, to a file.
  - Refusals, each with a non-zero exit and no child run:
    - `v:2`;
    - an unknown top-level key;
    - argv `["rm","-rf","/"]`;
    - argv `["crew","msg",…]`;
    - env `LD_PRELOAD`;
    - env `CREW_ID` value `a;b`;
    - a NUL inside a string;
    - stdin over 1 MiB;
    - a relative repo;
    - a repo whose origin slug ≠ the requested `slug` (message names both).
  - Accepted: argv containing `$(touch X)`, backticks, `;` and quotes arrives
    byte-identical in the dump, and file `X` does not exist.
  - `spec` arrives as the content of the file `DISPATCH_SPEC` points to, with
    mode 0600, and the scratch dir is gone after exit.
  - **Detached dispatch.** With argv `dispatch`, the fake child sleeps 2 s,
    then writes a marker. Run `Serve` in a subprocess (the `TestHelper…`
    re-exec pattern from `stream/fleet_test.go`), then kill that subprocess
    after 0.5 s. The marker still appears within 5 s.
  - Child stdout and stderr are relayed after the child exits.
  - The sweep removes a scratch dir older than 24 h (set mtime with
    `os.Chtimes`).
  - `CREW_WORKER_ID` set in the parent env reaches the child unchanged.
  - Verify: `go test ./internal/remote/ -run Serve` fails.

- [ ] **Step 7: implement `exec.go`** (implement: escalated).
  - Implement exactly §2 server steps 1–6:
    - 1 MiB `io.LimitReader` plus an overflow check;
    - `json.Decoder` with `DisallowUnknownFields`, into a struct with fields
      `V`, `Repo`, `Slug`, `Argv`, `Env`, `Spec`;
    - the NUL check, the repo check (`filepath.IsAbs`, a dir, `git -C repo rev-parse --git-common-dir`
      succeeds, the origin slug equals `Slug`), the argv allowlist and the env
      allowlist;
    - the scratch dir `<repo common dir>/crew/remote-exec/<pid>-<rand>`, with
      the 24 h sweep of its siblings;
    - spec written 0600, then `chdir`.
  - Run allowlisted `crew` subcommands as `Self` (`os.Executable`) and the
    others via `LookPath`. Use `exec.Cmd` with `Env = os.Environ() + allowlisted`;
    never a shell.
  - **Detached `dispatch`:** `SysProcAttr{Setsid: true}`, stdin `/dev/null`,
    stdout/stderr to 0600 files in the scratch dir. Wait for it, copy the
    files, remove the scratch dir.
  - **Everything else:** inherited stdio; the scratch dir is removed after
    `Wait`.
  - The exit status is the child's; 128+n for a signal.
  - Verify: `go test ./internal/remote/` passes.

- [ ] **Step 8: `crew dispatch-row` query** (`crew/internal/remote/row.go` + test).
  - `DispatchRow(log, crew, session, stdout) int` prints the newest
    `kind:"dispatch"` row with `crew_id==crew` and `session==session` as
    compact JSON plus `\n`, and exits 1 when there is none.
  - It validates `session` against `^s[0-9]+-[0-9]+$`, exiting 2.
  - Test it on a temp bus built with `bus.Append`.
  - Verify: `go test ./internal/remote/ -run DispatchRow` passes.

- [ ] **Step 9: wire the new subcommands into the grammar.**
  - In `crew/commands.go`, add:
    - `remoteRunCmd{Host string; Env []string; SpecFile *string; Argv []string (arg, passthrough:"all")}`;
    - `remoteExecCmd{}` (an argv-less typed cmd);
    - `relayCmd{Crew, Since, States, Interval, Heartbeat *string}`;
    - `dispatchRowCmd{Crew, Session *string}`.
  - In `crew/main.go`:
    - add the four names to `subcommands` and `typedSubcommands`, and the
      fields to `root`;
    - **Routing order matters.** sshd starts `crew remote-exec` in the remote
      user's `$HOME`, which is not a git repo, so `remote-exec` is routed
      **before** `os.Getwd`/`bus.Locate`, next to the `fleet`/`pi-agent-dir`
      arms. `Serve` chdirs into the requested repo itself.
    - `remote-run`, `dispatch-row` and `relay` need the local repo and go after
      `bus.Locate`.
    - Add a `main_test.go` case: `crew remote-exec`, run in a non-repo temp
      dir with an invalid request on stdin, exits with remote-exec's own
      refusal and not with the not-a-repo error.
    - `remote-run` takes the argv after `--`.
  - Add help texts `crew/internal/entry/texts/{remote-run,remote-exec,relay,dispatch-row}.txt`
    and their rows in `top.txt`/`help.go`, mirroring how `where.txt` is
    registered.
  - Update `TestAdmittedSubcommands`'s `admitted` list and its typed list.
  - Verify:
    - `go test . ./internal/entry/` passes;
    - `go build -o /tmp/crew-go . && /tmp/crew-go remote-run --host x -- crew where y`
      run outside any settings exits 2 with the unknown-host message;
    - `bats tests/crew.bats --filter 'help'` passes. If its grouped-help
      literal test fails, update the expectation it builds from the
      `subcommands` literal.

### C. Remote-side reads (§6, §7, §3.5)

- [ ] **Step 10: failing relay tests** (`crew/internal/watch/relay_test.go`).
  - `Relay(req RelayRequest{Crew, Since, States, Interval, Heartbeat}, paths, stdout, stderr, opts)`
    runs on a temp bus, with stubbed `Sleep` and `Now`.
  - (a) A blocked status row after `since` produces one
    `{"cursor":…,"events":[…]}` line identical to what `watch.Run` prints for
    the same bus.
  - (b) A second qualifying row produces a second batch with the advanced
    cursor; no event is repeated.
  - (c) `{"relay":"heartbeat","ts":…}` is printed when heartbeat seconds pass
    with nothing new.
  - (d) A writer returning `EPIPE` makes `Relay` return 0.
  - (e) No `watch.lock.d` and no `cursor` file are created.
  - Verify: `go test ./internal/watch/ -run Relay` fails.

- [ ] **Step 11: implement `Relay`.**
  - In `watch.go`, factor the poll body's `fold(paths.Log, c)` so `Relay`
    reuses it. Defaults are the same as watch: states, interval 2, and a
    30 s heartbeat.
  - No lock and no cursor write.
  - Wire `crew relay` in `main.go` to `watch.Relay`.
  - Verify: `go test ./internal/watch/` passes, so `watch` is unchanged.

- [ ] **Step 12: failing identity tests** (`identity_test.go`).
  - (a) With `CREW_IDENTITY_AVOID=<the branch's recorded name>` injected via
    `Options.Getenv`, `Assign` picks a different name.
  - (b) The tokens are added to the occupied set with no other change.
  - (c) A `remote:true` dispatch row younger than 24 h makes its name occupied
    with no status row. One older than 24 h does not.
  - (d) `windowNames` includes `@bridge_crew_name` values. The probe format
    becomes `#{@crew_name} #{@bridge_crew_name}`, and empty fields are
    ignored.
  - (e) `Occupied(rows, crew, probes, nowMS)` returns sorted raw names: a
    just-dispatched other branch with no status, plus local and bridge window
    names, never a `·`-suffixed string.
  - Verify: `go test ./internal/identity/` fails.

- [ ] **Step 13: implement identity changes.**
  - In `identity.go`:
    - `occupied()` treats `remote:true` dispatch rows by the 24 h rule;
    - `Assign` merges the avoid tokens from `CREW_IDENTITY_AVOID`, read via an
      injected `Getenv` in `Options`;
    - add `Occupied` and an `--occupied` flag. `identityCmd` (`commands.go:32`,
      positionals `Branch`, `Crew`) gains `Occupied bool \`name:"occupied"\``and`CrewFlag \*string \`name:"crew"\``.
    - The form is exactly `crew identity --occupied --crew <crew>`. The crew
      defaults to `bus.CrewID`.
    - The `main.go`/`identity.Request` check that refuses a missing branch
      admits `--occupied`.
    - `--occupied` with a branch positional is refused with exit 2:
      `crew: identity: --occupied takes no branch`.
    - It prints one raw name per line.
  - In `main.go`, extend the identity tmux probe format with `@bridge_crew_name`.
  - Keep `TestPoolsAreGolden` green.
  - Verify: `CREW_DRIFT_REQUIRED=1 go test ./internal/identity/` passes.

- [ ] **Step 14: `roster --live`** (test first in `roster_test.go`, then implement).
  - `Fold` gains an optional live-set input (`Probes.LiveBranches func() map[string]bool`, nil = off).
    When it is set, every row gets `live: <branch ∈ set>`.
  - In `main.go`, `rosterCmd` gains `Live bool`. Live set = branches from
    `tmux list-windows -a -F '#{@crew_branch}\t#{@crew_id}'` where `@crew_id`
    is empty or equals the crew.
  - Without `--live`, output is byte-identical: the existing roster tests are
    unchanged.
  - Verify: `CREW_DRIFT_REQUIRED=1 go test ./internal/roster/ .` passes.

### D. Routing (§5)

- [ ] **Step 15: `resolve-target` 5th column.**
  - The column is printed **only for `remote:true` rows**: `…\t<crew>\t1`.
    Every other row keeps exactly today's four columns, so the existing exact
    match at `tests/crew.bats:12833` and any Go golden stay byte-identical, and
    `cut -f5` reads empty for local rows.
  - In `resolve.jq`, append `"1"` to the array only when `.remote == true`. In
    `resolve.go`, `Row` gains `Remote bool`, and `Rows`/`Resolve` carry it.
  - Add a `remote:true` case to `resolve_test.go` and to the crew.bats
    resolve-target test near `:12833`.
  - Verify:
    - `go test ./internal/resolve/` passes;
    - `bats tests/crew.bats --filter 'resolve-target'` passes;
    - `bats tests/dispatch-resume.bats` passes (the consumer reads `-f1,3,4`).

- [ ] **Step 16: route `crew reply`** (test first in `reply_test.go`).
  - `reply.Options` gains `Remote func(host string, argv []string) int`
    (injected; nil means no routing).
  - After the existing `sender.Check`, if `remote.Route(rows, branchOf(to), crew)`
    is non-empty, call
    `Remote(host, ["crew","reply","--crew",crew,to,body])` and return its
    status. No local row is written.
  - The test asserts:
    - a remote target calls the fake with exactly that argv and writes no
      local row;
    - a local target is unchanged;
    - a refused sender (`CREW_WORKER_ID` set) never reaches `Remote`.
  - In `main.go`, wire `Remote` to `remote.Run` with `Env: ["CREW_ID="+crew]`
    and cwd.
  - Verify: `go test ./internal/reply/` passes.

- [ ] **Step 17: route `crew nudge` and `crew where`** (test first).
  - **Shared resolver.** Add `remote.ResolveTarget(rows, target, crew, localMatch bool) (host, branch string, err error)`:
    - It resolves via `resolve.Resolve` over the rows, filtered to those whose
      newest dispatch is `remote:true`.
    - `localMatch` is the caller's own local window match.
    - It returns the ambiguity error text from §5 when both sides match.
    - A `%pane` target is never routed.
  - **nudge** (`arm.go`): after the window read and before the local frame
    gate, compute the local match. If remote → `Remote(host, ["crew","nudge","--crew",crew,(--wait …),branch])`.
  - **where** (`where.go`):
    - for a remote target, first scan the local windows for
      `@bridge_crew_branch==branch` and `@bridge_crew_id` (empty or crew). The
      `whereProbes` list-windows format gains those two fields as trailing
      columns, kept tab-separated.
    - Found → print the usual line for that window with ` [host <h>]`
      appended.
    - Else → run `Remote(host, ["crew","where","--crew",crew,branch])`,
      capture its stdout, and print it with ` [host <h>]` appended before the
      newline.
  - Tests cover local-only (unchanged output), remote-only, both (ambiguous),
    `%pane` (never routed) and the bridge window hit.
  - Verify: `go test ./internal/nudge/ ./internal/where/ ./internal/remote/`
    passes, and `bats tests/crew.bats --filter 'where|nudge'` passes.

- [ ] **Step 18: roster remote merge** (test first, in `main_test.go` or a new
      `roster/remote_test.go` with a fake `Remote` that returns canned JSON).
  - In the default roster arm (`main.go`), after `roster.Fold`, for each host
    in `remote.RemoteHosts(events, crew)`, call
    `RemoteCapture(host, ["crew","roster",crew,"--live"])` with a 30 s
    context.
  - Keep the reply rows whose branch is in that host's remote branches, set
    `host`, and append them.
  - On failure or bad JSON, append one row per remote branch:
    `{branch, name (from the dispatch row), host, state:null, live:null, host_error:"<first stderr line or decode error>"}`.
  - No remote dispatch rows → output byte-identical.
  - Verify: `go test . ./internal/roster/` passes.

### E. Relay sources in the stream loop (§6)

- [ ] **Step 19: failing tests** (`crew/internal/stream/relay_test.go`).
  - Use a fake "self", a stub script, for `remote-run … -- crew relay …`.
    Driven by env, it prints canned batch and heartbeat lines and then exits
    with a chosen rc.
  - (a) A `remote:true` dispatch row on the loop's bus starts one relay.
    - Its batch reaches stdout as
      `{"cursor":C,"host":"mini","events":[{…,"host":"mini"}]}`;
    - `<CrewDir>/relay/mini.cursor` == C after the line is written.
  - (b) The child exiting 255 with stderr `boom` emits
    `{"stream":"error","crew":…,"host":"mini","rc":255,"detail":"relay to mini lost: boom",…}`.
    The relay then restarts after `retry` with `--since C`, asserted from the
    stub's argv log.
  - (c) No line for 3× heartbeat → the child is killed and an error line is
    emitted. Use small test values via loop fields.
  - (d) A second loop on the same bus does not start a relay while the first
    holds `relay/mini.lock.d`.
  - (e) A relayed batch whose event is `done` triggers
    `remote-run --host mini … -- crew reap --quiet`. Its stdout lines are
    emitted as `{"stream":"reap","crew":…,"host":"mini","lines":[…],…}`.
  - (f) After `cleanup`, the relay child's process group is gone.
  - (g) Fleet mode adds `repo` to the relayed batch.
  - Verify: `go test ./internal/stream/ -run Relay` fails.

- [ ] **Step 20: implement `stream/relay.go`** (implement: escalated).
  - **`relays` struct**, owned by `loop`. Fields: `hosts map[string]*relay`,
    the writer, `self`, `dir`, `crew`, `states`, `retry`, `heartbeat`,
    `cdir`, `repo`.
  - **`sync(log)`** reads the bus with `bus.ReadEventsTolerant`, takes
    `remote.RemoteHosts`, and starts a goroutine for each new host.
  - **Per relay:**
    - `lock.Acquire(cdir+"/relay/"+h+".lock.d")` (retry each sync if held);
    - read the cursor file;
    - spawn the child with `exec.Command(self, "remote-run","--host",h,"--env","CREW_ID="+crew,"--","crew","relay","--crew",crew,"--since",cursor,"--states",states)`,
      `Dir = dir`, `Setpgid`;
    - scan stdout lines (bufio with a 16 MiB max);
    - heartbeat lines reset the watchdog timer;
    - batch lines are decoded with `encoding/json`, not `jsonv` (the package
      is being converted off it), into
      `struct{ Cursor json.Number; Events []map[string]json.RawMessage }`
      plus a `map[string]json.RawMessage` for passthrough fields. They gain
      `host` on the batch and each event (plus `repo` in fleet mode), are
      re-encoded and written through the shared writer, and the cursor is
      then written (tmp+rename). Key order is not part of the contract.
    - A line that fails to decode is not forwarded. Emit an `error` line
      naming the host instead, and do not advance the cursor.
    - on exit or watchdog, emit an error line with the first stderr line, wait
      `retry`, and loop;
    - reap triggers use `wantsReap` on the raw batch, plus every `reapEvery`.
      One reap child per host at a time.
  - **`loop`:**
    - call `relays.sync` at the top of each `run` iteration in both modes;
    - `cleanup` stops all relays (kill the process groups, release the locks);
    - In single-stream `Run`, wrap stdout in the existing `laneWriter`
      (`fleet.go:393`) when the loop is constructed. Every loop write path
      then goes through one mutex-protected writer: `emit`, `printBatch`, the
      reap and hold lines, and anything else that writes `l.stdout`. Grep
      `l.stdout` and `stdout.Write` in `stream.go` and route each through it.
      Relays use the same writer.
    - **Fleet mode forces `c.reapEvery = 0`** (`fleet.go:111`), so the relays'
      periodic remote reap uses its own cadence there: `defaultReapEvery`, in
      fleet mode only (`l.repo != ""`). In single-stream mode,
      `--reap-every 0` still means no reaping, remote hosts included. A test asserts that a fleet-mode relay still
      schedules the periodic reap.
    - `errorLine` and `reapLine` gain `Host string \`json:"host,omitempty"\``.
  - Verify:
    - `go test ./internal/stream/` passes;
    - `go test -race ./internal/stream/` passes;
    - all existing stream tests are unchanged.

### F. dispatch / resume / permission-check (bash)

**Order.** Write Step 24's `tests/remote-host.bats` first, right after Step 20,
and run it red. Expect failures on the missing `--host`, routing and remote-mode
targeting. Steps 21–23 then turn it green; re-run it after each of them. Step
22's own verification is that file's `--host` cases.

- [ ] **Step 21: remote-mode tmux targeting and row emit** (`dispatch.sh`, `dispatch-resume.sh`).
  - **Window creation**, when `DISPATCH_TMUX_SESSION` is set, at
    `dispatch.sh` ≈5691:
    - validate it against `^crew-[A-Za-z0-9_-]+$` (else die);
    - skip the client-size block;
    - if `tmux has-session -t "=$s"` succeeds, run
      `_pane_client new-window -d -t "=$s:" …same args…`;
    - else run `_pane_client new-session -d -s "$s" -n "$sanitized" -c "$wt_path" -e … -P -F '#{window_id} #{pane_id}'`;
      if that fails, try the `new-window -t` form once.
  - **Remote mode also:**
    - writes `dispatcher_pane:` empty;
    - skips `_ensure_roster_render`.
  - **`DISPATCH_EMIT_ROW=1`:** print `dispatch_row: $line` right after
    `_crew_append "$line"` (≈5407).
  - **`dispatch-resume.sh` ≈1227:** the same targeting logic. Resume's other
    `tmux` uses are unchanged.
  - Verify:
    - `bats tests/dispatch.bats --filter 'remote mode'`. Add two cases using
      the stub tmux: the stub logs argv, and `has-session` answers per env.
      Assert the `new-session -d -s crew-c1` form when the session is absent,
      `new-window -d -t =crew-c1:` when it is present, the `dispatch_row:`
      line, and empty `dispatcher_pane:`.
    - `bats tests/dispatch.bats tests/dispatch-resume.bats` all green;
    - `shellcheck` clean.

- [ ] **Step 22: `dispatch --host` local path** (implement: escalated; `dispatch.sh`).
  - **`--recover` intercept.** Before the tier/model positional parse
    (≈3711, beside the `--engines` intercept at ≈3624), a first argument of
    `--host` followed by `<name> --recover <session> [--crew-id <id>]` is
    handled on its own:
    - validate the session and crew id (`--crew-id`, else `$CREW_ID`);
    - run the recovery (shared function, below), then exit.
    - No tier, model or effort is needed, so the printed recovery hint works as
      written.
  - **Parse loop:** `--host <name>` → `remote_host`.
    - **Remote argv capture.** Every option token the loop consumes, flag and
      value alike, is appended to a `host_fwd_args` array, except the
      `--host X` pair and `--crew-id X`.
      - This covers each `case` arm (`--agent X`, `--effort X`, …, `--draft`)
        and the tracker tokens in the fallback arm (Linear id, issue number),
        which the loop consumes as options.
      - The title is the remaining `"$@"` after the loop (and after `--`, if
        it was used).
    - The remote argv is then
      `dispatch "$tier" "$model" "${host_fwd_args[@]}" --crew-id "$crew_id" -- "$@"`,
      so `--crew-id` can never land inside the title, and a title that starts
      with a dash stays a title.
  - **Placement:** right after the crew-id block (≈4051–4055), when
    `remote_host` is set, call `_dispatch_remote` and `exit` with its status.
  - **`_dispatch_remote`** implements §3 steps 1–9 exactly:
    - Refusals first: `--add-dir`, `--base`, `--status`, `--lazy`,
      `DISPATCH_PRECHECK`, and a crew id not matching `^[A-Za-z0-9_-]+$`.
    - The preflight `crew remote-run --host "$remote_host" --env "CREW_ID=$crew_id" -- dispatch --engines`;
      the lead agent must be in its output.
    - The local claude budget gate: `_settings_load`, `budget_file` set as at
      4325, then `budget_stop claude` and `absolute_limit_stop claude` unless
      `--ignore-budget`.
    - The local duplicate guard over `$crew_dir/events.jsonl` and the claims
      index (§3.4; reuse `_branch_finished` for local rows; remote rows from
      another host count while `ts` is within 24 h).
    - The session id `s$(date +%s)-$$`.
    - The avoid list `crew identity --occupied --crew "$crew_id" | paste -sd, -`
      (tokens failing `^[A-Za-z0-9-]+$` dropped with a warning).
    - The remote call:
      `crew remote-run --host … --env CREW_ID=… --env DISPATCH_TMUX_SESSION=crew-… --env DISPATCH_EMIT_ROW=1 --env DISPATCH_SESSION_ID=… [--env CREW_IDENTITY_AVOID=…] [--spec-file "$DISPATCH_SPEC"] -- dispatch "$tier" "$model" "${host_fwd_args[@]}" --crew-id "$crew_id" -- "$@"`.
      (`"$@"` is the title tokens left after the parse loop.)
    - The rc 255 recovery loop (`DISPATCH_HOST_RECOVER_TRIES`/`_SLEEP`)
      calling `crew remote-run … -- crew dispatch-row --crew … --session …`.
    - Row validation with `jq -e` (§3.7).
    - `_crew_append` of the row `+ {ts, host, remote:true}` and
      `_claim_index_dispatch`. Set the variables that function reads (`branch`
      etc.) from the row, or add a row-argument variant.
    - og-remote-open, or the attach hint.
    - Output lines exactly as in §3.
  - **`--recover`** (the intercept above) runs the same recovery function as
    rc 255. Idempotence: when a local `remote:true` row with that session
    exists, only rerun step 8.
  - **Usage/help text:** add `--host`/`--recover` to the usage string near
    line 86.
  - Verify:
    - `shellcheck adapters/core/dispatch.sh`;
    - `bats tests/dispatch.bats` green (single-host unchanged);
    - `bats tests/remote-host.bats --filter 'scaffolds|unknown host|no clone mapped|mid-dispatch|metachar|recover'`:
      every `--host` case is green except those still waiting on Step 23
      routing (reply/roster live through `crew` are already Go).

- [ ] **Step 23: route `dispatch resume` and `permission-check`.**
  - **`dispatch-resume.sh` `_resolve_target_worktree` (≈481–497):**
    - read column 5 (`remote`);
    - when it is `1`, run
      `crew remote-run --host "$host" --env "CREW_ID=$t_crew" --env "DISPATCH_TMUX_SESSION=crew-$t_crew" -- dispatch resume "$t_branch" "${rest[@]}"`,
      where `rest` = the resume args after the target, saved at parse time;
    - then run the og-remote-open step (a shared helper, copied byte-identically
      if needed, following the existing parity pattern), and `exit` with the
      remote status;
    - the foreign-host refusal stays for rows without `remote`.
  - **`permission-check.sh`:**
    - right after arg parsing, when `--branch` is set and `--capture` mode is
      not, run `row="$(crew resolve-target "$BRANCH" 2>/dev/null || true)"`;
    - if `cut -f5` is `1`, run
      `rc=0; out="$(crew remote-run --host "$(cut -f3 <<<"$row")" -- permission-check "${orig_args[@]}")" || rc=$?`.
      The `|| rc=$?` keeps `set -e` from ending the script through the trap;
    - set `decided=1` before printing on **every** routed path. The EXIT trap
      at `permission-check.sh:44` otherwise appends `human: internal error`
      and forces exit 1;
    - if rc is 255 or `out` is empty, print `human: ssh to host <h> failed`
      and exit 2;
    - else print `out` and exit rc.
    - The bats case asserts the output is exactly one line.
  - Add tests:
    - `tests/permission-check.bats`: a case with stub `crew` answering
      `resolve-target` with column 5 `1` and recording `remote-run` argv.
      Assert the forwarded argv and the `human:` line on 255.
    - `tests/dispatch-resume.bats`: a routed-resume case with stub `crew`.
  - Verify: `bats tests/permission-check.bats tests/dispatch-resume.bats`
    green; `shellcheck` clean.

### G. End-to-end and negatives (AC2/AC3)

- [ ] **Step 24: write `tests/remote-host.bats`.** Executed before Step 21 (see
      "Order" under F). Red first; green once 21–23 land.
  - `setup` uses `load helpers` and `setup_repo`.
  - **Origin:** create a bare origin at `$BATS_TEST_TMPDIR/acme/widget.git`,
    so its slug is `acme/widget`. Push `main`, and set it as the local repo's
    origin.
  - **"Remote":** clone the origin to `$BATS_TEST_TMPDIR/remote/widget`. Give
    it its own `RHOME`, `XDG_{DATA,CONFIG,STATE}_HOME` under
    `$BATS_TEST_TMPDIR/rhome` and its own `TMUX_TMPDIR`.
  - **Settings:** `user_settings` with
    `{"hosts":{"mini":{"ssh":"mini","repos":{"acme/widget":"<remote clone>"}},"broken":{"ssh":"fail-broken","repos":{"acme/widget":"<remote clone>"}},"bare":{"ssh":"mini","repos":{}}}}`.
    `bare` serves the no-mapping case. Test titles carry the filter words used
    in Acceptance: `scaffolds`, `unknown host`, `no clone mapped`,
    `mid-dispatch`, `metachar`, `recover`.
    Point the remote's settings at the same `DISPATCH_CONFIG_BIN`.
  - **Bins dir on PATH**, shared by both sides:
    - `crew` → the real `$CREW_GO_BIN`, except that `stall-watch` and
      `roster-render` are no-ops;
    - `dispatch` → `exec bash <repo>/adapters/core/dispatch.sh "$@"`;
    - `dispatch-resume` likewise;
    - `permission-check` likewise;
    - stubs `wt`, `gh`, `direnv` and `claude`, copied from
      `tests/dispatch.bats` `stub_launch_bins`;
    - a stub `og-remote-open` that logs its argv;
    - **`ssh`:** parse and skip `-T`, `-o X` pairs and `--`; take the target,
      then the command words.
      - Target `fail-*`: exit 255, or after `$SSH_FAIL_AFTER` successful
        calls, counted in a file.
      - Otherwise `cd "$RHOME"`, as sshd would start in the remote home, never
        in a repo, then run `env -u TMUX -u TMUX_PANE -u CREW_ID -u CREW_WORKER_ID -u CREW_ROLE_ID HOME=$RHOME XDG_…=… TMUX_TMPDIR=$RTMUX sh -c "$*"`
        with stdin passed through.
  - **Tmux:** the remote uses the real tmux (`$REAL_TMUX`, via a `tmux`
    wrapper that is first on PATH and forwards to `REAL_TMUX`) on its private
    `TMUX_TMPDIR`. `teardown` runs `TMUX_TMPDIR=$RTMUX $REAL_TMUX kill-server`.
  - **AC2 case "dispatch --host scaffolds, relays, replies, rosters, and sees death":**
    1. Run `DISPATCH_SPEC=<spec file> dispatch standard sonnet --effort medium --host mini --crew-id c1 42 "Remote title"`.
       Assert:
       - success;
       - a `worker_id:` line;
       - the local bus has a `kind:"dispatch"` row with `host=="mini"` and
         `remote==true`;
       - the remote worktree has `WORKER_TASK.md` with `crew_id: c1` and an
         empty `dispatcher_pane:`;
       - `TMUX_TMPDIR=$RTMUX tmux list-windows -t =crew-c1 -F '#{@crew_branch}'`
         lists the branch;
       - the og stub log has `mini crew-c1 --into`. It runs only if
         `TMUX_PANE` is set: export a fake `TMUX_PANE`, and have the stub
         `tmux` wrapper answer `display-message … '#{session_name}'` with
         `main`.
    2. Start `crew stream --crew c1 --park 2 --retry 1 --coalesce 0 --interval 1`
       locally, in the background, to a file. Register a trap that kills it.
    3. In the remote env, run
       `CREW_WORKER_ID=<worker id> crew status <worker id> blocked "need input"`
       from the remote worktree, and poll the stream file for up to 30 s for a
       batch line whose `.host=="mini"` and whose event has
       `.body.state=="blocked"`.
    4. Run `crew reply --crew c1 <worker id> "hello"` locally. Then remote
       `crew inbox <worker id>` contains `hello`.
    5. Local `crew roster c1 | jq` has a row with `host=="mini"` and
       `live==true`.
    6. Kill the remote window with `tmux kill-window`. Local roster then shows
       `live==false`.
  - **AC3 cases:**
    - `--host nowhere` exits non-zero, the stderr has `unknown host 'nowhere'`,
      and the local bus is unchanged.
    - A host whose `repos` lacks `acme/widget` gives `has no clone mapped for acme/widget`.
    - `--host broken` with `SSH_FAIL_AFTER=1` (preflight OK, then dispatch and
      recovery fail): non-zero exit; stderr has `ssh link failed mid-dispatch`
      and `--recover`; no local `dispatch` row, no claims-index file row, and
      no `dispatch-*.lock` under the local crew dir.
    - **Metacharacters:**
      - a title like `x $(touch $MARK) \`touch $MARK\`; touch $MARK 'q"`;
      - a spec file containing `'; touch $MARK; '`, `$(touch $MARK)` and a
        line `## Not a heading?`;
      - the remote `WORKER_TASK.md` contains the spec text byte-identical
        (compare with `grep -F` per line) and the title verbatim;
      - `$MARK` does not exist.
    - **Recovery:** a mode where the shim exits 255 _after_ running the
      command, for the dispatch call only. The local side recovers the row
      (stderr notes the dropped link), exit 0, and the local row exists. Then
      `dispatch --host mini --recover <session> --crew-id c1` exits 0 without
      appending a second row.
  - **Wiring:**
    - in `scripts/bats-affected.sh`, add `remote-host` to the `dispatch_set`
      and `crew_set` arrays (or to the `dispatch-resume.sh`,
      `permission-check.sh` and `crew/*` rows);
    - run `bash scripts/bats-shard.sh --check`, and add shard-weights rows
      only if it errors.
  - Verify: `bats tests/remote-host.bats` all green; `bats tests/bats-affected.bats`
    green.

### H. Docs (§8)

- [ ] **Step 25: protocol docs.**
  - **`DISPATCHER_PROTOCOL.md`:**
    - add `[--host <name>]` to the usage line at ≈425;
    - add a "**Remote workers (`--host`).**" bullet in "Scaffold one worker per
      task" with the §8 content;
    - in "Read the bus", add one paragraph: relayed batches carry `host`,
      cursors are kept per `host`, `error` lines carry `host`, and the
      streaming lane is required.
  - **`WORKER_PROTOCOL.md` "When done":** add "(skip when `dispatcher_pane:`
    is empty — always on a remote host)".
  - Run `nix develop -c ./scripts/gen-adapters.sh` and commit the mirrors.
  - Verify:
    - `git diff --exit-code` after a second `gen-adapters.sh` run;
    - `bats tests/adapters.bats` green;
    - `bash scripts/harness-manifest.sh --check && bash scripts/harness-inventory.sh --check`.
      Regenerate with the scripts' non-check mode if a sampled bats case
      changed.

### I. Gate

- [ ] **Step 26: CI-equivalent gate** (AC4/AC5). Run every command from
      `.github/workflows/ci.yml`:
  - shellcheck;
  - `go vet` plus `golangci-lint`;
  - the two `CREW_DRIFT_REQUIRED=1 go test` commands;
  - gen-adapters plus `git diff --exit-code`;
  - `bats-shard.sh --check`;
  - the harness checks;
  - the bats files affected (`scripts/bats-affected.sh --base "$base_ref"`),
    plus `tests/module.bats`;
  - `nix flake check`;
  - `nix build .#default --no-link`.
  - Fix until green.

## Acceptance

- [ ] **AC1:** the spec and this plan are committed under
      `docs/superpowers/{specs,plans}/`. The spec's "Single-host site
      dispositions" table and "Trust boundary" cover every listed site. Proof:
      `git ls-files docs/superpowers`.
- [ ] **AC2:** `bats tests/remote-host.bats --filter 'scaffolds'`.
- [ ] **AC3:** `bats tests/remote-host.bats --filter 'unknown host|no clone mapped|mid-dispatch|metachar'`.
- [ ] **AC4:** the existing suites green (affected bats, plus all Go tests
      with `CREW_DRIFT_REQUIRED=1`).
- [ ] **AC5:** Step 26 commands locally, plus PR CI green: the `gh pr checks`
      run id on the head.
