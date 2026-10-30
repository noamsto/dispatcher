bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  run_crew() { "$CREW" "$@"; }
  setup_repo
  # reap only trusts a bus pr_url of this repo's origin.
  case "$BATS_TEST_DESCRIPTION" in reap:*) git config remote.origin.url https://github.com/o/r.git ;; esac
  # A dispatch records it before any worker exists; reap never does (#557).
  seed_git_baseline
  unset CREW_ID
  # Absent unless a test sets it, so pane-recording stays deterministic
  # regardless of whether bats itself runs inside a tmux pane.
  unset TMUX_PANE
  # No engine process by default, so quiet:->dead: escalation stays
  # deterministic regardless of what runs on the host tmux server.
  export CREW_STALL_PROC_CMD='printf ""'
  # await, the hold paths and stall-watch run on a virtual clock: their waits
  # advance this file instead of sleeping.
  export CREW_CLOCK="$BATS_TEST_TMPDIR/clock"
  # The real refresh-budget probes live accounts, which no test may do.
  export CREW_BUDGET_REFRESH_CMD=:
}

teardown() {
  # Before teardown_repo: a leaked fleet lane would race its rm -rf.
  stop_fleet_stream
  if [ -n "${HOLDER_PID:-}" ]; then
    kill -KILL "$HOLDER_PID" 2>/dev/null || true
  fi
  # A test that leaves a read-only dir names it here, so bats can delete it.
  if [ -n "${RESTORE_WRITE:-}" ]; then
    chmod -R u+w "$RESTORE_WRITE" 2>/dev/null || true
  fi
  teardown_repo
}

# Real tmux pane_current_path is kernel-canonical (pwd -P); git worktree paths
# resolve symlinks too (/var → /private/var on macOS) but $BATS_TEST_TMPDIR does
# not — canonicalize existing dirs in fixture text so absence-of-release tests
# can fail when occupancy silently misses (#52).
_canon_stub_path() {
  if [ -d "$1" ]; then
    (cd "$1" && pwd -P)
  else
    printf '%s' "$1"
  fi
}

_canon_stub_wins_body() {
  local body="$1"
  [ -n "$body" ] || return 0
  local line f1 f2 f3 rest
  while IFS= read -r line || [ -n "$line" ]; do
    if [ -z "$line" ]; then
      printf '\n'
      continue
    fi
    IFS=$'\t' read -r f1 f2 f3 rest <<< "$line"
    if [ -n "$f3" ]; then
      f3="$(_canon_stub_path "$f3")"
    fi
    printf '%s\t%s\t%s' "$f1" "$f2" "$f3"
    [ -n "$rest" ] && printf '\t%s' "$rest"
    printf '\n'
  done <<< "$body"
}

_canon_stub_panes_body() {
  local body="$1"
  [ -n "$body" ] || return 0
  local line cmd path
  while IFS= read -r line || [ -n "$line" ]; do
    if [ -z "$line" ]; then
      printf '\n'
      continue
    fi
    if [[ "$line" == *$'\t'* ]]; then
      printf '%s\n' "$line"
      continue
    fi
    cmd="${line%% *}"
    path="${line#* }"
    if [ "$path" != "$line" ] && [ -n "$path" ]; then
      path="$(_canon_stub_path "$path")"
      printf '%s %s\n' "$cmd" "$path"
    else
      printf '%s\n' "$line"
    fi
  done <<< "$body"
}

# stub_tmux <list-windows-body> <list-panes-body> — a tmux whose list output is
# fixed text. crew's real tmux calls are `|| true`-tolerant, so without this the
# occupancy tests would read the developer's live server and flake.
stub_tmux() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  _canon_stub_wins_body "$1" >"$STUB_DIR/wins.txt"
  _canon_stub_panes_body "$2" >"$STUB_DIR/panes.txt"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows)
  case "$*" in
  *window_activity*) [ -f "$STUB_DIR/human.txt" ] && cat "$STUB_DIR/human.txt" ;;
  *) cat "$STUB_DIR/wins.txt" ;;
  esac
  ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# stub_tmux_frames <wins-body> <panes4-body> [panes3-body] — a tmux stub for
# reap's reclaim pass: `list-panes` answers the new 4-field
# `window\tpane\tcmd\tpath` query with <panes4-body> (detected by
# `pane_current_path` + `window_id` both appearing in the -F format) and the
# legacy 3-field occupancy query (`_occupants`) with <panes3-body> otherwise.
# `list-windows -F …@crew_branch…` (the orphan-window report, run on every
# reap) answers from `$STUB_DIR/wins3.txt` if the test wrote one, else empty —
# most tests have no orphan to report. `capture-pane -t <pane>` answers from
# `$STUB_DIR/frames/<pane>` if the test wrote one, else empty (no capture);
# with `-e` (colored) it prefers `frames/<pane>.e`. The Nth plain capture of a
# pane answers from `frames/<pane>.<N>` when that exists, and runs
# `frames/<pane>.<N>.hook` first when that exists — how a test changes the
# pane, or the bus, between reap's two samples.
stub_tmux_frames() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  mkdir -p "$STUB_DIR/frames"
  _canon_stub_wins_body "$1" >"$STUB_DIR/wins.txt"
  _canon_stub_panes_body "$2" >"$STUB_DIR/panes4.txt"
  _canon_stub_panes_body "${3:-}" >"$STUB_DIR/panes.txt"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows)
  case "$*" in
  *window_activity*) [ -f "$STUB_DIR/human.txt" ] && cat "$STUB_DIR/human.txt" ;;
  *@crew_branch*) [ -f "$STUB_DIR/wins3.txt" ] && cat "$STUB_DIR/wins3.txt" ;;
  *) cat "$STUB_DIR/wins.txt" ;;
  esac
  ;;
list-panes)
  case "$*" in
  *pane_current_path*window_id* | *window_id*pane_current_path*) cat "$STUB_DIR/panes4.txt" ;;
  *) cat "$STUB_DIR/panes.txt" ;;
  esac
  ;;
capture-pane)
  pane=""
  prev=""
  for a in "$@"; do
    [ "$prev" = "-t" ] && pane="$a"
    prev="$a"
  done
  f="$STUB_DIR/frames/$pane"
  case " $* " in
  *" -e "*)
    [ -f "$f.e" ] && f="$f.e"
    ;;
  *)
    n=$(($(cat "$f.n" 2>/dev/null || echo 0) + 1))
    echo "$n" >"$f.n"
    [ -x "$f.$n.hook" ] && "$f.$n.hook"
    [ -f "$f.$n" ] && f="$f.$n"
    ;;
  esac
  [ -f "$f" ] && cat "$f"
  ;;
display-message) : ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# set_frame <pane-id> — write a capture-pane frame (stdin) for stub_tmux_frames.
set_frame() {
  mkdir -p "$STUB_DIR/frames"
  cat >"$STUB_DIR/frames/$1"
}

# #908: the sender check. A tmux stub lists the bats process as a pane, so the
# crew process (a descendant of this test) is anchored to that pane's stamps.
# _stamp_pane <branch> <role> <crew> — every field may be empty (unstamped).
# FAKE_EXTRA holds extra pane rows (target panes) in the same US-separated shape.
_stamp_pane() {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
case "$1" in
list-panes)
  printf '%s\037%s\037%s\037%s\037@1\037%%1\n' "$FAKE_PANE_PID" "$FAKE_BRANCH" "$FAKE_ROLE" "$FAKE_CREW"
  printf '%s' "${FAKE_EXTRA:-}"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export FAKE_PANE_PID=$$ FAKE_BRANCH="$1" FAKE_ROLE="$2" FAKE_CREW="$3"
}

_msg_rows() { bus | jq -s '[.[] | select(.kind=="msg")] | length'; }

@test "reply: a worker or role session cannot reply as the dispatcher (#908)" {
  CREW_WORKER_ID='worker:feat/x#s1-1' run run_crew reply 'worker:feat/x#s1-1' go --crew c1
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing sender"* ]]
  CREW_ROLE_ID='role:feat/x:reviewer' run run_crew reply 'worker:feat/x#s1-1' go --crew c1
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing sender"* ]]
  [ "$(_msg_rows)" = 0 ]
}

@test "msg/reply: the dispatcher still posts (#908)" {
  CREW_ID=c1 run run_crew msg dispatcher:c1 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"msg\") | .from'"
  [ "$output" = dispatcher:c1 ]
  CREW_ID=c1 run run_crew reply 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  CREW_ID= run run_crew reply 'worker:feat/x#s1-1' go --crew c1
  [ "$status" -eq 0 ]
  [ "$(_msg_rows)" = 3 ]

  # A dispatcher pane is anchored but unstamped.
  _stamp_pane '' '' ''
  CREW_ID=c1 run run_crew msg dispatcher:c1 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  CREW_ID=c1 run run_crew reply 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  [ "$(_msg_rows)" = 5 ]
}

_human_setup() {
  git commit --allow-empty -q -m init
  git branch feat/human
  human_wt="$BATS_TEST_TMPDIR/human-wt"
  git worktree add -q "$human_wt" feat/human
  human_wt=$(cd "$human_wt" && pwd -P)
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$human_wt")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/human#s1-1" done
}

# ---------------------------------------------------------------------------
# reap: idle-engine reclaim (#588) — merged/closed PR + terminal-or-pr_open
# state + a claude engine pane whose capture is provably idle → kill its
# window, then reclaim the worktree same as an engine-gone reap.
# ---------------------------------------------------------------------------

gh_stub_state() {
  cat >"$STUB_DIR/gh" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$*" in
*state*) printf '%s\n' '$1' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
}

@test "reap: merged + done + idle claude frame is reaped, its window killed" {
  git commit -q --allow-empty -m init
  git branch feat/idle-reap
  wt_path="$BATS_TEST_TMPDIR/idle-reap-wt"
  git worktree add -q "$wt_path" feat/idle-reap
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-reap#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-reap"* ]]
  grep -q 'kill-window -t @1' "$STUB_LOG"
}

@test "reap: merged + done + meter frame is kept — live turn" {
  git commit -q --allow-empty -m init
  git branch feat/meter-keep
  wt_path="$BATS_TEST_TMPDIR/meter-keep-wt"
  git worktree add -q "$wt_path" feat/meter-keep
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
✳ Perusing… (1m 2s · ↓ 3.1k tokens)
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/meter-keep#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"an engine is still running there (live turn, unsent input, or no idle input box)"* ]]
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: an open PR with an idle engine frame is kept — PR OPEN" {
  git commit -q --allow-empty -m init
  git branch feat/open-pr-idle
  wt_path="$BATS_TEST_TMPDIR/open-pr-idle-wt"
  git worktree add -q "$wt_path" feat/open-pr-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state OPEN
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/open-pr-idle#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"PR OPEN"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: merged + idle frame + tracked modification is kept — uncommitted changes" {
  git commit -q --allow-empty -m init
  git branch feat/dirty-idle
  wt_path="$BATS_TEST_TMPDIR/dirty-idle-wt"
  git worktree add -q "$wt_path" feat/dirty-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  echo tracked >"$wt_path/tracked.txt"
  git -C "$wt_path" add tracked.txt
  git -C "$wt_path" commit -q -m tracked
  echo dirty >>"$wt_path/tracked.txt"
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/dirty-idle#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"uncommitted changes"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: merged + done with no engine pane is reaped as before" {
  git commit -q --allow-empty -m init
  git branch feat/no-engine
  wt_path="$BATS_TEST_TMPDIR/no-engine-wt"
  git worktree add -q "$wt_path" feat/no-engine
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tfish\t%s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/no-engine#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/no-engine"* ]]
}

