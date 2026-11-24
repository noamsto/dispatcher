bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# Fake-ssh-host harness (setup, make_bins, ssh shim, helpers): tests/remote-host-helpers.bash.

load remote-host-helpers

setup() { remote_host_setup; }
teardown() { remote_host_teardown; }

@test "dispatch --host scaffolds a remote worker, relays its events, routes replies and rosters its liveness" {
  local spec="$BATS_TEST_TMPDIR/spec.md"
  printf 'Remote summary.\n\n## Remote spec\n\nBody line.\n\n## Acceptance\n\n- AC1: the worker lands.\n' >"$spec"

  # 1. Scaffold on the remote.
  TMUX_PANE=%0 DISPATCH_SPEC="$spec" run --separate-stderr dispatch_remote --host mini 42 "Remote title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  grep -q '^worker_id: ' <<<"$output"
  worker_id="$(sed -n 's/^worker_id: //p' <<<"$output" | head -1)"
  row="$(local_dispatch_rows)"
  [ "$(wc -l <<<"$row")" -eq 1 ]
  jq -e --arg w "$worker_id" '.host == "mini" and .remote == true and .crew_id == "c1" and .worker_id == $w' <<<"$row"
  jq -e --arg r "$(realpath -e -- "$TEST_REPO/.git")" '.repo == $r' <<<"$row"
  branch="$(jq -r .branch <<<"$row")"
  wt="$(remote_wt "$branch")"
  [ -d "$wt" ]
  grep -qx 'crew_id: c1' "$wt/WORKER_TASK.md"
  grep -Eq '^dispatcher_pane: ?$' "$wt/WORKER_TASK.md"
  grep -qF 'Body line.' "$wt/WORKER_TASK.md"
  remote_tmux list-windows -t =main -F '#{@crew_branch}' | grep -qxF "$branch"
  # The dispatch that started the remote tmux server left no CREW_ or
  # DISPATCH_ var (CREW_ID among them) in the server's global environment.
  # Names only, never values.
  [ "$(remote_tmux show-environment -g | cut -d= -f1 | grep -cE '^(CREW|DISPATCH)_')" -eq 0 ]
  grep -qxF 'mini main --into main --window @crew_id=c1' "$OG_LOG"
  assert_ssh_command_is_literal

  # 2. Follow it from the local dispatcher's lane.
  start_stream --crew c1 --park 2 --retry 1 --coalesce 1 --interval 1

  # 3. A remote status reaches the local stream tagged with its host.
  on_remote "$wt" env CREW_WORKER_ID="$worker_id" crew status "$worker_id" blocked "need input"
  poll_for 300 stream_has_blocked || {
    cat "$STREAM_ERR" >&2
    cat "$STREAM_OUT" >&2
    false
  }

  # 4. A local reply lands in the remote worker's inbox.
  run crew reply --crew c1 "$worker_id" "hello"
  [ "$status" -eq 0 ]
  run on_remote "$wt" crew inbox "$worker_id"
  [ "$status" -eq 0 ]
  [[ "$output" == *hello* ]]

  # 5. The local roster shows it live, judged on the remote's own tmux.
  run crew roster c1
  [ "$status" -eq 0 ]
  jq -e --arg b "$branch" '.[] | select(.branch == $b and .host == "mini") | .live == true' <<<"$output"

  # 6. Killing the remote window flips it.
  local win
  win="$(remote_tmux list-windows -t =main -F '#{window_id}' | head -1)"
  [ -n "$win" ]
  remote_tmux kill-window -t "$win"
  run crew roster c1
  [ "$status" -eq 0 ]
  jq -e --arg b "$branch" '.[] | select(.branch == $b and .host == "mini") | .live == false' <<<"$output"
  stop_stream
}

@test "dispatch --host names the local host as the owning dispatcher on the remote worker" {
  # Only the local side sees FAKE_UNAME_N (the remote env is built from a whitelist).
  cat >"$STUB_DIR/uname" <<EOF
#!/usr/bin/env bash
if [ "\${1:-}" = -n ] && [ -n "\${FAKE_UNAME_N:-}" ]; then
  printf '%s\n' "\$FAKE_UNAME_N"
else
  PATH="\${PATH#*$STUB_DIR:}" exec uname "\$@"
fi
EOF
  chmod +x "$STUB_DIR/uname"
  FAKE_UNAME_N=owner-box.example.org run --separate-stderr dispatch_remote --host mini 42 "Owned title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  wt="$(remote_wt "$(local_dispatch_rows | jq -r .branch)")"
  grep -qx 'dispatcher_host: owner-box' "$wt/WORKER_TASK.md"
  remote_tmux list-windows -t =crew-c1 -F '#{@crew_dispatcher_host}' | grep -qxF owner-box
}

