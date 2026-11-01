---
name: remote-host
description: Use when a worker runs on another machine (--host), to follow, recover and route its events
---

# remote-host

- **Remote workers (`--host <name>`).** Runs the worker on another machine.
  - **Settings.** `hosts.<name> = {ssh, repos."owner/repo" = path, chrome?}`: `programs.dispatcher.hosts` in home-manager, or user settings. The ssh target and path always come from settings, never from a bus row. `chrome = true` marks a host that runs the owner's browser; `--host <h> --chrome` needs it.
  - **Preconditions.** The clone exists at the mapped path; `crew`, `dispatch` and `tmux` are on the remote's non-interactive ssh `PATH`; the remote's engine logins are present. A macOS remote needs a running tmux server started from the GUI login session (one first started by sshd cannot reach the login keychain). Use `--host` only from a streaming lane (`crew stream`, `crew fleet stream`); a park-lane dispatcher (cursor, codex, pi) never uses it (`crew watch` has no relay for remote events).
  - **Refused with `--host`:** `--add-dir`, `--base`, `--status`, `--lazy`, `--profile` (expand it first with `dispatch --profiles`; the remote resolves profiles against its own settings), and the env overrides `DISPATCH_PRECHECK`, `DISPATCH_IGNORE_RUNG`, `DISPATCH_SKIP_MODEL_CHECK`, `DISPATCH_SHAPE`, `DISPATCH_CLAUDE_CONNECTORS` and `DISPATCH_CLASS_SIGIL` (the remote never sees this env); under `--host` a claude rung or pace refusal means drop to the standard rung or ask the human. `DISPATCH_DRAFT_PR=1` is the one override that travels, as `--draft`. The engine roster is the remote's (`--agent` must be in its `dispatch --engines`); claude quota is checked locally; the remote judges the fan-out budget and load gate.
  - **Window.** The worker runs in the remote tmux session `crew-<id>`, mirrored with `og-remote-open <ssh> crew-<id> --into <session>`; otherwise dispatch prints `ssh -t <ssh> tmux attach -t crew-<id>`. Only `dispatch` and `dispatch resume` set the mirror up: re-run that `og-remote-open` after a local tmux or dispatcher restart.
  - **Routed to the remote.** `crew reply`, `crew nudge`, `crew where` (prints ` [host <h>]`; the mirror window comes first), `permission-check` and `dispatch resume`. Name a remote worker by branch or codename, never by `%pane` (that host's, meaningless to local `tmux` and `crew where`). Exception: `permission-check --pane <%id> --branch <branch> [--answer]` takes the `%id` verbatim from the relayed `prompt:` detail. For any other `%id` from a batch carrying `host`, see the "verify, then act" recovery; never aim local `tmux` at it.
  - **Roster.** `crew roster` adds remote rows with `host` and `live` (judged on the remote's tmux). An unreachable host gives `host_error` rows with `state` and `live` null: never read that as dead.
  - **Inbox.** `crew inbox dispatcher:<c>` covers local rows only; a remote worker's messages arrive in stream batches (see "Read the bus").
  - **Broken link.** Exit 255 means the link dropped mid-dispatch and the remote worker could not be confirmed. Once the link is back, run the `dispatch --host <h> --recover <session> --crew-id <id>` the message names; it writes nothing when the remote made no worker.
  - **Holds.** `crew hold` does not record `--host`: name the host in the held spec's prose and re-pass `--host` on release.