@test "reap: --dry-run on an idle claude frame reports it would kill the window" {
  git commit -q --allow-empty -m init
  git branch feat/idle-dry
  wt_path="$BATS_TEST_TMPDIR/idle-dry-wt"
  git worktree add -q "$wt_path" feat/idle-dry
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-dry#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would kill window @1"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: an idle frame with unsent input in the box is kept" {
  git commit -q --allow-empty -m init
  git branch feat/input-box
  wt_path="$BATS_TEST_TMPDIR/input-box-wt"
  git worktree add -q "$wt_path" feat/input-box
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯ push it
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/input-box#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"input box"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a msg posted after done keeps the worktree" {
  git commit -q --allow-empty -m init
  git branch feat/later-msg
  wt_path="$BATS_TEST_TMPDIR/later-msg-wt"
  git worktree add -q "$wt_path" feat/later-msg
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/later-msg#s1-1" done "" "https://github.com/o/r/pull/1"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  later_ts=$(jq -nc 'now*1000|floor + 1000')
  jq -nc --argjson ts "$later_ts" \
    '{ts:$ts, crew_id:"c1", from:"worker:feat/later-msg#s1-1", to:"dispatcher:c1", kind:"msg", body:"hi"}' >>"$log"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"posted after its done"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: an exited state with a live idle engine still keeps — old reason" {
  git commit -q --allow-empty -m init
  git branch feat/exited-idle
  wt_path="$BATS_TEST_TMPDIR/exited-idle-wt"
  git worktree add -q "$wt_path" feat/exited-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/exited-idle#s1-1" exited "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/exited-idle — an engine is still running there"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a pr_open worker whose PR merged and engine is idle is reaped" {
  git commit -q --allow-empty -m init
  git branch feat/pr-open-idle
  wt_path="$BATS_TEST_TMPDIR/pr-open-idle-wt"
  git worktree add -q "$wt_path" feat/pr-open-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:(now*1000|floor), crew_id:"c1", from:"worker:feat/pr-open-idle#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"pr_open", pr_url:"https://github.com/o/r/pull/1"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/pr-open-idle"* ]]
}

@test "reap: an orphan-stamped window whose branch has no worktree is reported, not killed" {
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  stub_bin gh
  stub_bin wt
  stub_tmux_frames "" ""
  printf '@9\tfeat/gone\t%s/crew\n' "$common" >"$STUB_DIR/wins3.txt"
  # A non-terminal event so `$log` exists and the reap arm runs far enough to
  # reach the orphan-window report (its `[ -f "$log" ] || exit 0` guard sits
  # above everything else); it yields no candidate of its own.
  CREW_ID=c1 run_crew status "worker:feat/other#s1-1" working
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"window @9 is stamped feat/gone but its worktree is gone — not killed"* ]]
  run ! grep -q 'kill-window -t @9' "$STUB_LOG"
}

@test "reap: a live reap.lock.d held by another live pid is skipped" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  log_dir="$(dirname "$log")"
  CREW_ID=c1 run_crew status "worker:feat/locked-out#s1-1" done "" "https://github.com/o/r/pull/1"
  mkdir -p "$log_dir/reap.lock.d"
  echo $$ >"$log_dir/reap.lock.d/pid"
  stub_bin gh
  stub_bin wt
  CREW_ID=c1 run run_crew reap --no-wait
  [ "$status" -eq 0 ]
  [[ "$output" == *"another reap is running — skipped"* ]]
  run ! grep -q 'pr' "$STUB_LOG"
}

@test "reap: by default waits out a live reap.lock.d, then does its work" {
  git commit -q --allow-empty -m init
  git branch feat/lock-wait
  wt_path="$BATS_TEST_TMPDIR/lock-wait-wt"
  git worktree add -q "$wt_path" feat/lock-wait
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" ""
  CREW_ID=c1 run_crew status "worker:feat/lock-wait#s1-1" done "" "https://github.com/o/r/pull/1"
  log_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  sleep 2 &
  holder=$!
  mkdir -p "$log_dir/reap.lock.d"
  echo "$holder" >"$log_dir/reap.lock.d/pid"
  start=$(date +%s)
  CREW_ID=c1 run run_crew reap --quiet
  kill "$holder" 2>/dev/null || true
  wait "$holder" 2>/dev/null || true
  [ "$status" -eq 0 ]
  [ "$(($(date +%s) - start))" -ge 1 ]
  [[ "$output" == *"reaped feat/lock-wait"* ]]
  [[ "$output" != *"skipped"* ]]
}

@test "reap: an idle frame is still classified idle under LC_ALL=C" {
  git commit -q --allow-empty -m init
  git branch feat/idle-c-locale
  wt_path="$BATS_TEST_TMPDIR/idle-c-locale-wt"
  git worktree add -q "$wt_path" feat/idle-c-locale
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-c-locale#s1-1" done "" "https://github.com/o/r/pull/1"
  LC_ALL=C CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-c-locale"* ]]
}

@test "reap: an idle frame with a trailing NBSP in the input box is still idle" {
  git commit -q --allow-empty -m init
  git branch feat/idle-nbsp
  wt_path="$BATS_TEST_TMPDIR/idle-nbsp-wt"
  git worktree add -q "$wt_path" feat/idle-nbsp
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  printf '  \xe2\x8e\xbf  Done (14 tool uses \xc2\xb7 58.2k tokens \xc2\xb7 1m 9s)\n\xe2\x9c\xbb Churned for 36s \xc2\xb7 done 11:20 AM\n\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80 reef \xe2\x94\x80\n\xe2\x9d\xaf \xc2\xa0\n\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\n  -- INSERT -- \xe2\x8f\xb5\xe2\x8f\xb5 auto mode on \xc2\xb7 \xe2\x86\x90 for agents\n' >"$STUB_DIR/frames/%1"
  CREW_ID=c1 run_crew status "worker:feat/idle-nbsp#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-nbsp"* ]]
}

# reap_idle_fixture <slug> [engine] — a done worker whose PR merged, at a
# fresh worktree with one <engine> (default claude) pane %1 in window @1. The
# caller writes the pane's frame(s) and runs reap.
reap_idle_fixture() {
  git commit -q --allow-empty -m init
  git branch "feat/$1"
  wt_path="$BATS_TEST_TMPDIR/$1-wt"
  git worktree add -q "$wt_path" "feat/$1"
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\t%s\t%s\n' "${2:-claude}" "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/$1#s1-1" done "" "https://github.com/o/r/pull/1"
}

# assert_engine_kept <reason> — reap kept the worker for <reason> and touched
# neither its window nor its worktree.
assert_engine_kept() {
  [ "$status" -eq 0 ]
  [[ "$output" == *"an engine is still running there ($1)"* ]]
  run ! grep -qE 'kill-window|^remove' "$STUB_LOG"
}

@test "reap: an esc-to-interrupt turn above an empty box is kept" {
  reap_idle_fixture idle-working
  set_frame %1 <<'EOF'
✻ Working (2s · esc to interrupt)
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: a thinking turn with a todo list between it and the box is kept" {
  reap_idle_fixture idle-thinking
  set_frame %1 <<'EOF'
✻ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)
  ⎿  ☐ Read the reap arm
     ☐ Reuse the idle classifier
     ☐ Re-sample before the kill
     ☐ Wait on the reap lock
     ☐ Run the suite
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: a live subagent row above a finished-looking box is kept" {
  reap_idle_fixture idle-subagent
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
  ◯ general-purpose  Revise spec per critic                              2m 3s · ↓ 71.5k tokens
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: an option-select prompt frame is kept" {
  reap_idle_fixture idle-select
  set_frame %1 <<'EOF'
  2. Gate everything on 3.8
     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.
  3. Require 3.8, drop legacy
  4. Type something.
──────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "prompt on screen"
}

@test "reap: a finished turn still running a background shell is kept" {
  reap_idle_fixture idle-shell
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM · 1 shell still running
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "background shell or monitor still running"
}

@test "reap: a non-claude engine pane is kept — no idle signature" {
  reap_idle_fixture idle-codex codex
  set_frame %1 <<'EOF'
──────────────────────────────
❯
──────────────────────────────
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "codex has no idle signature"
}

@test "reap: a pi pane with no footer row under its box is kept — not provably idle" {
  reap_idle_fixture idle-pi pi
  set_frame %1 <<'EOF'
──────────────────────────────
❯
──────────────────────────────
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "pi frame: live turn or no idle input box"
}

@test "reap: an empty box with no finished-turn marker (fresh boot) is kept" {
  reap_idle_fixture idle-boot
  set_frame %1 <<'EOF'
╭───────────────────────────────────────────╮
│ ✻ Welcome to Claude Code!                 │
╰───────────────────────────────────────────╯
────────────────── reef ─
❯
──────────────────
  ? for shortcuts
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "no finished-turn marker above the input box"
}

@test "reap: a real unsent draft (not dimmed in the colored capture) is kept" {
  reap_idle_fixture idle-draft
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯ push it
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  printf '✻ Churned for 36s · done 11:20 AM\n────────────────── reef ─\n❯\xc2\xa0push it\n──────────────────\n  -- INSERT --\n' >"$STUB_DIR/frames/%1.e"
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: a dimmed prompt suggestion in the box is ghost text — reaped" {
  reap_idle_fixture idle-ghost
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯ push it
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  printf '✻ Churned for 36s · done 11:20 AM\n────────────────── reef ─\n❯\xc2\xa0\033[2mpush it\033[0m\n──────────────────\n  -- INSERT --\n' >"$STUB_DIR/frames/%1.e"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-ghost"* ]]
  grep -q 'kill-window -t @1' "$STUB_LOG"
}

@test "reap: a pane that changes between the two samples is kept" {
  reap_idle_fixture idle-moved
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  set_frame %1.2 <<'EOF'
✻ Churned for 36s · done 11:20 AM
> go on
✻ Churned for 2s · done 11:21 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-moved — pane changed between samples"* ]]
  run ! grep -qE 'kill-window|^remove' "$STUB_LOG"
}

@test "reap: a branch that posts between the two samples is kept" {
  reap_idle_fixture idle-posted
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  cat >"$STUB_DIR/frames/%1.2.hook" <<EOF
#!/usr/bin/env bash
jq -nc '{ts:(now*1000|floor + 1000), crew_id:"c1", from:"worker:feat/idle-posted#s2-2", to:"dispatcher:c1", kind:"msg", body:"back"}' >>"$log"
EOF
  chmod +x "$STUB_DIR/frames/%1.2.hook"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-posted — the branch posted since"* ]]
  run ! grep -qE 'kill-window|^remove' "$STUB_LOG"
}