@test "dispatch --host carries DISPATCH_DRAFT_PR=1 to the remote worker" {
  DISPATCH_DRAFT_PR=1 run --separate-stderr dispatch_remote --host mini 42 "Draft title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  grep -qx 'draft: true' "$(remote_wt "$(local_dispatch_rows | jq -r .branch)")/WORKER_TASK.md"
}

# The mirror launcher is resolved from PATH, then `og remote open`, then the
# @remote_open_bin tmux option; the dispatch itself succeeds either way.
# without_og — drop the PATH dirs holding a real `og`, so only stubs resolve.
without_og() {
  local d p=""
  local IFS=:
  for d in $PATH; do
    [ -x "$d/og" ] && [ "$d" != "$STUB_DIR" ] || p="${p:+$p:}$d"
  done
  PATH="$p"
  write_remote_env
}

mirror_dispatch() {
  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 42 "Remote title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
}

@test "dispatch --host mirrors via og-remote-open on PATH before og remote open" {
  printf '#!/usr/bin/env bash\necho "og $*" >>"$OG_LOG"\n' >"$STUB_DIR/og"
  chmod +x "$STUB_DIR/og"
  mirror_dispatch
  grep -qxF 'mini main --into main --window @crew_id=c1' "$OG_LOG"
  ! grep -q '^og ' "$OG_LOG"
}

@test "dispatch --host mirrors via og remote open when og-remote-open is not on PATH" {
  rm "$STUB_DIR/og-remote-open"
  printf '#!/usr/bin/env bash\necho "og $*" >>"$OG_LOG"\n' >"$STUB_DIR/og"
  chmod +x "$STUB_DIR/og"
  mirror_dispatch
  grep -qxF 'og remote open mini main --into main --window @crew_id=c1' "$OG_LOG"
}

@test "dispatch --host mirrors via the @remote_open_bin executable as the last resort" {
  rm "$STUB_DIR/og-remote-open"
  without_og
  printf '#!/usr/bin/env bash\necho "opt $*" >>"$OG_LOG"\n' >"$BATS_TEST_TMPDIR/opt-launcher"
  chmod +x "$BATS_TEST_TMPDIR/opt-launcher"
  STUB_REMOTE_OPEN_BIN="$BATS_TEST_TMPDIR/opt-launcher" mirror_dispatch
  grep -qxF 'opt mini main --into main --window @crew_id=c1' "$OG_LOG"
}

@test "dispatch --host names every launcher source when none resolves, and still succeeds" {
  rm "$STUB_DIR/og-remote-open"
  without_og
  STUB_REMOTE_OPEN_BIN="$BATS_TEST_TMPDIR/not-executable" mirror_dispatch
  [ ! -e "$OG_LOG" ]
  grep -qF 'remote window not mirrored (no og-remote-open launcher (PATH, og remote open, @remote_open_bin))' <<<"$stderr"
}

@test "dispatch --host --chrome reaches a host with hosts.<h>.chrome" {
  run --separate-stderr dispatch_remote --host chromebox --chrome 42 "t"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  grep -qx 'chrome: true' "$(remote_wt "$(local_dispatch_rows | jq -r .branch)")/WORKER_TASK.md"
}

@test "dispatch --host --chrome is refused for a host without hosts.<h>.chrome, before any ssh" {
  run --separate-stderr dispatch_remote --host mini --chrome 42 "t"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"hosts.mini.chrome"* ]]
  [ ! -e "$SSH_LOG" ]
  assert_nothing_recorded_locally
}

@test "dispatch --host --chrome with an unknown host reports the unknown host, before any ssh" {
  run --separate-stderr dispatch_remote --host nowhere --chrome 42 "t"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"unknown host 'nowhere'"* ]]
  [[ "$stderr" != *"hosts.nowhere.chrome"* ]]
  [ ! -e "$SSH_LOG" ]
  assert_nothing_recorded_locally
}

@test "dispatch --host --chrome runs the remote's own ban" {
  jq '. + {workerDeniedMcpServers: ["claude-in-chrome"]}' "$RHOME/config/dispatcher/settings.json" >"$BATS_TEST_TMPDIR/rs.json"
  cp "$BATS_TEST_TMPDIR/rs.json" "$RHOME/config/dispatcher/settings.json"
  run --separate-stderr dispatch_remote --host chromebox --chrome 42 "t"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"workerDeniedMcpServers lists claude-in-chrome"* ]]
  assert_nothing_recorded_locally
}

