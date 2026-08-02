# Shared bats helpers.

# setup_repo — a throwaway git repo as $TEST_REPO, cwd set to it.
setup_repo() {
  TEST_REPO="$(mktemp -d)"
  cd "$TEST_REPO" || return 1
  git init -q -b main .
  git config user.email test@example.com
  git config user.name test
  export TEST_REPO
}

teardown_repo() {
  [ -n "${TEST_REPO:-}" ] && rm -rf "$TEST_REPO" "$TEST_REPO.wt"
}

# stub_tmux_window — a `tmux` stub that answers `new-window -P -F …` with a
# window/pane id pair. The inert stub prints nothing, leaving dispatch with an
# empty pane id, so it never reaches `send-keys`.
stub_tmux_window() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  cat >"$STUB_DIR/tmux" <<'TMUXSTUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = new-window ]; then
  printf '@9 %%9\n'
fi
exit 0
TMUXSTUB
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# stub_wt_worktree — a `wt` stub that really creates the worktree, so dispatch
# resolves it via `git worktree list` and reaches its launch line. The inert
# stub_bin stub cannot: dispatch aborts at "could not locate worktree".
stub_wt_worktree() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  cat >"$STUB_DIR/wt" <<'WTSTUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = switch ] && [ "$2" = -c ]; then
  git worktree add -q -b "$3" "$TEST_REPO.wt/${3//\//-}"
fi
exit 0
WTSTUB
  chmod +x "$STUB_DIR/wt"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# stub_bin <name> — put a logging stub for <name> first on PATH.
# The stub appends its argv (NUL-free, one invocation per line) to
# $STUB_LOG and exits 0.
stub_bin() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  cat >"$STUB_DIR/$1" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/$1"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}