@test "reap: a msg from another session of the branch after done keeps it" {
  reap_idle_fixture idle-sibling
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc '{ts:(now*1000|floor + 1000), crew_id:"c1", from:"worker:feat/idle-sibling#s2-2", to:"dispatcher:c1", kind:"msg", body:"hi"}' >>"$log"
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "session posted after its done"
}

@test "reap: a branch whose latest session failed with no pr_url reaps via an earlier session's MERGED PR (#836)" {
  git commit -q --allow-empty -m init
  git branch feat/xsess
  wt_path="$BATS_TEST_TMPDIR/xsess-wt"
  git worktree add -q "$wt_path" feat/xsess
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/xsess)" ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/xsess#s1-1" done "" "https://github.com/o/r/pull/5"
  CREW_ID=c1 run_crew status "worker:feat/xsess#s2-2" failed "boom"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/xsess (MERGED)"* ]]
  [ ! -d "$wt_path" ]
}

@test "reap: a kind:reap row's PR is used when no session posted one (#836)" {
  git commit -q --allow-empty -m init
  git branch feat/reaprow
  wt_path="$BATS_TEST_TMPDIR/reaprow-wt"
  git worktree add -q "$wt_path" feat/reaprow
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/reaprow)" ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/reaprow" done
  jq -nc '{ts:(now*1000|floor - 1000), kind:"reap", branch:"feat/reaprow", pr:"https://github.com/o/r/pull/6", pr_state:"MERGED", worktree:"/old/path"}' >>"$log"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/reaprow (MERGED)"* ]]
  [ ! -d "$wt_path" ]
}

@test "reap: a PR found only via gh pr list is reaped and its branch deleted (#836)" {
  git commit -q --allow-empty -m init
  git branch feat/ghlist
  wt_path="$BATS_TEST_TMPDIR/ghlist-wt"
  git worktree add -q "$wt_path" feat/ghlist
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
tip=$(git rev-parse refs/heads/feat/ghlist)
case "$*" in
*"pr list"*) printf '[{"url":"https://github.com/o/r/pull/7","state":"MERGED","headRefOid":"%s","isCrossRepository":false}]\n' "$tip" ;;
*headRefOid*) printf '%s\n' "$tip" ;;
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/ghlist" done
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/ghlist (MERGED)"* ]]
  [ ! -d "$wt_path" ]
  run ! git show-ref --verify --quiet refs/heads/feat/ghlist
}

@test "reap: a cross-repo pr_url to a merged PR is ignored and keeps the worktree (#843)" {
  git commit -q --allow-empty -m init
  git branch feat/xrepo
  wt_path="$BATS_TEST_TMPDIR/xrepo-wt"
  git worktree add -q "$wt_path" feat/xrepo
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
tip=$(git rev-parse refs/heads/feat/xrepo)
case "$*" in
*"pr list"*) printf '[]\n' ;;
*"issue view"*) printf '%s\n' 'CLOSED' ;;
*headRefOid*) printf '%s\n' "$tip" ;;
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/xrepo" done "" "https://github.com/evil/other/pull/7"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/xrepo"* ]]
  [[ "$output" == *"ignoring cross-repo pr_url https://github.com/evil/other/pull/7"* ]]
  [ -d "$wt_path" ]
  run ! grep -q 'remove-label' "$STUB_LOG"
  run ! grep -q 'pr view https://github.com/evil' "$STUB_LOG"
  run run_crew reap
  [ "$status" -eq 0 ]
  [ -d "$wt_path" ]
  run ! grep -q 'remove-label' "$STUB_LOG"
}

@test "reap: a cross-repo pr_url falls through to a same-repo merged PR from gh pr list (#843)" {
  git commit -q --allow-empty -m init
  git branch feat/xfall
  wt_path="$BATS_TEST_TMPDIR/xfall-wt"
  git worktree add -q "$wt_path" feat/xfall
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
tip=$(git rev-parse refs/heads/feat/xfall)
case "$*" in
*"pr list"*) printf '[{"url":"https://github.com/o/r/pull/7","state":"MERGED","headRefOid":"%s","isCrossRepository":false}]\n' "$tip" ;;
*headRefOid*) printf '%s\n' "$tip" ;;
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/xfall" done "" "https://github.com/evil/other/pull/7"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/xfall (MERGED)"* ]]
  [ ! -d "$wt_path" ]
  run ! grep -q 'pr view https://github.com/evil' "$STUB_LOG"
}

@test "reap: a github.com pr_url is ignored when origin is not on github.com (#843)" {
  git config remote.origin.url https://ghe.example.com/o/r.git
  seed_git_baseline
  git commit -q --allow-empty -m init
  git branch feat/ghe
  wt_path="$BATS_TEST_TMPDIR/ghe-wt"
  git worktree add -q "$wt_path" feat/ghe
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
tip=$(git rev-parse refs/heads/feat/ghe)
case "$*" in
*"pr list"*) printf '[]\n' ;;
*"issue view"*) printf '%s\n' 'CLOSED' ;;
*headRefOid*) printf '%s\n' "$tip" ;;
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/ghe" done "" "https://github.com/o/r/pull/7"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/ghe"* ]]
  [[ "$output" == *"ignoring cross-repo pr_url https://github.com/o/r/pull/7"* ]]
  [ -d "$wt_path" ]
  run ! grep -q 'pr view https://github.com/o/r' "$STUB_LOG"
}

@test "reap: an empty pr_url does not promote an earlier session's PR to the latest session's (#843)" {
  git commit -q --allow-empty -m init
  git branch feat/emptyurl
  wt_path="$BATS_TEST_TMPDIR/emptyurl-wt"
  git worktree add -q "$wt_path" feat/emptyurl
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  old=$(git rev-parse feat/emptyurl)
  git -C "$wt_path" commit -q --allow-empty -m more
  cat >"$STUB_DIR/gh" <<GH
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$*" in
*"pr list"*) printf '[]\n' ;;
*headRefOid*) printf '%s\n' '$old' ;;
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", from:"worker:feat/emptyurl#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done", pr_url:"https://github.com/o/r/pull/7"}}' >>"$log"
  jq -nc '{ts:2000, crew_id:"c1", from:"worker:feat/emptyurl#s2-2", to:"dispatcher:c1", kind:"status", body:{state:"done", pr_url:""}}' >>"$log"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/emptyurl"* ]]
  [[ "$output" == *"its tip has commits past https://github.com/o/r/pull/7"* ]]
  [[ "$output" != *"would reap"* ]]
  [ -d "$wt_path" ]
}

@test "reap: a failing gh pr list keeps the worker (#836)" {
  git commit -q --allow-empty -m init
  git branch feat/ghfail
  wt_path="$BATS_TEST_TMPDIR/ghfail-wt"
  git worktree add -q "$wt_path" feat/ghfail
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*"pr list"*) exit 1 ;;
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/ghfail" done
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/ghfail — could not list PRs for feat/ghfail"* ]]
  [ -d "$wt_path" ]
}

# reap_issue_fixture <slug> <issue state> [claim] — a done worker on feat/<slug>
# with one commit, no PR anywhere, and (with `claim`) a claim-issue row for #77.
reap_issue_fixture() {
  git commit -q --allow-empty -m init
  git branch "feat/$1"
  wt_path="$BATS_TEST_TMPDIR/$1-wt"
  git worktree add -q "$wt_path" "feat/$1"
  wt_path=$(cd "$wt_path" && pwd -P)
  printf 'x\n' >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m work
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<GH
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$*" in
*"pr list"*) printf '%s\n' '[]' ;;
*"issue view"*) printf '%s\n' '$2' ;;
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/$1" done
  if [ "${3:-}" = claim ]; then
    jq -nc --arg b "feat/$1" '{ts:(now*1000|floor), crew_id:"c1", kind:"claim-issue", issue:"77", branch:$b, pid:1}' >>"$log"
  fi
}

@test "reap: done with no PR and a CLOSED claimed issue reclaims, keeping the branch (#836)" {
  reap_issue_fixture issue-closed CLOSED claim
  git cherry-pick "$(git rev-parse refs/heads/feat/issue-closed)" >/dev/null
  git update-ref refs/remotes/origin/main main
  git symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/main
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/issue-closed (issue #77 CLOSED)"* ]]
  [ ! -d "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/issue-closed
  grep -qF 'issue edit 77 --remove-label dispatched' "$STUB_LOG"
  jq -se 'map(select(.kind == "reap" and .branch == "feat/issue-closed")) | .[0] | .pr == "" and .pr_state == "ISSUE_CLOSED" and .issues == ["77"]' "$log"
}

@test "reap: a closed issue with commits on no remote keeps the worker (#836)" {
  reap_issue_fixture issue-unpushed CLOSED claim
  git update-ref refs/remotes/origin/main main
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/issue-unpushed — done but no PR on the bus or GitHub; 1 commit(s) not on any remote and not patch-equivalent to origin/main"* ]]
  [ -d "$wt_path" ]
}

@test "reap: an OPEN claimed issue keeps the worker (#836)" {
  reap_issue_fixture issue-open OPEN claim
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/issue-open — done but no PR on the bus or GitHub; issue #77 is OPEN"* ]]
  [ -d "$wt_path" ]
}

@test "reap: no PR and no claim-issue row keeps the worker (#836)" {
  reap_issue_fixture issue-none CLOSED
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/issue-none — done but no PR on the bus or GitHub; no claim-issue row"* ]]
  [ -d "$wt_path" ]
}

# reap_discard_fixture <slug> <PR state> — a done worker on feat/<slug> with a
# bus PR in that state and a dirty tree: an unstaged edit, a staged edit, an
# untracked file and an untracked scaffold file.
reap_discard_fixture() {
  printf 'a\n' >t.txt
  printf 'b\n' >s.txt
  git add t.txt s.txt
  git commit -q -m init
  git branch "feat/$1"
  wt_path="$BATS_TEST_TMPDIR/$1-wt"
  git worktree add -q "$wt_path" "feat/$1"
  wt_path=$(cd "$wt_path" && pwd -P)
  printf 'a2\n' >"$wt_path/t.txt"
  printf 'b2\n' >"$wt_path/s.txt"
  git -C "$wt_path" add s.txt
  printf 'new\n' >"$wt_path/new.txt"
  printf 'notes\n' >"$wt_path/REVIEW_NOTES.md"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<GH
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$*" in
*"pr list"*) printf '%s\n' '[]' ;;
*"issue view"*) printf '%s\n' 'CLOSED' ;;
*headRefOid*) git rev-parse "refs/heads/feat/$1" ;;
*state*) printf '%s\n' '$2' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  log="$common/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/$1" done "" "https://github.com/o/r/pull/9"
}

