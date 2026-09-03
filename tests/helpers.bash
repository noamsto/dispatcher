# Shared bats helpers.

# `load helpers` runs once per test setup, so this is a fresh suite-local store
# even for tests that do not need a throwaway repository.
export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"

# setup_repo — a throwaway git repo as $TEST_REPO, cwd set to it.
setup_repo() {
  TEST_REPO="$(mktemp -d)"
  cd "$TEST_REPO" || return 1
  # Every test gets a private data store; never let crew state leak in from
  # the shell running bats.
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  assert_isolated_xdg_data_home || return 1
  # Overriding $HOME is not enough: $XDG_CONFIG_HOME survives it, so git still
  # reads ~/.config/git/config, whose commit.gpgSign and tilde-relative
  # signingkey then re-expand against the new HOME and kill every commit.
  export GIT_CONFIG_GLOBAL=/dev/null
  git init -q -b main .
  git config user.email test@example.com
  git config user.name test
  export TEST_REPO
}

assert_isolated_xdg_data_home() {
  case "${XDG_DATA_HOME:-}" in
  "$BATS_TEST_TMPDIR"/*) return 0 ;;
  esac
  printf 'XDG_DATA_HOME must be under BATS_TEST_TMPDIR (got %s)\n' \
    "${XDG_DATA_HOME:-<unset>}" >&2
  return 1
}

teardown_repo() {
  [ -n "${TEST_REPO:-}" ] && rm -rf "$TEST_REPO"
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
