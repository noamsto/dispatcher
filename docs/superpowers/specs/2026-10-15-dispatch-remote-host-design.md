# Remote workers: `dispatch --host` — design

Closes #1104.

## Goal

`dispatch --host <name> …` runs a worker on another machine. The worktree,
tmux window, engine and watchdog all live on that host. The local dispatcher
keeps following the worker from its own session as if it were local:

- the worker's bus events stream back;
- replies, nudges, `where`, permission answers and resumes are routed to it;
- its window shows up in the dispatcher's tmux session through tmux-og's
  `og-remote-open … --into`.

**Design principle (kept).** Everything that touches a worker's pane,
transcript or worktree runs on the worker's host, using the unmodified
single-host machinery. Only two things cross hosts:

- **the bus:** remote → local is read-only relay; local → remote writes are
  routed `ssh <host> crew …` calls;
- **the window:** the og-remote-open mirror.

The code survey confirms the principle holds. Every single-host tool already
resolves its bus, panes, transcripts and processes from its own cwd and its own
tmux server, so it runs correctly when invoked _on_ the remote host. None of
them needs to reach over ssh.

**Out of scope:**

- auto-clone;
- cross-host stacked work (`--base` is refused with `--host`);
- moving a worker between hosts;
- `--add-dir` grants on a remote host (refused with `--host`: grants are local
  paths resolved against local `grantRoots`).

## Components

### 1. Settings: `hosts`

New top-level settings key, resolved by `dispatch-config.sh` exactly like
`engines` (base → user → locked → env merge; jq `*` deep-merges objects):

```json
"hosts": {
  "mini": {
    "ssh": "noam@mini.local",
    "repos": { "noamsto/dispatcher": "/Users/noam/git/dispatcher" }
  }
}
```

- **Host name:** `^[A-Za-z0-9][A-Za-z0-9._-]*$`.
- **`ssh`:** an ssh destination matching `^[A-Za-z0-9_][A-Za-z0-9._@%+-]*$`.
  It can never start with `-`, so it can never be read as an ssh option. Use
  an `~/.ssh/config` alias for ports or jump hosts.
- **`repos`:** keys `owner/repo` (`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`); values
  absolute paths with no control characters.
- `dispatch-config.sh` validates the merged tree and dies naming the bad key,
  the same `need`/violations pattern as `laneProfiles`.
- **User layer allowed (not stripped like `grantRoots`).** A host entry only
  chooses where the dispatcher sends work, and grants the worker nothing.
  Editing `~/.config/dispatcher/settings.json` is a path-named write outside
  any worker's working dirs, so a worker cannot make that edit silently.
- **home-manager:** `programs.dispatcher.hosts`, type
  `nullOr (attrsOf (submodule { ssh = str; repos = attrsOf str; }))`, emitted
  into the locked layer when non-null.
- **README** "Settings" lists the key.

### 2. Remote transport: `crew remote-run` (client) and `crew remote-exec` (server)

This is the single place that builds an ssh call. Every routed operation goes
through it, whether its caller is bash or Go.

**Client: `crew remote-run --host <name> [--env K=V]… [--spec-file <path>] -- <argv…>`**

Runs in a local checkout.

1. Load settings via `dispatch-config`. This is `DISPATCH_CONFIG_BIN`, else
   `dispatch-config` on PATH — the `piagentdir` precedent.
2. Resolve the host and look up the local origin's `owner/repo`, derived the
   way `claimindex` derives it, in `hosts.<name>.repos`.
3. Refuse, before any ssh, with exit 2:
   - an unknown host: `crew: remote-run: unknown host '<h>' (no hosts.<h> in settings)`;
   - a missing mapping: `crew: remote-run: host '<h>' has no clone mapped for <owner/repo> (settings hosts.<h>.repos)`;
   - a bad `--env` key or value, or an invalid `ssh` value.
4. Build the request as one JSON object:
   `{"v":1,"repo":<remote path>,"slug":<owner/repo>,"argv":[…],"env":{…},"spec":<file text, if --spec-file>}`.
5. Run `ssh -T -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o PermitLocalCommand=no -- <ssh> crew remote-exec`
   with the request on stdin and stdout/stderr passed through.
   - `-T` means no pty; `BatchMode` means no prompts. `--` ends ssh's options,
     and the target cannot start with `-` in any case.
   - Forwarding is forced off whatever the user's `ssh_config` says. Otherwise
     an agent or X11 forward would sit in the remote environment, where remote
     workers (same uid) could use it: new authority.
   - The keepalives detect a link that died, e.g. while the controller slept,
     within about 45 s rather than at the TCP timeout.
   - The remote command string is the fixed literal `crew remote-exec`. No
     caller data ever reaches the remote login shell, which may be bash, zsh or
     fish; all three parse that literal identically.
6. Exit with ssh's status. On 255, also print
   `crew: remote-run: ssh to host '<h>' (<ssh>) failed (exit 255)` to stderr.