@test "reap: --discard saves a dirty merged tree as a patch that applies to the branch tip, then removes it (#836)" {
  reap_discard_fixture discard-ok MERGED
  CREW_ID=c1 run run_crew reap --discard feat/discard-ok
  [ "$status" -eq 0 ]
  [[ "$output" == *"saved uncommitted state to $common/crew/artifacts/feat/discard-ok/discarded-"*".patch"* ]]
  [[ "$output" == *"reaped feat/discard-ok (MERGED, discarded)"* ]]
  patch=$(printf '%s\n' "$common"/crew/artifacts/feat/discard-ok/discarded-*.patch)
  [ -s "$patch" ]
  [ ! -e "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/discard-ok
  jq -se --arg p "$patch" 'map(select(.kind == "reap" and .branch == "feat/discard-ok")) | .[0].discarded == $p' "$log"
  fresh="$BATS_TEST_TMPDIR/discard-ok-fresh"
  git worktree add -q "$fresh" feat/discard-ok
  git -C "$fresh" apply --check "$patch"
  git -C "$fresh" apply "$patch"
  [ "$(cat "$fresh/t.txt")" = a2 ]
  [ "$(cat "$fresh/s.txt")" = b2 ]
  [ "$(cat "$fresh/new.txt")" = new ]
  [ "$(cat "$fresh/REVIEW_NOTES.md")" = notes ]
}

@test "reap: --discard refuses an OPEN PR and touches nothing (#836)" {
  reap_discard_fixture discard-open OPEN
  CREW_ID=c1 run run_crew reap --discard feat/discard-open
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard feat/discard-open — PR OPEN"* ]]
  [ -d "$wt_path" ]
  [ ! -e "$common/crew/artifacts/feat/discard-open" ]
}

@test "reap: --discard refuses a tree with a live engine pane (#836)" {
  reap_discard_fixture discard-live MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  CREW_ID=c1 run run_crew reap --discard feat/discard-live
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard feat/discard-live — an engine is running there"* ]]
  [ -d "$wt_path" ]
  [ ! -e "$common/crew/artifacts/feat/discard-live" ]
}

@test "reap: --discard refuses a branch whose latest status is working (#836)" {
  reap_discard_fixture discard-working MERGED
  CREW_ID=c1 run_crew status "worker:feat/discard-working" working
  CREW_ID=c1 run run_crew reap --discard feat/discard-working
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard feat/discard-working — latest status is working"* ]]
  [ -d "$wt_path" ]
  [ ! -e "$common/crew/artifacts/feat/discard-working" ]
}

@test "reap: --discard on a clean tree with a closed issue removes it, keeping the branch and writing no patch (#836)" {
  reap_issue_fixture discard-issue CLOSED claim
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  CREW_ID=c1 run run_crew reap --discard feat/discard-issue --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"no uncommitted changes to save"* ]]
  [[ "$output" == *"reaped feat/discard-issue (issue #77 CLOSED, discarded)"* ]]
  [ ! -e "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/discard-issue
  [ -z "$(find "$common/crew/artifacts" -name 'discarded-*' 2>/dev/null)" ]
  jq -se 'map(select(.kind == "reap" and .branch == "feat/discard-issue")) | .[0].discarded == ""' "$log"
}

@test "reap: --discard never runs a worker-planted clean filter (#836)" {
  git commit -q --allow-empty -m init
  printf '' >f
  printf 'f filter=x\n' >.gitattributes
  git add f .gitattributes
  git commit -q -m 'track f under filter x'
  git branch feat/discard-557
  wt_path="$BATS_TEST_TMPDIR/discard-557-wt"
  git worktree add -q "$wt_path" feat/discard-557
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  seed_git_baseline
  git config filter.x.clean "$BATS_TEST_TMPDIR/hit.sh"
  echo x >"$wt_path/f"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/discard-557" done "" "https://github.com/o/r/pull/557"
  CREW_ID=c1 run run_crew reap --discard feat/discard-557
  [ "$status" -eq 1 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"refusing --discard feat/discard-557"* ]]
}

@test "reap: --discard never runs a baselined worktree-relative filter the worker rewrote (#836)" {
  mkdir -p tools
  printf '#!/bin/sh\ncat\n' >tools/conv.sh
  chmod +x tools/conv.sh
  printf a >f
  printf 'f filter=x\n' >.gitattributes
  git add tools/conv.sh f .gitattributes
  git commit -q -m 'track f under filter x'
  git config filter.x.clean tools/conv.sh
  seed_git_baseline
  git branch feat/discard-578
  wt_path="$BATS_TEST_TMPDIR/discard-578-wt"
  git worktree add -q "$wt_path" feat/discard-578
  wt_path=$(cd "$wt_path" && pwd -P)
  printf '#!/bin/sh\ntouch %q\ncat\n' "$BATS_TEST_TMPDIR/SENTINEL" >"$wt_path/tools/conv.sh"
  git -C "$wt_path" commit -qam rewrite
  rm -f "$BATS_TEST_TMPDIR/SENTINEL"
  printf b >"$wt_path/f"
  printf 'u\n' >"$wt_path/untracked.txt"
  touch -d '+5 seconds' "$wt_path/f"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/discard-578" done "" "https://github.com/o/r/pull/578"
  CREW_ID=c1 run run_crew reap --discard feat/discard-578
  [ "$status" -eq 0 ]
  [[ "$output" == *"saved uncommitted state to "* ]]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ ! -d "$wt_path" ]
}

@test "reap: --discard --dry-run reports the patch path and writes nothing (#836)" {
  reap_discard_fixture discard-dry MERGED
  CREW_ID=c1 run run_crew reap --discard feat/discard-dry --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would save uncommitted state to $common/crew/artifacts/feat/discard-dry/discarded-"* ]]
  [[ "$output" == *"would reap feat/discard-dry (MERGED) @ $wt_path"* ]]
  [ -d "$wt_path" ]
  [ ! -e "$common/crew/artifacts/feat/discard-dry" ]
}

@test "reap: --discard refuses an invalid branch name (#836)" {
  CREW_ID=c1 run_crew status "worker:feat/x" done
  CREW_ID=c1 run run_crew reap --discard '@{-1}'
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard @{-1} — not a valid branch name"* ]]
}

@test "reap: plain reap never discards a dirty merged tree (#836)" {
  reap_discard_fixture discard-plain MERGED
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/discard-plain — uncommitted changes"* ]]
  [ -d "$wt_path" ]
  [ -z "$(find "$common/crew/artifacts" -name 'discarded-*' 2>/dev/null)" ]
}

@test "reap: --discard refuses a symlinked artifacts dir and writes nothing through it (#836)" {
  reap_discard_fixture discard-sym MERGED
  victim="$BATS_TEST_TMPDIR/victim"
  mkdir -p "$victim" "$common/crew/artifacts/feat"
  ln -s "$victim" "$common/crew/artifacts/feat/discard-sym"
  CREW_ID=c1 run run_crew reap --discard feat/discard-sym
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard feat/discard-sym — $common/crew/artifacts/feat/discard-sym is a symlink or not a directory"* ]]
  [ -z "$(ls -A "$victim")" ]
  [ -d "$wt_path" ]
}

@test "reap: --discard refuses a symlink at a parent of a slashed branch's artifacts dir (#836)" {
  reap_discard_fixture discard-par MERGED
  victim="$BATS_TEST_TMPDIR/victim"
  mkdir -p "$victim" "$common/crew/artifacts"
  ln -s "$victim" "$common/crew/artifacts/feat"
  CREW_ID=c1 run run_crew reap --discard feat/discard-par
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard feat/discard-par — $common/crew/artifacts/feat is a symlink or not a directory"* ]]
  [ -z "$(ls -A "$victim")" ]
  [ -d "$wt_path" ]
}

@test "reap: --discard never writes through a symlink planted at a predictable temp name (#836)" {
  reap_discard_fixture discard-tmp MERGED
  victim="$BATS_TEST_TMPDIR/victim.txt"
  printf 'keep\n' >"$victim"
  art="$common/crew/artifacts/feat/discard-tmp"
  mkdir -p "$art"
  now=$(date -u +%s)
  for off in -1 0 1 2 3 4 5 6 7 8 9 10; do
    ln -sf "$victim" "$art/discarded-$(date -u -d "@$((now + off))" +%Y%m%dT%H%M%SZ).patch.tmp"
  done
  CREW_ID=c1 run run_crew reap --discard feat/discard-tmp
  [ "$status" -eq 0 ]
  [ "$(cat "$victim")" = keep ]
  [ ! -e "$wt_path" ]
}

@test "reap: --discard refuses a tree holding an untracked nested git repo (#836)" {
  reap_discard_fixture discard-nest MERGED
  git init -q "$wt_path/sub"
  printf 'inner\n' >"$wt_path/sub/inner.txt"
  git -C "$wt_path/sub" add inner.txt
  git -C "$wt_path/sub" -c user.email=t@e -c user.name=t commit -q -m inner
  CREW_ID=c1 run run_crew reap --discard feat/discard-nest
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard feat/discard-nest — it holds an embedded git repository"* ]]
  [ -d "$wt_path/sub/.git" ]
  [ -f "$wt_path/sub/inner.txt" ]
  [ -z "$(find "$common/crew/artifacts" -name 'discarded-*' 2>/dev/null)" ]
}

@test "reap: --discard writes a standard patch whatever the operator's diff config (#836)" {
  reap_discard_fixture discard-cfg MERGED
  gcfg="$BATS_TEST_TMPDIR/global.gitconfig"
  printf '[diff]\n\tnoprefix = true\n\tmnemonicPrefix = true\n[color]\n\tdiff = always\n' >"$gcfg"
  GIT_CONFIG_GLOBAL="$gcfg" CREW_ID=c1 run run_crew reap --discard feat/discard-cfg
  [ "$status" -eq 0 ]
  patch=$(printf '%s\n' "$common"/crew/artifacts/feat/discard-cfg/discarded-*.patch)
  fresh="$BATS_TEST_TMPDIR/discard-cfg-fresh"
  git worktree add -q "$fresh" feat/discard-cfg
  git -C "$fresh" apply --check "$patch"
}

@test "reap: --discard with the reap lock busy refuses instead of skipping (#836)" {
  reap_discard_fixture discard-lock MERGED
  mkdir -p "$common/crew/reap.lock.d"
  echo $$ >"$common/crew/reap.lock.d/pid"
  CREW_ID=c1 run run_crew reap --discard feat/discard-lock --no-wait
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing --discard feat/discard-lock — another reap is running"* ]]
  [ -d "$wt_path" ]
}

@test "reap: --discard keeps a tree written into an already-dirty path after the save (#836)" {
  reap_discard_fixture discard-race MERGED
  cat >"$STUB_DIR/tmux" <<TMUX
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1" in
list-windows) printf 'late\n' >>"$wt_path/t.txt" ;;
esac
exit 0
TMUX
  CREW_ID=c1 run run_crew reap --discard feat/discard-race
  [ "$status" -eq 1 ]
  [[ "$output" == *"keeping feat/discard-race — changed while saving"* ]]
  [ -d "$wt_path" ]
  [ -z "$(find "$common/crew" -maxdepth 1 -name 'discard.*')" ]
}

@test "reap: a --discard killed by TERM mid-save leaves no discard temp dir (#836)" {
  reap_discard_fixture discard-term MERGED
  real_git=$(command -v git)
  cat >"$STUB_DIR/git" <<GIT
#!/usr/bin/env bash
case "\$*" in
*" add -A"*)
  case "\${GIT_INDEX_FILE:-}" in
  */discard.*) kill -TERM \$PPID; sleep 1 ;;
  esac
  ;;
