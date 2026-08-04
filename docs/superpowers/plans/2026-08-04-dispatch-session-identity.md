# Dispatch reuse-or-refuse + session-scoped bus identity — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `dispatch` from stacking a second agent in an occupied worktree, and make bus identity session-scoped so one session's directive can never be delivered to its successor.

**Architecture:** Two new stateless `crew` seams — `occupants <path>` (worker windows at a worktree, keyed on the `@crew_name` window option) and `sessions <branch>` (per-session fold of the event log) — are consumed by a gate in `dispatch` and by `reap`. Worker identity becomes `worker:<branch>#<sid>`, issued by `dispatch` and carried to the worker in the `CREW_WORKER_ID` env var rather than self-derived from the branch.

**Tech Stack:** bash (POSIX-ish, `set -euo pipefail`), jq 1.8, tmux, bats 1.12, Nix `writeShellApplication`.

**Spec:** `docs/superpowers/specs/2026-08-04-dispatch-session-identity-design.md` (issue #17)

## Global Constraints

- Source files in `adapters/core/*.sh` are **function bodies only** — no shebang, no `set -euo pipefail`; `writeShellApplication` prepends both. `dispatch-notify.sh` is the exception (it is a hook script and keeps its shebang).
- Every edited `.sh` must pass `shellcheck` (0.11) with no new warnings; each file already carries `# shellcheck shell=bash`.
- **No `flake.nix` changes.** `crew` already has `runtimeInputs = [git jq coreutils gnugrep tmux gh gtrash] ++ [pr-watch]`; `dispatch` already has `[gh git jq gnused coreutils tmux] ++ [crew]`. Nothing new is needed.
- Worker id format: `worker:<branch>#<sid>`, `sid = s<epoch>-<pid>`. **Parse on the LAST `#`** — `#` is legal in a git refname (verified: `git check-ref-format --branch 'feat/12-a#b'` passes). jq idiom: `ltrimstr("worker:") | sub("#[^#]*$";"")`.
- Terminal states are exactly `done|failed|exited`. `pr_open` is **not** terminal.
- Never reclaim a window whose `@crew_name` is `dispatcher`, nor the window owning `$TMUX_PANE`.
- `adapters/{claude-code,codex,cursor}` are **generated**. Never hand-edit them; run `scripts/gen-adapters.sh` (Task 9). CI gates on no-diff.
- Markdown is prettier-formatted by pre-commit. If a commit fails on `prettier ... files were modified`, `git add` the file and commit again — that is the hook fixing formatting, not an error.
- Run tests from the worktree root: `bats tests/crew.bats`, `bats tests/dispatch.bats`. Filter with `-f '<regex>'`.
- Conventional commits, one per task.

## File Structure

| File                                             | Responsibility                                                                                                                                  | Tasks       |
| ------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------- | ----------- |
| `adapters/core/crew.sh`                          | `_occupants`/`occupants`, `_sessions`/`sessions`, `reply` resolution, `roster` collapse, `report`/`rate`/`reap` id stripping, reap idle release | 1,2,3,6,7,8 |
| `adapters/core/dispatch.sh`                      | mints + stamps + exports the session id; the occupancy gate                                                                                     | 4,5         |
| `adapters/core/dispatch-notify.sh`               | SessionEnd `exited` keyed on `$CREW_WORKER_ID`                                                                                                  | 4           |
| `adapters/core/protocols/WORKER_PROTOCOL.md`     | worker uses `$CREW_WORKER_ID`, not `$(git branch --show-current)`                                                                               | 9           |
| `adapters/core/protocols/DISPATCHER_PROTOCOL.md` | refusal remedies, `sessions[]`, no cross-session inheritance                                                                                    | 9           |
| `tests/crew.bats`                                | `stub_tmux` helper + bus tests                                                                                                                  | 1,2,3,6,7,8 |
| `tests/dispatch.bats`                            | identity issuance + gate tests                                                                                                                  | 4,5         |

**Deviation from the spec, deliberate:** the spec put the tmux occupancy query inline in `dispatch.sh`. It lives in `crew occupants` instead, because `reap`'s idle release (Task 8) needs the identical query — inlining it would duplicate the logic across two files.

**Deviation from the spec, deliberate:** the spec had `rate` key each run's event fold on the session. Task 7 keeps the existing dispatch-to-next-dispatch **time window** and only adds the `session` field. With the Task 5 gate in place two sessions on one branch cannot overlap in time, so the window and the session coincide — session-keying would add a legacy `if` branch to a large jq program for no behavioural gain.

---

### Task 1: `crew occupants <worktree-path>`

Worker windows rooted at a worktree. Keyed on the `@crew_name` window option — which `dispatch` sets on every worker window and which **survives the engine process exiting** — not on `pane_current_command`. A command match would report a finished agent's window (now a shell prompt) as empty, which is exactly how the second worker gets stacked onto the tree.

**Files:**

- Modify: `adapters/core/crew.sh` (add `_occupants`, and an `occupants` case in the no-repo block next to `identity`)
- Test: `tests/crew.bats`

**Interfaces:**

- Produces: `crew occupants <path>` → JSON array on stdout, `[]` when unoccupied. Each element `{window, name, pane, engine}` — `window` is a tmux window id, `name` the `@crew_name`, `pane` the first engine pane id or `null`, `engine` a boolean. Also the shell function `_occupants <path>` for in-process callers (Task 8).

- [ ] **Step 1: Add the `stub_tmux` helper to the test file**

A scriptable `tmux` so occupancy is deterministic and never reads the developer's own tmux server. Add to `tests/crew.bats` just below `teardown()`:

```bash
# stub_tmux <list-windows-body> <list-panes-body> — a tmux whose list output is
# fixed text. crew's real tmux calls are `|| true`-tolerant, so without this the
# occupancy tests would read the developer's live server and flake.
stub_tmux() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  printf '%s' "$1" >"$STUB_DIR/wins.txt"
  printf '%s' "$2" >"$STUB_DIR/panes.txt"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}
```

- [ ] **Step 2: Write the failing tests**

Append to `tests/crew.bats`:

```bash
@test "occupants: reports a worker window with a live engine" {
  stub_tmux "$(printf '@23\tsage\t/wt/a\n@9\t\t/wt/a\n')" "$(printf '@23\t%%33\tclaude\n')"
  run run_crew occupants /wt/a
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].window')" = "@23" ]
  [ "$(echo "$output" | jq -r '.[0].name')" = "sage" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "%33" ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "true" ]
}

@test "occupants: a finished agent that dropped to a shell is still an occupant" {
  stub_tmux "$(printf '@23\tsage\t/wt/a\n')" "$(printf '@23\t%%33\tfish\n')"
  run run_crew occupants /wt/a
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "false" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "null" ]
}

@test "occupants: ignores other paths, unnamed windows and the dispatcher" {
  stub_tmux "$(printf '@1\tsage\t/wt/b\n@2\t\t/wt/a\n@3\tdispatcher\t/wt/a\n')" "$(printf '@1\t%%1\tclaude\n@3\t%%3\tclaude\n')"
  run run_crew occupants /wt/a
  [ "$output" = "[]" ]
}

@test "occupants: never reports the caller's own window" {
  stub_tmux "$(printf '@23\tsage\t/wt/a\n')" "$(printf '@23\t%%33\tclaude\n')"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
case "$1" in
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
display-message) printf '%s\n' '@23' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  TMUX_PANE=%33 run run_crew occupants /wt/a
  [ "$output" = "[]" ]
}

@test "occupants: needs a path" {
  run run_crew occupants
  [ "$status" -eq 1 ]
  [[ "$output" == *"occupants <worktree-path>"* ]]
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `bats tests/crew.bats -f 'occupants'`
Expected: 5 failures — `crew` falls through to the usage case, so status is 1 and output contains `usage: crew id | identity …`.

- [ ] **Step 4: Implement `_occupants`**

In `adapters/core/crew.sh`, add immediately after the `_identity()` function:

```bash
# _occupants <worktree_path> -> [{window,name,pane,engine}] — worker windows
# rooted at that path. Keyed on @crew_name (dispatch stamps it on every worker
# window), NOT on the pane's running command: a finished agent drops back to a
# shell prompt, and a command match would then read its window as empty and let
# the next dispatch stack a second worker onto the same tree (#17). Excludes the
# dispatcher's own window — it carries @crew_name too — and the caller's.
_occupants() {
  local wtp="$1" self_win="" wins panes out wid nm path pw pid cmd epane
  if [ -n "${TMUX_PANE:-}" ]; then
    self_win=$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}' 2>/dev/null || true)
  fi
  wins=$(tmux list-windows -a -F '#{window_id}	#{@crew_name}	#{pane_current_path}' 2>/dev/null || true)
  panes=$(tmux list-panes -a -F '#{window_id}	#{pane_id}	#{pane_current_command}' 2>/dev/null || true)
  out='[]'
  while IFS=$'\t' read -r wid nm path; do
    [ -n "$wid" ] || continue
    [ "$path" = "$wtp" ] || continue
    [ -n "$nm" ] || continue
    [ "$nm" != dispatcher ] || continue
    [ "$wid" != "$self_win" ] || continue
    epane=""
    while IFS=$'\t' read -r pw pid cmd; do
      [ "$pw" = "$wid" ] || continue
      case "$cmd" in
      claude | codex | cursor-agent)
        epane="$pid"
        break
        ;;
      esac
    done <<PANES
