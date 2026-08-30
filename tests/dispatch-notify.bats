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
  ! grep -q display-message "$STUB_LOG"
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
  ! grep -q display-message "$STUB_LOG"
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
  ! grep -q '"state":"exited"' "$LOG"
}