esac
exec "$real_git" "\$@"
GIT
  chmod +x "$STUB_DIR/git"
  CREW_ID=c1 run run_crew reap --discard feat/discard-term
  [ "$status" -ne 0 ]
  [ -d "$wt_path" ]
  [ -z "$(find "$common/crew" -maxdepth 1 -name 'discard.*')" ]
}

@test "reap: an unfetched PR head gives a could-not-compare keep reason (#836)" {
  git commit -q --allow-empty -m init
  git branch feat/unfetched
  wt_path="$BATS_TEST_TMPDIR/unfetched-wt"
  git worktree add -q "$wt_path" feat/unfetched
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*headRefOid*) printf '%s\n' '0123456789abcdef0123456789abcdef01234567' ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/unfetched#s1-1" done "" "https://github.com/o/r/pull/5"
  CREW_ID=c1 run_crew status "worker:feat/unfetched#s2-2" failed "boom"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/unfetched — could not compare with https://github.com/o/r/pull/5 head (not fetched?)"* ]]
  [[ "$output" != *"commits past"* ]]
  [ -d "$wt_path" ]
}

@test "reap: an empty branch or empty state on the bus shifts no other branch's columns (#836)" {
  git commit -q --allow-empty -m init
  git branch done
  wt_path="$BATS_TEST_TMPDIR/done-wt"
  git worktree add -q "$wt_path" done
  wt_path=$(cd "$wt_path" && pwd -P)
  git -C "$wt_path" commit -q --allow-empty -m later
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*"pr list"*) printf '%s\n' '[]' ;;
*state*) printf '%s\n' 'MERGED' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/done~1)" ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:done#s1-1" done "" "https://github.com/o/r/pull/5"
  CREW_ID=c1 run_crew status "worker:done#s2-2" failed "boom"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc '{ts:8999999999998, crew_id:"c1", from:"worker:", to:"dispatcher:c1", kind:"status", body:{state:"done", pr_url:"https://github.com/o/r/pull/96"}}' >>"$log"
  jq -nc '{ts:8999999999999, crew_id:"c1", from:"worker:#a", to:"dispatcher:c1", kind:"status", body:{state:"done", pr_url:"https://github.com/o/r/pull/97"}}' >>"$log"
  jq -nc '{ts:9000000000000, crew_id:"c1", from:"worker:#b", to:"dispatcher:c1", kind:"status", body:{state:"done", pr_url:"https://github.com/o/r/pull/99"}}' >>"$log"
  jq -nc '{ts:9000000000001, crew_id:"c1", from:"worker:feat/other#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"", pr_url:"https://github.com/o/r/pull/98"}}' >>"$log"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping done — its tip has commits past https://github.com/o/r/pull/5"* ]]
  [[ "$output" != *"reaped"* ]]
  [ "$(grep -c 'keeping done' <<<"$output")" -eq 1 ]
  [ -d "$wt_path" ]
  jq -nc '{ts:9000000000002, crew_id:"c1", from:"worker:done#s3-3", to:"dispatcher:c1", kind:"status", body:{state:"", pr_url:"https://github.com/o/r/pull/97"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --discard done
  [ "$status" -eq 1 ]
  [[ "$output" == *"latest status is none"* ]]
  [ -d "$wt_path" ]
}

@test "reap: idle release ignores an empty-branch bus row instead of shifting it onto a real branch (#836)" {
  git commit --allow-empty -q -m init
  git branch done
  wt_path="$BATS_TEST_TMPDIR/idle-empty-wt"
  git worktree add -q "$wt_path" done
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc '{ts:1000, crew_id:"c1", from:"worker:#done", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
  [ -d "$wt_path" ]
}

@test "reap: idle release keeps the state column when a session id is empty (#836)" {
  git commit --allow-empty -q -m init
  git branch feat/xsess-empty
  wt_path="$BATS_TEST_TMPDIR/xsess-empty-wt"
  git worktree add -q "$wt_path" feat/xsess-empty
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\t.claude-wrapped\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc '{ts:1000, crew_id:"c1", from:"worker:feat/xsess-empty#", to:"dispatcher:c1", kind:"status", body:{state:"exited"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/xsess-empty — exited but an engine is still running there"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: an earlier session's MERGED PR does not vouch for commits past its head (#836)" {
  git commit -q --allow-empty -m init
  git branch feat/xpast
  wt_path="$BATS_TEST_TMPDIR/xpast-wt"
  git worktree add -q "$wt_path" feat/xpast
  wt_path=$(cd "$wt_path" && pwd -P)
  git -C "$wt_path" commit -q --allow-empty -m later
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/xpast~1)" ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/xpast#s1-1" done "" "https://github.com/o/r/pull/5"
  CREW_ID=c1 run_crew status "worker:feat/xpast#s2-2" failed "boom"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/xpast — its tip has commits past https://github.com/o/r/pull/5"* ]]
  [ -d "$wt_path" ]
}

@test "reap: a closed issue whose commits sit on a remote-tracking ref reclaims, keeping the branch (#836)" {
  reap_issue_fixture issue-pushed CLOSED claim
  git update-ref refs/remotes/origin/main main
  git update-ref refs/remotes/origin/feat/issue-pushed refs/heads/feat/issue-pushed
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/issue-pushed (issue #77 CLOSED)"* ]]
  [ ! -d "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/issue-pushed
}

# ---------------------------------------------------------------------------
# stall-watch harness
# ---------------------------------------------------------------------------

# bus — the raw event log for the test repo. Exported so `run bash -c "bus | ..."`
# (a new bash process, not a fork) can see it.
bus() {
  cat "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl" 2>/dev/null || true
}
export -f bus

# seed_raw <from> <state> <detail> <source> [ts_ms] — append a status event
# directly, so a test can plant a `source:"watchdog"` event or one dated into a
# previous run (things `crew status` cannot express).
seed_raw() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg f "$1" --arg s "$2" --arg d "$3" --arg src "$4" \
    --argjson ts "${5:-$(($(date +%s) * 1000))}" \
    '{ts:$ts, crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status",
      body:({state:$s}
            + (if $d!="" then {detail:$d} else {} end)
            + (if $src!="" then {source:$src} else {} end))}' >>"$logf"
}

# seed_start <dispatch|resume> <session> <ts_ms> — a session-start row on feat/x,
# shaped like dispatch.sh's and dispatch-resume.sh's, which `crew` cannot write.
seed_start() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg k "$1" --arg s "$2" --argjson ts "$3" \
    '{ts:$ts, crew_id:"c1", kind:$k, branch:"feat/x", session:$s,
      worker_id:("worker:feat/x#" + $s)}' >>"$logf"
}

# stall_sampler <frame-file>... — install a CREW_STALL_SAMPLE_CMD that emits the
# given frames one per call and repeats the last one forever. The literal token
# GONE makes the sampler exit non-zero from that call on (pane vanished).
stall_sampler() {
  SAMPLER_DIR="$BATS_TEST_TMPDIR/sampler.$$"
  mkdir -p "$SAMPLER_DIR"
  printf '%s\n' "$@" >"$SAMPLER_DIR/frames"
  printf '0' >"$SAMPLER_DIR/n"
  cat >"$SAMPLER_DIR/sample" <<'EOS'
#!/usr/bin/env bash
d="$(dirname "$0")"
n=$(cat "$d/n")
n=$((n + 1))
printf '%s' "$n" >"$d/n"
total=$(wc -l <"$d/frames")
i="$n"
[ "$i" -gt "$total" ] && i="$total"
f=$(sed -n "${i}p" "$d/frames")
[ "$f" = GONE ] && exit 1
cat "$f"
EOS
  chmod +x "$SAMPLER_DIR/sample"
  export CREW_STALL_SAMPLE_CMD="$SAMPLER_DIR/sample"
}

# frame_file <name> — read a frame from stdin, write it, echo its path.
frame_file() {
  local f="$BATS_TEST_TMPDIR/frame.$1"
  cat >"$f"
  printf '%s' "$f"
}

# ---- fixtures, pinned from EVIDENCE-2026-08-10.txt ------------------------

# [field] session 3 — the workspace-trust frame that wedged three healthy
# workers. Footer is `Enter to confirm`, marker is ASCII `>`, no `Esc to cancel`.
fx_prompt_trust() {
  frame_file prompt_trust <<'EOF'
Quick safety check: Is this a project you created or one you trust?
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
EOF
}

# Captured from the Codex worker hook-review prompt in WORKER_TASK.md. This is
# intentionally verbatim: Codex gets no broader prompt signature without a
# captured frame.
fx_codex_hooks_review() {
  frame_file codex_hooks_review <<'EOF'
Hooks need review
  1 hook is new or changed.
  Hooks can run outside the sandbox after you trust them.
› 1. Review hooks  2. Trust all and continue  3. Continue without trusting
EOF
}

fx_codex_hooks_review_reordered() {
  frame_file codex_hooks_review_reordered <<'EOF'
Hooks need review
  Hooks can run outside the sandbox after you trust them.
  1 hook is new or changed.
› 1. Review hooks  2. Trust all and continue  3. Continue without trusting
EOF
}

# The same trust frame scrolled into the transcript: the input box is the last
# non-empty line, so the geometry anchor must reject it (this is what keeps this
# very test file from being a false-positive source).
fx_prompt_scrollback() {
  frame_file prompt_scrollback <<'EOF'
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
EOF
}

# Same, with the option-select footer, to assert the widening is symmetric.
fx_select_scrollback() {
  frame_file select_scrollback <<'EOF'
  5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
EOF
}

# fx_bgwait_* — finished claude turns parked on a background shell, captured
# from healthy workers (claude Code v2.1.280, #353). The `❯` line is a prompt
# SUGGESTION, not unsent input.
fx_bgwait_crunched() {
  frame_file bgwait_crunched <<'EOF'
✻ Crunched for 1m 12s · done 11:16 AM · 1 shell still running
────────────────── reef ─
❯ push it and re-post pr_open once CI is green
──────────────────
  🤖 Opus 5.5 🧠 high | 📊 350k/1M | ⚡ 29% (1h53m → 13:10)
  -- INSERT -- ⏵⏵ auto mode on · PR #331 · 1 shell · ← for agents
EOF
}

fx_bgwait_churned() {
  frame_file bgwait_churned <<'EOF'
✻ Churned for 36s · done 11:20 AM · 1 shell still running
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
}

# The same finished turn with the shell gone: nothing is being waited on.
fx_bgwait_noshell() {
  frame_file bgwait_noshell <<'EOF'
✻ Churned for 36s · done 11:20 AM
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
}

# A3 — a finished/idle pane: no meter, no prompt, ends on the input box.
fx_idle_box() {
  frame_file idle_box <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents
EOF
}

# fx_meter <clock> <tokens> — D2's shape: a live meter with NO live subagent
# row. The `⎿ Done` history line is included on purpose: it is the decoy that
# must NOT read as a subagent row, or D2 would be neutered forever.
fx_meter() {
  frame_file "meter.$1.$2" <<EOF
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✳ Perusing… ($1 · ↓ $2 tokens · thinking more with high effort)
EOF
}

