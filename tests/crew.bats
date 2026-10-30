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
  # await and the hold paths run on a virtual clock: their waits
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

# anchor_record_path <wt> — the dispatcher anchor record path for <wt>, as the
# shared worktree-git lib derives it (#556).
anchor_record_path() {
  printf '%s/crew/worktrees/%s\n' "$XDG_DATA_HOME" \
    "$(printf %s "$(realpath -e "$1")" | sha256sum | cut -c1-64)"
}

# write_anchor_record <wt> — plant the record `dispatch` would have written for
# a worktree, so reap has something to prune.
write_anchor_record() {
  local path
  path="$(anchor_record_path "$1")"
  mkdir -p "$(dirname "$path")"
  printf 'record\n' >"$path"
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

@test "reap: rejects an unknown flag" {
  CREW_ID=c1 run run_crew reap --bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"reap takes --quiet, --dry-run, --no-wait, --idle S and --discard BRANCH"* ]]
}

@test "reap: is a no-op when the bus has no events" {
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
}

@test "reap: --dry-run removes no worktree" {
  # A candidate only enters reap's sweep when `from` carries the real
  # `worker:<branch>` convention (WORKER_PROTOCOL.md: `n worker:<branch> done`)
  # AND a worktree for that branch actually exists AND its PR reads back as
  # merged/closed — a bare `status worker done` (no prefix, no worktree, no
  # PR) is filtered out before reap ever calls gh or wt, so the stub log is
  # never created and the original assertion errors on a missing file rather
  # than proving anything about --dry-run. Wire up a real candidate so the
  # test exercises the actual dry-run gate.
  git commit -q --allow-empty -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
echo MERGED
exit 0
EOF
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/reap-me" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/reap-me"* ]]
  run grep -c 'remove' "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "reap: a sessioned worker becomes a candidate" {
  git commit --allow-empty -q -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
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
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [[ "$output" == *"would reap feat/reap-me"* ]]
}

@test "reap: dry-run reports leftover processes whose cwd is in the worktree" {
  git commit --allow-empty -q -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-proc-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
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
  export CREW_REAP_PROC_CMD="printf '4242\t$wt_path/subdir\n9999\t/elsewhere\n'"
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [[ "$output" == *"would kill pid 4242"* ]]
  [[ "$output" != *"would kill pid 9999"* ]]
}

@test "reap: a real run kills (best-effort) leftover worktree processes" {
  # kill is best-effort: the fake pid below does not exist, so `kill 4242 || true`
  # succeeds via the || true and the say still fires. The assertion is the wiring:
  # the step runs before the removal and names the right pid, never the /elsewhere one.
  git commit --allow-empty -q -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-proc-real-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
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
  export CREW_REAP_PROC_CMD="printf '4242\t$wt_path/subdir\n9999\t/elsewhere\n'"
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [[ "$output" == *"killed pid 4242"* ]]
  [[ "$output" != *"killed pid 9999"* ]]
}

@test "reap: keeps a worktree whose pane runs the wrapped engine" {
  git commit --allow-empty -q -m init
  git branch feat/reap-live
  wt_path="$BATS_TEST_TMPDIR/reap-live-wt"
  git worktree add -q "$wt_path" feat/reap-live
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
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
  # 4-field panes query, no frame planted: an unreadable/empty capture keeps
  # too (defensive rule), so this still asserts the same substring.
  stub_tmux_frames "" "$(printf '@1\t%%1\t.claude-wrapped\t%s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/reap-live#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [[ "$output" == *"an engine is still running there"* ]]
  [ -d "$wt_path" ]
  run ! grep -q remove "$STUB_LOG"
}

# Same exact-path requirement as the roster sibling-path test above, exercised
# through reap's own exact-path engine check.
@test "reap: a sibling-path engine pane does not keep the worktree" {
  git commit --allow-empty -q -m init
  git branch feat/reap-sib
  wt_path="$BATS_TEST_TMPDIR/reap-sib-wt"
  git worktree add -q "$wt_path" feat/reap-sib
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
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
  stub_tmux_frames "" "$(printf '@1\t%%1\t.claude-wrapped\t%s-sibling\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/reap-sib#s1-1" done "" "https://github.com/o/r/pull/1"
  CREW_ID=c1 run run_crew reap
  [[ "$output" != *"an engine is still running there"* ]]
}

@test "reap: releases a terminal session's window past --idle, keeping the worktree" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
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

_live_claude_done_setup() {
  git commit --allow-empty -q -m init
  git branch feat/idle-done-live
  wt_path="$BATS_TEST_TMPDIR/idle-done-live-wt"
  git worktree add -q "$wt_path" feat/idle-done-live
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  export CREW_RELEASE_GAP=0
  stub_tmux_frames "$(printf '@23\tsage\t%s\n' "$wt_path")" "" "$(printf '@23\t%%33\t.claude-wrapped\n')"
}

