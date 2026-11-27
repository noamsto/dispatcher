# spikes/headless-supervisor

Spike for #1853: a small supervisor (`hsup`) that launches one claude or pi
worker as a child process and owns its stdin/stdout through the engine's
structured interface, instead of typing into and scraping a tmux pane.
**Not wired into dispatch or crew** — nothing in `dispatch`, `crew`,
`adapters/` or the nix module references this directory. It is evidence for
the findings in `docs/headless-workers-spike.md`, not a shipped feature.

## Why Go

The crew stack is Go and the devshell already has `go` and `golangci-lint`.
A supervisor is mostly concurrent child I/O (read frames, write replies,
watch for silence, serve a control socket), which goroutines and channels own
cleanly. The module uses the standard library only. `pi/gate.ts` is the one
TypeScript file because pi extensions are TypeScript.

## What it does

- Launches a child per engine and logs every raw frame to a JSONL transcript.
- Surfaces permission requests, questions and plan approvals as structured
  events and answers them: allow once, allow for the session, deny with a
  reason the model sees, or answer an option.
- Delivers a user message between turns and mid-turn; interrupts a turn and
  steers afterwards.
- Detects a silent child (process alive, no events for a limit), interrupts
  it, and as a last resort kills and resumes on the same session id.
- Hands a session to the real TUI in a tmux window and back, under a file
  lock so two processes never write the session at once.

## What it does not do

- It does not read or write the crew bus, roster or any dispatch state.
- It does not replace or delete any watchdog, nudge or pane code.
- It does not drive codex or cursor.
- The case scripts are one-shot checks on haiku, not a production worker loop.

## Layout

| Path                  | Role                                                                                                        |
| --------------------- | ----------------------------------------------------------------------------------------------------------- |
| `cmd/hsup`            | CLI: `cases`, `case`, `serve`, `ctl`                                                                        |
| `cmd/mcpecho`         | stdio MCP server with tools `echo` and `permission_prompt` (the mcp and bash-deny-mcptool cases)            |
| `internal/event`      | Engine-neutral `Event`, `Request`, `Decision`, `Engine` interface, shared `Options`                         |
| `internal/claude`     | claude stream-json adapter: control protocol (`can_use_tool`, `hook_callback`, `interrupt`), frame decoding |
| `internal/pi`         | pi `--mode rpc` adapter: `extension_ui_request`/`response`, `steer`, `abort`, retry events                  |
| `internal/acp`        | Minimal ACP client (`session/request_permission`) for pi-bolt and claude-agent-acp                          |
| `internal/supervisor` | Child lifecycle, `WaitFor`, silence detection, control socket, session state file                           |
| `internal/lock`       | `flock(2)` session lock, compatible with `flock(1)`                                                         |
| `internal/transcript` | JSONL writer and scrubber                                                                                   |
| `internal/stub`       | Local HTTP provider stub that returns scripted 429/529/400 for the error case                               |
| `internal/cases`      | The scripted cases                                                                                          |
| `pi/gate.ts`          | pi extension: `tool_call` gate that asks the supervisor, plus an `ask_user` tool                            |
| `pi/errstub.ts`       | pi extension that registers a `stub` provider at `$HSUP_STUB_URL`                                           |
| `*.sh`                | `run.sh`, `takeover.sh`, `demo-takeover.sh`: reproduction and take-over scripts                             |
| `transcripts/`        | Committed, scrubbed evidence: `<engine>-<case>.jsonl`                                                       |

## Engine-neutral contract

Both adapters, and the ACP client, implement one interface in `internal/event`:

- `Event` with a `Kind` (Init, TurnStart, Text, ToolStart, ToolResult,
  Permission, TurnEnd, Retry, Error, Exit), timestamp, tool name and id, a
  `Detail`, text, session id, and for Permission a `*Request`.
- `Detail` carries what a decision needs: kind (bash, write, read, mcp, web,
  ask, plan, other), command, path, URL, domain, server, options, questions.
- `Request` is the thing to answer; `Decision` is a verdict (`allow_once`,
  `allow_session`, `deny`, `answer`) plus a reason or answers.
- `Engine` has `Start`, `Events`, `Send`, `Interrupt`, `Respond`, `SessionID`,
  `Close`, `Kill`, `Wait`.

Cases and the supervisor code against this only. One `Policy` decides each
request from `Detail`; the same policy runs on claude, pi and ACP, so a matrix
row differs only in the mechanism underneath.

## How to run

From this directory, inside `nix develop`:

```fish
./run.sh bash-allow claude
./run.sh all pi
```

`run.sh <case|all> <engine>` builds `hsup` and `mcpecho` into a temp dir, runs
the case in a throwaway work dir (never the repo), writes
`transcripts/<engine>-<case>.jsonl`, and prints one `PASS ...` or `FAIL ...`
line per case. `all` runs every case defined for the engine and prints a
`SUMMARY`. Engines: `claude`, `pi`, `pi-acp`, `claude-acp`. `hsup cases` lists
`<case> <engine>` pairs.