# fx_subbatch <parent-clock> <subagent-elapsed> — A1's measured false-positive
# driver, pane %129, verbatim shape: meter present, clock rising, parent token
# string static at 73.2k, and a LIVE subagent row painted throughout.
fx_subbatch() {
  frame_file "subbatch.$1.$2" <<EOF
  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)
✶ Hatching… ($1 · ↓ 73.2k tokens)
  ◯ general-purpose  Revise spec per critic                              $2 · ↓ 71.5k tokens
EOF
}

# _release_setup stages a finished worker: a feat/x worktree, its window stubbed
# at @23 with a claude pane %9, and a done row on the bus. fx_done_idle is the
# idle frame that pane shows.
_release_setup() {
  git commit --allow-empty -q -m init
  git branch feat/x
  rel_wt="$BATS_TEST_TMPDIR/rel-wt"
  git worktree add -q "$rel_wt" feat/x
  rel_wt=$(cd "$rel_wt" && pwd -P)
  stub_tmux "$(printf '@23\tsage\t%s\n' "$rel_wt")" "$(printf '@23\t%%9\tclaude\n')"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
}

fx_done_idle() {
  frame_file done_idle <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
}

# ---- D7 runaway: (#650) -----------------------------------------------------
# fx_pi_runaway <down-tokens> — the 2026-10-02 incident, RECONSTRUCTED from the
# issue description (no raw capture): a leaked DeepSeek sentinel followed by a
# word-salad stream, pi footer with a climbing `↓`.
fx_pi_runaway() {
  frame_file "pi_runaway.$1" <<EOF
I'll trust evidence/inspection-based verification <｜end▁of▁thinking｜> dragon phoenix griffin
kraken quark lepton boson carabiner piton cam nut granite basalt gneiss schist chert obsidian
entropy enthalpy torque momentum inertia viscosity catalyst isotope lattice manifold
─────────────────────────────────────────────────────────────────────
⠇ Working
↑50k ↓$1 R1.6M CH99.2% \$0.027 8.7%/1.0M (auto)
EOF
}

# fx_pi_busy <down-tokens> — a normal busy pi frame: prose, no sentinel.
fx_pi_busy() {
  frame_file "pi_busy.$1" <<EOF
I'll read the config first, then adjust the watchdog flags and run the suite.
─────────────────────────────────────────────────────────────────────
⠇ Working
↑50k ↓$1 R1.6M CH99.2% \$0.027 8.7%/1.0M (auto)
EOF
}

# fx_claude_tool_sentinel <tokens> — claude reading a file that contains
# sentinel strings: they sit inside a `⎿` tool-result block.
fx_claude_tool_sentinel() {
  frame_file "claude_toolsent.$1" <<EOF
⏺ Read(adapters/core/crew.sh)
  ⎿  re_sentinel='<｜[^｜>]{1,40}｜>|<|im_end|>'
     <｜end▁of▁thinking｜> appears in this fixture
     <|endoftext|>
⏺ Now I will update the detector.
✳ Perusing… (1m 2s · ↓ $1 tokens · thinking)
EOF
}

# fx_pi_tool_sentinel <down-tokens> — the same, in pi's expanded tool output.
fx_pi_tool_sentinel() {
  frame_file "pi_toolsent.$1" <<EOF
Tool output: read adapters/core/crew.sh
  re_sentinel='<｜[^｜>]{1,40}｜>' and <|im_end|> appear in the fixture

Now I will update the detector.
─────────────────────────────────────────────────────────────────────
⠇ Working
↑50k ↓$1 R1.6M CH99.2% \$0.027 8.7%/1.0M (auto)
EOF
}

# fx_claude_long_answer <tokens> — a long, legitimate, punctuated answer.
fx_claude_long_answer() {
  frame_file "claude_long.$1" <<EOF
⏺ Here is the full analysis. The watchdog samples the pane every fifteen seconds, and
  each detector keeps its own episode state; when the evidence goes away, it posts a
  clearance. First, the prompt detector requires a verified geometry. Second, the
  quiet detector requires byte identity. Third, the load detector reads the host.
✳ Composing… (2m 1s · ↓ $1 tokens · thinking)
EOF
}

runaway_flags=(--grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 6 --runaway-hits 3 --runaway-tokens 1500)

@test "stall-watch: D7 posts blocked/runaway: on the pi incident frame" {
  a=$(fx_pi_runaway 19.0k)
  b=$(fx_pi_runaway 19.8k)
  c=$(fx_pi_runaway 20.5k)
  d=$(fx_pi_runaway 21.2k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|runaway: leaked model sentinel in pane %9"* ]]
}

@test "stall-watch: D7 stays silent when the sentinel persists but tokens do not grow" {
  a=$(fx_pi_runaway 19.0k)
  stall_sampler "$a" "$a" "$a" "$a" "$a"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores a normal busy pi frame, however fast tokens climb" {
  a=$(fx_pi_busy 19.0k)
  b=$(fx_pi_busy 25.0k)
  c=$(fx_pi_busy 31.0k)
  d=$(fx_pi_busy 37.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores sentinel strings inside a claude tool-result block" {
  a=$(fx_claude_tool_sentinel 10.0k)
  b=$(fx_claude_tool_sentinel 12.0k)
  c=$(fx_claude_tool_sentinel 14.0k)
  d=$(fx_claude_tool_sentinel 16.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores sentinel strings inside pi tool output" {
  a=$(fx_pi_tool_sentinel 19.0k)
  b=$(fx_pi_tool_sentinel 21.0k)
  c=$(fx_pi_tool_sentinel 23.0k)
  d=$(fx_pi_tool_sentinel 25.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores a long legitimate claude answer" {
  a=$(fx_claude_long_answer 10.0k)
  b=$(fx_claude_long_answer 13.0k)
  c=$(fx_claude_long_answer 16.0k)
  d=$(fx_claude_long_answer 19.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 clears when the sentinel leaves the frame" {
  a=$(fx_pi_runaway 19.0k)
  b=$(fx_pi_runaway 19.8k)
  c=$(fx_pi_runaway 20.5k)
  d=$(fx_pi_runaway 21.2k)
  e=$(fx_pi_busy 21.5k)
  stall_sampler "$a" "$b" "$c" "$d" "$e" "$e"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "blocked|runaway:"* ]]
  [ "${lines[1]}" = "working|runaway: cleared" ]
}

# fx_claude_runaway <tokens> — sentinel streamed in the newest assistant block.
fx_claude_runaway() {
  frame_file "claude_runaway.$1" <<EOF
⏺ Read(adapters/core/crew.sh)
  ⎿  line one

     <|im_end|> after a blank line inside the tool block
> please do not mention <|endoftext|> in your answer
⏺ I'll trust inspection <｜end▁of▁thinking｜> dragon phoenix griffin kraken quark
✳ Perusing… (1m 2s · ↓ $1 tokens · thinking)
EOF
}

fx_claude_stale_sentinel() {
  frame_file "claude_stale.$1" <<EOF
> please do not mention <|endoftext|> in your answer
⏺ Bash(grep -n '<|im_end|>' crew.sh)
  ⎿  crew.sh:12:<|im_end|>

     <|im_end|> after a blank line
⏺ Done reading; updating the detector now.
✳ Perusing… (1m 2s · ↓ $1 tokens · thinking)
EOF
}

@test "stall-watch: D7 posts runaway: on a claude frame with a streamed sentinel" {
  a=$(fx_claude_runaway 10.0k)
  b=$(fx_claude_runaway 11.0k)
  c=$(fx_claude_runaway 12.0k)
  d=$(fx_claude_runaway 13.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: D7 ignores stale sentinels in input rows, tool headers and blank-split tool output" {
  a=$(fx_claude_stale_sentinel 10.0k)
  b=$(fx_claude_stale_sentinel 12.0k)
  c=$(fx_claude_stale_sentinel 14.0k)
  d=$(fx_claude_stale_sentinel 16.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 survives a sentinel frame with no token meter" {
  p=$(frame_file no_meter <<<'⏺ done <|im_end|>')
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D3 posts blocked/quiet: on a byte-identical pane in steady state" {
  # --max-life 8: D3 needs `quiet_for >= --idle 2`, and the run must still be
  # alive when it fires; at 4 one stretched pre-sample gap could exit the loop
  # first and starve the single expected event (#185, same as D0's fix above).
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|quiet: pane unchanged for "* ]]
}

@test "stall-watch: D5 flags a pane still a shell past --launch as launch-not-started" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD='printf fish'
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 2 --window 60 --stall 999 --idle 999 --dead 999 --max-life 6
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|stalled: launch-not-started"* ]]
}

@test "stall-watch: D5 clears itself when the engine appears late" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  # The engine appears from the 4th sample on, once D5 has posted.
  export CREW_STALL_PROC_CMD="[ \"\$(cat $SAMPLER_DIR/n)\" -ge 4 ] && printf claude || printf fish"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 1 --window 60 --stall 999 --idle 999 --dead 999 --max-life 9
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "blocked|stalled: launch-not-started"* ]]
  [ "${lines[1]}" = "working|stalled: launch-not-started cleared" ]
}

@test "stall-watch: D5 is silent when the engine is the pane's command" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD='printf .claude-wrapped'
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 1 --window 60 --stall 999 --idle 999 --dead 999 --max-life 5
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D5 is silent while a shell pane is inside --launch" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD='printf fish'
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 999 --window 60 --stall 999 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a finished turn waiting on a background shell posts nothing" {
  a=$(fx_bgwait_crunched)
  b=$(fx_bgwait_churned)
  stall_sampler "$a" "$a" "$a" "$b" "$b" "$b" "$b" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 2 --dead 2 --max-life 15
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a finished turn with no background shell still posts stalled:" {
  p=$(fx_bgwait_noshell)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: a live spinner under a stale done line is not a background-shell wait" {
  p=$(frame_file bgwait_spinner <<'EOF'
✻ Crunched for 1m 12s · done 11:16 AM · 1 shell still running
✻ Pondering… (3s)
──────────────────
❯
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: a background-shell wait past --bg-wait still reaches quiet:" {
  p=$(fx_bgwait_crunched)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --bg-wait 3 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|quiet: pane unchanged for "* ]]
}

@test "stall-watch: healthy subagent batch produces ZERO events" {
  # A1's measured false-positive driver, verbatim: meter present, clock rising,
  # parent token string static at 73.2k, live subagent row throughout. D2 is
  # vetoed by the row, D3 by the byte changes, D1 by the meter.
  # Every frame before GONE is byte-distinct ON PURPOSE: the detector
  # thresholds are wall-clock (`date +%s`) deltas, so one stretched
  # inter-sample gap satisfies `--idle 2` — a repeated final frame let D3 post
  # `quiet:` and flake this zero-events oracle under `bats --jobs` load (#185).
  a=$(fx_subbatch "26m 57s" "3m 29s")
  b=$(fx_subbatch "27m 12s" "3m 45s")
  c=$(fx_subbatch "27m 27s" "4m 0s")
  d=$(fx_subbatch "27m 42s" "4m 15s")
  e=$(fx_subbatch "27m 58s" "4m 30s")
  f=$(fx_subbatch "28m 13s" "4m 45s")
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  stall_sampler "$a" "$b" "$c" "$d" "$e" "$f" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 15
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: the subagent-row veto survives LC_ALL=C" {
  # C-5: a single-character bracket expression consumes one BYTE under LC_ALL=C,
  # so a non-multibyte-safe class would never match `◯` and D2 would lose its
  # only measured guard — silently.
  a=$(fx_subbatch "26m 57s" "3m 29s")
  b=$(fx_subbatch "27m 12s" "3m 45s")
  c=$(fx_subbatch "27m 27s" "4m 0s")
  # The sampler repeats its last frame, so the pane goes byte-static here where
  # test 3a's does not: --idle 3 against a 4s life keeps that from arming D3, so
  # a non-zero count can only mean the veto failed.
  stall_sampler "$a" "$b" "$c" "$c" "$c"
  LC_ALL=C CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 3 --dead 999 --max-life 4
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a meter with rising tokens produces ZERO events" {
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "26.1k")
  c=$(fx_meter "5m 59s" "27.4k")
  d=$(fx_meter "6m 14s" "28.8k")
  # GONE stops the sampler repeating its last frame: an un-GONE'd tail would
  # sample `d` twice, and one stretched wall-clock gap meets `--idle 2` and
  # lets D3 post, breaking the zero-events oracle (#185). The run exits on
  # --max-life 5 with fails=1 here, which is fine — the oracle only needs the
  # detectors silent.
  stall_sampler "$a" "$b" "$c" "$d" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a working heartbeat damps D2" {
  CREW_ID=c1 run_crew status worker:feat/x working
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  # A heartbeat buys the window exactly one tick, so the tick has to be wider
  # than the one-second truncation slop for the difference to be observable:
  # without the damping the second sample fires at 3s, with it nothing fires
  # before the 6s life runs out.
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 3 --window 0 --idle 3 --dead 999 --max-life 6
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a self-reported blocked suppresses every detector" {
  CREW_ID=c1 run_crew status worker:feat/x blocked "which approach?"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 2 --dead 999 --max-life 4
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a prompt frame in scrollback produces ZERO events, both footers" {
  for fx in fx_prompt_scrollback fx_select_scrollback; do
    rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
    p=$($fx)
    stall_sampler "$p" "$p" "$p" "$p"
    CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
      --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
    run bash -c "bus | grep -c . || true"
    [ "$output" = "0" ]
  done
}