$panes
PANES
    out=$(printf '%s' "$out" | jq -c --arg w "$wid" --arg n "$nm" --arg p "$epane" \
      '. + [{window:$w, name:$n, pane:(if $p=="" then null else $p end), engine:($p!="")}]')
  done <<WINS
$wins
WINS
  printf '%s' "$out"
}
```

The two literal tabs inside the `-F` format strings are real tab characters, matching the `IFS=$'\t'` reads.

- [ ] **Step 5: Add the subcommand**

In `adapters/core/crew.sh`, after the `if [ "$sub" = identity ]; then … fi` block (it needs no repo and no crew id, same as `identity`):

```bash
if [ "$sub" = occupants ]; then
  [ -n "${1:-}" ] || {
    echo "crew: occupants <worktree-path>" >&2
    exit 1
  }
  _occupants "$1"
  printf '\n'
  exit 0
fi
```

- [ ] **Step 6: Add it to the usage string**

In the final `*)` case, insert `| occupants <worktree-path>` after `identity <branch>`.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `bats tests/crew.bats -f 'occupants'`
Expected: 5 passing.

- [ ] **Step 8: Shellcheck, then full suite**

Run: `shellcheck adapters/core/crew.sh && bats tests/crew.bats`
Expected: no shellcheck output; all tests pass.

- [ ] **Step 9: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): occupants — worker windows at a worktree, keyed on @crew_name"
```

---

### Task 2: `crew sessions <branch>`

The per-session fold of the event log. One place knows how sessions come out of the bus; three callers consume it.

**Files:**

- Modify: `adapters/core/crew.sh` (add `_sessions`, a `sessions` case)
- Test: `tests/crew.bats`

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces: `crew sessions <branch> [--crew ID]` → JSON array sorted oldest → newest, `[]` when the branch has none. Elements: `{session, worker_id, state, ts, age_s, terminal}`. `session` is `null` for legacy branch-keyed events; `state` is `null` for a session that has a `dispatch` event but no status yet. Also the shell function `_sessions <branch> <crew_or_empty>`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/crew.bats`:

```bash
@test "sessions: folds each session separately, oldest first" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  run run_crew sessions feat/x
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r 'length')" = "2" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s1-1" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "done" ]
  [ "$(echo "$output" | jq -r '.[0].terminal')" = "true" ]
  [ "$(echo "$output" | jq -r '.[1].session')" = "s2-2" ]
  [ "$(echo "$output" | jq -r '.[1].state')" = "working" ]
  [ "$(echo "$output" | jq -r '.[1].terminal')" = "false" ]
  [ "$(echo "$output" | jq -r '.[1].worker_id')" = "worker:feat/x#s2-2" ]
}

@test "sessions: pr_open is not terminal" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r '.[0].terminal')" = "false" ]
}

@test "sessions: a branch with a '#' in its name folds on the last '#'" {
  CREW_ID=c1 run_crew status "worker:feat/a#b#s1-1" working
  run run_crew sessions 'feat/a#b'
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s1-1" ]
}

@test "sessions: legacy branch-keyed events fold in as a null session" {
  CREW_ID=c1 run_crew status "worker:feat/x" done
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r '.[0].session')" = "null" ]
  [ "$(echo "$output" | jq -r '.[0].worker_id')" = "worker:feat/x" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "done" ]
}

@test "sessions: a dispatched session with no status yet has a null state" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:(now*1000|floor), crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s9-9"}' >>"$log"
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s9-9" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "null" ]
  [ "$(echo "$output" | jq -r '.[0].terminal')" = "false" ]
}

@test "sessions: --crew scopes the fold" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c2 run_crew status "worker:feat/x#s2-2" working
  run run_crew sessions feat/x --crew c2
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s2-2" ]
}

@test "sessions: an unknown branch is an empty array" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  run run_crew sessions feat/nope
  [ "$output" = "[]" ]
}