`claude-acp` needs the adapter binary, which is unfree in nixpkgs:

```fish
set -x HSUP_CLAUDE_ACP_BIN (NIXPKGS_ALLOW_UNFREE=1 nix build --impure --no-link --print-out-paths nixpkgs#claude-agent-acp)/bin/claude-agent-acp
./run.sh acp-bash-allow claude-acp
```

Overrides:

| Variable              | Meaning                                                                                  |
| --------------------- | ---------------------------------------------------------------------------------------- |
| `HSUP_CLAUDE_BIN`     | claude binary (default: the wrapped binary resolved from the PATH wrapper's `exec` line) |
| `HSUP_PI_BIN`         | pi binary (same resolution)                                                              |
| `HSUP_PIBOLT_BIN`     | pi-bolt binary for `pi-acp`                                                              |
| `HSUP_CLAUDE_ACP_BIN` | claude-agent-acp binary for `claude-acp`                                                 |
| `HSUP_PI_MODEL`       | pi model id (default `anthropic/claude-haiku-4-5`)                                       |
| `HSUP_MCPECHO`        | path of the built `mcpecho` (set by `run.sh`)                                            |
| `HSUP_GATE`           | path of `pi/gate.ts` (set by `run.sh`)                                                   |

The children run the wrapped binaries, not the PATH wrappers: the claude
wrapper injects the owner's settings, hooks and plugins, and the pi wrapper
switches to pi-bolt when `CREW_WORKER_ID` is unset. Claude children also get
`--setting-sources local --strict-mcp-config` so the owner's allow rules do
not pre-approve tools. These variables are removed from the child's
environment: `CREW_WORKER_ID`, `CREW_ID`, `CREW_LEAD_*`, `TMUX`, `TMUX_PANE`,
`CLAUDECODE`, `CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_CODE_ENTRYPOINT`,
`CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_EXECPATH`,
`CLAUDE_CODE_SUBAGENT_EFFORT`, `CLAUDE_CODE_SSE_PORT`.

### Cases

| Case                | What it proves                                                                            |
| ------------------- | ----------------------------------------------------------------------------------------- |
| `bash-allow`        | A Bash permission arrives with the command; allow once runs it                            |
| `bash-deny`         | Deny with a reason; the model sees the reason, the command does not run                   |
| `bash-session`      | Allow for the session; a second call needs no new request                                 |
| `outside-write`     | Write outside the work dir arrives with the path; allow once                              |
| `outside-read`      | Read outside the work dir arrives with the path; allow once                               |
| `mcp`               | MCP tool permission arrives with the server; allow once                                   |
| `web`               | Web fetch permission is domain-scoped; session allow covers the next fetch                |
| `ask`               | The model's question arrives with options; the chosen option reaches the model            |
| `plan`              | Plan approval arrives; approving lets the model proceed to write                          |
| `error`             | Provider errors arrive as `Retry`/`TurnEnd` events (local stub provider)                  |
| `midturn`           | A message sent mid-turn is folded into the running turn                                   |
| `interrupt`         | Interrupt ends the turn at once; a follow-up in the same session runs                     |
| `hang`              | Silence past the limit is detected and recovered by interrupt                             |
| `kill-resume`       | Kill mid-tool, resume on the same session id, no conversation state lost                  |
| `takeover`          | Pause, TUI turn under the lock, hand-back; both turns recalled                            |
| `bash-deny-hook`    | claude only: SDK `PreToolUse` `hook_callback` denial reaches the model                    |
| `bash-deny-mcptool` | claude only: `--permission-prompt-tool mcp__echo__permission_prompt` denial reaches model |
| `acp-bash-allow`    | `pi-acp` and `claude-acp` only: bash permission through `session/request_permission`      |

## Take-over

```fish
set s (mktemp -d)
hsup serve --engine claude --work /tmp/w --state $s --model haiku --policy allow
hsup ctl $s send "refactor the parser"
hsup ctl $s status
./takeover.sh $s [--tmux-socket NAME]
```

`hsup serve` runs the child under `<state>/session.lock` and listens on a
control socket (mode 0600) in the state dir. The state dir must be a real
directory (not a symlink) owned by the caller with no group or other bits, so a
fixed path such as `/tmp/s` is refused; use `mktemp -d`. `hsup ctl` and
`takeover.sh` apply the same check, and read `session.json` only as a regular
file owned by the caller and not writable by group or others. Pause completes
when the child was reaped even with a non-zero exit, reporting the exit error
as a note; it keeps the lock only while the child may still be alive. Children
run under `exec.CommandContext` (cancel closes stdin, 10s `WaitDelay`), so
`hsup case --timeout` and SIGTERM stop them gracefully. `--policy allow|deny` decides every
permission request; the default is `deny`, and `allow` answers allow once.
The child inherits the lock fd, so the flock lives until both hsup and the
child are gone; the inherited fd is not close-on-exec, so a grandchild (for
example a background shell a tool started) can keep holding it after the
engine exits, and take-over then refuses until it exits. `hsup ctl <state>
<verb>`:

| Verb      | Effect                                                                          |
| --------- | ------------------------------------------------------------------------------- |
| `pause`   | stop the child and release the session lock (kept while the child may be alive) |
| `resume`  | re-acquire the lock and resume the child on the same session id                 |
| `send`    | deliver a user message (mid-turn if a turn is running)                          |
| `status`  | liveness: process state and time since the last event                           |
| `session` | print `session.json` (`engine`, `session_id`, `work`, `bin`, ...)               |
| `stop`    | close the child and exit                                                        |

`takeover.sh <state-dir> [--tmux-socket NAME]` pauses the headless worker,
reads the engine and session from `session.json` (no engine argument), opens
`claude --resume <id>` or `pi --session <id>` in a new tmux window under
`flock -o -n` on the session lock, waits for the TUI to exit, then runs
`hsup ctl resume`. If the lock is held it refuses and headless stays paused.

`demo-takeover.sh <claude|pi> <transcript> [work-dir]` is the automated proof
on a scratch tmux server: a headless turn, a TUI turn under the lock (headless
resume refused meanwhile), then a headless turn that must recall both.

The claude TUI shows a workspace-trust dialog for a new directory; the demo
selects Yes, which records trust for the temp dir in the owner's claude
config.

Claude plan mode (the `plan` case) writes the plan to `~/.claude/plans/` in the
owner's home, outside the throwaway dir.

## Transcripts

One JSON object per line: `{"t": <ms>, "dir": "in|out|sup", "frame": ...}`.
`in` is child to supervisor, `out` is supervisor to child, `sup` is a
supervisor decision or note. Frames are written after scrubbing:

- Paths become `$HOME`, `$WORK`, `$OUTSIDE`, `$STATE`, `$TMP`; a prefix only
  matches after a path boundary, so streamed fragments do not double a
  placeholder. The OS user name becomes `$USER`.
- Secret shapes are redacted: `sk-ant-`, `sk-or-` and other `sk-` keys,
  `gh*_` tokens, `github_pat_`, `AIza` keys, JWTs, `Bearer ...` values and
  env-dump lines (`NAME_KEY=...`, `_TOKEN`, `_SECRET`, `_PASSWORD`). Keys that
  look like api key, token, secret, authorization, password, cookie or
  credential become `"[redacted]"`; email-shaped strings become `[email]`; the
  hostname becomes `$HOST` (replaced before the user name). Secret patterns
  are word-bounded.
- claude `system/init`, the `initialize` response and `rate_limit_event`
  frames are reduced to a few fields; `slash_commands`, `skills`, `agents`,
  `plugins`, `mcp_servers` and `commands` become counts; `account`,
  `authStatus`, `credential_expires_at`, `env`, `environment` and keys ending `_dir`/`Dir` are dropped.
- ACP `_claude/rateLimit` keeps only `status`, `rateLimitType` and `resetsAt`;
  `availableCommands` becomes a count; `models`, `modes` and
  `configOptions` are dropped, keeping only the current mode and model ids.
- If a frame cannot be scrubbed it is not written; a `scrub_error` marker
  stands in its place.

These rewrites are global. A missing key or a placeholder in a transcript is
a scrubbing result, not a protocol fact.

The pi transcripts, claude ask and takeover, and pi-acp were recorded with the
current scrubber. Claude plan, bash-allow and claude-acp were recorded after
the first review round and then post-processed. The remaining claude
transcripts were recorded before the scrubber had distinct placeholders, so in
them `$WORK` also stands for the outside and state dirs. Post-processing of
the older transcripts: placeholder normalization (a temp path doubled into
`$WORK$WORK` became `$TMP/tmp`), `credential_expires_at` and ACP rate-limit
detail removed, and the ACP command and model inventory stripped.

One known scrubber gap remains in new runs: a path rule only matches when the
path is followed by `/` or the end of the string, so where pi embeds the work
dir in text (`<cwd>\n…/work\n</cwd>` in its system prompt) the frame shows
`$TMP/tmp.<rand>/work` instead of `$WORK`. Nothing leaks (the `/tmp` prefix is
still replaced); it is a follow-up.

## Cost

All live runs used haiku: claude children `--model haiku` (claude-haiku-5-5,
claude-code 2.1.332, owner OAuth subscription), pi children
`anthropic/claude-haiku-4-5` on pi 1.6.1's direct anthropic provider. claude
runs draw the owner's subscription, and `run.sh all claude` is not cheap to
repeat; run single cases when iterating.

## Tests

```fish
go test ./...
```

Tests use fake children (re-exec helpers speaking each protocol) and never
start a live engine. The lock test takes the lock from an external process.
`nix develop -c shellcheck *.sh` and `go vet ./... && golangci-lint run ./...`
are the other gates.