@test "reap: releases a done window once its claude pane is provably idle on two samples" {
  _live_claude_done_setup
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  set_frame %33 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "reap: keeps a done window whose claude pane shows a live turn, at the default flags" {
  _live_claude_done_setup
  set_frame %33 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✳ Perusing… (1m2s · ↓ 3.1k tokens · thinking more with high effort)
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  old_ts=$((($(date +%s) - 400) * 1000))
  jq -nc --argjson ts "$old_ts" '{ts:$ts, crew_id:"c1", from:"worker:feat/idle-done-live#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-done-live — pane busy"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose pane changes between the two samples" {
  _live_claude_done_setup
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  set_frame %33 <<'EOF'
✻ Churned for 36s · done 11:20 AM
❯
EOF
  set_frame %33.2 <<'EOF'
✻ Churned for 37s · done 11:20 AM
❯
EOF
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %33 is still changing"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: never releases a watchdog-posted failed window" {
  _live_claude_done_setup
  set_frame %33 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  seed_raw "worker:feat/idle-done-live#s1-1" failed "dead: quiet: no output" watchdog "$((($(date +%s) - 400) * 1000))"
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" != *"released"* ]]
  [[ "$output" != *"pane busy"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose pane cannot be read" {
  _live_claude_done_setup
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %33 is unreadable"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose non-claude engine pane shows a prompt" {
  _live_claude_done_setup
  stub_tmux_frames "$(printf '@23\tsage\t%s\n' "$wt_path")" "" "$(printf '@23\t%%33\tpi\n')"
  set_frame %33 <<'EOF'
  2. Gate everything on 3.8
 Enter to select · ↑/↓ to navigate · Esc to cancel
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %33 is not idle (pi frame: prompt on screen)"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a second busy engine pane in the window keeps it" {
  _live_claude_done_setup
  stub_tmux_frames "$(printf '@23\tsage\t%s\n' "$wt_path")" "" "$(printf '@23\t%%33\t.claude-wrapped\n@23\t%%34\t.claude-wrapped\n')"
  set_frame %33 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  set_frame %34 <<'EOF'
✳ Perusing… (1m2s · ↓ 3.1k tokens · thinking more with high effort)
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %34"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: default grace releases a done window older than it and keeps a younger one" {
  git commit --allow-empty -q -m init
  git branch feat/old-done
  git branch feat/new-done
  old_wt="$BATS_TEST_TMPDIR/old-done-wt"
  new_wt="$BATS_TEST_TMPDIR/new-done-wt"
  git worktree add -q "$old_wt" feat/old-done
  git worktree add -q "$new_wt" feat/new-done
  old_wt=$(cd "$old_wt" && pwd -P)
  new_wt=$(cd "$new_wt" && pwd -P)
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n@24\tbird\t%s\n' "$old_wt" "$new_wt")" "$(printf '@23\t%%33\tfish\n@24\t%%34\tfish\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  old_ts=$((($(date +%s) - 400) * 1000))
  new_ts=$((($(date +%s) - 100) * 1000))
  jq -nc --argjson ts "$old_ts" '{ts:$ts, crew_id:"c1", from:"worker:feat/old-done#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  jq -nc --argjson ts "$new_ts" '{ts:$ts, crew_id:"c1", from:"worker:feat/new-done#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  run ! grep -q 'kill-window -t @24' "$STUB_LOG"
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

@test "reap: keeps a done window that is visible in an attached tmux client" {
  _human_setup
  printf '@23\t1\t1\t%s\n' "$(($(date +%s) - 9999))" >"$STUB_DIR/human.txt"
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/human — human present: @23 is visible in an attached tmux client"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a done window that is active but in a detached session is released" {
  _human_setup
  printf '@23\t1\t0\t%s\n' "$(($(date +%s) - 9999))" >"$STUB_DIR/human.txt"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "reap: keeps a done window with activity inside the grace" {
  _human_setup
  printf '@23\t0\t0\t%s\n' "$(($(date +%s) - 5))" >"$STUB_DIR/human.txt"
  CREW_ID=c1 run run_crew reap --idle 60
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose transcript has a user turn newer than the status" {
  _human_setup
  export CLAUDE_CONFIG_DIR="$BATS_TEST_TMPDIR/claude"
  tdir="$CLAUDE_CONFIG_DIR/projects/$(printf '%s' "$human_wt" | sed 's/[^A-Za-z0-9]/-/g')"
  mkdir -p "$tdir"
  newer=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%S.000Z)
  printf '%s\n' \
    '{"type":"user","timestamp":"2020-01-01T00:00:00.000Z","message":{"content":"start"}}' \
    "{\"type\":\"user\",\"timestamp\":\"$newer\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}" >"$tdir/s.jsonl"
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"human present: the engine transcript has a user turn newer than the status"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: an old user turn and a newer tool result do not count as a human" {
  _human_setup
  export CLAUDE_CONFIG_DIR="$BATS_TEST_TMPDIR/claude"
  tdir="$CLAUDE_CONFIG_DIR/projects/$(printf '%s' "$human_wt" | sed 's/[^A-Za-z0-9]/-/g')"
  mkdir -p "$tdir"
  newer=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%S.000Z)
  printf '%s\n' \
    '{"type":"user","timestamp":"2020-01-01T00:00:00.000Z","message":{"content":"start"}}' \
    "{\"type\":\"user\",\"timestamp\":\"$newer\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"content\":\"ok\"}]}}" >"$tdir/s.jsonl"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "reap: keeps an idle exited window when an engine is still live" {
  git commit --allow-empty -q -m init
  git branch feat/idle-exited-live
  wt_path="$BATS_TEST_TMPDIR/idle-exited-live-wt"
  git worktree add -q "$wt_path" feat/idle-exited-live
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\t.claude-wrapped\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-exited-live#s1-1" exited
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-exited-live — exited but an engine is still running there"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
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
  run ! grep -q 'kill-window' "$STUB_LOG"
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
  run ! grep -q 'kill-window' "$STUB_LOG"
}

# One worktree hosts at most one live window, so idle release must collapse the
# branch to its NEWEST session before testing terminality. An older `done`
# session must not release a window a newer `working` session still owns.
@test "reap: a newer working session masks an older done one on the same branch" {
  git commit --allow-empty -q -m init
  git branch feat/two-sess
  wt_path="$BATS_TEST_TMPDIR/two-sess-wt"
  git worktree add -q "$wt_path" feat/two-sess
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tclaude\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", from:"worker:feat/two-sess#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  jq -nc '{ts:2000, crew_id:"c1", from:"worker:feat/two-sess#s2-2", to:"dispatcher:c1", kind:"status", body:{state:"working"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: --dry-run only reports the release" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-me#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0 --dry-run
  [[ "$output" == *"would release @23"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: rejects a non-numeric --idle" {
  CREW_ID=c1 run run_crew reap --idle nope
  [ "$status" -eq 1 ]
  [[ "$output" == *"--idle"* ]]
}

@test "reap: removes the dispatched label from the issue a merged PR closes" {
  git commit -q --allow-empty -m init
  git branch feat/42-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/42-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '42' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/42-reap-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/42-reap-me" done "" "https://github.com/o/r/pull/7"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/42-reap-me"* ]]
  grep -q 'pr view https://github.com/o/r/pull/7 --json closingIssuesReferences' "$STUB_LOG"
  grep -q 'issue edit 42 --remove-label dispatched' "$STUB_LOG"
  # A merged PR's squash-merged branch is not an ancestor of main — reap
  # deletes it deliberately once gh confirms the merge (#194). The stub's
  # headRefOid answers the local tip so the delete guard passes.
  run ! git show-ref --verify --quiet refs/heads/feat/42-reap-me
}

@test "reap: releases the dispatched label from every issue a bundled PR closes (#615)" {
  git commit -q --allow-empty -m init
  git branch feat/42-reap-bundle
  wt_path="$BATS_TEST_TMPDIR/reap-bundle-wt"
  git worktree add -q "$wt_path" feat/42-reap-bundle
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' 42 43 44 ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/42-reap-bundle)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/42-reap-bundle" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/42-reap-bundle"* ]]
  grep -q 'issue edit 42 --remove-label dispatched' "$STUB_LOG"
  grep -q 'issue edit 43 --remove-label dispatched' "$STUB_LOG"
  grep -q 'issue edit 44 --remove-label dispatched' "$STUB_LOG"
}

@test "reap: a dispatched label-removal failure does not abort the sweep" {
  git commit -q --allow-empty -m init
  git branch feat/43-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt2"
  git worktree add -q "$wt_path" feat/43-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '43' ;;
*remove-label*) exit 1 ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/43-reap-me" done "" "https://github.com/o/r/pull/9"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/43-reap-me"* ]]
  [[ "$output" == *"could not remove the dispatched label from #43"* ]]
}

@test "reap: a failed closing-issue resolution is logged, not treated as no closing issue" {
  git commit -q --allow-empty -m init
  git branch feat/45-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt4"
  git worktree add -q "$wt_path" feat/45-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*)
  echo "gh: rate limited" >&2
  exit 1
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/45-reap-me" done "" "https://github.com/o/r/pull/11"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/45-reap-me"* ]]
  [[ "$output" == *"could not resolve closing issues for PR https://github.com/o/r/pull/11 (feat/45-reap-me)"* ]]
  run ! grep -q 'issue edit' "$STUB_LOG"
}

@test "reap: a PR with no closing issue reaps without attempting a label removal" {
  git commit -q --allow-empty -m init
  git branch feat/44-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt3"
  git worktree add -q "$wt_path" feat/44-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/44-reap-me" done "" "https://github.com/o/r/pull/10"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/44-reap-me"* ]]
  run ! grep -q 'issue edit' "$STUB_LOG"
}

@test "reap: a squash-merged PR is reaped by outcome — reap row, label, branch deletion" {
  # #194 Gap 1: this repo squash-merges, so a merged PR's branch is never an
  # ancestor of main. The removal never deletes the branch, and reap must judge
  # success by the observable outcome (the worktree is gone): write the reap
  # row, release the dispatched label, and delete the local branch deliberately.
  git commit -q --allow-empty -m init
  git branch feat/squash-me
  wt_path="$BATS_TEST_TMPDIR/squash-wt"
  git worktree add -q "$wt_path" feat/squash-me
  wt_path=$(cd "$wt_path" && pwd -P)
  # The squash shape from the wild: the branch really is ahead of main (its
  # commits are gone from the squash merge), which is why git reads it as
  # unmerged and why reap must delete it deliberately.
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "squash-me work"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/squash-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf 'wt %s\n' "$*" >>"$STUB_LOG"
exit 1
EOF
  chmod +x "$STUB_DIR/wt"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/squash-me" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/squash-me (MERGED)"* ]]
  grep -q 'pr view https://github.com/o/r/pull/8 --json closingIssuesReferences' "$STUB_LOG"
  grep -q 'issue edit 99 --remove-label dispatched' "$STUB_LOG"
  jq -e 'select(.kind=="reap" and .branch=="feat/squash-me")' "$log" >/dev/null
  [ ! -d "$wt_path" ]
  run ! git show-ref --verify --quiet refs/heads/feat/squash-me
  run ! grep -q '^wt ' "$STUB_LOG"
}

reap_merged_fixture() {
  git commit -q --allow-empty -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/reap-me
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "reap-me work"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/reap-me)" ;;
esac
exit 0
GH
  chmod +x "$STUB_DIR/gh"
  cat >"$STUB_DIR/wt" <<'WT'
#!/usr/bin/env bash
printf 'wt %s\n' "$*" >>"$STUB_LOG"
exit 1
WT
  chmod +x "$STUB_DIR/wt"
}

@test "reap: a merged pr_open worker gets one reaped status row; --dry-run writes none" {
  reap_merged_fixture
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" pr_open "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [ "$(jq -c 'select(.body.source? == "reap")' "$log" | wc -l)" -eq 0 ]
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [ "$(jq -c 'select(.body.source? == "reap")' "$log" | wc -l)" -eq 1 ]
  jq -e 'select(.kind=="status" and .body.source=="reap" and .body.state=="reaped" and .from=="worker:feat/reap-me#s1-1" and .crew_id=="c1" and (.body.detail | contains("#8") and contains("MERGED")))' "$log" >/dev/null
  CREW_ID=c1 run run_crew roster c1
  [ "$status" -eq 0 ]
  echo "$output" | jq -e 'any(.[]; .branch == "feat/reap-me" and .state == "reaped")' >/dev/null
}

reap_key_file() {
  # Mirrors the crew's per-key file naming: sanitized key, then its cksum.
  printf '%s.%s' "$(printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_')" "$(printf '%s' "$1" | cksum | cut -d' ' -f1)"
}

reap_seed_records() {
  local d=$1 b=$2 id=$3 f
  mkdir -p "$d/grants/$(dirname "$b")" "$d/protocol-dirs/$(dirname "$b")" "$d/leads/$(dirname "$b")"
  for f in grants protocol-dirs leads; do : >"$d/$f/$b"; done
  mkdir -p "$d/await" "$d/await.lock" "$d/phase-status" "$d/directive-delivery"
  : >"$d/await/$(reap_key_file "c1-$id")"
  : >"$d/await.lock/$(reap_key_file "c1-$id")"
  : >"$d/phase-status/$(reap_key_file "$id")"
  printf '0\n%s\n' "$id" >"$d/directive-delivery/$(printf '%s' "$id" | tr -c 'A-Za-z0-9._-' '_')"
}

reap_records_present() {
  local d=$1 b=$2 id=$3
  [ -e "$d/grants/$b" ] && [ -e "$d/protocol-dirs/$b" ] && [ -e "$d/leads/$b" ] &&
    [ -e "$d/await/$(reap_key_file "c1-$id")" ] && [ -e "$d/await.lock/$(reap_key_file "c1-$id")" ] &&
    [ -e "$d/phase-status/$(reap_key_file "$id")" ] &&
    [ -e "$d/directive-delivery/$(printf '%s' "$id" | tr -c 'A-Za-z0-9._-' '_')" ]
}

@test "reap: removes the reaped branch's records, keeps artifacts and other branches'" {
  reap_merged_fixture
  d="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" pr_open "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run_crew status "worker:feat/reap#s2-2" working
  reap_seed_records "$d" feat/reap-me "worker:feat/reap-me#s1-1"
  reap_seed_records "$d" feat/reap "worker:feat/reap#s2-2"
  reap_seed_records "$d" feat/reap-me-too "worker:feat/reap-me-too#s3-3"
  mkdir -p "$d/artifacts/feat/reap-me"
  : >"$d/artifacts/feat/reap-me/x.patch"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  me="worker:feat/reap-me#s1-1"
  [ ! -e "$d/grants/feat/reap-me" ]
  [ ! -e "$d/protocol-dirs/feat/reap-me" ]
  [ ! -e "$d/leads/feat/reap-me" ]
  [ ! -e "$d/await/$(reap_key_file "c1-$me")" ]
  [ ! -e "$d/await.lock/$(reap_key_file "c1-$me")" ]
  [ ! -e "$d/phase-status/$(reap_key_file "$me")" ]
  [ ! -e "$d/directive-delivery/$(printf '%s' "$me" | tr -c 'A-Za-z0-9._-' '_')" ]
  [ -e "$d/artifacts/feat/reap-me/x.patch" ]
  reap_records_present "$d" feat/reap "worker:feat/reap#s2-2"
  reap_records_present "$d" feat/reap-me-too "worker:feat/reap-me-too#s3-3"
}

@test "reap: keeps a directive-delivery file whose recorded id is a different session" {
  reap_merged_fixture
  d="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" pr_open "" "https://github.com/o/r/pull/8"
  mkdir -p "$d/directive-delivery"
  f="$d/directive-delivery/$(printf '%s' "worker:feat/reap-me#s1-1" | tr -c 'A-Za-z0-9._-' '_')"
  printf '0\n%s\n' "worker:feat/reap_me#s1-1" >"$f"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [ -e "$f" ]
}

@test "reap: a merged worker's anchor record is pruned (#556)" {
  git commit -q --allow-empty -m init
  git branch feat/anchor-me
  wt_path="$BATS_TEST_TMPDIR/anchor-wt"
  git worktree add -q "$wt_path" feat/anchor-me
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-me work"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  [ -e "$anchor" ]
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/anchor-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-me" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/anchor-me (MERGED)"* ]]
  [ ! -e "$anchor" ]
  [ ! -d "$wt_path" ]
}

@test "reap: a kept worker's anchor record survives (#556)" {
  git commit -q --allow-empty -m init
  git branch feat/anchor-kept
  wt_path="$BATS_TEST_TMPDIR/anchor-kept-wt"
  git worktree add -q "$wt_path" feat/anchor-kept
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-kept work"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'OPEN' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-kept" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/anchor-kept — PR OPEN"* ]]
  [ -e "$anchor" ]
  [ -d "$wt_path" ]
}

@test "reap: --dry-run names the anchor record it would prune and removes nothing (#556)" {
  git commit -q --allow-empty -m init
  git branch feat/anchor-dry
  wt_path="$BATS_TEST_TMPDIR/anchor-dry-wt"
  git worktree add -q "$wt_path" feat/anchor-dry
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-dry work"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/anchor-dry)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-dry" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --quiet --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/anchor-dry (MERGED) @ $wt_path"* ]]
  [[ "$output" == *"would prune record $anchor"* ]]
  [ -e "$anchor" ]
  [ -d "$wt_path" ]
}

@test "reap: a leftover anchor with no worktree is not an error (#556)" {
  git commit -q --allow-empty -m init
  # Simulates a record a pre-#556 reap left behind: the worktree is already
  # gone, but the keyed record survives. reap must neither fail nor invent a
  # reclaim for a worktree it cannot see.
  ghost="$BATS_TEST_TMPDIR/ghost-wt"
  anchor="$(printf '%s/crew/worktrees/%s\n' "$XDG_DATA_HOME" \
    "$(printf %s "$ghost" | sha256sum | cut -c1-64)")"
  mkdir -p "$(dirname "$anchor")"
  printf 'record\n' >"$anchor"
  stub_tmux "" ""
  CREW_ID=c1 run_crew status "worker:feat/ghost" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [ -e "$anchor" ]
}

@test "reap: an undeletable anchor record does not abort the reclaim (#556)" {
  # Review-fix guard: `rm -f` still fails on a directory (or EACCES/EROFS).
  # Under `set -e` an unguarded rm would abort reap right after the removal
  # deleted the worktree, before the reap row, branch delete and label
  # release — permanently losing that bookkeeping (no worktree next sweep).
  git commit -q --allow-empty -m init
  git branch feat/anchor-dir
  wt_path="$BATS_TEST_TMPDIR/anchor-dir-wt"
  git worktree add -q "$wt_path" feat/anchor-dir
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-dir work"
  anchor="$(anchor_record_path "$wt_path")"
  mkdir -p "$anchor"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/anchor-dir)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-dir" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/anchor-dir (MERGED)"* ]]
  [ ! -d "$wt_path" ]
  run ! git show-ref --verify --quiet refs/heads/feat/anchor-dir
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -e 'select(.kind=="reap" and .branch=="feat/anchor-dir")' "$log" >/dev/null
  [ -d "$anchor" ]
}

@test "reap: a merged branch whose local tip diverges from the PR head is kept" {
  # Review-fix guard: the branch is only force-deleted when the local tip is
  # exactly the merged PR head. A resumed run or a human that committed past
  # the merge would otherwise have those commits orphaned by -D. The worktree
  # is still reaped and the label still released — only the branch survives.
  git commit -q --allow-empty -m init
  git branch feat/diverged
  wt_path="$BATS_TEST_TMPDIR/diverged-wt"
  git worktree add -q "$wt_path" feat/diverged
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "diverged work"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' '0000000000000000000000000000000000000000' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/diverged" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/diverged (MERGED)"* ]]
  [[ "$output" == *"kept local branch feat/diverged — tip diverges from the merged PR head"* ]]
  grep -q 'issue edit 99 --remove-label dispatched' "$STUB_LOG"
  [ ! -d "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/diverged
}

@test "reap: a genuinely failed removal is still kept — no reap row, no label, no branch delete" {
  # #194 Gap 1 guard: a failed removal is judged by the worktree still being
  # present — the branch is reported kept, no reap row is written, the
  # dispatched label stays, and the local branch survives. A locked worktree
  # fails for real: a single --force still refuses it.
  git commit -q --allow-empty -m init
  git branch feat/stuck
  wt_path="$BATS_TEST_TMPDIR/stuck-wt"
  git worktree add -q "$wt_path" feat/stuck
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  git worktree lock "$wt_path"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/stuck" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/stuck — worktree removal failed"* ]]
  run ! grep -q 'remove-label' "$STUB_LOG"
  [ -d "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/stuck
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: REVIEW_NOTES.md and round plans are scaffold — trashed, not deleted, worktree reaped" {
  # #194 Gap 2: EVIDENCE_REVIEW.md's worktree-root REVIEW_NOTES.md ledger and
  # the superpowers writing-plans round artifacts (PLAN_ROUND4.md observed in
  # a real worker tree) are untracked scaffold "our own pipeline wrote". They
  # must neither read as uncommitted work nor survive as physical files that
  # block the removal — they are gtrash'd like WORKER_TASK.md, so a
  # post-mortem can still recover them.
  git commit -q --allow-empty -m init
  git branch feat/noted
  wt_path="$BATS_TEST_TMPDIR/noted-wt"
  git worktree add -q "$wt_path" feat/noted
  wt_path=$(cd "$wt_path" && pwd -P)
  : >"$wt_path/WORKER_TASK.md"
  : >"$wt_path/REVIEW_NOTES.md"
  : >"$wt_path/PLAN_ROUND4.md"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  # gtrash that really moves the file into a trash dir, so the test proves
  # "trashed, not deleted" — the files must survive for recovery.
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = put ]; then
  mkdir -p "$STUB_DIR/trash"
  mv "$2" "$STUB_DIR/trash/"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
  CREW_ID=c1 run_crew status "worker:feat/noted" done "" "https://github.com/o/r/pull/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/noted (MERGED)"* ]]
  [ -f "$STUB_DIR/trash/WORKER_TASK.md" ]
  [ -f "$STUB_DIR/trash/REVIEW_NOTES.md" ]
  [ -f "$STUB_DIR/trash/PLAN_ROUND4.md" ]
  [ ! -d "$wt_path" ]
}

