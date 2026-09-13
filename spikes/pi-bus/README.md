# spikes/pi-bus

Spike A prototype for #142: a thin pi extension that lets a `pi` worker talk
to the crew bus (`adapters/core/crew.sh`) through tools and heartbeats instead
of shelling out to `crew` directly. **Not wired into dispatch** — nothing in
`adapters/` loads this, and there is no bus format change. This is evidence
for the compose-vs-build evaluation, not a shipped feature.

## What it does

- `crew-bus.ts` — default-exported pi extension. No-op unless `CREW_WORKER_ID`
  is set (dispatch sets this per window; see `dispatch.sh:1446`).
  - Registers four tools — `crew_status`, `crew_msg`, `crew_inbox`,
    `crew_roster` — each a thin `pi.exec` wrapper around the matching `crew`
    subcommand, with plain JSON Schema `parameters` and `promptGuidelines`
    steering the LLM to use the tool instead of a bash `crew` call.
  - Posts heartbeats on `turn_start`, `tool_execution_start`, and
    `agent_settled` as a `crew msg <id> heartbeat:<crew> '{"event",...,"ts"}'`
    row — a message to a synthetic sink, not a `status` row, so a `working`
    heartbeat can never overwrite a worker's own `blocked` in the roster.
    Throttled to at most one per `CREW_HEARTBEAT_S` (default 60s, `0` means no
    throttle); `agent_settled` always posts.
  - The heartbeat sink id is resolved lazily, on the first heartbeat attempt,
    not at extension load — pi awaits extension factories serially at
    startup, so an eager `crew id` there would block session start. It's
    cached only once resolved, so a transient failure (e.g. `WORKER_TASK.md`
    not written yet) is retried on the next heartbeat attempt, and a failed
    resolution never consumes the throttle.
- `bus.ts` — the pure logic (arg builders, throttle with an injected clock).
  Zero imports.
- `stall-check.sh` — read-only `jq` query over the bus log. Groups worker ids
  seen in `status` rows (session suffix stripped, so a resumed worker reads as
  one identity) into "stale" (has ≥1 heartbeat row, but the latest is older
  than the threshold) and "no-heartbeat" (never posted one — extension not
  loaded, not itself evidence of a stall). Workers whose latest status is
  `done`/`failed`/`exited` are skipped entirely.

## What it does not do

- **A single long silent tool call emits no heartbeat events.** Heartbeats
  fire on `turn_start`/`tool_execution_start`/`agent_settled`, not on a timer,
  so a long-running tool (e.g. the hung `nix flake check` behind lazytmux
  545's true-positive stall) produces no heartbeat traffic for its duration —
  matching the existing D3 quiet-window blind spot, not fixing it.
- **`stall-watch` does not read heartbeat rows.** It watches panes and posts
  `status` rows; this prototype's heartbeats are invisible to it today. Using
  heartbeats to change watchdog behavior would be a follow-on change, not part
  of this spike.
- **Log growth is roughly 1 row/minute per worker** while heartbeats are
  active (bounded by `CREW_HEARTBEAT_S`), on top of whatever `status`/`msg`
  traffic the worker already produces. The bus is an append-only log read
  wholesale (`jq -s`) by several commands, so this is a real, if modest, cost.
- **pi API churn.** This prototype pins to `@earendil-works/pi-coding-agent`
  0.85.1 semantics. Every surface it uses is cited below; a pi upgrade should
  re-check these before trusting the extension still behaves as written:
  - `pi.exec(command, args, {signal, timeout, cwd})` → `docs/extensions.md:1668`
  - `pi.registerTool({...parameters, promptGuidelines, execute})` →
    `docs/extensions.md:1365`
  - `agent_settled` (agent will not continue automatically) →
    `docs/extensions.md:567`

## Environment variables

| Var                | Default | Meaning                                                                                                                                    |
| ------------------ | ------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `CREW_WORKER_ID`   | —       | Required. Unset/empty means the extension registers nothing.                                                                               |
| `CREW_BIN`         | `crew`  | Binary to exec for every bus operation.                                                                                                    |
| `CREW_HEARTBEAT_S` | `60`    | Minimum seconds between non-`agent_settled` heartbeats. Unset/empty/non-numeric/negative falls back to 60; `0` is honoured as no throttle. |
| `CREW_ID`          | —       | Fallback heartbeat-sink id, used only if `crew id` (which itself reads `WORKER_TASK.md` first) fails or returns empty.                     |

Neither `crew id` nor `CREW_ID` is consulted until the first heartbeat
attempt (see above). If neither yields an id, that heartbeat is skipped (an
empty `heartbeat:` sink would be rejected by `crew`) but the four tools still
register, and resolution is retried on the next heartbeat attempt.

## How to run

Inside a dispatched worker window (where `CREW_WORKER_ID`/`CREW_ID` are
already set by `dispatch.sh`):

```sh
pi -e ./spikes/pi-bus/crew-bus.ts
```

Or drop/symlink it into `$PI_CODING_AGENT_DIR/extensions/` (default
`~/.pi/agent/extensions/`) for global auto-discovery — dispatch launches pi
with `--no-approve`, which ignores project-local `.pi/extensions` for the run
(`usage.md:249`), so global discovery is the only way an extension loads
without passing `-e` explicitly.

## stall-check.sh usage

```sh
spikes/pi-bus/stall-check.sh <threshold_s> [events.jsonl]
```

Defaults to `$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl`
when no log path is given. Prints one line per flagged worker:

```
stale worker:feat/x 187
no-heartbeat worker:feat/y
```

## Tests

No npm dependencies; Node's built-in test runner with type stripping. Pinned
to this repo's `flake.lock`, not the user's node/npm registry:

```sh
nix shell --inputs-from . nixpkgs#nodejs_24 -c node --test 'spikes/pi-bus/test/*.test.ts'
```

(the quoted glob matters — `--test` treats bare args as globs from Node 21
on). Requires `git`, `jq`, and `bash` on PATH — run it inside `nix develop`.

## CI proposal

Add one step running the pinned test command above (or add `nodejs` to the
devshell and run `node --test` under plain `nix develop`, the same way
`shellcheck` runs today). Cost: the locked `nodejs_24` closure is 228.4 MiB
slim / 253.0 MiB full (`nix path-info --inputs-from . -Sh`). Separately, CI's
`shellcheck` step currently lints only `adapters/core/*.sh scripts/*.sh`, so
`spikes/pi-bus/stall-check.sh` would need adding to that step's file list.