**Server: `crew remote-exec`**

Runs on the remote host, under sshd.

1. Read at most 1 MiB of stdin and decode exactly one JSON object.
   - Refuse unknown top-level keys, `v != 1`, and any string containing NUL.
2. Check the repo:
   - `repo` is absolute, and is a directory inside a git checkout;
   - its origin derives to `slug`. Otherwise:
     `crew: remote-exec: <repo> is not a clone of <slug> (origin: <x>)`.
     This is the "no clone" refusal.
3. Check `argv` against the allowlist, matched on the first one or two words:

   | argv                                                            | purpose                                                 |
   | --------------------------------------------------------------- | ------------------------------------------------------- |
   | `dispatch …` (incl. `--engines`, `resume`)                      | dispatch, preflight, resume                             |
   | `crew reply\|nudge\|where\|roster\|relay\|reap\|dispatch-row …` | routed writes, queries, relay, reap, link-loss recovery |
   | `permission-check …`                                            | routed permission answer                                |
   - `crew` subcommands re-exec this same binary (`os.Executable`). The others
     are found with `exec.LookPath`.
   - Execution is `execve`-style argv, never `sh -c`.

4. Check `env` against its own allowlist, each key with a value regex:
   - `CREW_ID` `^[A-Za-z0-9_-]+$`;
   - `DISPATCH_TMUX_SESSION` `^crew-[A-Za-z0-9_-]+$`;
   - `DISPATCH_EMIT_ROW` `^1$`;
   - `DISPATCH_SESSION_ID` `^s[0-9]+-[0-9]+$` (already honoured at
     `dispatch.sh:5206`);
   - `CREW_IDENTITY_AVOID` `^[A-Za-z0-9-]+(,[A-Za-z0-9-]+)*$` (component 3,
     step 5).
   - Keys are added to the inherited environment.
   - `CREW_WORKER_ID` and `CREW_ROLE_ID` are **not** touched (see Trust boundary).
5. `spec`, when present, is written to a 0600 file in the scratch dir (step 6)
   and exported as `DISPATCH_SPEC`.
6. `chdir(repo)`. **Scratch files** go in `<remote crew dir>/remote-exec/<pid>-<rand>/`:
   the spec file and the detached dispatch's output files. That dir is removed
   when the child finishes and `remote-exec` is still alive. Every
   `remote-exec` start also sweeps entries older than 24 h, so files orphaned
   by a hang-up are bounded. Then run the child:
   - **Every argv except `dispatch …`** runs with stdio inherited. Exit with
     its status; a signal maps to 128+n. When the link drops, the child dies
     with it. That is wanted for `relay`, and harmless for the one-shot
     queries and writes.
   - **`dispatch …` (including `resume`)** is detached so a dropped link
     cannot kill it halfway through scaffolding:
     - it runs in its own session (`setsid`), with stdin `/dev/null` and
       stdout/stderr going to two 0600 temp files;
     - `remote-exec` waits for it, then copies the files to its own
       stdout/stderr and removes them.
     - If sshd hangs up first, `remote-exec` dies but the dispatch runs to
       completion. The worker then either fully exists or the remote dispatch
       failed by its own rules. Never a SIGPIPE half.

### 3. `dispatch --host <name>` (local side, `dispatch.sh`)

`--host <name>` is parsed in the main flag loop.

**Refused alongside it:** `--add-dir`, `--base`, `--status`, `--lazy`, and
`DISPATCH_PRECHECK`. Those flags are local by nature, or would be stacked work.

**When to branch.** Right after the crew id is resolved (≈`dispatch.sh:4051`),
and before any engine, budget, fleet or claim step, dispatch takes the remote
path. That path has no side effects until the remote call succeeds:

1. Validate the crew id against `^[A-Za-z0-9_-]+$`. It names the remote tmux
   session `crew-<id>`.
2. **Engine roster.** Run
   `crew remote-run --host <h> --env CREW_ID=<id> -- dispatch --engines`.
   - Refuse unless the lead `--agent` (default `claude`) is listed.
   - This call is also the preflight for unknown host, no mapping, no clone and
     ssh failure; each surfaces its own message.
3. **Claude quota on the dispatcher host.** When the lead engine is `claude`,
   run the existing `budget_stop claude` and `absolute_limit_stop claude`
   against the local `engine-budget.json`. `--ignore-budget` waives them as
   before.
   - The fan-out budget and the load gate (`fleet_gate`) are per host. They are
     **not** run locally; the remote `dispatch` runs its own.