@test "reap: a finished grid window (lead + role panes) is released, then its worktree reclaimed" {
  # #194 Gap 3: grid leads never send the {"final":true} release, so a
  # finished grid window — a lead pane and role panes, all engine commands —
  # idles in place. reap must reclaim it without a manual tmux kill-window:
  # the idle-release phase kills the window (engine or not, once the lead's
  # status is terminal), and the reclaim phase of the SAME pass removes the
  # worktree once the PR merged and no pane is live. The tmux stub is
  # stateful: kill-window removes the window's rows, exactly like the real
  # server, so the post-release engine scan sees no panes.
  git commit -q --allow-empty -m init
  git branch feat/grid-me
  wt_path="$BATS_TEST_TMPDIR/grid-wt"
  git worktree add -q "$wt_path" feat/grid-me
  wt_path=$(cd "$wt_path" && pwd -P)
  : >"$wt_path/WORKER_TASK.md"
  : >"$wt_path/REVIEW_NOTES.md"
  export CREW_RELEASE_GAP=0
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tcodex\n@23\t%%34\tcodex\n')"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
kill-window) : >"$STUB_DIR/wins.txt"; : >"$STUB_DIR/panes.txt" ;;
capture-pane) echo "pi: finished" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/grid-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = put ]; then
  mkdir -p "$STUB_DIR/trash"
  mv "$2" "$STUB_DIR/trash/"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
  CREW_ID=c1 run_crew status "worker:feat/grid-me#s1-1" done "" "https://github.com/o/r/pull/30"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  [[ "$output" == *"reaped feat/grid-me (MERGED)"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  [ -f "$STUB_DIR/trash/REVIEW_NOTES.md" ]
  [ ! -d "$wt_path" ]
  run ! git show-ref --verify --quiet refs/heads/feat/grid-me
  # A later pass is a clean no-op — the worktree is already gone.
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
}


@test "reap: an exited worker with a merged PR is reclaimed" {
  # #68 defect 1: the reclaim filter used to accept only "done", pinning the
  # worktree of every worker that ended via `exited` (compaction, context
  # limit, or a human taking the pane) forever even after its PR merged.
  git commit -q --allow-empty -m init
  git branch feat/exited-me
  wt_path="$BATS_TEST_TMPDIR/exited-wt"
  git worktree add -q "$wt_path" feat/exited-me
  stub_bin gh
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
  CREW_ID=c1 run_crew status "worker:feat/exited-me" exited "" "https://github.com/o/r/pull/20"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/exited-me"* ]]
}

@test "reap: a failed worker with an open PR is kept" {
  # The PR gate — not the worker's own terminal state — is what must decide
  # keep vs. reclaim; widening the state filter must not let an open PR through.
  git commit -q --allow-empty -m init
  git branch feat/failed-open
  wt_path="$BATS_TEST_TMPDIR/failed-open-wt"
  git worktree add -q "$wt_path" feat/failed-open
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
  CREW_ID=c1 run_crew status "worker:feat/failed-open" failed "" "https://github.com/o/r/pull/21"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/failed-open — PR OPEN"* ]]
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: a worktree dirty only with pipeline scaffold is reclaimed" {
  # #68 defect 2: SPEC.md/PLAN.md/DECOMPOSITION.md/WORKER_TASK.md and a
  # superpowers plan doc are all untracked files OUR OWN pipeline writes;
  # none of them should make a finished worktree read as dirty.
  git commit -q --allow-empty -m init
  git branch feat/scaffold-me
  wt_path="$BATS_TEST_TMPDIR/scaffold-wt"
  git worktree add -q "$wt_path" feat/scaffold-me
  : >"$wt_path/WORKER_TASK.md"
  : >"$wt_path/SPEC.md"
  : >"$wt_path/PLAN.md"
  : >"$wt_path/DECOMPOSITION.md"
  mkdir -p "$wt_path/docs/superpowers/plans"
  : >"$wt_path/docs/superpowers/plans/2026-08-29-scaffold.md"
  stub_bin gh
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
  CREW_ID=c1 run_crew status "worker:feat/scaffold-me" done "" "https://github.com/o/r/pull/22"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/scaffold-me"* ]]
}

@test "reap: a tracked modification keeps the worktree" {
  # A TRACKED change to one of the scaffold names (e.g. a real src/PLAN.md)
  # must still count as dirt — only untracked scaffold is ever ignored.
  echo one >README.md
  git add README.md
  git commit -q -m init
  git branch feat/tracked-dirty
  wt_path="$BATS_TEST_TMPDIR/tracked-dirty-wt"
  git worktree add -q "$wt_path" feat/tracked-dirty
  echo two >"$wt_path/README.md"
  stub_bin gh
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
  CREW_ID=c1 run_crew status "worker:feat/tracked-dirty" done "" "https://github.com/o/r/pull/23"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/tracked-dirty — uncommitted changes"* ]]
}

@test "reap: an untracked non-scaffold file keeps the worktree" {
  git commit -q --allow-empty -m init
  git branch feat/stray-file
  wt_path="$BATS_TEST_TMPDIR/stray-wt"
  git worktree add -q "$wt_path" feat/stray-file
  : >"$wt_path/notes.txt"
  stub_bin gh
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
  CREW_ID=c1 run_crew status "worker:feat/stray-file" done "" "https://github.com/o/r/pull/24"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/stray-file — uncommitted changes"* ]]
}

@test "reap: a forged gitlink never runs its gitdir's config (#539)" {
  # #539: a worker's Edit/Write can rewrite the worktree's .git gitlink to
  # point at a gitdir it built, and plain `git -C wtpath status` (discovery)
  # then reads that gitdir's config and runs core.fsmonitor in the
  # dispatcher's own shell. reap must refuse before any such status call.
  git commit -q --allow-empty -m init
  git branch feat/539-a
  wt_path="$BATS_TEST_TMPDIR/539-a-wt"
  git worktree add -q "$wt_path" feat/539-a
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  fakegit="$BATS_TEST_TMPDIR/fakegit"
  cp -r .git "$fakegit"
  rm -rf "$fakegit/worktrees"
  git --git-dir="$fakegit" config core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
  git --git-dir="$fakegit" symbolic-ref HEAD refs/heads/feat/539-a
  echo "gitdir: $fakegit" >"$wt_path/.git"
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
  CREW_ID=c1 run_crew status "worker:feat/539-a" done "" "https://github.com/o/r/pull/539"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/539-a"* ]]
  [[ "$output" == *"does not point at its git admin dir"* ]]
}

@test "reap: a submodule in the worktree is never recursed into (#539)" {
  # #539: git status by default recurses into a gitlink (mode 160000) index
  # entry via discovery through sub/.git, which the worker fully controls —
  # --ignore-submodules=all on the command line is the only thing .gitmodules
  # cannot override, but reap must refuse before status even runs.
  git commit -q --allow-empty -m init
  git branch feat/539-b
  wt_path="$BATS_TEST_TMPDIR/539-b-wt"
  git worktree add -q "$wt_path" feat/539-b
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  git init -q "$wt_path/sub"
  git -C "$wt_path/sub" config user.email test@example.com
  git -C "$wt_path/sub" config user.name test
  git -C "$wt_path/sub" commit -q --allow-empty -m sub-init
  git -C "$wt_path/sub" config core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
  sub_oid=$(git -C "$wt_path/sub" rev-parse HEAD)
  git -C "$wt_path" update-index --add --cacheinfo 160000,"$sub_oid",sub
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
  CREW_ID=c1 run_crew status "worker:feat/539-b" done "" "https://github.com/o/r/pull/539"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/539-b"* ]]
  [[ "$output" == *"it has submodules"* ]]
}

@test "reap: a config.worktree in the admin dir is refused (#539)" {
  # #539: per-worktree config lives in the admin dir and is still read under
  # an anchored gitdir; it can carry keys no -c list enumerates, so reap must
  # refuse outright rather than try to filter it.
  git commit -q --allow-empty -m init
  git branch feat/539-c
  wt_path="$BATS_TEST_TMPDIR/539-c-wt"
  git worktree add -q "$wt_path" feat/539-c
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  git config extensions.worktreeConfig true
  git -C "$wt_path" config --worktree core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
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
  CREW_ID=c1 run_crew status "worker:feat/539-c" done "" "https://github.com/o/r/pull/539"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/539-c"* ]]
  [[ "$output" == *"config.worktree"* ]]
}

@test "reap: a worker-planted clean filter never runs (#557)" {
  # #557: a worker's `git config filter.x.clean <cmd>` in a linked worktree
  # writes the COMMON config that dispatcher-run git reads. The git-config
  # baseline (recorded by a dispatch before any worker existed) is
  # TOFU for the human's own config, so a key planted afterward must refuse
  # reap's status call rather than let the filter run in the dispatcher shell.
  git commit -q --allow-empty -m init
  printf '' >f
  printf 'f filter=x\n' >.gitattributes
  git add f .gitattributes
  git commit -q -m 'track f under filter x'
  git branch feat/557-a
  wt_path="$BATS_TEST_TMPDIR/557-a-wt"
  git worktree add -q "$wt_path" feat/557-a
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
  CREW_ID=c1 run_crew status "worker:feat/557-a" done "" "https://github.com/o/r/pull/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-a"* ]]
  [[ "$output" == *"filter.x.clean"* ]]
}