@test "stall-watch: codex leaves an uncaptured Claude prompt frame to startup silence" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  # --max-life 8: the single expected stalled: line must fire before the
  # top-of-loop exit; at 4 a stretched pre-sample gap could starve it (#185,
  # same as D0's fix above).
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  # The static Claude frame is not classifiable for Codex, so it falls to D0.
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
  run bash -c "bus | grep -c 'prompt:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: codex Hooks need review capture posts blocked/prompt:" {
  p=$(fx_codex_hooks_review)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|prompt: interactive prompt in pane %9"* ]]
}

@test "stall-watch: codex reordered Hooks need review lines fall through to startup silence" {
  p=$(fx_codex_hooks_review_reordered)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: a missing --engine enables no signature detector" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: one post per episode, then a working clearance that re-arms" {
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  p=$(fx_prompt_trust)
  q=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$q" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 20
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 3 ]
  [[ "${lines[0]}" == blocked\|prompt:* ]]
  [ "${lines[1]}" = "working|prompt: cleared" ]
  [[ "${lines[2]}" == blocked\|prompt:* ]]
}

@test "stall-watch: a quiet: episode escalates to failed after --dead" {
  # --max-life 15 keeps the `blocked`→`failed` (+--dead 2) sequence inside the
  # run even when stretched samples push the D3 fire late (#185).
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 15
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == blocked\|quiet:* ]]
  [[ "${lines[1]}" == "failed|dead: quiet: unchanged for "* ]]
}

@test "stall-watch: a clearance before --dead cancels the escalation" {
  p=$(fx_idle_box)
  q=$(fx_meter "5m 29s" "25.0k")
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  stall_sampler "$p" "$p" "$p" "$q" "$q" "$q" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 3 --max-life 15
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet: cleared' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a prompt: episode NEVER escalates (C-1)" {
  # An unanswered answerable question is waiting work, not death. Escalating it
  # would reproduce session 3 with a 30-minute delay.
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 20
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'prompt:' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a prompt: episode held past --idle and --dead still never escalates or gets superseded by quiet:" {
  # C-1's real shape. A frozen prompt frame is byte-identical by construction, so
  # it satisfies D3 too — and the C-1 test above pins --idle above --max-life, so
  # it never reaches that. Here --idle and --dead are both crossed while the
  # prompt: episode is open: quiet: must not supersede it, and the timer must not
  # launder it into a failed.
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 8
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet:' || true"
  [ "$output" = "0" ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == blocked\|prompt:* ]]
}

@test "stall-watch: INV-W0 a — a bare-id worker terminal state mutes a suffixed watchdog" {
  CREW_ID=c1 run_crew status worker:feat/x done
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1786338213-54181" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 b — a suffixed-id worker terminal state mutes a bare watchdog" {
  seed_raw "worker:feat/x#s99" done "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 c — a suffixed heartbeat damps a bare-invoked watchdog" {
  seed_raw "worker:feat/x#s99" working "" ""
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 --engine claude \
    --grace 0 --interval 3 --window 0 --idle 3 --dead 999 --max-life 6
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 d — writes carry the invoking session id, roster still shows ONE row" {
  CREW_ID=c1 run_crew status worker:feat/x working
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1786338213-54181" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x#s1786338213-54181" ]
  CREW_ID=c1 run run_crew roster c1
  run bash -c "printf '%s' '$output' | jq 'length'"
  [ "$output" = "1" ]
}

@test "stall-watch: a bare invocation writes a bare from" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x" ]
}

@test "stall-watch: a '#' branch keeps its full branch key" {
  seed_raw "worker:feat/a#zz#s2-2" done "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/a#b#s1-1" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/a#b#s1-1" ]
}

@test "stall-watch: a watchdog steps aside once a newer session posts on its branch" {
  seed_raw "worker:feat/x#s2-2" working "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1-1" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: an older session's post does not disarm a newer watchdog" {
  seed_raw "worker:feat/x#s1-1" working "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s2-2" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x#s2-2" ]
}

@test "stall-watch: a resumed worker's stale watchdog cannot capture a branch-only reply" {
  t=$((($(date +%s) - 60) * 1000))
  seed_start dispatch s1-1 "$t"
  seed_raw "worker:feat/x#s1-1" working "" "" "$((t + 1000))"
  seed_start resume s2-2 "$((t + 2000))"
  seed_raw "worker:feat/x#s2-2" working resumed ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1-1" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  CREW_ID=c1 run_crew reply "worker:feat/x" "resume-directive"
  run run_crew inbox "worker:feat/x#s2-2" c1
  [[ "$output" == *"resume-directive"* ]]
}

@test "stall-watch: INV-W1 — a terminal state already on the bus produces zero writes" {
  CREW_ID=c1 run_crew status worker:feat/x failed "gate red"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W3 — a second watchdog does not re-post an open episode" {
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %9 — worker is waiting on input nobody can give" watchdog
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: C-3 — a previous run's terminal state does not mute a new watchdog" {
  seed_raw worker:feat/x failed "dead: quiet: unchanged for 1800s" watchdog "$((($(date +%s) - 3600) * 1000))"
  seed_raw worker:feat/x exited "" "" "$((($(date +%s) - 3500) * 1000))"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: pr_open does not exit the watchdog, done does" {
  CREW_ID=c1 run_crew status worker:feat/x pr_open "" https://example.com/pr/1
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: survives 2 sample failures and exits after the 3rd" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" GONE GONE "$p" "$p" "$p" GONE GONE GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 30
  [ "$status" -eq 0 ]
  # It survived the pair at samples 2-3 (the prompt confirmed on 4+5 and posted),
  # then exited on the triple rather than running to --max-life 30.
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: writes zero msg events" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c '\"kind\":\"msg\"' || true"
  [ "$output" = "0" ]
  CREW_ID=c1 run run_crew inbox "dispatcher:c1"
  [ -z "$output" ]
}

@test "stall-watch: rejects an unknown flag" {
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --bogus 1
  [ "$status" -eq 1 ]
  [[ "$output" == *"unknown arg"* ]]
}

@test "burn map conformance: the doc slice names every classed rung" {
  # Copied from the Model map/Burn classes table, so it makes drift loud
  # rather than impossible — same tolerance as dispatch.bats's precedent
  # "tier map conformance" test. kimi-k3* is deliberately excluded: the burn
  # doc does not class it and _burn_weight falls it through to "" on purpose.
  # The cursor rungs listed are the non-fast defaults; `-fast` is a price
  # multiplier the sibling behavioural test below pins.
  doc="$BATS_TEST_DIRNAME/../adapters/core/protocols/dispatch-orchestration.md"
  doc_slice="$(sed -n '/^## Model map/,/^### Tier map/p' "$doc")"
  for token in opus sonnet haiku fable composer-2.5 \
    gpt-5.6-luna gpt-5.6-terra gpt-5.6-sol \
    grok-4.7-low grok-4.7-medium grok-4.7-high \
    claude-fable-5; do
    grep -qF "$token" <<<"$doc_slice" || {
      printf 'token %s missing from the Model map/Burn classes doc slice\n' "$token" >&2
      return 1
    }
  done
}

# ---------------------------------------------------------------------------
# watch --crew harness
# ---------------------------------------------------------------------------

# crew_dir [id] — the per-crew state dir `watch` and `stream` share.
crew_dir() {
  printf '%s' "$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/${1:-c1}"
}

