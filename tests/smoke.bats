setup() {
  load helpers
  setup_repo
}

teardown() {
  teardown_repo
}

@test "setup_repo creates a git repo" {
  run git rev-parse --is-inside-work-tree
  [ "$status" -eq 0 ]
  [ "$output" = "true" ]
}

@test "stub_bin intercepts a command and logs argv" {
  stub_bin tmux
  tmux send-keys -t pane0 'hello' Enter
  run cat "$STUB_LOG"
  [ "$status" -eq 0 ]
  [[ "$output" == *"send-keys -t pane0 hello Enter"* ]]
}