4. **Local duplicate guard** (GitHub issue tokens only: the primary and every
   `--also-closes`).
   - Refuse when the local bus or the local claims index holds a
     `kind:"dispatch"` row naming the issue (a `feat/<N>-*` branch or
     `also_closes`) whose host differs from `<h>`, or which is a local worker
     (no `remote:true`). Name the branch and its host.
     - A local worker's row counts while it is unreaped (`_branch_finished`).
     - A remote row on another host counts while it is under 24 h old, the
       claims-index aging rule: the local bus never sees that host's
       `reaped` row.
   - A same-host row passes. The remote `dispatch` then decides resume vs
     refuse, with its own claim and label logic, unchanged.
   - Reason for the guard: the `dispatched` label is shared through GitHub, but
     the remote's claim evidence cannot see this machine's bus. Without the
     guard, the remote would treat a local in-flight worker's label as stale and
     re-claim it.
5. **Remote dispatch.** Run
   `crew remote-run --host <h> --env CREW_ID=<id> --env DISPATCH_TMUX_SESSION=crew-<id> --env DISPATCH_EMIT_ROW=1 [--spec-file "$DISPATCH_SPEC"] -- dispatch <original argv minus --host <h>> --crew-id <id>`.
   - The title, `--owner-auth` text and spec all travel inside the JSON request.
   - Local dispatch mints the session id (`s<epoch>-<pid>`, the existing
     shape) and passes it as `DISPATCH_SESSION_ID`, so it can find the row
     again after a link failure (step 6).
   - **Codenames stay unique across hosts.** `identity.Assign` avoids only the
     names on its own bus and its own tmux (`identity.go:206-235`), so the
     remote would otherwise hand out a name a local or other-host worker
     already holds.
     - Before the call, local dispatch runs `crew identity --occupied --crew <id>`.
       This new flag prints, one per line, the **raw** codenames `Assign` would
       treat as taken here:
       - the local bus's `occupied()` set: other branches' dispatch rows whose
         newest local `worker:` status is not terminal. This includes a
         just-dispatched worker with no status yet, since `occupied()` keys on
         dispatch rows and not on roster rows;
       - the `@crew_name` of every local window;
       - the `@bridge_crew_name` of every mirror window.
     - Names come from dispatch rows and window stamps, never from the roster,
       so they are never `roster_final.jq`'s `name·<id>` display suffixes.
     - Local dispatch passes the list, comma-joined, as `CREW_IDENTITY_AVOID`.
       A token that fails the env regex is dropped with a stderr warning;
       remote-exec never refuses the whole dispatch over it.
     - On the remote, `Assign` adds those names to its occupied set: a recorded
       name that is on the list is re-picked, exactly as for a locally
       occupied one.
     - **The same function serves local dispatches.** `occupied()` and
       `windowNames()` are extended for every caller:
       - a `remote:true` row counts as occupied while it is under 24 h old.
         Its terminal status never reaches the local bus, so without an age
         rule a remote codename would never free and the 32-name pool would
         drain;
       - mirror windows' `@bridge_crew_name` count as taken.
     - A local dispatch therefore also avoids live remote workers' names.
     - **Accepted residual.** `--occupied` has no branch to exclude, because a
       minted branch is not known locally. A same-host re-dispatch of a remote
       branch younger than 24 h may therefore get a new codename. This is
       cosmetic.
   - Remote stderr passes through to local stderr. Remote stdout is captured.
6. **Failure.** On non-zero rc other than 255, the remote dispatch itself
   failed under its own rules (its stderr says why). Print
   `dispatch: --host <h>: remote dispatch failed (exit <rc>)` and exit with rc.
   Nothing local has been written: no bus row, claims-index row, window or lock.
   - **rc 255 is a broken link.** The detached remote dispatch (component 2,
     step 6) may still be completing, so run a bounded recovery:
     - Up to 6 queries, 10 s apart (`DISPATCH_HOST_RECOVER_TRIES` and
       `DISPATCH_HOST_RECOVER_SLEEP` override this for tests), of
       `crew remote-run --host <h> --env CREW_ID=<id> -- crew dispatch-row --crew <id> --session <session>`.
     - `crew dispatch-row` is a new read-only subcommand. It prints the newest
       `kind:"dispatch"` row on its own bus with that crew and session as
       compact JSON, and exits 1 when there is none.
     - **Row found:** take the success path (step 7 onward) and add a stderr
       note that the link dropped after the remote created the worker. Exit 0:
       the worker exists and is now followed.
     - **A query fails with 255 (link still down; this includes a
       connect-time failure):** stop at once. Do not wait out the remaining
       tries.
     - **No row after all tries, or the link is down:** print
       `dispatch: --host <h>: ssh link failed mid-dispatch (exit 255); the remote worker could not be confirmed — once the link is back run: dispatch --host <h> --recover <session> --crew-id <id>`
       and exit 255.
     - **`dispatch --host <h> --recover <session> [--crew-id <id>]`** is the
       re-runnable form, for the case where the remote dispatch took longer
       than the window or the link stayed down.
       - It skips straight to this recovery: the same query loop, then steps
         7-9 on a row.
       - It writes nothing when no row exists, printing
         `dispatch: --host <h>: no remote dispatch row for session <session>`
         and exiting 1.
       - It is idempotent: when the local bus already has a `remote:true` row
         with that session, it only re-runs step 8 (the window) and exits 0.
       - `<session>` must match `^s[0-9]+-[0-9]+$`.
     - Local state stays clean, because the local row is written only from a
       confirmed remote row.
