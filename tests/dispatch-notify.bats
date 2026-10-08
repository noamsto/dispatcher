bats_require_minimum_version 1.5.0 # `run !`

setup() {
  load helpers
  NOTIFY="$BATS_TEST_DIRNAME/../adapters/core/dispatch-notify.sh"
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  setup_repo
  # The suite runs from a dispatched worker session, which exports CREW_WORKER_ID.
  # Inheriting it would make the session-less cases below pass against the very
  # fallback they exist to pin.
  unset CREW_WORKER_ID CREW_ID
  # setup_repo leaves HEAD unborn, and the hook's `git rev-parse --abbrev-ref HEAD`
  # then exits 128 and resolves the branch to '?'.
  git commit --allow-empty -qm init
  git switch -qc feat/x
  LOG="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
}

teardown() {
  teardown_repo
}

# run_notify — drive the hook the way the plugin does: hook JSON on stdin.
run_notify() {
  jq -nc --arg c "$PWD" '{cwd:$c,hook_event_name:"SessionEnd",reason:"other"}' |
    bash -euo pipefail "$NOTIFY"
}

# run_notify_reason <reason> — a SessionEnd payload with an explicit `reason`,
# or the key omitted for `__absent__`. The hook keys only on mode and reason, so
# one claude-shaped payload covers pi's and claude's SessionEnd alike.
run_notify_reason() {
  if [ "$1" = __absent__ ]; then
    jq -nc --arg c "$PWD" '{cwd:$c,hook_event_name:"SessionEnd"}' |
      bash -euo pipefail "$NOTIFY"
  else
    jq -nc --arg c "$PWD" --arg r "$1" '{cwd:$c,hook_event_name:"SessionEnd",reason:$r}' |
      bash -euo pipefail "$NOTIFY"
  fi
}

# in_process_switch_posts_nothing <reason> — the switch reasons (#866) must not
# post `exited` or ping while the worker is still alive.
in_process_switch_posts_nothing() {
  stub_tmux
  task_doc %9
  seed_status working
  before="$BATS_TEST_TMPDIR/events.before"
  cp "$LOG" "$before"

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify_reason "$1"
  [ "$status" -eq 0 ]

  cmp -s "$LOG" "$before"
  run ! grep -q display-message "$STUB_LOG"
}

# real_end_still_posts_exited <reason> — a genuine process end must still post.
real_end_still_posts_exited() {
  task_doc
  seed_status working

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify_reason "$1"
  [ "$status" -eq 0 ]

  tail -1 "$LOG" | jq -e '.from == "worker:feat/x#s1-1" and .body.state == "exited"'
}

# seed_status <state> — a prior status from the live session.
seed_status() {
  CREW_ID=c1 bash -euo pipefail "$CREW" status 'worker:feat/x#s1-1' "$1"
}

# task_doc [pane] — the doc the hook keys on. `crew_id:` is load-bearing: without it
# the hook skips the bus path entirely and every no-write assertion passes vacuously.
task_doc() {
  printf 'crew_id: c1\n' >WORKER_TASK.md
  if [ -n "${1:-}" ]; then
    printf 'dispatcher_pane: %s\n' "$1" >>WORKER_TASK.md
  fi
}

# stub_bin creates $STUB_DIR but not the log until tmux is first called; truncating it
# keeps a `grep` for an absent invocation from writing to stderr.
stub_tmux() {
  stub_bin tmux
  : >"$STUB_LOG"
}

@test "notify: a session with no CREW_WORKER_ID writes nothing and pings nothing" {
  stub_tmux
  task_doc %9
  seed_status working
  before="$BATS_TEST_TMPDIR/events.before"
  cp "$LOG" "$before"

  run run_notify
  [ "$status" -eq 0 ]

  cmp -s "$LOG" "$before"
  run ! grep -q display-message "$STUB_LOG"
}

@test "notify: an empty CREW_WORKER_ID is treated as absent" {
  stub_tmux
  task_doc %9
  seed_status working
  before="$BATS_TEST_TMPDIR/events.before"
  cp "$LOG" "$before"

  CREW_WORKER_ID= run run_notify
  [ "$status" -eq 0 ]

  cmp -s "$LOG" "$before"
  run ! grep -q display-message "$STUB_LOG"
}