@test "reap: the removal never runs a baselined worktree-relative filter the worker rewrote (#578)" {
  # A baselined filter.x.clean naming a repo-relative program passes the
  # guard; reap's anchored status and forced removal read no in-tree
  # attributes, so the worker's rewritten copy never runs.
  mkdir -p tools
  printf '#!/bin/sh\ncat\n' >tools/conv.sh
  chmod +x tools/conv.sh
  printf a >f
  printf 'f filter=x\n' >.gitattributes
  git add tools/conv.sh f .gitattributes
  git commit -q -m 'track f under filter x'
  git config filter.x.clean tools/conv.sh
  seed_git_baseline
  git branch feat/578-a
  wt_path="$BATS_TEST_TMPDIR/578-a-wt"
  git worktree add -q "$wt_path" feat/578-a
  wt_path=$(cd "$wt_path" && pwd -P)
  printf '#!/bin/sh\ntouch %q\ncat\n' "$BATS_TEST_TMPDIR/SENTINEL" >"$wt_path/tools/conv.sh"
  git -C "$wt_path" commit -qam rewrite
  rm -f "$BATS_TEST_TMPDIR/SENTINEL"
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
  CREW_ID=c1 run_crew status "worker:feat/578-a" done "" "https://github.com/o/r/pull/578"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ ! -d "$wt_path" ]
}

