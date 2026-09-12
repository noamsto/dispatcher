setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
}

teardown() {
  teardown_repo
}

# #54: a worker must reach its dispatcher from WORKER_TASK.md alone, so
# _crew_id resolves task-document-first with CREW_ID only as a fallback —
# these pin the ordering and the cwd-independence directly, rather than via
# `status`/`msg` side effects as tests/crew.bats does.

@test "crew id: resolves from WORKER_TASK.md with no CREW_ID in the environment at all" {
  printf 'crew_id: c-from-taskdoc\n' >WORKER_TASK.md
  run env -u CREW_ID bash -euo pipefail "$CREW" id
  [ "$status" -eq 0 ]
  [ "$output" = "c-from-taskdoc" ]
}

@test "crew id: the task document wins over a disagreeing CREW_ID" {
  printf 'crew_id: c-from-taskdoc\n' >WORKER_TASK.md
  CREW_ID=c-from-env run run_crew id
  [ "$status" -eq 0 ]
  [ "$output" = "c-from-taskdoc" ]
}

@test "crew id: resolves from a subdirectory of the worktree" {
  printf 'crew_id: c-from-taskdoc\n' >WORKER_TASK.md
  mkdir -p a/b/c
  cd a/b/c
  run env -u CREW_ID bash -euo pipefail "$CREW" id
  [ "$status" -eq 0 ]
  [ "$output" = "c-from-taskdoc" ]
}

@test "crew id: CREW_ID still resolves when no WORKER_TASK.md exists" {
  run env CREW_ID=c-from-env bash -euo pipefail "$CREW" id
  [ "$status" -eq 0 ]
  [ "$output" = "c-from-env" ]
}

@test "crew id: falls back to CREW_ID when WORKER_TASK.md has no crew_id: line" {
  printf 'title: some task\n' >WORKER_TASK.md
  run env CREW_ID=c-from-env bash -euo pipefail "$CREW" id
  [ "$status" -eq 0 ]
  [ "$output" = "c-from-env" ]
}