@test "notify: a grid role pane carrying the lead's CREW_WORKER_ID writes nothing and pings nothing" {
  stub_tmux
  task_doc %9
  seed_status working
  before="$BATS_TEST_TMPDIR/events.before"
  cp "$LOG" "$before"

  CREW_WORKER_ID='worker:feat/x#s1-1' CREW_ROLE_ID=role:feat/x:reviewer run run_notify
  [ "$status" -eq 0 ]

  cmp -s "$LOG" "$before"
  run ! grep -q display-message "$STUB_LOG"
}

@test "notify: roster still reports the live session after a session-less SessionEnd" {
  stub_tmux
  task_doc %9
  seed_status working

  run run_notify
  [ "$status" -eq 0 ]

  roster="$(CREW_ID=c1 bash -euo pipefail "$CREW" roster c1)"
  printf '%s' "$roster" | jq -e 'length == 1'
  printf '%s' "$roster" | jq -e '.[0].state == "working"'
  printf '%s' "$roster" | jq -e '.[0].session != null'
  printf '%s' "$roster" | jq -e '[.[0].sessions[] | select(.session == null)] | length == 0'
}

@test "notify: the dispatched worker's SessionEnd records exited under its sessioned id" {
  task_doc
  seed_status working

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify
  [ "$status" -eq 0 ]

  tail -1 "$LOG" | jq -e '.from == "worker:feat/x#s1-1" and .body.state == "exited"'
}

@test "notify: the dispatched worker's SessionEnd still pings the dispatcher pane" {
  stub_tmux
  task_doc %9
  seed_status working

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify
  [ "$status" -eq 0 ]

  [ "$(grep -c display-message "$STUB_LOG")" -eq 1 ]
  grep -q 'display-message -t %9' "$STUB_LOG"
}

@test "notify: a worker that already reported done is not overridden" {
  stub_tmux
  task_doc %9
  seed_status done

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify
  [ "$status" -eq 0 ]

  # The ping pins that the hook ran past the guard, so an absent `exited` is the
  # done-check suppressing the write rather than an early exit.
  grep -q 'display-message -t %9' "$STUB_LOG"
  run ! grep -q '"state":"exited"' "$LOG"
}

# ---------------------------------------------------------------------------
# Cursor: `stop` (end of turn), and a payload whose .cwd is empty
# ---------------------------------------------------------------------------

# run_notify_cursor — cursor's payload shape: .cwd empty, workspace in
# workspace_roots[0], plus the --turn-end mode its `stop` event needs.
run_notify_cursor() {
  jq -nc --arg c "$PWD" '{cwd:"",workspace_roots:[$c],hook_event_name:"stop"}' |
    bash -euo pipefail "$NOTIFY" --turn-end
}

@test "notify: cursor's empty .cwd resolves through workspace_roots" {
  task_doc
  seed_status working

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify_cursor
  [ "$status" -eq 0 ]

  tail -1 "$LOG" | jq -e '.from == "worker:feat/x#s1-1" and .body.state == "exited"'
}

@test "notify: --turn-end leaves a blocked worker alone — it is waiting, not gone" {
  stub_tmux
  task_doc %9
  seed_status blocked

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify_cursor
  [ "$status" -eq 0 ]

  run ! grep -q '"state":"exited"' "$LOG"
  run ! grep -q display-message "$STUB_LOG"
}

@test "notify: SessionEnd still overrides blocked — that session is really gone" {
  task_doc
  seed_status blocked

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify
  [ "$status" -eq 0 ]

  tail -1 "$LOG" | jq -e '.body.state == "exited"'
}

@test "notify: --turn-end on a worker that reported done writes nothing and stays quiet" {
  stub_tmux
  task_doc %9
  seed_status done

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify_cursor
  [ "$status" -eq 0 ]

  run ! grep -q '"state":"exited"' "$LOG"
  run ! grep -q display-message "$STUB_LOG"
}

# ---------------------------------------------------------------------------
# #866: in-process session switches emit a session-end event while the process
# stays alive — they are not a worker death.
# ---------------------------------------------------------------------------

@test "notify: in-process switch reason new posts nothing" {
  in_process_switch_posts_nothing new
}

@test "notify: in-process switch reason resume posts nothing" {
  in_process_switch_posts_nothing resume
}

@test "notify: in-process switch reason fork posts nothing" {
  in_process_switch_posts_nothing fork
}

@test "notify: in-process switch reason reload posts nothing" {
  in_process_switch_posts_nothing reload
}