@test "reap: a worker-planted include never runs (#557)" {
  # #557: include.path can smuggle in an arbitrary [filter] block that no -c
  # list would enumerate directly, so reap must refuse on the include key
  # itself rather than trying to see through it.
  git commit -q --allow-empty -m init
  printf '' >f
  printf 'f filter=x\n' >.gitattributes
  git add f .gitattributes
  git commit -q -m 'track f under filter x'
  git branch feat/557-b
  wt_path="$BATS_TEST_TMPDIR/557-b-wt"
  git worktree add -q "$wt_path" feat/557-b
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  seed_git_baseline
  cat >"$BATS_TEST_TMPDIR/include.gitconfig" <<EOF
[filter "x"]
	clean = $BATS_TEST_TMPDIR/hit.sh
EOF
  git config include.path "$BATS_TEST_TMPDIR/include.gitconfig"
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
  CREW_ID=c1 run_crew status "worker:feat/557-b" done "" "https://github.com/o/r/pull/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-b"* ]]
  [[ "$output" == *"include.path"* ]]
}

@test "reap: a worker-planted fsmonitor never runs (#557)" {
  # #557: the config-drift guard refuses the planted core.fsmonitor before
  # any status runs, so the hook never fires and the tree is kept.
  git commit -q --allow-empty -m init
  git branch feat/557-c
  wt_path="$BATS_TEST_TMPDIR/557-c-wt"
  git worktree add -q "$wt_path" feat/557-c
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  seed_git_baseline
  git config core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
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
  CREW_ID=c1 run_crew status "worker:feat/557-c" done "" "https://github.com/o/r/pull/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-c"* ]]
  [[ "$output" == *"core.fsmonitor"* ]]
}