@test "dispatch --host with an unknown host is refused before any ssh or local write" {
  run --separate-stderr dispatch_remote --host nowhere 42 "t"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"unknown host 'nowhere'"* ]]
  [ ! -e "$SSH_LOG" ]
  assert_nothing_recorded_locally
}

@test "dispatch --host with no clone mapped for the repo is refused before any ssh or local write" {
  run --separate-stderr dispatch_remote --host bare 42 "t"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"has no clone mapped for acme/widget"* ]]
  [ ! -e "$SSH_LOG" ]
  assert_nothing_recorded_locally
}

@test "dispatch --host reports a link that fails mid-dispatch and leaves nothing local" {
  # Preflight succeeds, then the dispatch call and the recovery query get 255.
  SSH_FAIL_AFTER=1 DISPATCH_HOST_RECOVER_TRIES=3 DISPATCH_HOST_RECOVER_SLEEP=0 \
    run --separate-stderr dispatch_remote --host broken 42 "t"
  [ "$status" -eq 255 ]
  [[ "$stderr" == *"ssh link failed mid-dispatch"* ]]
  [[ "$stderr" == *"--host broken"* ]]
  [[ "$stderr" == *"--recover"* ]]
  # preflight, dispatch, and one query: a 255 query stops the loop.
  [ "$(wc -l <"$SSH_LOG")" -eq 3 ]
  assert_nothing_recorded_locally
  [ "$(git -C "$REMOTE_CLONE" worktree list --porcelain | grep -c '^worktree ')" -eq 1 ]
}

@test "dispatch --host delivers title and spec metachar verbatim and runs none of them" {
  local mark="$BATS_TEST_TMPDIR/pwned" spec="$BATS_TEST_TMPDIR/meta-spec.md" title line
  title="x \$(touch $mark) \`touch $mark\`; touch $mark 'q\""
  {
    printf '%s\n' "'; touch $mark; '"
    printf '%s\n' "\$(touch $mark)"
    printf '%s\n' "\`touch $mark\` && touch $mark"
    printf '%s\n' '## Not a heading?'
    printf '%s\n' '## Acceptance'
    printf '%s\n' '- AC1: delivered verbatim'
  } >"$spec"

  DISPATCH_SPEC="$spec" run --separate-stderr dispatch_remote --host mini 42 "$title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  branch="$(local_dispatch_rows | jq -r .branch)"
  task="$(remote_wt "$branch")/WORKER_TASK.md"
  [ -f "$task" ]
  grep -qxF -- "title: $title" "$task"
  while IFS= read -r line; do
    grep -qxF -- "$line" "$task"
  done <"$spec"
  [ ! -e "$mark" ]
  assert_ssh_command_is_literal
}

@test "dispatch --host recovers a worker the dropped link already created" {
  # The remote dispatch completes, then the link dies before its output arrives.
  SSH_DROP_AFTER_DISPATCH=1 DISPATCH_HOST_RECOVER_TRIES=3 DISPATCH_HOST_RECOVER_SLEEP=1 \
    run --separate-stderr dispatch_remote --host mini 42 "Recover title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  grep -q '^worker_id: ' <<<"$output"
  [[ "$stderr" == *"link dropped"* ]]
  row="$(local_dispatch_rows)"
  [ "$(wc -l <<<"$row")" -eq 1 ]
  jq -e '.host == "mini" and .remote == true and .crew_id == "c1"' <<<"$row"
  [ -d "$(remote_wt "$(jq -r .branch <<<"$row")")" ]
}