7. **Success.** Remote stdout carries `worker_id: …` and one
   `dispatch_row: <json>` line (component 4).
   - Validate it: an object with `kind=="dispatch"`, `crew_id==<id>`, a
     `branch` that passes `git check-ref-format --branch`, `session` equal to
     the minted session id, and `worker_id == "worker:<branch>#<session>"`.
     This is the same check on every path; on the success path the
     `worker_id:` stdout line must agree with it too.
   - Append to the local bus that row, plus `{ts:<local now>, host:<h>, remote:true}`.
     The remote's own `uname` host is overwritten; the remote bus keeps it.
   - Mirror the row into the local claims index (`_claim_index_dispatch`).
   - A missing or invalid row is an error naming the remote, since the remote
     worker exists but cannot be followed. Same message shape as step 6.
8. **Window.**
   - When `og-remote-open` is on PATH and `$TMUX_PANE` is set, run
     `og-remote-open <ssh> crew-<id> --into <session of $TMUX_PANE>`. It is
     idempotent per the contract, so it runs on every dispatch and resume.
   - When the command is absent or fails, print
     `dispatch: remote window not mirrored (<reason>); attach with: ssh -t <ssh> tmux attach -t crew-<id>`
     and still exit 0.
9. **Output.** Print `worker_id: …` and
   `dispatch: remote worker on host <h> — followed by this repo's crew stream`.

### 4. Remote side of `dispatch` and `dispatch resume`

The remote host runs its own unmodified `dispatch`. Two env inputs change only
its tmux targeting and its output.

**`DISPATCH_TMUX_SESSION=crew-<id>`** (re-validated with the same regex). The
new-window sites at `dispatch.sh:5691` and `dispatch-resume.sh:1227` handle it
the same way:

- When `tmux has-session -t "=crew-<id>"` succeeds, run
  `new-window -d -t "=crew-<id>:"`.
- Otherwise run `new-session -d -s crew-<id> -n <name> -c <wt> -e …` with the
  same `-P -F`. The worker window then _is_ the session's first window, so no
  placeholder shell window gets mirrored.
- If two dispatches race and `new-session` fails with "duplicate session",
  fall back to `new-window`.
- Client-size sizing is skipped: there is no client, and the mirror resizes.
- `dispatcher_pane:` is stamped empty. `dispatch-notify.sh:172` already skips
  its ping on an empty pane.
- `_ensure_roster_render` is skipped: the remote has no dispatcher pane, and the
  diagram belongs to the dispatcher's host.
- Everything else runs unchanged on the remote: worktree, claims and labels,
  pre-trust, stamps, send-keys launch, stall-watch, role grid, and
  `crew reap --quiet`.

**`DISPATCH_EMIT_ROW=1`.** After `_crew_append` of the dispatch row, print
`dispatch_row: <the same compact JSON>` to stdout.

**`CREW_IDENTITY_AVOID`.** Read by `crew identity`'s `Assign`, so it works the
same in any caller. Each listed name is added to the occupied set before the
recorded-name check. When the variable is unset, behaviour is unchanged.

`dispatch resume` does not write a dispatch row, so it never prints this line.

### 5. Routing local → remote

**The route lookup** is new code in package `crew/internal/remote`. For a
branch in crew `<c>`, it takes the newest `kind:"dispatch"` row on the
**local** bus. If that row has `remote==true`, the route is its `host`. The
ssh target and path are never read from the row; they always come from
settings (Trust boundary).

`crew resolve-target` gains a 5th TSV column, `1`, printed only on
`remote:true` rows. Every other row keeps today's four columns byte-for-byte.
Its only consumer, `dispatch-resume.sh`, uses `cut -f1,3,4` and is unaffected;
`cut -f5` reads empty for a local row.