@test "reap: baseline core.hooksPath and credential.helper do not refuse (#557)" {
  # #557 TOFU: keys already sitting in the human's config when the baseline
  # is first seeded are the trusted baseline, not a worker's doing — reap
  # must reap normally even though both are exec-capable keys.
  git commit -q --allow-empty -m init
  git config core.hooksPath .husky
  git config credential.helper store
  seed_git_baseline
  baseline="$TEST_REPO/.git/crew/git-config-baseline"
  grep -q 'core.hookspath' "$baseline"
  git branch feat/557-d
  wt_path="$BATS_TEST_TMPDIR/557-d-wt"
  git worktree add -q "$wt_path" feat/557-d
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/557-d" done "" "https://github.com/o/r/pull/557"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/557-d"* ]]
}

# write_full_anchor_record <wt> <branch> — the 4-line record `dispatch` writes
# (wt, crew dir, branch, admin dir) for a linked worktree, so crew's
# _wt_trusted_cwd recognises it.
write_full_anchor_record() {
  local path common
  path="$(anchor_record_path "$1")"
  common="$(git -C "$TEST_REPO" rev-parse --path-format=absolute --git-common-dir)"
  mkdir -p "$(dirname "$path")"
  {
    realpath -e "$1"
    realpath -m "$common/crew"
    printf '%s\n' "$2"
    realpath -e "$(git -C "$1" rev-parse --absolute-git-dir)"
  } >"$path"
}

# make_fake_gitdir <dir> <branch> — a standalone clone of $TEST_REPO with
# <branch> at its origin tip, whose reference-transaction hook touches
# $BATS_TEST_TMPDIR/SENTINEL: the repo a worker's swapped `.git` would name.
make_fake_gitdir() {
  git clone -q "$TEST_REPO" "$1"
  git -C "$1" branch "$2" "origin/$2"
  cat >"$1/.git/hooks/reference-transaction" <<EOF
#!/bin/sh
: >"$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$1/.git/hooks/reference-transaction"
}