@test "dispatch --host --recover records a worker the downed link could not confirm, once" {
  # The link stays down after the dispatch call: nothing can be confirmed yet.
  SSH_DROP_AFTER_DISPATCH=1 SSH_DOWN_AFTER_DROP=1 DISPATCH_HOST_RECOVER_TRIES=3 DISPATCH_HOST_RECOVER_SLEEP=0 \
    run --separate-stderr dispatch_remote --host mini 42 "Recover later"
  [ "$status" -eq 255 ]
  [[ "$stderr" == *"ssh link failed mid-dispatch"* ]]
  assert_nothing_recorded_locally
  session="$(grep -o -- '--recover s[0-9]*-[0-9]*' <<<"$stderr" | head -1 | cut -d' ' -f2)"
  [[ "$session" =~ ^s[0-9]+-[0-9]+$ ]]
  [ "$(git -C "$REMOTE_CLONE" worktree list --porcelain | grep -c '^worktree ')" -eq 2 ]

  # Link back: --recover finds the remote row, records it and mirrors the window.
  rm -f "$SSH_DOWN"
  unset SSH_DROP_AFTER_DISPATCH SSH_DOWN_AFTER_DROP
  TMUX_PANE=%0 DISPATCH_HOST_RECOVER_TRIES=2 DISPATCH_HOST_RECOVER_SLEEP=0 \
    run --separate-stderr dispatch --host mini --recover "$session" --crew-id c1
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(local_dispatch_rows | wc -l)" -eq 1 ]
  local_dispatch_rows | jq -e --arg s "$session" '.session == $s and .host == "mini" and .remote == true'
  grep -qxF 'mini crew-c1 --into main' "$OG_LOG"

  # A second run is a no-op: still one row.
  DISPATCH_HOST_RECOVER_TRIES=2 DISPATCH_HOST_RECOVER_SLEEP=0 \
    run --separate-stderr dispatch --host mini --recover "$session" --crew-id c1
  [ "$status" -eq 0 ]
  [ "$(local_dispatch_rows | wc -l)" -eq 1 ]
}

@test "dispatch --host --recover with no remote row exits 1 and writes nothing" {
  DISPATCH_HOST_RECOVER_TRIES=2 DISPATCH_HOST_RECOVER_SLEEP=0 \
    run --separate-stderr dispatch --host mini --recover s1-1 --crew-id c1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"no remote dispatch row for session s1-1"* ]]
  assert_nothing_recorded_locally
}

@test "dispatch --host from session dispatcher targets the remote dispatcher session, creating it, stamped and decorated" {
  STUB_LOCAL_SESSION=dispatcher TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 42 "Remote title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  run remote_tmux list-windows -t =dispatcher -F '#{@crew_id}|#{@crew_branch}|#{@crew_name}|#{@crew_color}'
  [ "$status" -eq 0 ]
  grep -Eq '^c1\|feat/42-[^|]*\|[^|]+\|[^|]+$' <<<"$output"
  run remote_tmux show-window-options -v -t =dispatcher: pane-border-style
  [ "$status" -eq 0 ]
  [[ "$output" == *fg=* ]]
  run ! remote_tmux has-session -t =crew-c1
  [ "$(cat "$OG_LOG")" = 'mini dispatcher --into dispatcher --window @crew_id=c1' ]

  # The session is shared with the owner's windows: a later pane inherits no CREW_ID.
  local f="$BATS_TEST_TMPDIR/pane-env"
  remote_tmux new-window -d -t =dispatcher: "sh -c 'printenv CREW_ID >$f; echo rc=\$? >>$f'"
  poll_for 50 grep -q '^rc=' "$f"
  grep -qx 'rc=1' "$f"
}

@test "dispatch --host adds its window to an existing remote session of that name" {
  on_remote "$RHOME" tmux new-session -d -s dispatcher
  local before
  before="$(remote_tmux list-windows -t =dispatcher -F '#{window_id}' | wc -l)"
  STUB_LOCAL_SESSION=dispatcher TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 42 "Remote title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(remote_tmux list-windows -t =dispatcher -F '#{window_id}' | wc -l)" -eq $((before + 1)) ]
  remote_tmux list-windows -t =dispatcher -F '#{@crew_id}' | grep -qx ''
  remote_tmux list-windows -t =dispatcher -F '#{@crew_id}' | grep -qx 'c1'
}

@test "dispatch --host refuses local session names unsafe for the remote" {
  local name
  for name in '-x' 'a;b' 'a b' '$(x)' 'a.b' 'a:b'; do
    rm -f "$SSH_LOG" "$OG_LOG"
    STUB_LOCAL_SESSION="$name" TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 42 "Remote title"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"cannot name a remote session"* ]]
    assert_nothing_recorded_locally
    [ ! -e "$OG_LOG" ]
    [ ! -e "$SSH_LOG" ]
    [ "$(git -C "$REMOTE_CLONE" worktree list --porcelain | grep -c '^worktree ')" -eq 1 ]
  done
}

@test "dispatch --host --recover mirrors a worker still in an old crew-<id> session" {
  run --separate-stderr dispatch_remote --host mini 42 "Remote title"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  remote_tmux has-session -t =crew-c1
  local session
  session="$(local_dispatch_rows | jq -r .session)"
  rm -f "$OG_LOG"
  TMUX_PANE=%0 run --separate-stderr dispatch --host mini --recover "$session" --crew-id c1
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  grep -qxF 'mini crew-c1 --into main' "$OG_LOG"
}