@test "notify: in-process switch reason clear posts nothing" {
  in_process_switch_posts_nothing clear
}

@test "notify: a real quit still posts exited" {
  real_end_still_posts_exited quit
}

@test "notify: a missing reason still posts exited" {
  real_end_still_posts_exited __absent__
}

@test "notify: reason prompt_input_exit still posts exited" {
  real_end_still_posts_exited prompt_input_exit
}

@test "notify: reason logout still posts exited" {
  real_end_still_posts_exited logout
}

# ---------------------------------------------------------------------------
# #531: a child session's end must not mark a live lead `exited`
# ---------------------------------------------------------------------------

# run_nested_hook — run the hook as a grandchild of a fake lead engine:
#   lead(claude) -> child(cursor-agent) -> hook
# so the hook's two engine-named ancestors model a child session ending while
# the lead process is alive. A `#!/bin/sh` script whose basename is an engine
# name reports that name from `ps -o comm=` (verified on this host).
run_nested_hook() {
  root="$BATS_TEST_TMPDIR/engines"
  mkdir -p "$root/lead" "$root/child"
  cat >"$root/child/cursor-agent" <<EOF
#!/bin/sh
jq -nc --arg c "\$PWD" '{cwd:\$c,hook_event_name:"SessionEnd",reason:"other"}' | bash -euo pipefail "$NOTIFY"
EOF
  cat >"$root/lead/claude" <<EOF
#!/bin/sh
"$root/child/cursor-agent"
EOF
  chmod +x "$root/child/cursor-agent" "$root/lead/claude"
  "$root/lead/claude"
}

@test "notify: a child session's SessionEnd under a live lead posts nothing" {
  stub_tmux
  task_doc %9
  seed_status working
  before="$BATS_TEST_TMPDIR/events.before"
  cp "$LOG" "$before"

  CREW_WORKER_ID='worker:feat/x#s1-1' run run_nested_hook
  [ "$status" -eq 0 ]

  cmp -s "$LOG" "$before"
  run ! grep -q display-message "$STUB_LOG"
}

@test "notify: the lead's own SessionEnd (one engine ancestor) still posts exited" {
  task_doc
  seed_status working
  root="$BATS_TEST_TMPDIR/engine-single"
  mkdir -p "$root"
  cat >"$root/cursor-agent" <<EOF
#!/bin/sh
sleep 0.2
jq -nc --arg c "\$PWD" '{cwd:\$c,hook_event_name:"SessionEnd",reason:"other"}' | bash -euo pipefail "$NOTIFY"
EOF
  chmod +x "$root/cursor-agent"
  # A real SessionEnd runs under its own engine — exactly one engine-named
  # ancestor. The dev loop runs bats under the worker's engine, so orphan the
  # fake engine (`( … & )` reparents it to init) to isolate the chain; otherwise
  # the ambient engine counts as a second one and the guard fires.
  ( CREW_WORKER_ID='worker:feat/x#s1-1' "$root/cursor-agent" >/dev/null 2>&1 & )
  exited=""
  for _ in $(seq 1 50); do
    if [ -f "$LOG" ] && tail -1 "$LOG" | jq -e '.from == "worker:feat/x#s1-1" and .body.state == "exited"' >/dev/null 2>&1; then
      exited=1
      break
    fi
    sleep 0.1
  done
  [ -n "$exited" ]
}

@test "notify: a turn-end (stop) while the lead engine is alive posts nothing" {
  stub_tmux
  task_doc %9
  seed_status working
  before="$BATS_TEST_TMPDIR/events.before"
  cp "$LOG" "$before"

  CREW_WORKER_ID='worker:feat/x#s1-1' TMUX_PANE=%1 CREW_NOTIFY_PROC_CMD='printf node' run run_notify_cursor
  [ "$status" -eq 0 ]

  cmp -s "$LOG" "$before"
  run ! grep -q display-message "$STUB_LOG"
}

@test "notify: a turn-end (stop) with the lead engine gone still posts exited" {
  task_doc
  seed_status working

  CREW_WORKER_ID='worker:feat/x#s1-1' TMUX_PANE=%1 CREW_NOTIFY_PROC_CMD='printf bash' run run_notify_cursor
  [ "$status" -eq 0 ]

  tail -1 "$LOG" | jq -e '.from == "worker:feat/x#s1-1" and .body.state == "exited"'
}
