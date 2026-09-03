# Reap-side tests for #32: dispatch's pre-reap folding a just-launched worker
# on a branch whose *previous* session had already gone `done`. Kept out of
# tests/crew.bats (three other workers have concurrent edits landing there).
#
# setup()/teardown()/run_crew/stub_tmux are copied verbatim from
# tests/crew.bats rather than shared, since stub_tmux lives there (not in
# tests/helpers.bash) and promoting it would mean editing that file.

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
}

teardown() {
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
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# canon_wt_path <branch> — the path `crew reap`/`_occupants` actually compare
# against (git's own porcelain output), NOT $BATS_TEST_TMPDIR directly. On
# macOS the two differ (/private/var/... vs /var/...) because git resolves
# symlinks in worktree paths but bats' tmpdir var does not — feeding the raw
# tmpdir path to stub_tmux would make every occupancy check silently miss.
canon_wt_path() {
  git worktree list --porcelain | awk -v b="refs/heads/$1" '/^worktree /{p=$2} $0=="branch "b{print p}'
}

@test "reap: a claim on a branch masks an older done session and is not released" {
  git commit --allow-empty -q -m init
  git branch feat/race
  git worktree add -q "$BATS_TEST_TMPDIR/race-wt" feat/race
  stub_bin gh
  stub_bin wt
  wt_path="$(canon_wt_path feat/race)"
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tclaude\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", from:"worker:feat/race#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  jq -nc '{ts:2000, crew_id:"c1", from:"worker:feat/race#s2-2", kind:"claim"}' >>"$log"
  # Fixture sanity: occupancy must be non-empty, or the release loop
  # `continue`s before ever reaching kill-window and this test is vacuous
  # regardless of which branch of the fix is under test.
  CREW_ID=c1 run run_crew occupants "$wt_path"
  [ "$output" != "[]" ]
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a superseded claim does not block reaping a later terminal status" {
  git commit --allow-empty -q -m init
  git branch feat/race2
  git worktree add -q "$BATS_TEST_TMPDIR/race2-wt" feat/race2
  stub_bin gh
  stub_bin wt
  wt_path="$(canon_wt_path feat/race2)"
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", from:"worker:feat/race2#s2-2", kind:"claim"}' >>"$log"
  jq -nc '{ts:2000, crew_id:"c1", from:"worker:feat/race2#s2-2", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  CREW_ID=c1 run run_crew occupants "$wt_path"
  [ "$output" != "[]" ]
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "reap: a genuinely stale done worker on a merged PR is still reaped despite an unrelated claim" {
  git commit --allow-empty -q -m init
  git branch feat/stale-done
  wt_path="$BATS_TEST_TMPDIR/stale-done-wt"
  git worktree add -q "$wt_path" feat/stale-done
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
  CREW_ID=c1 run_crew status "worker:feat/stale-done#s1-1" done "" "https://example.com/pr/1"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc '{ts:9999999999999, crew_id:"c1", from:"worker:other-branch#s9-9", kind:"claim"}' >>"$log"
  CREW_ID=c1 run run_crew reap --dry-run
  [[ "$output" == *"would reap feat/stale-done"* ]]
}
