#!/usr/bin/env bats

setup() {
  FIX="$BATS_TEST_DIRNAME/fixtures/code-mode"
  STUB="$BATS_TEST_TMPDIR/bin"
  CM_LOG="$BATS_TEST_TMPDIR/crew.log"
  TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
  mkdir -p "$STUB"
  cat >"$STUB/tmux" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$TMUX_LOG"
[ -z "${CM_TMUX_FAIL:-}" ] || exit 1
cat "$CM_FRAME"
STUBEOF
  cat >"$STUB/crew" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CM_LOG"
STUBEOF
  chmod +x "$STUB/tmux" "$STUB/crew"
  export PATH="$STUB:$PATH" CM_LOG TMUX_LOG
  export DISPATCH_CODE_MODE_WATCH_SECS=1
  # shellcheck source=/dev/null
  . "$BATS_TEST_DIRNAME/../adapters/core/launcher-common.sh"
}

# watch <frame-file> — run the watch the way a launcher does: under -euo pipefail.
watch() {
  CM_FRAME="$1" bash -euo pipefail -c '. "$1"; code_mode_watch w1 c1 %7' _ \
    "$BATS_TEST_DIRNAME/../adapters/core/launcher-common.sh"
}

sink() { grep -F ' code-mode:c1 ' "$CM_LOG"; }
dispatcher_msgs() { grep -F ' dispatcher:c1 ' "$CM_LOG"; }

@test "code-mode watch: an agent-teams refusal posts refused with the reason and tells the dispatcher" {
  run watch "$FIX/agent-teams-refusal.txt"
  [ "$status" -eq 0 ]
  [ "$(sink | wc -l)" -eq 1 ]
  [ "$(sink)" = 'msg w1 code-mode:c1 {"state":"refused","why":"it can'\''t be used together with agent teams yet"}' ]
  [ "$(dispatcher_msgs | wc -l)" -eq 1 ]
  dispatcher_msgs | grep -q 'agent teams'
}

@test "code-mode watch: a tools refusal posts the tools reason" {
  run watch "$FIX/tools-refusal.txt"
  [ "$status" -eq 0 ]
  sink | grep -q '"state":"refused"'
  sink | grep -q 'tool options (--restricted, --tools, --disallowedTools or a deny rule) leave it out'
  [ "$(dispatcher_msgs | wc -l)" -eq 1 ]
}

@test "code-mode watch: the first-use opt-in dialog posts refused" {
  run watch "$FIX/optin-dialog.txt"
  [ "$status" -eq 0 ]
  sink | grep -q '"state":"refused"'
  sink | grep -q 'opt-in dialog'
  [ "$(dispatcher_msgs | wc -l)" -eq 1 ]
}

@test "code-mode watch: the declined banner posts refused" {
  printf '  Code Mode (experimental) is off for this session, as you chose.\n' >"$BATS_TEST_TMPDIR/declined.txt"
  run watch "$BATS_TEST_TMPDIR/declined.txt"
  [ "$status" -eq 0 ]
  sink | grep -q '"why":"declined at the opt-in dialog"'
}

@test "code-mode watch: the on banner posts on and nothing to the dispatcher" {
  run watch "$FIX/on-banner.txt"
  [ "$status" -eq 0 ]
  [ "$(sink)" = 'msg w1 code-mode:c1 {"state":"on"}' ]
  [ "$(wc -l <"$CM_LOG")" -eq 1 ]
}

@test "code-mode watch: an empty frame posts unknown at the deadline" {
  : >"$BATS_TEST_TMPDIR/empty.txt"
  run watch "$BATS_TEST_TMPDIR/empty.txt"
  [ "$status" -eq 0 ]
  sink | grep -q '"state":"unknown"'
  sink | grep -q 'no Code Mode banner within 1s'
  [ "$(dispatcher_msgs | wc -l)" -eq 1 ]
}

@test "code-mode watch: the capture joins wrapped lines and takes the full scrollback" {
  run watch "$FIX/on-banner.txt"
  [ "$status" -eq 0 ]
  grep -q -- 'capture-pane .*-J' "$TMUX_LOG"
  grep -q -- 'capture-pane .*-S -' "$TMUX_LOG"
  grep -q -- '-t %7' "$TMUX_LOG"
}

@test "code-mode watch: a vanished pane posts unknown even under set -euo pipefail" {
  CM_TMUX_FAIL=1 run watch "$FIX/on-banner.txt"
  [ "$status" -eq 0 ]
  sink | grep -q '"state":"unknown"'
  sink | grep -q 'lead pane gone'
  [ "$(dispatcher_msgs | wc -l)" -eq 1 ]
}

@test "code-mode watch: the earliest banner wins over later output quoting another" {
  {
    cat "$FIX/on-banner.txt"
    printf 'worker: saw "Code Mode (experimental) isn'\''t available: bogus. Starting with the ordinary tools."\n'
  } >"$BATS_TEST_TMPDIR/quoted.txt"
  run watch "$BATS_TEST_TMPDIR/quoted.txt"
  [ "$status" -eq 0 ]
  [ "$(sink)" = 'msg w1 code-mode:c1 {"state":"on"}' ]
}

@test "code-mode watch: a failing crew msg does not stop the watch posting the rest" {
  cat >"$STUB/crew" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CM_LOG"
exit 1
STUBEOF
  run watch "$FIX/agent-teams-refusal.txt"
  [ "$status" -eq 0 ]
  [ "$(wc -l <"$CM_LOG")" -eq 2 ]
}

@test "code-mode watch: spawn orphans the watch and returns at once" {
  CM_FRAME="$FIX/on-banner.txt" spawn_code_mode_watch w1 c1 %7
  for _ in $(seq 50); do
    [ -s "$CM_LOG" ] && break
    sleep 0.1
  done
  [ "$(sink)" = 'msg w1 code-mode:c1 {"state":"on"}' ]
}