# poll_for <tries> <cmd…> — run cmd every 0.1s until it succeeds, at most
# <tries> times. Every wait below is a bounded poll rather than a fixed sleep:
# a state change that already happened costs nothing, and one that never
# happens fails the test instead of hanging a --jobs 16 run.
poll_for() {
  local tries="$1" i=0
  shift
  while [ "$i" -lt "$tries" ]; do
    "$@" && return 0
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# Predicates for poll_for.
proc_gone() { ! kill -0 "$1" 2>/dev/null; }

# The lock belongs to the park, so a park killed mid-sleep has to drop it: bash
# ran its EXIT trap for TERM, INT and HUP, and `crew stream` stops and retries
# its inner watch that way. A leaked watch.lock.d would refuse every later watch
# of this crew until the dead pid was reclaimed.
@test "watch: a TERM mid-park releases the lock and the next watch starts at once" {
  cdir="$(crew_dir c1)"

  # Not `run_crew … &`: backgrounding the helper function backgrounds a subshell
  # of its own, and its pid is not the one the park writes in the lock.
  "$CREW" watch --crew c1 --timeout 30 --interval 1 >"$BATS_TEST_TMPDIR/park.out" 2>&1 &
  local pid=$!
  poll_for 100 test -f "$cdir/watch.lock.d/pid"
  # The pid the arm wrote is `$$`, and the exec'd Go binary keeps it.
  [ "$(cat "$cdir/watch.lock.d/pid")" = "$pid" ]

  kill -TERM "$pid"
  local rc=0
  wait "$pid" || rc=$?
  [ "$rc" -eq 143 ]

  [ ! -d "$cdir/watch.lock.d" ]
  [ ! -e "$cdir/cursor" ]

  # The group-shaped signal: a terminal's Ctrl-C reaches the park and its
  # in-flight `sleep` child, and `set -e` exited on the killed child, so the
  # status is the signal's. Killing the child alone is the same shape.
  "$CREW" watch --crew c1 --timeout 60 --interval 5 >/dev/null 2>&1 &
  local pid2=$!
  poll_for 100 test -f "$cdir/watch.lock.d/pid"
  local wpid
  wpid="$(cat "$cdir/watch.lock.d/pid")"
  poll_for 50 pkill -TERM -P "$wpid" sleep
  local rc2=0
  wait "$pid2" || rc2=$?
  [ "$rc2" -eq 143 ]
  [ ! -d "$cdir/watch.lock.d" ]

  # The next watch starts rather than refusing.
  CREW_ID=c1 run_crew status worker:feat/x done
  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.state == "done"' <<<"$output"
  [ "$status" -eq 0 ]
}

# --- reap: stacked-base compatibility (#274) ---

@test "reap: keeps an open-PR parent worktree with a stacked child in place" {
  git commit -q --allow-empty -m init
  git branch feat/10-parent
  parent_wt="$BATS_TEST_TMPDIR/parent-wt"
  git worktree add -q "$parent_wt" feat/10-parent

  git branch feat/11-child
  child_wt="$BATS_TEST_TMPDIR/child-wt"
  git worktree add -q "$child_wt" feat/11-child
  printf 'base: feat/10-parent\n' >"$child_wt/WORKER_TASK.md"

  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'OPEN' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/10-parent" done "" "https://github.com/o/r/pull/30"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/10-parent — PR OPEN"* ]]
  [ -d "$parent_wt" ]
  [ -d "$child_wt" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: reaps a merged parent without touching its stacked child" {
  git commit -q --allow-empty -m init
  git branch feat/10-parent
  parent_wt="$BATS_TEST_TMPDIR/parent-wt2"
  git worktree add -q "$parent_wt" feat/10-parent
  parent_wt=$(cd "$parent_wt" && pwd -P)
  echo unique >"$parent_wt/work.txt"
  git -C "$parent_wt" add work.txt
  git -C "$parent_wt" commit -q -m "parent work"

  git branch feat/11-child
  child_wt="$BATS_TEST_TMPDIR/child-wt2"
  git worktree add -q "$child_wt" feat/11-child
  printf 'base: feat/10-parent\n' >"$child_wt/WORKER_TASK.md"
  child_task_before="$(cat "$child_wt/WORKER_TASK.md")"

  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/10-parent)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/10-parent" done "" "https://github.com/o/r/pull/31"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/10-parent (MERGED)"* ]]
  [ ! -d "$parent_wt" ]
  run ! git show-ref --verify --quiet refs/heads/feat/10-parent

  [ -d "$child_wt" ]
  git show-ref --verify --quiet refs/heads/feat/11-child
  [ "$(cat "$child_wt/WORKER_TASK.md")" = "$child_task_before" ]
  jq -e 'select(.kind=="reap" and .branch=="feat/10-parent")' "$log" >/dev/null
}

@test "stall-watch: keeps the window after a watchdog-posted failed on a static non-claude pane" {
  _release_setup
  seed_raw "worker:feat/x#s1-1" failed "dead: quiet: no output" watchdog "$(($(date +%s) * 1000 + 500))"
  p=$(fx_done_idle)
  stall_sampler "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 5 --release 1
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: harness-injected user turns (task-notification, isMeta) do not count as a human" {
  _human_setup
  export CLAUDE_CONFIG_DIR="$BATS_TEST_TMPDIR/claude"
  tdir="$CLAUDE_CONFIG_DIR/projects/$(printf '%s' "$human_wt" | sed 's/[^A-Za-z0-9]/-/g')"
  mkdir -p "$tdir"
  newer=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%S.000Z)
  printf '%s\n' \
    "{\"type\":\"user\",\"timestamp\":\"$newer\",\"message\":{\"content\":\"<task-notification>done</task-notification>\"}}" \
    "{\"type\":\"user\",\"isMeta\":true,\"timestamp\":\"$newer\",\"message\":{\"content\":\"Base directory for this skill\"}}" >"$tdir/s.jsonl"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

# start_fleet_stream <args…> — launch `crew fleet stream` as a REAL process.
# Never through run_crew: backgrounding a shell function makes $! the subshell's
# pid, so the lock's pid comparison and every kill below would name the wrong
# process, and </dev/null keeps the lane off bats' own pipes. It adds no
# --reap-every, which the fleet lane refuses, and its lock lives under
# $XDG_STATE_HOME, not in a repo. The state and sweep-root defaults keep a test
# that forgets them off the real home.
start_fleet_stream() {
  export XDG_STATE_HOME="${XDG_STATE_HOME:-$BATS_TEST_TMPDIR/state}"
  export CREW_SWEEP_ROOTS="${CREW_SWEEP_ROOTS:-$BATS_TEST_TMPDIR/fleetroot}"
  FLEET_N=$((${FLEET_N:-0} + 1))
  FLEET_OUT="$BATS_TEST_TMPDIR/fleet-stream.$FLEET_N.out"
  FLEET_ERR="$BATS_TEST_TMPDIR/fleet-stream.$FLEET_N.err"
  local args=("$@") i crew_id=""
  for ((i = 0; i < ${#args[@]}; i++)); do
    [ "${args[i]}" = --crew ] && crew_id="${args[i + 1]}"
  done
  FLEET_LOCK="$XDG_STATE_HOME/crew/fleet-stream/$crew_id/stream.lock.d"
  "$CREW" fleet stream "${args[@]}" >"$FLEET_OUT" 2>"$FLEET_ERR" </dev/null &
  FLEET_PID=$!
  FLEET_PIDS="${FLEET_PIDS:-} $FLEET_PID"
}

# stop_fleet_stream — TERM every lane this test started and wait for the lock
# dir, the last thing a shutdown removes, then reap them.
stop_fleet_stream() {
  local fpid
  [ -n "${FLEET_PIDS:-}" ] || return 0
  for fpid in $FLEET_PIDS; do
    kill -TERM "$fpid" 2>/dev/null || true
  done
  poll_for 60 test ! -d "$FLEET_LOCK" || {
    for fpid in $FLEET_PIDS; do
      kill -KILL "$fpid" 2>/dev/null || true
    done
  }
  for fpid in $FLEET_PIDS; do
    wait "$fpid" 2>/dev/null || true
  done
  FLEET_PIDS=""
  FLEET_PID=""
}

fleet_locked() { [ -d "$FLEET_LOCK" ]; }

# seed_fleet_repo <dir> — a git repo with a c1 dispatch and a working worker.
seed_fleet_repo() {
  mkdir -p "$1/.git/crew"
  git init -q -b main "$1"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", engine:"claude", model:"sonnet", tier:"standard", effort:"medium", title:"t"}' \
    >>"$1/.git/crew/events.jsonl"
  jq -nc '{ts:1500, crew_id:"c1", from:"worker:feat/x", to:"dispatcher:c1", kind:"status", body:{state:"working"}}' \
    >>"$1/.git/crew/events.jsonl"
}

# post_status <dir> <state> — a status row for the worker seed_fleet_repo made.
post_status() {
  jq -nc --argjson ts "$(($(date +%s) * 1000))" --arg st "$2" \
    '{ts:$ts, crew_id:"c1", from:"worker:feat/x", to:"dispatcher:c1", kind:"status", body:{state:$st, detail:"PR"}}' \
    >>"$1/.git/crew/events.jsonl"
}

@test "fleet stream: a hard-killed lane's orphaned watch does not swallow the next status" {
  root="$BATS_TEST_TMPDIR/fleetroot"
  seed_fleet_repo "$root/alpha"
  seed_fleet_repo "$root/beta"
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  export XDG_STATE_HOME="$BATS_TEST_TMPDIR/state"
  export CREW_SWEEP_ROOTS="$root"
  outside="$BATS_TEST_TMPDIR/outside"
  mkdir -p "$outside"
  cd "$outside"

  start_fleet_stream --crew c1 --park 30 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1 --rediscover 1
  poll_for 100 fleet_locked
  post_status "$root/beta" pr_open
  poll_for 200 grep -q '"state":"pr_open"' "$FLEET_OUT"
  lane1_out="$FLEET_OUT"
  # The batch is printed during --coalesce while no watch holds the lock, so wait
  # for the loop to re-park its next watch: killing then orphans a live watch.
  poll_for 200 test -d "$root/beta/.git/crew/crews/c1/watch.lock.d"
  [ -d "$root/beta/.git/crew/crews/c1/watch.lock.d" ]

  # Hard-kill the lane only. The parked inner watch orphans; before the fix it
  # kept beta's watch lock and swallowed the next status.
  kill -KILL "$FLEET_PID" 2>/dev/null || true
  wait "$FLEET_PID" 2>/dev/null || true

  start_fleet_stream --crew c1 --park 30 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1 --rediscover 1
  poll_for 100 fleet_locked
  post_status "$root/beta" done
  poll_for 400 grep -q '"state":"done"' "$FLEET_OUT"

  # The restarted lane delivered beta's next status, exactly once; the killed
  # lane replayed the backlog and stopped at pr_open.
  run jq -s -e '[.[] | select(.repo == "beta" and .events != null) | .events[] | select(.body.state == "done")] | length == 1' "$FLEET_OUT"
  [ "$status" -eq 0 ]
  run jq -s -e '[.[] | select(.repo == "beta" and .events != null) | .events[] | select(.body.state == "done")] | length == 0' "$lane1_out"
  [ "$status" -eq 0 ]
  stop_fleet_stream
}

@test "reap: a commondir planted in the main git dir refuses before any gh call and keeps the worktree (#993)" {
  git commit -q --allow-empty -m init
  git branch feat/993-e
  wt_path="$BATS_TEST_TMPDIR/993-e-wt"
  git worktree add -q "$wt_path" feat/993-e
  stub_tmux "" ""
  printf '#!/bin/sh\nprintf "gh %%s\\n" "$*" >>%q\nprintf "MERGED\\n"\n' "$BATS_TEST_TMPDIR/gh.log" >"$STUB_DIR/gh"
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/993-e" done "" "https://github.com/o/r/pull/993"
  plant_commondir
  CREW_ID=c1 run run_crew reap
  [ "$status" -ne 0 ]
  [[ "$output" == *"refusing git: "* ]]
  [[ "$output" == *"$PLANTED_COMMONDIR"* ]]
  [[ "$output" == *"redirects the git common dir"* ]]
  [ -d "$wt_path" ]
  [ ! -e "$BATS_TEST_TMPDIR/gh.log" ]
  [ ! -e "$SENTINEL" ]
}