@test "sessions: needs a branch" {
  run run_crew sessions
  [ "$status" -eq 1 ]
  [[ "$output" == *"sessions <branch>"* ]]
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/crew.bats -f 'sessions:'`
Expected: 8 failures on the usage fallthrough.

- [ ] **Step 3: Implement `_sessions`**

In `adapters/core/crew.sh`, add after `_occupants`:

```bash
# _sessions <branch> <crew_or_empty> -> [{session,worker_id,state,ts,age_s,terminal}]
# oldest -> newest. Every fold is per SESSION: aggregating across a branch is how
# three workers came to read as one flip-flopping identity (#17). A session with a
# dispatch event but no status yet is still listed (state null) — dispatch needs to
# see a booting worker. No crew filter by default, same reason `reap` has none: the
# sessions worth inspecting are the ones from earlier dispatcher crews.
_sessions() {
  local branch="$1" crewf="$2"
  [ -f "$log" ] || {
    printf '[]'
    return 0
  }
  jq -s -c --arg b "$branch" --arg crew "$crewf" '
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
      map(select($crew=="" or .crew_id==$crew))
      | ( map(select(.kind=="dispatch" and .branch==$b))
          | map({session:(.session // null), ts:.ts}) ) as $disp
      | ( map(select(.kind=="status"
                     and ((.from // "") | startswith("worker:"))
                     and ((.from) | wid_branch) == $b))
          | map({session:((.from) | wid_session), state:.body.state, ts:.ts}) ) as $st
      | ( ($disp + $st) | map(.session) | unique ) as $ids
      | [ $ids[] as $s
          | ($st | map(select(.session == $s)) | sort_by(.ts) | last) as $latest
          | ($disp | map(select(.session == $s)) | sort_by(.ts) | last) as $d
          | { session: $s,
              worker_id: ("worker:" + $b + (if $s == null then "" else "#" + $s end)),
              state: ($latest.state // null),
              ts: ($latest.ts // $d.ts),
              terminal: ((["done","failed","exited"] | index($latest.state // "")) != null) } ]
      | sort_by(.ts)
      | map(. + {age_s: (((now*1000) - .ts) / 1000 | floor)})' "$log"
}
```

- [ ] **Step 4: Add the subcommand**

Insert a new case into the main `case "$sub" in` block, immediately before `roster)`:

```bash
sessions)
  branch="${1:-}"
  shift || true
  screw=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew)
      [ -n "${2:-}" ] || {
        echo "crew: --crew needs a value" >&2
        exit 1
      }
      screw="$2"
      shift 2
      ;;
    *)
      echo "crew: sessions <branch> [--crew ID]" >&2
      exit 1
      ;;
    esac
  done
  [ -n "$branch" ] || {
    echo "crew: sessions <branch> [--crew ID]" >&2
    exit 1
  }
  _sessions "$branch" "$screw"
  printf '\n'
  ;;
```

- [ ] **Step 5: Add it to the usage string**

In the final `*)` case, insert `| sessions <branch> [--crew ID]` before `| roster [crew]`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `bats tests/crew.bats -f 'sessions:'`
Expected: 8 passing.

- [ ] **Step 7: Shellcheck + full suite**

Run: `shellcheck adapters/core/crew.sh && bats tests/crew.bats`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): sessions — per-session fold of the bus log"
```

---

### Task 3: `crew reply` resolves a branch to its live session

Keeps the dispatcher writing `crew reply worker:<branch>` while the message lands on a session that exists **now**. A stopped session's directive can then never be inherited, because resolution happens at send time.

**Files:**

- Modify: `adapters/core/crew.sh` (the `reply)` case)
- Test: `tests/crew.bats`

**Interfaces:**

- Consumes: `_sessions <branch> <crew>` from Task 2.
- Produces: no new surface. `crew reply worker:<branch> <body>` now writes `.to = worker:<branch>#<sid>` of the newest session, exits 1 if that session is terminal or there is none. `crew reply worker:<branch>#<sid> <body>` and non-`worker:` targets are unchanged.

- [ ] **Step 1: Write the failing tests**

Append to `tests/crew.bats`:

```bash
@test "reply: a branch-only worker target resolves to the newest live session" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  CREW_ID=c1 run_crew reply "worker:feat/x" "go"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "worker:feat/x#s2-2" ]
}

@test "reply: refuses when the newest session is terminal" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"re-dispatch"* ]]
}

@test "reply: refuses a branch with no sessions" {
  CREW_ID=c1 run run_crew reply "worker:feat/nope" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"no session"* ]]
}

@test "reply: an explicit session id is honoured verbatim" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew reply "worker:feat/x#s1-1" "go"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "worker:feat/x#s1-1" ]
}

@test "reply: a non-worker target is untouched" {
  CREW_ID=c1 run_crew reply "metrics:c1" "{}"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "metrics:c1" ]
}

@test "reply: a directive for session 1 is not delivered to session 2" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew reply "worker:feat/x" "STOP - do not push"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  run run_crew inbox "worker:feat/x#s2-2" c1
  [ -z "$output" ]
  run run_crew inbox "worker:feat/x#s1-1" c1
  [[ "$output" == *"STOP - do not push"* ]]
}
```

The last test is the regression test for the observed correctness bug: session 3 draining session 2's `DO NOT PUSH`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/crew.bats -f 'reply:'`
Expected: the resolution tests fail (`.to` is the bare `worker:feat/x`); the refusal tests fail with status 0; the inheritance test fails because `inbox` for session 2 returns the directive — that failure **is** the bug.

- [ ] **Step 3: Implement the resolution**

In `adapters/core/crew.sh`, inside the `reply)` case, between the `mkdir -p "$dir"` line and the `line=$(jq -nc …)` line:

```bash
  # A branch-only worker target resolves to the newest session on that branch, so
  # the dispatcher keeps writing `worker:<branch>` while the message lands on a
  # session that exists NOW. Resolving at send time is what makes inheritance
  # impossible: a stopped session's successor has a different id, so a directive
  # written for the former is never addressed to the latter (#17).
  to="${1:-}"
  case "$to" in
  worker:*'#'*) ;; # explicit session — honoured verbatim
  worker:*)
    br="${to#worker:}"
    newest=$(_sessions "$br" "$crew" | jq -c 'last')
    [ -n "$newest" ] && [ "$newest" != null ] || {
      echo "crew: no session on $br — dispatch a worker before replying to one" >&2
      exit 1
    }
    if [ "$(printf '%s' "$newest" | jq -r .terminal)" = true ]; then
      echo "crew: newest session on $br is $(printf '%s' "$newest" | jq -r .state) — a stopped session never reads its inbox; re-dispatch with the context baked in" >&2
      exit 1
    fi
    to=$(printf '%s' "$newest" | jq -r .worker_id)
    ;;
  esac
```

Then change the `line=$(jq …)` invocation in that case to pass the resolved value — replace `--arg to "${1:-}"` with `--arg to "$to"`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `bats tests/crew.bats -f 'reply:'`
Expected: 6 passing.

- [ ] **Step 5: Shellcheck + full suite**

Run: `shellcheck adapters/core/crew.sh && bats tests/crew.bats`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "fix(crew): resolve a branch reply to its live session, never its successor"
```

---

### Task 4: `dispatch` issues the session id

Identity stops being self-derived. `dispatch` mints it, exports it into the engine's process environment, stamps it in the task doc for humans, and prints it so the dispatcher can address the session before the worker boots.

The env var — not `WORKER_TASK.md` — is the authority, because the task doc is **overwritten by the next dispatch on that worktree**. Anything re-reading the doc later would attribute one session's event to another, which is the misattribution this whole change removes.

**Files:**

- Modify: `adapters/core/dispatch.sh`
- Modify: `adapters/core/dispatch-notify.sh`
- Test: `tests/dispatch.bats`

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces: `$worker_id` (`worker:<branch>#s<epoch>-<pid>`) and `$session` in `dispatch.sh`; `worker_id:` in `WORKER_TASK.md`; `session` on the `dispatch` bus event; `CREW_WORKER_ID` in the engine environment; `worker_id: <id>` on dispatch's stdout; `crew stall-watch <worker_id>` (was `<branch>`). Task 5 consumes `$session` and `$worker_id`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch.bats`:

```bash
@test "session: stamps worker_id, exports CREW_WORKER_ID and prints the id" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"worker_id: worker:feat/42-do-a-thing#s7-7"* ]]
  wt_path="$TEST_REPO/.dispatch-wt/feat-42-do-a-thing"
  grep -qx 'worker_id: worker:feat/42-do-a-thing#s7-7' "$wt_path/WORKER_TASK.md"
  grep -q 'CREW_WORKER_ID=worker:feat/42-do-a-thing#s7-7 claude ' "$STUB_LOG"
}

@test "session: the dispatch event carries the session" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="dispatch") | .session' "$log"
  [ "$output" = "s7-7" ]
}

@test "session: stall-watch is handed the worker id, not the branch" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  grep -q 'stall-watch worker:feat/42-do-a-thing#s7-7 --pane' "$STUB_LOG"
}

@test "session: a minted id is epoch-pid shaped" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [[ "$output" =~ worker_id:\ worker:feat/42-do-a-thing#s[0-9]+-[0-9]+ ]]
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch.bats -f 'session:'`
Expected: 4 failures — no `worker_id:` in the output or the task doc, `.session` is `null`, `stall-watch` receives the bare branch.

- [ ] **Step 3: Mint the id**

In `adapters/core/dispatch.sh`, immediately after the `sanitized="${branch//\//-}"` line:

```bash
# Session-scoped worker identity (#17). The worker used to derive its own id from
# the branch, so N sessions on one branch shared one roster row AND one durable
# inbox — a directive written for one was drained by its successor. The id is
# issued here instead and carried in the environment. epoch+pid, because two
# same-second dispatches on one branch would otherwise collide back into a single
# identity, which is exactly the bug.
session="${DISPATCH_SESSION_ID:-s$(date +%s)-$$}"
worker_id="worker:$branch#$session"
```

- [ ] **Step 4: Stamp, log and print it**

Add `session` to the dispatch event — in the `jq -nc` call that writes `kind:"dispatch"`, add `--arg session "$session"` to the arguments and `session:$session,` to the object after `branch:$branch,`.

In the `WORKER_TASK.md` heredoc block, extend the `printf` format with `worker_id: %s\n` and pass `"$worker_id"`. Place it directly after `agent_name: %s\n` / `"$agent_name"` so the field order matches the protocol's reading order:

```bash
  printf 'tier: %s\nengine: %s\nmodel: %s\neffort: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\nworker_id: %s\n' \
    "$tier" "$agent" "$model" "$effort" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name" "$worker_id"
```

Print it on stdout, immediately after the `read -r win pane < <(tmux new-window …)` line:

```bash
# Printed so the dispatcher can address this session in the gap before the worker
# boots — its startup drain is unbounded, so a scoping note posted now still lands.
echo "worker_id: $worker_id"
```

- [ ] **Step 5: Export it into all three engines**

Prefix each of the three `tmux send-keys` command strings with `CREW_WORKER_ID=$worker_id `:

- codex: `"CREW_WORKER_ID=$worker_id codex --profile worker -m $model …"`
- cursor: `"CREW_WORKER_ID=$worker_id CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force …"`
- claude: `"CREW_WORKER_ID=$worker_id claude --name $agent_name …"`

- [ ] **Step 6: Hand the worker id to the watchdog**

Change the last line of the file from `crew stall-watch "$branch"` to:

```bash
CREW_ID="$crew_id" nohup crew stall-watch "$worker_id" --pane "$pane" >/dev/null 2>&1 &
```

- [ ] **Step 7: Make `stall-watch` take a worker id**

In `adapters/core/crew.sh`, the `stall-watch)` case used the branch only to build `me`. Rename the positional and drop the reconstruction — replace:

```bash
  branch="${1:-}"
  shift || true
  [ -n "$branch" ] || {
    echo "crew: stall-watch <branch> --pane <id> [--grace S] [--stall S] [--window S] [--interval S]" >&2
    exit 1
  }
```

with:

```bash
  me="${1:-}"
  shift || true
  [ -n "$me" ] || {
    echo "crew: stall-watch <worker-id> --pane <id> [--grace S] [--stall S] [--window S] [--interval S]" >&2
    exit 1
  }
```

and delete the later `me="worker:$branch"` line. Update the `*)` usage string: `stall-watch <worker-id> --pane <id> …`.

- [ ] **Step 8: Key the SessionEnd backstop on the environment**

In `adapters/core/dispatch-notify.sh`, replace `me="worker:$branch"` with:

```bash
    # The worker id comes from the environment, never from WORKER_TASK.md: the doc
    # is overwritten by the next dispatch on this worktree, so reading it here
    # would post THIS session's `exited` against its successor (#17). A session
    # launched before this change has no CREW_WORKER_ID and keeps the old id.
    me="${CREW_WORKER_ID:-worker:$branch}"
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `bats tests/dispatch.bats -f 'session:'`
Expected: 4 passing.

- [ ] **Step 10: Shellcheck + full suites**

Run: `shellcheck adapters/core/dispatch.sh adapters/core/dispatch-notify.sh adapters/core/crew.sh && bats tests/`
Expected: clean. If an existing `stall-watch` test asserted a bare branch argument, update it to the worker id — that assertion is now wrong by design.

- [ ] **Step 11: Commit**

```bash
git add adapters/core/dispatch.sh adapters/core/dispatch-notify.sh adapters/core/crew.sh tests/dispatch.bats
git commit -m "feat(dispatch): issue a per-session worker id and carry it in the environment"
```

---

### Task 5: The occupancy gate

**Files:**

- Modify: `adapters/core/dispatch.sh`
- Test: `tests/dispatch.bats`

**Interfaces:**

- Consumes: `crew occupants <path>` (Task 1), `crew sessions <branch>` (Task 2), `$crew_dir` and `$branch` from `dispatch.sh`.
- Produces: exit 1 on refusal; a `reclaim` bus event `{ts, crew_id, kind:"reclaim", branch, state, windows}` on takeover.

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch.bats`. These need a `crew` stub that answers `occupants` and `sessions`, so each test writes its own:

```bash
# stub_crew_gate <occupants-json> <sessions-json> — a crew stub that feeds the
# gate fixed answers while still logging every call.
stub_crew_gate() {
  printf '%s' "$1" >"$STUB_DIR/occ.json"
  printf '%s' "$2" >"$STUB_DIR/sess.json"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
identity) printf '%s\n' '{"name":"sage","color":"green","tmux":"colour28"}' ;;
occupants) cat "$STUB_DIR/occ.json" ;;
sessions) cat "$STUB_DIR/sess.json" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

# An existing worktree for the branch the --pr path resolves to, so the gate has
# something to find.
setup_occupied_branch() {
  stub_launch_bins
  git -C "$TEST_REPO" branch eng-7691-foo
  mkdir -p "$TEST_REPO/.worktrees"
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.worktrees/eng-7691-foo" eng-7691-foo
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
pr\ view\ *) printf '%s\n' '{"headRefName":"eng-7691-foo","isCrossRepository":false}' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
}

@test "gate: refuses when a live engine occupies the target worktree" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":"working","ts":1,"age_s":412,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 1 ]
  [[ "$output" == *"worker:eng-7691-foo#s1-1"* ]]
  [[ "$output" == *"working"* ]]
  [[ "$output" == *"@23"* ]]
  [[ "$output" == *"crew reply worker:eng-7691-foo"* ]]
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q 'send-keys' "$STUB_LOG"
  ! grep -q 'kill-window' "$STUB_LOG"
  ! grep -q '^switch' "$STUB_LOG"
}

@test "gate: refuses a booting session that has posted no status yet" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":null,"ts":1,"age_s":3,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 1 ]
  ! grep -q 'new-window' "$STUB_LOG"
}

@test "gate: reclaims a finished session and proceeds" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":"done","ts":1,"age_s":900,"terminal":true}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  [[ "$output" == *"reclaimed @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="reclaim") | "\(.branch) \(.state) \(.windows[0])"' "$log"
  [ "$output" = "eng-7691-foo done @23" ]
}

@test "gate: reclaims a worker window whose engine already exited" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":null,"engine":false}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":"working","ts":1,"age_s":900,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

@test "gate: an unoccupied existing worktree dispatches normally" {
  setup_occupied_branch
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  ! grep -q 'kill-window' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

@test "gate: a fresh branch never consults occupants" {
  stub_launch_bins
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  ! grep -q '^occupants' "$STUB_LOG"
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch.bats -f 'gate:'`
Expected: the two refusal tests fail with status 0 and a `new-window` in the log; the reclaim tests fail with no `kill-window`; the last two may pass incidentally.

- [ ] **Step 3: Move `crew_dir` above the gate**

The reclaim event is appended before `wt switch` runs, so the bus dir must already be resolved. Cut these two lines from below the `wt_path=` block:

```bash
crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
mkdir -p "$crew_dir"
```

and paste them directly **above** the `wt_post_switch='post-switch.tmux=""'` line. The value is identical either side of the switch — it resolves the common git dir, which no worktree operation moves.

- [ ] **Step 4: Insert the gate**

In `adapters/core/dispatch.sh`, replace the **entire** `if [ -n "$pr_number" ]; then … else … fi` block — from `# Review attach (--pr N): resolve headRefName once…` down to and including the `wt switch -c "$branch" …` line and its closing `fi` — with the following. The block is split so it only _computes_ `branch`, `closes` and `switch_mode`; the gate then runs; only then does a single `case` perform the switch. That ordering is the whole point: a refusal must cost no worktree and no window.

```bash
# --pr resolves the head ref; the switch itself happens after the gate below, so
# a refusal costs no worktree and no window.
if [ -n "$pr_number" ]; then
  pr_json=$(gh pr view "$pr_number" --json headRefName,isCrossRepository)
  head=$(printf '%s' "$pr_json" | jq -r .headRefName)
  cross=$(printf '%s' "$pr_json" | jq -r .isCrossRepository)
  [ -n "$head" ] && [ "$head" != null ] || {
    echo "dispatch: could not resolve headRefName for PR $pr_number" >&2
    exit 1
  }
  branch="$head"
  closes="pr: $pr_number"
  if git show-ref --verify --quiet "refs/heads/$head" ||
    git show-ref --verify --quiet "refs/remotes/origin/$head"; then
    switch_mode=name
  elif [ "$cross" = false ]; then
    switch_mode=fetch-name
  else
    switch_mode=pr-ref
  fi
else
  # Identity + closes line. Linear mode derives both from the ticket (no gh); a
  # passed GitHub issue number reuses that issue (no gh call). Otherwise GitHub
  # mode mints an issue and aborts cleanly if that fails (issues disabled) rather
  # than scaffolding a half-broken worker off an empty number.
  if [ -n "$linear_id" ]; then
    branch="$(printf '%s' "$linear_id" | tr '[:upper:]' '[:lower:]')-$slug"
    closes="Closes $linear_id"
  elif [ -n "$gh_issue" ]; then
    branch="feat/$gh_issue-$slug"
    closes="Closes #$gh_issue"
  else
    url=$(gh issue create --assignee @me --title "$title" --body "Dispatched worker task." 2>/dev/null || true)
    num=$(printf '%s' "$url" | sed -nE 's#.*/([0-9]+)$#\1#p')
    [ -n "$num" ] || {
      echo "dispatch: could not create a GitHub issue (issues disabled?). Pass a Linear id, e.g. dispatch $tier $model ENG-1234 $title" >&2
      exit 1
    }
    branch="feat/$num-$slug"
    closes="Closes #$num"
  fi
  switch_mode=create
fi

# Reuse-or-refuse (#17). git allows exactly one worktree per branch, so a dispatch
# onto a branch that already has one lands in the SAME directory. Ungated, it
# opened a second window and a second agent there — two committers on one index,
# which only stayed safe by luck. Occupancy is a WORKER WINDOW (crew occupants,
# keyed on @crew_name), not a running engine: a finished agent drops to a shell
# prompt, and a command-based check would call its window empty.
prev_wt="$(git worktree list --porcelain | awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')"
if [ -n "$prev_wt" ]; then
  occ=$(crew occupants "$prev_wt")
  if [ "$occ" != "[]" ]; then
    newest=$(crew sessions "$branch" | jq -c 'last')
    state=$(printf '%s' "$newest" | jq -r '.state // "none"')
    terminal=$(printf '%s' "$newest" | jq -r '.terminal // false')
    engine=$(printf '%s' "$occ" | jq -r 'map(select(.engine)) | length')
    if [ "$engine" -gt 0 ] && [ "$terminal" != true ]; then
      nm=$(printf '%s' "$occ" | jq -r '.[0].name')
      win=$(printf '%s' "$occ" | jq -r '.[0].window')
      wid=$(printf '%s' "$newest" | jq -r '.worker_id // ""')
      {
        echo "dispatch: $nm ($wid) is $state in that worktree (window $win) — git allows one worktree per branch."
        echo "  redirect it:  crew reply worker:$branch \"<directive>\""
        echo "  or take over: tmux kill-window -t $win, then re-dispatch"
      } >&2
      exit 1
    fi
    # Finished work squatting the tree (terminal, or the engine is already gone).
    # Reclaim rather than stack beside it. Best-effort, like reap's kills.
    for w in $(printf '%s' "$occ" | jq -r '.[].window'); do
      tmux kill-window -t "$w" 2>/dev/null || true
      echo "dispatch: reclaimed $w at $prev_wt (session $state)"
    done
    jq -nc --arg crew "$crew_id" --arg branch "$branch" --arg state "$state" --argjson occ "$occ" \
      '{ts:(now*1000|floor), crew_id:$crew, kind:"reclaim", branch:$branch, state:$state,
          windows:($occ|map(.window))}' >>"$crew_dir/events.jsonl"
  fi
fi

case "$switch_mode" in
create) wt switch -c "$branch" -y --config-set "$wt_post_switch" ;;
name) wt switch "$branch" -y --config-set "$wt_post_switch" ;;
fetch-name)
  git fetch origin "$branch"
  wt switch "$branch" -y --config-set "$wt_post_switch"
  ;;
pr-ref) wt switch "pr:$pr_number" -y --config-set "$wt_post_switch" ;;
esac
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bats tests/dispatch.bats -f 'gate:'`
Expected: 6 passing.

- [ ] **Step 6: Shellcheck + full suites**

Run: `shellcheck adapters/core/dispatch.sh && bats tests/`
Expected: clean. The pre-existing `--pr` tests assert `switch eng-7691-foo` and the absence of `-c`; the `switch_mode` refactor must keep both true.

- [ ] **Step 7: Commit**

```bash
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "fix(dispatch): reuse-or-refuse — never stack a second agent in one worktree"
```

---

### Task 6: `roster` collapses per branch and enumerates sessions

**Files:**

- Modify: `adapters/core/crew.sh` (the `roster)` case)
- Test: `tests/crew.bats`

**Interfaces:**

- Consumes: the id format from Task 2.
- Produces: roster rows gain `branch`, `session` and `sessions: [{session,state,age_s}]`. One row per branch. `name`/`color`/`tmux` stay derived from the **branch**, so `crew identity <branch>` keeps its contract and the tmux tint still matches the window.

- [ ] **Step 1: Write the failing tests**

Append to `tests/crew.bats`:

```bash
@test "roster: collapses sessions of one branch into a single row" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].branch')" = "feat/x" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "working" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s2-2" ]
  [ "$(echo "$output" | jq -r '.[0].sessions | length')" = "2" ]
  [ "$(echo "$output" | jq -r '.[0].sessions[0].state')" = "done" ]
  [ "$(echo "$output" | jq -r '.[0].sessions[1].state')" = "working" ]
}

@test "roster: the codename still derives from the branch" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  expected="$(run_crew identity feat/x | jq -r .name)"
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].name')" = "$expected" ]
}

@test "roster: one session's exited is not resolved from another's history" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" exited
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].state')" = "exited" ]
  [ "$(echo "$output" | jq -r '.[0].prev_state // "none"')" = "none" ]
}

@test "roster: joins the title on the branch" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1", title:"Do a thing"}' >>"$log"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].title')" = "Do a thing" ]
}

@test "roster: a legacy branch-keyed row still renders" {
  CREW_ID=c1 run_crew status "worker:feat/x" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].branch')" = "feat/x" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "null" ]
}
```

The third test guards F4: with the old branch-wide fold, session 1's `working` would surface as session 2's `prev_state` and mislabel a real `exited`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/crew.bats -f 'roster:'`
Expected: the collapse test fails with `length == 2` (one row per session id), and `.branch`/`.session`/`.sessions` are all null/absent.

- [ ] **Step 3: Rewrite the `base=` fold**

In the `roster)` case, replace the whole `base=$(jq -c -s --arg crew "$crew" ' … ')` assignment with:

```bash
  # Fold per SESSION first, then collapse per BRANCH for display. Both halves are
  # load-bearing: aggregating across a branch is what made three workers read as
  # one flip-flopping identity, and it would also let an older session's `working`
  # resurrect a newer session's `exited` through prev_state (#17).
  base=$(jq -c -s --arg crew "$crew" '
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
      # title lives on the dispatch event (keyed by branch); join it per branch.
      # last wins on re-dispatch. missing (pre-title dispatch events) -> null.
      (map(select(.crew_id==$crew and .kind=="dispatch"))
        | map({key:.branch, value:(.title // null)}) | from_entries) as $titles
      | map(select(.crew_id==$crew and .kind=="status"
                   and ((.from // "") | startswith("worker:"))))
      | group_by(.from)
      | map(
          (max_by(.ts)) as $latest
          | {from: $latest.from,
             branch: ($latest.from | wid_branch),
             session: ($latest.from | wid_session),
             state: $latest.body.state,
             ts: $latest.ts,
             # carry forward last-known pr_url — the terminal `done` event drops it
             pr_url: (map(.body.pr_url) | map(select(. != null)) | last),
             # last state that was NOT the exited backstop, so a spurious exited
             # can be resolved back to what the worker itself last reported.
             prev_state: (map(select(.body.state != "exited")) | max_by(.ts) | .body.state),
             age_s: ((now - ($latest.ts/1000))|floor)})
      | group_by(.branch)
      | map((sort_by(.ts) | last)
            + {title: (.[0].branch as $b | $titles[$b] // null),
               sessions: (sort_by(.ts) | map({session, state, age_s}))})' "$log")
```

- [ ] **Step 4: Read the branch from the row**

Two sites in the same case still derive the branch by stripping the prefix. Replace:

```bash
    branch=$(printf '%s' "$row" | jq -r '.from | sub("^worker:";"")')
```

with:

```bash
    branch=$(printf '%s' "$row" | jq -r '.branch')
```

and in the `idmap` loop, replace:

```bash
  for from in $(printf '%s' "$resolved" | jq -r '.[].from | select(startswith("worker:"))'); do
    id=$(_identity "${from#worker:}")
    idmap=$(printf '%s' "$idmap" | jq -c --arg k "$from" --argjson v "$id" '. + {($k): $v}')
  done
```

with:

```bash
  for br in $(printf '%s' "$resolved" | jq -r '.[].branch'); do
    id=$(_identity "$br")
    idmap=$(printf '%s' "$idmap" | jq -c --arg k "$br" --argjson v "$id" '. + {($k): $v}')
  done
```

- [ ] **Step 5: Key the final decoration on the branch**

In the closing `printf '%s' "$resolved" | jq --argjson m "$idmap" '…'`, change `$m[.from]` to `$m[.branch]`, and change the collision suffix to read the branch field:

```bash
      | map(if (.name as $n | $dupes | index($n))
            then .name = (.name + "·" + ((.branch | capture("(?:[a-z]+/)?(?<id>[A-Za-z]+-[0-9]+|[0-9]+)") | .id) // .branch))
            else . end)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `bats tests/crew.bats -f 'roster:'`
Expected: 5 passing.

- [ ] **Step 7: Shellcheck + full suite**

Run: `shellcheck adapters/core/crew.sh && bats tests/crew.bats`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): roster collapses per branch and enumerates its sessions"
```

---

### Task 7: `report`, `rate` and `reap` accept sessioned ids

Three readers strip `worker:` and compare against a branch. They must strip the session too, or every sessioned worker silently drops out of the report, the ratings sweep and the reap sweep.

**Files:**

- Modify: `adapters/core/crew.sh` (`report)`, `rate)`, `reap)`)
- Test: `tests/crew.bats`

**Interfaces:**

- Consumes: the id format from Task 2.
- Produces: `rate` records gain a `session` field. `run_id` stays `repo:branch:ts0`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/crew.bats`:

```bash
@test "report: rows resolve for a sessioned worker id" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1",
           engine:"claude", model:"sonnet", tier:"standard", effort:"medium", title:"T"}' >>"$log"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  run run_crew report c1
  [[ "$output" == *"done"* ]]
}

@test "rate: sweeps a sessioned worker and records its session" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1",
           engine:"claude", model:"sonnet", tier:"standard", effort:"medium", title:"T"}' >>"$log"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/share"
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '"\(.branch) \(.session) \(.reached_pr)"' "$XDG_DATA_HOME/crew/ratings.jsonl"
  [ "$output" = "feat/x s1-1 true" ]
}

@test "reap: a sessioned worker becomes a candidate" {
  git commit --allow-empty -q -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/reap-me
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [[ "$output" == *"would reap feat/reap-me"* ]]
}
```

The `reap` test needs `$STUB_DIR` to exist first — call `stub_bin gh` before overwriting it, mirroring the existing `reap: --dry-run` test.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/crew.bats -f 'report: rows resolve|rate: sweeps|reap: a sessioned'`
Expected: `report` prints `—` for outcome, `rate` writes a record with `reached_pr:false` and no `session`, `reap` finds no candidate.

- [ ] **Step 3: Fix `report`**

In the `report)` case's jq program, add the def and use it — replace:

```bash
    | ($all | map(select(.kind == "status" and ((.from // "") | ltrimstr("worker:")) == $b))) as $st
```

with:

```bash
    | ($all | map(select(.kind == "status"
        and ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b))) as $st
```

- [ ] **Step 4: Fix `rate`**

In the `rate)` case's jq program, replace:

```bash
              and (((.from // "") | ltrimstr("worker:")) == $b)
```

with:

```bash
              and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
```

and add the session to the emitted record, directly after the `branch: $b,` line:

```bash
            session: ($d.session // null),
```

- [ ] **Step 5: Fix `reap`'s candidate fold**

In the `reap)` case, replace the `candidates=$(jq …)` program with a per-session fold collapsed per branch:

```bash
  candidates=$(jq -s -r '
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      map(select(.kind=="status" and ((.from // "") | startswith("worker:"))))
      | group_by(.from) | map(
          (max_by(.ts)) as $latest
          | {branch: ($latest.from | wid_branch),
             ts: $latest.ts,
             state: $latest.body.state,
             pr_url: (map(.body.pr_url) | map(select(. != null)) | last)})
      | group_by(.branch) | map(sort_by(.ts) | last)
      | map(select(.state == "done"))
      | .[] | [.branch, (.pr_url // "-")] | @tsv' "$log")
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `bats tests/crew.bats -f 'report:|rate:|reap:'`
Expected: all passing, including the pre-existing `reap`/`rate` tests.

- [ ] **Step 7: Shellcheck + full suite**

Run: `shellcheck adapters/core/crew.sh && bats tests/crew.bats`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "fix(crew): report, rate and reap read sessioned worker ids"
```

---

### Task 8: `crew reap --idle` releases finished-but-resident windows

A finished session that never left its window keeps the whole worktree occupied, and `reap` proper will not touch it until the PR lands. Release the **window only** — the tree stays PR-gated, because a worker sits in `done` for as long as its PR takes to merge.

**Files:**

- Modify: `adapters/core/crew.sh` (the `reap)` case)
- Test: `tests/crew.bats`

**Interfaces:**

- Consumes: `_occupants` (Task 1), the id format (Task 2).
- Produces: `crew reap [--quiet] [--dry-run] [--idle S]`, default `--idle 3600`. Appends `{ts, kind:"release", branch, session, state, windows}`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/crew.bats`:

```bash
@test "reap: releases a terminal session's window past --idle, keeping the worktree" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-me#s1-1" failed "gate never passed"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  [ -d "$wt_path" ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="release") | "\(.branch) \(.session) \(.state)"' "$log"
  [ "$output" = "feat/idle-me s1-1 failed" ]
}

@test "reap: leaves a terminal session inside --idle alone" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-me#s1-1" failed
  CREW_ID=c1 run run_crew reap --idle 3600
  [ "$status" -eq 0 ]
  ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: never releases a non-terminal session" {
  git commit --allow-empty -q -m init
  git branch feat/busy
  wt_path="$BATS_TEST_TMPDIR/busy-wt"
  git worktree add -q "$wt_path" feat/busy
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tclaude\n')"
  CREW_ID=c1 run_crew status "worker:feat/busy#s1-1" working
  CREW_ID=c1 run run_crew reap --idle 0
  ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: --dry-run only reports the release" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-me#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0 --dry-run
  [[ "$output" == *"would release @23"* ]]
  ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: rejects a non-numeric --idle" {
  CREW_ID=c1 run run_crew reap --idle nope
  [ "$status" -eq 1 ]
  [[ "$output" == *"--idle"* ]]
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/crew.bats -f 'reap: releases|reap: leaves|reap: never releases|reap: --dry-run only|reap: rejects a non-numeric'`
Expected: `--idle` is rejected as an unknown flag (status 1, "reap takes --quiet and --dry-run").

- [ ] **Step 3: Accept the flag**

In the `reap)` case's argument loop, add before the `*)` arm:

```bash
    --idle)
      [ -n "${2:-}" ] || {
        echo "crew: --idle needs a value in seconds" >&2
        exit 1
      }
      idle="$2"
      shift
      ;;
```

Initialise `idle=3600` next to `quiet=""` / `dry=""`, update the `*)` message to `reap takes --quiet, --dry-run and --idle S`, and validate after the loop:

```bash
  case "$idle" in '' | *[!0-9]*)
    echo "crew: --idle must be a non-negative integer number of seconds" >&2
    exit 1
    ;;
  esac
```

Note the `--idle` arm uses a single `shift` because the loop already shifts once per iteration.

- [ ] **Step 4: Implement the release pass**

Insert **before** the `for tool in gh wt; do … done` availability check (after the `say`/`note` definitions). Releasing a window needs neither `gh` nor `wt`, so it must not be skipped on a box where those are missing — only the worktree sweep below depends on them:

```bash
  # Idle release: a session that reached a terminal state but whose window is
  # still sitting there keeps the tree occupied, and the PR gate below deliberately
  # will not touch it while its PR is open. Kill the WINDOW only — the worktree
  # stays, because a worker legitimately sits in `done` for as long as its PR
  # takes to merge (#17).
  while IFS=$'\t' read -r rbranch rsession rstate; do
    [ -n "$rbranch" ] || continue
    rwt=$(git worktree list --porcelain |
      awk -v b="refs/heads/$rbranch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    [ -n "$rwt" ] && [ -d "$rwt" ] || continue
    rocc=$(_occupants "$rwt")
    [ "$rocc" != '[]' ] || continue
    for w in $(printf '%s' "$rocc" | jq -r '.[].window'); do
      if [ -n "$dry" ]; then
        say "would release $w at $rwt ($rbranch $rstate)"
        continue
      fi
      tmux kill-window -t "$w" 2>/dev/null || true
      say "released $w at $rwt ($rbranch $rstate)"
    done
    [ -n "$dry" ] || jq -nc --arg branch "$rbranch" --arg session "$rsession" \
      --arg state "$rstate" --argjson occ "$rocc" \
      '{ts:(now*1000|floor), kind:"release", branch:$branch, session:$session,
          state:$state, windows:($occ|map(.window))}' >>"$log"
  done <<EOF
$(jq -s -r --argjson idle "$idle" '
    def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
    def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
    map(select(.kind=="status" and ((.from // "") | startswith("worker:"))))
    | group_by(.from) | map(max_by(.ts))
    | group_by(.from | wid_branch) | map(max_by(.ts))
    | map(select((["done","failed","exited"] | index(.body.state)) != null))
    | map(select((((now*1000) - .ts) / 1000) >= $idle))
    | .[] | [(.from | wid_branch), ((.from | wid_session) // "-"), .body.state] | @tsv' "$log")
EOF
```

`say` (not `note`) is deliberate: `dispatch` calls `crew reap --quiet`, and a window this pass killed must never be invisible.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bats tests/crew.bats -f 'reap:'`
Expected: all `reap` tests passing, old and new.

- [ ] **Step 6: Shellcheck + full suite**

Run: `shellcheck adapters/core/crew.sh && bats tests/`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): reap --idle releases a finished worker's window, keeping the tree"
```

---

### Task 9: Protocols and generated adapters

**Files:**

- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md`
- Modify: `adapters/core/protocols/DISPATCHER_PROTOCOL.md`
- Regenerate: `adapters/claude-code/**`, `adapters/codex/**`, `adapters/cursor/**`
- Test: `tests/adapters.bats` (existing no-diff gate)

**Interfaces:**

- Consumes: `$CREW_WORKER_ID` (Task 4), refusal semantics (Task 5), `sessions[]` (Task 6).
- Produces: no code surface.

- [ ] **Step 1: Point the worker at its issued id**

In `WORKER_PROTOCOL.md`, replace every `worker:$(git branch --show-current)` and `worker:<branch>` occurrence (~15 sites, including the "Bus contract" list and the metrics `crew msg` line) with `$CREW_WORKER_ID`. Then rewrite the **First action** identity sentences:

```markdown
Read `WORKER_TASK.md`. It stamps `tier:`, authoritative `engine:`, `model:`, and `effort:`, `dispatcher_pane:`, `crew_dir:`, `crew_id:`, `agent_name:` (your FleetView-style codename — use it in human-facing pings), and `worker_id:` (your bus identity). Read that engine/model/effort tuple verbatim for any recovery decision; never infer it from prose, aliases, or process inspection. `crew` is a CLI on your PATH (not a shell function) and auto-reads `crew_id` from this file, so you can call it straight from your bash tool — no env setup. Announce yourself:
`crew status "$CREW_WORKER_ID" working`
Use `$CREW_WORKER_ID` as your agent id for every bus call below — it is exported into your environment by `dispatch` and identifies **this session**, not just this branch. Never rebuild it from the branch name: several sessions can have run on this branch, and a branch-keyed id let one session drain a directive that was written for another.
```

And the drain block:

```
seen=$(jq -n 'now*1000|floor')
crew inbox "$CREW_WORKER_ID"
```

- [ ] **Step 2: Document the refusal and sessions in the dispatcher protocol**

In `DISPATCHER_PROTOCOL.md`, extend the `crew roster` bullet with `sessions[]`, and replace the durability sentences in the `blocked` bullet (currently "A reply you post after it stopped isn't lost (durable in the log); the worker picks it up on its next activation.") with:

```markdown
- A worker that's `blocked` has posted its question and is **awaiting your reply in-band** (a bounded ~300s wait). Answer promptly with `crew reply worker:<branch> "<answer>"` — it resumes in place, no tmux, no re-dispatch. `crew reply` resolves `worker:<branch>` to the **session** running there now, and refuses once that session is terminal. **Messages do not outlive their session:** a directive you post for a stopped worker is never inherited by the next worker on that branch (#17) — to reach the next one, re-dispatch with the context baked in. A directive posted **immediately after `dispatch`**, before the worker is up, still lands: `dispatch` prints `worker_id:` and every worker drains its inbox unbounded before starting its pipeline (`WORKER_PROTOCOL.md` → First action).
- **`dispatch` refuses to stack a second worker on an occupied worktree.** git allows one worktree per branch, so a dispatch onto a branch already being worked lands in the same directory. If a live worker is there, `dispatch` exits non-zero and names both remedies: `crew reply` to redirect it, or `tmux kill-window` to take over. A worker that has already finished is reclaimed automatically. **Do not retry a refused dispatch unchanged** — redirect the live worker, or wait for it.
```

Add `sessions[]` to the roster bullet:

```markdown
- `crew roster` — at-a-glance dashboard: one row per **branch** with its newest session's state + age, its `title` (the task, joined from the dispatch event), a `sessions[]` list enumerating every session that has run on that branch, plus a `name`/`color` codename derived from its branch (FleetView-style — `dispatch` colors the matching tmux window the same). **Refer to workers by codename** (e.g. "sage is blocked, atlas opened a PR") so it tracks the colored windows.
```

- [ ] **Step 3: Regenerate the adapters**

Run: `./scripts/gen-adapters.sh`
Expected: no output; `git status --short` shows modified files under `adapters/claude-code/`, `adapters/codex/` and `adapters/cursor/`.

- [ ] **Step 4: Verify the drift gate and the whole suite**

Run: `bats tests/`
Expected: all pass, including the adapters idempotence and no-diff tests.

- [ ] **Step 5: Confirm no stale branch-derived ids remain**

Run: `rg -n 'git branch --show-current' adapters/`
Expected: no matches in any `WORKER_PROTOCOL.md`.

- [ ] **Step 6: Commit**

```bash
git add adapters/
git commit -m "docs(protocols): session-scoped worker identity and the dispatch refusal contract"
```

---

## Final verification

- [ ] `shellcheck adapters/core/crew.sh adapters/core/dispatch.sh adapters/core/dispatch-notify.sh` — clean
- [ ] `bats tests/` — all pass
- [ ] `nix build .#dispatcher-all` — the package still builds
- [ ] `git log --oneline extract..HEAD` — nine implementation commits plus the spec commit
- [ ] Re-read `docs/superpowers/specs/2026-08-04-dispatch-session-identity-design.md` and confirm each Part maps to a landed task