@test "reap: the merged-branch delete lands in the real repo when the caller's .git is swapped (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-cand
  cand_wt="$BATS_TEST_TMPDIR/633-cand-wt"
  git worktree add -q "$cand_wt" feat/633-cand
  echo unique >"$cand_wt/work.txt"
  git -C "$cand_wt" add work.txt
  git -C "$cand_wt" commit -q -m "cand work"
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  write_full_anchor_record "$w1" feat/633-here
  scratch="$BATS_TEST_TMPDIR/633-fake"
  make_fake_gitdir "$scratch" feat/633-cand
  stub_tmux "" ""
  # The first headRefOid lookup swaps the caller's .git for the fake, as a
  # worker could at any point during the sweep; it still prints the real tip
  # so a discovery-based `git rev-parse` in the fake agrees with it.
  export SWAP_REPO="$TEST_REPO" SWAP_W1="$w1" SWAP_FAKE="$scratch" SWAP_CAND=feat/633-cand SWAP_MARK="$BATS_TEST_TMPDIR/swapped"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*)
  if [ ! -e "$SWAP_MARK" ]; then
    : >"$SWAP_MARK"
    rm -f "$SWAP_W1/.git"
    mv "$SWAP_FAKE/.git" "$SWAP_W1/.git"
  fi
  git -C "$SWAP_REPO" rev-parse "refs/heads/$SWAP_CAND"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/633-cand" done "" "https://github.com/o/r/pull/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [[ "$output" == *"reaped feat/633-cand"* ]]
  [ ! -d "$cand_wt" ]
  run ! git -C "$TEST_REPO" show-ref --verify --quiet refs/heads/feat/633-cand
}

@test "reap: the branch delete stays anchored when an unrecorded caller's .git is swapped (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-cand
  cand_wt="$BATS_TEST_TMPDIR/633-cand-wt"
  git worktree add -q "$cand_wt" feat/633-cand
  echo unique >"$cand_wt/work.txt"
  git -C "$cand_wt" add work.txt
  git -C "$cand_wt" commit -q -m "cand work"
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  scratch="$BATS_TEST_TMPDIR/633-fake"
  make_fake_gitdir "$scratch" feat/633-cand
  stub_tmux "" ""
  export SWAP_REPO="$TEST_REPO" SWAP_W1="$w1" SWAP_FAKE="$scratch" SWAP_CAND=feat/633-cand SWAP_MARK="$BATS_TEST_TMPDIR/swapped"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*)
  if [ ! -e "$SWAP_MARK" ]; then
    : >"$SWAP_MARK"
    rm -f "$SWAP_W1/.git"
    mv "$SWAP_FAKE/.git" "$SWAP_W1/.git"
  fi
  git -C "$SWAP_REPO" rev-parse "refs/heads/$SWAP_CAND"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/633-cand" done "" "https://github.com/o/r/pull/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap --quiet
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  run ! git -C "$TEST_REPO" show-ref --verify --quiet refs/heads/feat/633-cand
  git --git-dir="$w1/.git" show-ref --verify --quiet refs/heads/feat/633-cand
}

@test "reap: keeps the recorded worktree the caller stands in after relocating (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  write_full_anchor_record "$w1" feat/633-here
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/633-here" done "" "https://github.com/o/r/pull/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"it is the current worktree"* ]]
  [ -d "$w1" ]
}

