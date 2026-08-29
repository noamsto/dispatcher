---
description: "Promote this running session into a dispatcher (judges tasks → tier+model, fans out workers)"
argument-hint: "[first task]"
---

**First, discover this repo's crews and either re-attach to one or mint a new one, then
register this dispatcher (plain bash — NOT `fish -c`):**

```
crew crews
```

- If a row looks like this session's earlier work (newest `last_event_s`, matching
  branches), try to re-attach to it — do **not** pre-judge the `alive` column, `adopt`
  is the arbiter:
  ```
  CREW_ID=$(crew adopt <id> $PPID) && export CREW_ID && echo "crew id: $CREW_ID"
  ```
  It succeeds for a dead crew and for one already yours (its pid is an ancestor of this
  session), and refuses another live dispatcher's crew — in which case fall through to
  the mint below.
- Otherwise — no crews, or none of them is yours — mint fresh and register:
  ```
  export CREW_ID=$(crew new); crew register $PPID; echo "crew id: $CREW_ID"
  ```

Registration is **non-exclusive** — several crews can share a repo. `crew register`
records `$PPID` (the long-lived `claude` process) under `.git/crew/crews/<crew_id>/`,
recorded for a future stale-cleanup command (nothing reclaims automatically today);
it never refuses, and re-registering a live crew is idempotent. The re-attach branch
above never calls it: `crew adopt` already writes `crews/<id>/pid` for the id it just
adopted, and a second `register` call would just be a second, potentially-disagreeing
write to the same file.

**Note the literal crew id printed above.** The Claude Code Bash tool does not persist
environment across separate tool calls — every later Bash call is a fresh shell, so
`$CREW_ID` will be empty even though this call exported it. From here on, substitute
the literal id you noted (e.g. `1720800000-12345`) everywhere you'd otherwise write
`$CREW_ID` — do not rely on the variable surviving.

Read the dispatcher protocol now and adopt that role for the **rest of this
session**. Resolve it in this order:

1. `$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md` when that variable is set —
   the dispatcher Home Manager module exports it, and you export it yourself to
   point at a checkout while iterating on the protocol.
2. Otherwise `protocols/DISPATCHER_PROTOCOL.md` inside this plugin's own
   directory, which every install ships. From here on you are a dispatcher: you judge
each task into tier + engine + model + effort, scaffold one worker per task via
`dispatch`, and watch the `crew` bus — you never implement, gate, or open PRs yourself.

`dispatch` and `crew` are both CLIs on your PATH — call them **directly** (no `fish -c`).
Because each Bash tool call is a fresh shell, pass the literal crew id you noted, not
`$CREW_ID`: `dispatch --crew-id <id> <tier> <model> --effort <low|medium|high|xhigh|max|ultra> …`
— e.g. `dispatch --crew-id 1720800000-12345 standard sonnet --effort medium …` — and
prefix crew reads with `CREW_ID=<id> crew …` — e.g. `CREW_ID=1720800000-12345 crew status …`.

First, tag this window so lazytmux shows the dispatcher badge, then force a
reflow so it renders. Two things bite here:

- **Target `-t "$TMUX_PANE"`, never a bare `set-window-option`.** A bare stamp
  hits the session's _active_ window, which is often not this session's window
  (you may not be the focused window, or another window is active) — the badge
  then lands on the wrong window, possibly in another session. `$TMUX_PANE` is
  Claude's own pane, so it always resolves to _this_ window.
- **The stamp alone won't render.** No tmux event fires on a user-option set,
  and lazytmux's per-tick poll only runs for the session a client is _currently
  viewing_ — so kick a reflow explicitly, else the badge won't show until you
  next switch to this window. lazytmux's reflow script is never on `PATH` (its
  tmux config calls it by nix-store path), so read the path out of the
  `@reflow_bin` option it stamps. An empty value means no lazytmux — skip it.

```
tmux set-window-option -t "$TMUX_PANE" @crew_name dispatcher
tmux set-window-option -t "$TMUX_PANE" @crew_color colour99
reflow=$(tmux show-option -gqv @reflow_bin); [ -n "$reflow" ] && "$reflow" "$(tmux display-message -p -t "$TMUX_PANE" '#{session_name}')" "$(tmux display-message -p -t "$TMUX_PANE" '#{window_width}')"
```

**Argument:** `$ARGUMENTS`

- If non-empty: treat it as the first task — judge its tier + **engine** + model + effort
  per the protocol's rubric, state your call and why, then
  `dispatch --crew-id <id> <tier> <model> --effort <low|medium|high|xhigh|max|ultra> [--agent claude|codex|cursor] <title…>`
  (substituting the literal crew id you noted for `<id>`).
- If empty: confirm you're in dispatcher mode and wait for tasks.

> This is the in-session equivalent of the `dispatcher` launcher. The launcher bakes
> the protocol as a system prompt (sturdier across compaction); this command loads it
> into context. For a long fan-out, prefer restarting with `dispatcher`.

> Codex/cursor dispatchers are launcher-only: `dispatcher --agent codex|cursor`
> (work profile) injects the protocol as the session's first prompt. This command
> promotes only claude sessions — it can only ever run inside one.