| operation                                            | local behaviour for a remote worker                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| ---------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `crew reply <worker id> <body>`                      | Run the local `dispatcher:` sender check first, so a local worker still cannot reply. Then `remote-run --host <h> --env CREW_ID=<c> -- crew reply <target> <body>`. The remote writes the row to its own bus as `dispatcher:<c>`, and the remote worker's `crew await`, role watcher and directive hooks see it unchanged.                                                                                                                                                                                                                                                                                                            |
| `crew nudge <target>`                                | **Codename and branch resolution, shared by nudge, where and resume.** A non-`%pane` target is resolved twice: against the local windows the command already reads (`@crew_name`/`@crew_branch`, unchanged), and through `resolve.Resolve` over the local bus restricted to `remote:true` rows. Exactly one side matching picks that side. Both sides matching is refused as `ambiguous target '<t>' — matches a local worker (<branch>) and a remote one on <h> (<branch>); pass a branch`. When it routes: `remote-run … -- crew nudge <branch> <other args>`. The remote runs its own frame and anchor gates against its own tmux. |
| `crew where <target>`                                | Target resolution as for nudge. For a remote target, first look for a local mirror window whose `@bridge_crew_branch`/`@bridge_crew_id` match (contract item 3). If found, print the usual line for that local window with ` [host <h>]` appended: this is the jump target. Otherwise route `crew where <branch>` to the remote and append ` [host <h>]` to its line. A `%pane` target is never routed (remote pane ids are meaningless locally).                                                                                                                                                                                     |
| `permission-check --pane %N --branch <b> [--answer]` | When `crew resolve-target <b>` says remote: `crew remote-run --host <h> -- permission-check <same args>`, passing its output and status through. On rc 255 or an empty stdout it prints `human: ssh to host <h> failed`, keeping the one-line contract, and exits 2. The `%N` came from the remote watchdog's `prompt:` detail, so it names a remote pane. The remote reads its own `~/.claude/projects`, `ps` table and tmux.                                                                                                                                                                                                        |
| `dispatch resume <target>`                           | `crew resolve-target` already resolves over every dispatch row, local and `remote:true`, and refuses ambiguity across both (exit 2). In `_resolve_target_worktree`, a remote row routes the call instead of refusing: `crew remote-run --host <h> --env CREW_ID=<c> --env DISPATCH_TMUX_SESSION=crew-<c> -- dispatch resume <branch> <remaining args>`, then the og-remote-open step. The remote resolves the target from its own bus, whose row carries its own uname, so its host check passes. A row with a foreign `host` and no `remote:true` keeps today's refusal.                                                             |
| `crew reap`                                          | Not routed per call. The relay triggers it (component 6).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |

### 6. Relay remote → local: `crew relay` plus relay sources in the stream loop

**Remote: `crew relay --crew <c> --since <ts> [--states S] [--interval N] [--heartbeat N]`**

A lock-free follower of the remote's own bus. It takes no `watch.lock.d` and
writes no cursor file, so it never fights a remote `crew watch`.

- It reuses `crew watch`'s fold (`watch.jq`, `me = dispatcher:<c>`, same
  default states), exported from `internal/watch`.
- Whenever new events exist past its in-memory `since`, it prints a
  `{"cursor":…,"events":[…]}` batch line and advances `since`.
- Every `--heartbeat` seconds (default 30) it prints `{"relay":"heartbeat","ts":…}`.
- A stdout write error ends it with exit 0. This is how a dropped ssh link
  stops the remote process.

**Local: relay sources in `crew stream` and `crew fleet stream`**

They live in the per-bus `loop` (`internal/stream`), so both lanes get them.

- **Discovery.** On each cycle the loop reads its local bus for
  `kind:"dispatch"`, `crew_id==<c>`, `remote==true` rows. Each new host name
  gets one relay goroutine.
- **Exclusive per (host, repo).** Each relay first takes the lock
  `<CrewDir(c)>/relay/<host>.lock.d` (`internal/lock`). If a per-repo stream
  and a fleet lane both run, only one relays; the other retries the lock each
  cycle.
- **The child.** Each relay runs
  `crew remote-run --host <h> --env CREW_ID=<c> -- crew relay --crew <c> --since <cursor> --states <stream's states>`
  as a child, in its own process group.
- **Output.** For every batch line it emits through the stream's shared writer
  `{"cursor":…,"host":"<h>","events":[…each event + {"host":"<h>"}]}`, plus
  `repo` in fleet mode.
- **Cursor.** After the line is written, it stores the cursor in
  `<CrewDir(c)>/relay/<host>.cursor`. That cursor is per (host, repo): one local
  bus maps to one remote path per host. The first connection replays the
  remote backlog, the way fleet joins do.
- **Link loss.** When the child exits, or no line arrives for 3× the heartbeat
  (then the child is killed), the relay emits
  `{"stream":"error","crew":"<c>","host":"<h>","rc":<rc>,"detail":"relay to <h> lost: <first stderr line>","ts":…}`.
  It then reconnects after the stream's `--retry` delay (default 30 s) from the
  stored cursor. It never goes silent.
- **Reap.** A relayed batch that `wants_reap.jq` says wants reaping, and every
  `--reap-every` period while the host has relayed rows, runs
  `crew remote-run --host <h> --env CREW_ID=<c> -- crew reap --quiet` in the
  background. Its output lines are emitted as
  `{"stream":"reap","crew":"<c>","host":"<h>","lines":[…],"ts":…}`. The remote
  reaps its own merged or closed workers with the unmodified reap.
- **Shutdown.** Stream exit (signal or lock loss) kills every relay child's
  process group.