@test "reap: refuses from inside a worker worktree whose .git was swapped (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-cand
  cand_wt="$BATS_TEST_TMPDIR/633-cand-wt"
  git worktree add -q "$cand_wt" feat/633-cand
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  write_full_anchor_record "$w1" feat/633-here
  scratch="$BATS_TEST_TMPDIR/633-fake"
  make_fake_gitdir "$scratch" feat/633-cand
  rm -f "$w1/.git"
  mv "$scratch/.git" "$w1/.git"
  stub_tmux "" ""
  CREW_ID=c1 run_crew status "worker:feat/633-cand" done "" "https://github.com/o/r/pull/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 1 ]
  [[ "$output" == *"inside the worker worktree"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$cand_wt" ]
}

# make_filter_fake_gitdir <dir> <marker> — a standalone clone of $TEST_REPO
# whose own attributes select a clean filter that touches <marker>: the repo a
# worker's swapped `.git` would name.
make_filter_fake_gitdir() {
  git clone -q "$TEST_REPO" "$1"
  printf '* filter=x\n' >"$1/.git/info/attributes"
  git -C "$1" config filter.x.clean "$(printf 'touch %q; cat' "$2")"
}

@test "reap: a .git swapped after the gitlink check is never discovered (#677)" {
  # reap checks the worker's gitlink, then trashes scaffold (where the stub
  # swaps .git for a standalone repo) before removing. Any git that discovered
  # the repo from the tree after that would read the fake's config and
  # attributes and run its clean filter on the stat-dirty f.
  printf a >f
  git add f
  git commit -q -m f
  git branch feat/677-a
  wt_path="$BATS_TEST_TMPDIR/677-a-wt"
  git worktree add -q "$wt_path" feat/677-a
  wt_path=$(cd "$wt_path" && pwd -P)
  seed_git_baseline
  : >"$wt_path/WORKER_TASK.md"
  make_filter_fake_gitdir "$BATS_TEST_TMPDIR/677-fake" "$BATS_TEST_TMPDIR/SENTINEL"
  touch -d '+5 seconds' "$wt_path/f"
  stub_tmux "" ""
  export SWAP_WT="$wt_path" SWAP_FAKE="$BATS_TEST_TMPDIR/677-fake" SWAP_MARK="$BATS_TEST_TMPDIR/swapped" SWAP_REPO="$TEST_REPO"
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ ! -e "$SWAP_MARK" ]; then
  : >"$SWAP_MARK"
  mv "$SWAP_WT/.git" "$SWAP_WT.gitfile"
  mv "$SWAP_FAKE/.git" "$SWAP_WT/.git"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) git -C "$SWAP_REPO" rev-parse refs/heads/feat/677-a ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = remove ]; then
  branch="${!#}"
  wtp=$(git worktree list --porcelain | awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
  if [ -n "$wtp" ]; then
    git -C "$wtp" status --porcelain >/dev/null 2>&1
    rm -rf "$wtp"
  fi
  git worktree prune
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-a" done "" "https://github.com/o/r/pull/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/677-a — its .git changed during the reap"* ]]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: a .git swapped after the last re-check is refused by the anchored removal (#677)" {
  # The swap fires inside the removal call itself, after every re-check: git's
  # gitfile validation refuses the swapped tree and reap relays the tampering
  # line. The git wrapper also logs any post-swap git that discovers its repo
  # from the tree, which pins the removal (and all after it) as anchored.
  printf a >f
  git add f
  git commit -q -m f
  git branch feat/677-b
  wt_path="$BATS_TEST_TMPDIR/677-b-wt"
  git worktree add -q "$wt_path" feat/677-b
  wt_path=$(cd "$wt_path" && pwd -P)
  seed_git_baseline
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  make_filter_fake_gitdir "$BATS_TEST_TMPDIR/677-b-fake" "$BATS_TEST_TMPDIR/SENTINEL"
  touch -d '+5 seconds' "$wt_path/f"
  real_git=$(command -v git)
  stub_tmux "" ""
  export REAL_GIT="$real_git" SWAP_WT="$wt_path" SWAP_FAKE="$BATS_TEST_TMPDIR/677-b-fake" \
    SWAP_SAVE="$BATS_TEST_TMPDIR/real-gitfile" SWAP_MARK="$BATS_TEST_TMPDIR/swapped" SWAP_REPO="$TEST_REPO"
  cat >"$STUB_DIR/git" <<'EOF2'
#!/usr/bin/env bash
if [ ! -e "$SWAP_MARK" ] && [ "${!#}" = "$SWAP_WT" ]; then
  prev=
  for arg in "$@"; do
    if [ "$prev" = worktree ] && [ "$arg" = remove ]; then
      : >"$SWAP_MARK"
      mv "$SWAP_WT/.git" "$SWAP_SAVE"
      mv "$SWAP_FAKE/.git" "$SWAP_WT/.git"
      break
    fi
    prev=$arg
  done
fi
if [ -e "$SWAP_MARK" ] && [ -z "${GIT_DIR:-}" ]; then
  anchored= in_wt=
  case "$PWD/" in "$SWAP_WT"/*) in_wt=1 ;; esac
  prev=
  for arg in "$@"; do
    case "$arg" in --git-dir=*) anchored=1 ;; esac
    if [ "$prev" = -C ]; then
      case "$arg/" in "$SWAP_WT"/*) in_wt=1 ;; esac
    fi
    prev=$arg
  done
  if [ -z "$anchored" ] && [ -n "$in_wt" ]; then
    printf '%s\n' "$*" >>"$BATS_TEST_TMPDIR/discovered"
  fi
fi
exec "$REAL_GIT" "$@"
EOF2
  chmod +x "$STUB_DIR/git"
  cat >"$STUB_DIR/gh" <<'EOF2'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) "$REAL_GIT" -C "$SWAP_REPO" rev-parse refs/heads/feat/677-b ;;
esac
exit 0
EOF2
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-b" done "" "https://github.com/o/r/pull/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/677-b — its .git changed during the reap"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/discovered" ]
  [ -e "$anchor" ]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: a gitlink staged after the first submodule gate keeps the worktree (#677)" {
  # --force skips git's own submodule refusal and the status re-check hides
  # gitlinks, so the pre-removal re-check must look at the index again. The
  # gtrash stub, like the real one, removes what it is given, and stages a
  # gitlink over a fresh embedded repo between the first gate and the re-check.
  git commit -q --allow-empty -m init
  git branch feat/677-c
  wt_path="$BATS_TEST_TMPDIR/677-c-wt"
  git worktree add -q "$wt_path" feat/677-c
  wt_path=$(cd "$wt_path" && pwd -P)
  : >"$wt_path/WORKER_TASK.md"
  stub_tmux "" ""
  export SWAP_WT="$wt_path" SWAP_MARK="$BATS_TEST_TMPDIR/staged" SWAP_REPO="$TEST_REPO"
  SWAP_ADMIN=$(git -C "$wt_path" rev-parse --absolute-git-dir)
  export SWAP_ADMIN
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ ! -e "$SWAP_MARK" ]; then
  : >"$SWAP_MARK"
  git init -q "$SWAP_WT/sub"
  git --git-dir="$SWAP_ADMIN" --work-tree="$SWAP_WT" update-index --add \
    --cacheinfo "160000,$(git -C "$SWAP_REPO" rev-parse HEAD),sub"
fi
rm -f -- "${@:2}"
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-c" done "" "https://github.com/o/r/pull/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/staged" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/677-c — it has submodules"* ]]
  [ -d "$wt_path/sub/.git" ]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: a removal that fails partway is reported, not called tampering (#677)" {
  # git deletes the admin dir even when part of the tree cannot go, so the
  # tree's gitlink then fails to resolve without any tampering.
  [ "$(id -u)" != 0 ] || skip "root ignores directory permissions"
  git commit -q --allow-empty -m init
  git branch feat/677-d
  wt_path="$BATS_TEST_TMPDIR/677-d-wt"
  git worktree add -q "$wt_path" feat/677-d
  wt_path=$(cd "$wt_path" && pwd -P)
  printf 'ro/\n' >>"$TEST_REPO/.git/info/exclude"
  mkdir "$wt_path/ro"
  : >"$wt_path/ro/f"
  RESTORE_WRITE="$wt_path"
  chmod 0555 "$wt_path/ro"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-d" done "" "https://github.com/o/r/pull/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/677-d — removal failed partway; $wt_path is no longer a worktree"* ]]
  [[ "$output" != *"possible tampering"* ]]
  [ ! -e "$anchor" ]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: no baseline keeps the worktree and records none (#557)" {
  # #557: reap runs once workers exist, so it must never record a baseline.
  git commit -q --allow-empty -m init
  rm "$TEST_REPO/.git/crew/git-config-baseline"
  git branch feat/557-e
  wt_path="$BATS_TEST_TMPDIR/557-e-wt"
  git worktree add -q "$wt_path" feat/557-e
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF2'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF2
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/557-e" done "" "https://github.com/o/r/pull/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-e"* ]]
  [ ! -e "$TEST_REPO/.git/crew/git-config-baseline" ]
}

@test "reap: a live engine pane keeps a terminal, merged-PR worktree" {
  # Widening the terminal-state filter (defect 1) must not let an exited
  # worker whose PR merged get reclaimed out from under a human (or another
  # session) still sitting at the pane. The live-pane check runs unconditionally,
  # after the terminal-state filter, so it must still guard exited/failed the
  # same way it already guarded done.
  git commit -q --allow-empty -m init
  git branch feat/live-engine-me
  wt_path="$BATS_TEST_TMPDIR/live-engine-wt"
  git worktree add -q "$wt_path" feat/live-engine-me
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note in the idle-release tests
  stub_bin gh
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
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/live-engine-me" exited "" "https://github.com/o/r/pull/25"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/live-engine-me — an engine is still running there"* ]]
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: an exited worker with no PR is kept" {
  # Same guard as the long-standing "done but no PR" keep, now exercised for
  # a state defect 1 newly admits into the candidate pool.
  git commit -q --allow-empty -m init
  git branch feat/exited-no-pr
  wt_path="$BATS_TEST_TMPDIR/exited-no-pr-wt"
  git worktree add -q "$wt_path" feat/exited-no-pr
  stub_bin gh
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/exited-no-pr" exited
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/exited-no-pr — exited but no PR on the bus"* ]]
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
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
# bus helpers shared with the reap cases
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