**Lanes.** The relay lives in the streaming lanes only. A dispatcher that
parks on one-shot `crew watch` calls instead (non-Monitor engines) does not see
remote events. `DISPATCHER_PROTOCOL.md` states that following a `--host` worker
requires a `crew stream` or `crew fleet stream` lane, and the dispatch success
line repeats it. Adding relay sources to `crew watch`'s park is a follow-up, not
this iteration.

**Fleet lanes reap remote hosts.** A fleet lane runs no reaps for local repos
today. It does run the remote reap above when it holds a host's relay lock,
because the remote host has no other reaper. This is a deliberate deviation.

**Relayed events are never appended to the local bus.** They exist only as
stream lines. So they cannot satisfy a local gate (a waiver check, a sender
check) and cannot create a local claim or row (Trust boundary).

### 7. Roster and liveness

`crew roster` (the default fold, `main.go:829-897`) merges in remote rows:

- For each host that has `remote:true` dispatch rows for this crew on the
  local bus, it runs
  `crew remote-run --host <h> --env CREW_ID=<c> -- crew roster <c> --live`,
  with a 30 s bound.
- From the remote reply it keeps the rows whose branch has a local remote
  dispatch row for `<h>`, tags them `host:<h>`, and appends them to the local
  rows.
- **`crew roster --live`** (new flag) adds `live: true|false` to each row: true
  when a window on _that host's_ tmux carries `@crew_branch == branch` and
  `@crew_id` equal to the crew or unset.
- A remote worker's liveness is therefore judged on its own host. Killing its
  remote window flips `live` to false, even while the last status still reads
  `working`.
- When a host is unreachable or errors, each of its local dispatch rows becomes
  `{branch, name, host, state:null, live:null, host_error:"<first stderr line>"}`.
  It is never reported dead because no local window exists.
- Local rows are byte-identical to today, since `--live` is only passed
  remotely.

**Not changed:**

- **`crew fleet` and `crew roster-render`.** Their liveness reads only judge
  rows on the local bus, and a remote worker has no status rows there. The
  diagram redraws on local bus changes, which a remote worker never makes.
  Drawing remote rows needs a relay-driven redraw trigger, which is a
  follow-up. Teaching their window probes about `@bridge_crew_branch` now would
  be dead code.
- **The window-colour code.** Inside this repo, `@crew_color` is read only by
  the border formats `dispatch` sets on the worker's own window. That window is
  on the remote host and is rendered there. The mirror window's tab colour
  comes from tmux-og and the owner's tmux status config (`@bridge_crew_color`),
  outside this repo.
- **`crew where`** is the one in-repo consumer the bridge stamps serve
  (component 5).

### 8. Protocol docs

- **`DISPATCHER_PROTOCOL.md`.**
  - Add `--host <name>` to the dispatch usage line.
  - Add a short **Remote workers (`--host`)** subsection under "Scaffold one
    worker per task". It covers:
    - settings and preconditions: the clone already exists, `crew`, `dispatch`
      and `tmux` are on the remote's non-interactive ssh PATH, and the remote's
      engine logins are present;
    - what is routed;
    - that batches with `host` come from a relay, so cursors are remembered per
      `host` (and per `repo` in fleet mode);
    - that `error` lines carrying `host` mean the link dropped and will
      reconnect;
    - that `crew inbox dispatcher:<c>` covers local rows only, and remote msgs
      arrive in stream batches;
    - the attach fallback, and re-running
      `og-remote-open <ssh> crew-<id> --into <session>` after a local tmux or
      dispatcher restart (the mirror is set up only by dispatch and resume);
    - that following a remote worker requires a streaming lane (component 6);
    - that `crew hold` does not record `--host`, so `--host` must be named in
      the held spec's prose and re-passed on release, the same as `--base`;
    - on a macOS remote, the engine's keychain login works only in a tmux
      server started from the GUI login session. Keep one running there; a
      server first started by sshd cannot reach the login keychain.
  - Single-host text is not duplicated.
- **`WORKER_PROTOCOL.md` "When done".** The `dispatcher_pane` ping is skipped
  when `dispatcher_pane:` is empty (always the case on a remote host).
- Run `scripts/gen-adapters.sh` and commit the mirrors.

## Single-host site dispositions

| site                                                                                                                         | disposition                                                                                                                                        | reason                                                                                                                    |
| ---------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| window creation `dispatch.sh:5691` (no `-t`), size `:5667-5697`, stamps `:5717-5726`, send-keys `:5906`, stall-watch `:5930` | **remote-local, targeted**: `DISPATCH_TMUX_SESSION` adds `-t`/new-session and skips client sizing; stamps, send-keys and stall-watch are unchanged | they run on the worker's host against its own tmux; only the target session was implicit                                  |
| `dispatch-resume.sh:1227` new-window, stamps `:1270-1272`                                                                    | **remote-local, targeted**, the same way                                                                                                           | same                                                                                                                      |
| `bus.Locate` (`bus.go:215`)                                                                                                  | **unchanged**                                                                                                                                      | each host's tools find their own bus from cwd                                                                             |
| worker hooks `dispatch-notify.sh:143`, `phase-status.sh:325`, `directive-delivery.sh:88`                                     | **unchanged**                                                                                                                                      | they resolve the remote worktree's own bus                                                                                |
| `dispatch-notify.sh:140,173` dispatcher-pane ping                                                                            | **unchanged code**; the remote stamps `dispatcher_pane:` empty, so it is skipped                                                                   | a local pane id is meaningless on the remote tmux server; the dispatcher learns of the exit from the relayed `exited` row |
| liveness `roster/live.go:36`, `main.go` roster/fleet, `rosterrender/probe.go`                                                | roster **routed query** (`--live` on the remote); fleet and rosterrender **out of scope**                                                          | see component 7                                                                                                           |
| `where/where.go`                                                                                                             | **bridge stamp, then routed**                                                                                                                      | component 5                                                                                                               |
| `crew nudge` (`nudge/nudge.go`, `arm.go`)                                                                                    | **routed**                                                                                                                                         | frame and anchor gates need the remote pane                                                                               |
| `permission-check.sh:128,171`                                                                                                | **routed** (whole command)                                                                                                                         | it reads remote transcripts, `ps` and panes                                                                               |
| reap `remove.go`, `candidate.go`, `release.go`                                                                               | **remote-local, unchanged**, triggered by the relay through routed `crew reap --quiet`; the remote `dispatch` also reaps before each dispatch      | worktrees, windows and processes are remote                                                                               |
| sender anchoring `sender/sender.go`                                                                                          | **remote-local, unchanged**                                                                                                                        | Trust boundary §4                                                                                                         |
| `host:` on dispatch rows (`dispatch.sh:5395`)                                                                                | the remote row keeps the remote uname; the **local** row carries the settings host name plus `remote:true`                                         | routing key                                                                                                               |
| `crew resolve-target` (`resolve.jq:39`)                                                                                      | **extended** with a `remote` column                                                                                                                | dispatch-resume and permission-check routing                                                                              |
| `dispatch resume` host refusal (`dispatch-resume.sh:492-497`)                                                                | **routed** for `remote:true` rows; refusal kept otherwise                                                                                          | component 5                                                                                                               |
| `crew fleet stream` relay seam (`stream/fleet.go`)                                                                           | **extended**: relay sources live in the shared per-bus loop                                                                                        | component 6                                                                                                               |
| claims index                                                                                                                 | the local row is mirrored locally; the remote keeps its own index                                                                                  | the local index guards this machine's dispatches, plus the component 3 step 4 guard                                       |
| claims index (residual)                                                                                                      | a reaped remote worker's mirrored local row stays an unreleased claim until it ages past 24 h                                                      | the local bus never sees the remote's `reaped`; follow-up                                                                 |
| repo registry                                                                                                                | **remote-local, unchanged**                                                                                                                        | the remote `dispatch` registers its own clone                                                                             |
| fan-out budget / load gate (`fleet_gate`, `_host_load` `dispatch.sh:1182`) and fan-out ledger (#927)                         | **remote-local**: run by the remote `dispatch` only                                                                                                | the per-host load is the point                                                                                            |
| local-model slots                                                                                                            | **remote-local**                                                                                                                                   | the slots are per machine                                                                                                 |
| claude quota (`engine-budget.json`)                                                                                          | **local** (component 3 step 3); the remote's own check also runs and fails open on a missing cache                                                 | the shared account's cache is on the dispatcher host                                                                      |

## Trust boundary (ssh)

1. **No caller data in a remote shell line.** The remote command line is the
   literal `crew remote-exec`. The title, spec, `--owner-auth` text, reply
   bodies and every other argument are JSON fields on stdin. `remote-exec`
   `execve`s an allowlisted argv, never through a shell. The spec reaches
   `dispatch` as a file, the same `DISPATCH_SPEC` contract as locally.
2. **The ssh target and remote path come only from settings.** The relay,
   reply, nudge, where, permission-check and resume routes take the host _name_
   from a local `kind:"dispatch"` row and look it up in settings.
   - Only `dispatch` writes those rows. A worker can only forge one with a
     path-named write to the crew dir, which is out of scope per the existing
     boundary.
   - A forged row could at most point a reply at another configured host's
     bus.
   - The host name is regex-checked before it is used as a key.
   - The `ssh` value cannot start with `-`, and `--` ends ssh's options.
3. **Remote input validation.**
   - `remote-exec` refuses unknown request keys, versions, env keys, env values
     outside their regexes, and argv outside the allowlist.
   - It verifies that the repo's origin matches the requested slug before
     running anything.
   - `BatchMode=yes` means ssh never prompts.
   - Agent, X11 and port forwarding are forced off, so no dispatcher-side
     credential becomes reachable on the remote.
4. **Dispatcher identity on the remote.**
   - A routed write runs under sshd, outside tmux, with no `CREW_WORKER_ID` or
     `CREW_ROLE_ID` in its env. The remote's unchanged sender check therefore
     accepts `dispatcher:<c>`.
   - A remote worker is held by the same tripwire as on a single host.
     `remote-exec` does not clear `CREW_WORKER_ID`/`CREW_ROLE_ID`, and adds no
     authority of its own. So a worker calling `crew remote-exec`, `crew reply`
     or `crew msg` from its pane still carries its worker env and its stamped
     pane ancestry, and is refused `dispatcher:` exactly as on one host.
   - The residual is the existing one: the sender is self-asserted, the uid is
     shared, and a process that edits its env or double-forks out of its pane
     can post as anything. The protocol already documents this; no new hole is
     added.
5. **Relayed data carries no authority.**
   - Relayed events are tagged `host` and printed as stream lines. They are
     never appended to the local bus, so they satisfy no local gate (waiver
     check, sender check, claim).
   - The relay parses each line as JSON and only adds fields.
   - The ssh stderr line in an `error` detail is JSON-encoded.
6. **Laundering check (the protocol's three-part test).** The new inputs a
   remote claude worker controls are its own remote bus rows. They reach the
   dispatcher only as stream data, the same as a local worker's rows today: no
   execution, grant or identity. `dispatch_row:` is produced by the remote
   `dispatch` process, not the worker, and is validated before the local append.

## Testing

**Fake remote host (AC2/AC3).** A bats file, `tests/remote-host.bats`, sets up:

- **An `ssh` shim on PATH.** It parses `-T`, `-o …` and `--`, then
  `<target> <cmd…>`. It runs `<cmd…>` with `sh -c` in a "remote" environment:
  - separate `HOME`, `XDG_{DATA,CONFIG,STATE}_HOME` and `TMUX_TMPDIR`;
  - `TMUX`, `TMUX_PANE` and `CREW_*` unset;
  - stdin passed through.
  - Targets named `fail-*` exit 255, either immediately or after N calls.
- **A second clone** of the bare origin as the "remote" repo.
- **The real tmux**, on the remote's private `TMUX_TMPDIR`.
- **Stubs** for `wt`, `gh` and the engine, on the dispatch.bats pattern.
- **A stub `og-remote-open`.**

**Go unit tests** cover:

- `internal/remote` (settings parsing and validation, request build, route
  lookup);
- `remote-exec` (allowlists, origin check, env, spec file, detached `dispatch` surviving the caller's death);
- `crew dispatch-row`, and `crew identity` honouring `CREW_IDENTITY_AVOID`;
- `crew relay` (batches, heartbeat, EPIPE exit);
- the stream relay (host tagging, cursor persistence, the error line and
  reconnect against a fake `remote-run`, lock exclusivity);
- `roster --live` and the remote merge;
- `where` bridge-stamp resolution;
- `resolve-target`'s 5th column.

**`tests/dispatch-config.bats` and `tests/module.bats`** cover the `hosts`
validation and the module option.

## Acceptance mapping

- **AC1:** this spec plus the plan, under `docs/superpowers/`.
- **AC2:** `tests/remote-host.bats`, which checks that:
  - the remote worktree, window and `WORKER_TASK.md` exist;
  - a remote `crew status … blocked` shows up in a local `crew stream` batch
    with `host`. The test posts `blocked` because `working` is not in the
    stream's default states;
  - a local `crew reply` shows up in the remote `crew inbox`;
  - local `crew roster` shows the remote row with `live:true`;
  - after `tmux kill-window` on the remote socket, the row shows `live:false`.
- **AC3:** bats cases for:
  - an unknown host;
  - a missing repo mapping;
  - ssh exiting 255 on the dispatch call after the preflight succeeded, with the
    recovery query failing too: no local dispatch row, claims-index row or lock
    is left, and stderr names `--host`/ssh;
  - the recovery path, where ssh returns 255 after the remote dispatch
    completed: the row is found and recorded locally;
  - `dispatch --host <h> --recover <session>` with no row: exit 1 and nothing
    written; with a row: recorded once, and a second run is a no-op;
  - codename resolution: a target matching both a local window and a
    `remote:true` row is refused as ambiguous (Go unit);
  - `crew identity --occupied` lists unsuffixed names, including a
    just-dispatched worker with no status and a mirror window's
    `@bridge_crew_name` (Go unit);
  - a title and spec containing `$(touch …)`, backticks, `;`, quotes and
    newlines (in the spec): they arrive verbatim in the remote
    `WORKER_TASK.md`, and no marker file is created.
- **AC4/AC5:** the existing suites, plus the CI gate commands.
