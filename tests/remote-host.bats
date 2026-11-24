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
  # A layout-1 origin speaks v1 only and leaves no visiting crew behind.
  [ -s "$SSH_REQS" ]
  jq -s -e 'all(.[]; .v == 1 and (.env // {} | has("CREW_LAYOUT") | not) and .scope != "crew")' "$SSH_REQS"
  [ ! -e "$RHOME/state/crew/crews/c1/meta.json" ]
  jq -e 'has("crew_layout") | not' <<<"$row"

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

@test "dispatch --host --recover on a layout-2 crew records the row in its log and indexes the crew once" {
  l2_origin
  l2_log="$L2_LOG"
  CREW_CALLS="$BATS_TEST_TMPDIR/crew-calls"
  export CREW_CALLS
  mv "$STUB_DIR/crew" "$STUB_DIR/crew-wrapped"
  cat >"$STUB_DIR/crew" <<EOF
#!/usr/bin/env bash
case "\${1:-} \${2:-}" in
"index add") printf '%s\n' "\$*" >>"\$CREW_CALLS" ;;
esac
exec "$STUB_DIR/crew-wrapped" "\$@"
EOF
  chmod +x "$STUB_DIR/crew"

  SSH_DROP_AFTER_DISPATCH=1 SSH_DOWN_AFTER_DROP=1 DISPATCH_HOST_RECOVER_TRIES=3 DISPATCH_HOST_RECOVER_SLEEP=0 \
    run --separate-stderr dispatch_remote --host mini 42 "Recover later"
  [ "$status" -eq 255 ]
  session="$(grep -o -- '--recover s[0-9]*-[0-9]*' <<<"$stderr" | head -1 | cut -d' ' -f2)"
  [[ "$session" =~ ^s[0-9]+-[0-9]+$ ]]
  [ ! -e "$CREW_CALLS" ]

  rm -f "$SSH_DOWN"
  unset SSH_DROP_AFTER_DISPATCH SSH_DOWN_AFTER_DROP
  TMUX_PANE=%0 DISPATCH_HOST_RECOVER_TRIES=2 DISPATCH_HOST_RECOVER_SLEEP=0 \
    run --separate-stderr dispatch --host mini --recover "$session" --crew-id c1
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(grep -cxF 'index add --crew c1' "$CREW_CALLS")" -eq 1 ]
  [ "$(wc -l <"$CREW_CALLS")" -eq 1 ]
  [ "$(jq -c --arg s "$session" 'select(.kind == "dispatch" and .session == $s and .host == "mini" and .remote == true)' "$l2_log" | wc -l)" -eq 1 ]
  [ ! -e "$LBUS" ]
  jq -s -e 'any(.[]; .scope == "crew" and .argv[1] == "dispatch-row")' "$SSH_REQS"
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

# l2_dispatch — a layout-2 dispatch of issue 42 to mini from a pane whose local
# session names the remote session `main`.
l2_dispatch() {
  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 42 "Layout two"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
}

@test "layout-2 origin: visiting crew on the remote, rows in the layout-2 logs, v2 requests, crew-scope relay and reply" {
  l2_origin
  l2_dispatch
  worker_id="$(sed -n 's/^worker_id: //p' <<<"$output" | head -1)"
  [ -n "$worker_id" ]

  local rmeta="$RHOME/state/crew/crews/c1/meta.json" rlog="$RHOME/state/crew/crews/c1/events.jsonl"
  jq -e --arg h "$(jq -r .home_host "$L2_META")" \
    '.layout == 2 and .id == "c1" and .visiting == true and .home_host == $h and .home_repo == null and .created_by == "remote-dispatch"' "$rmeta"
  jq -e --arg w "$worker_id" 'select(.kind == "dispatch" and .worker_id == $w)' "$rlog"
  [ -z "$(jq -c 'select(.kind == "dispatch")' "$REMOTE_CLONE/.git/crew/events.jsonl" 2>/dev/null || true)" ]

  row="$(jq -c 'select(.kind == "dispatch")' "$L2_LOG")"
  [ "$(wc -l <<<"$row")" -eq 1 ]
  jq -e --arg w "$worker_id" '.host == "mini" and .remote == true and .worker_id == $w and .crew_layout == 2' <<<"$row"
  [ -z "$(local_dispatch_rows)" ]

  jq -s -e 'any(.[]; .argv[0] == "dispatch" and .argv[1] != "--engines" and .v == 2 and .env.CREW_LAYOUT == "2")' "$SSH_REQS"
  jq -s -e 'any(.[]; .argv[0] == "dispatch" and .argv[1] == "--engines" and .v == 2)' "$SSH_REQS"
  # dispatch created `main` here, so the new-session pin must not linger in the session env shared with other windows.
  run remote_tmux show-environment -t =main XDG_STATE_HOME
  [[ "$output" != "XDG_STATE_HOME=$RHOME/state" ]]

  wt="$(remote_wt "$(jq -r .branch <<<"$row")")"
  [ -d "$wt" ]
  start_stream --crew c1 --park 2 --retry 1 --coalesce 1 --interval 1
  on_remote "$wt" env CREW_WORKER_ID="$worker_id" crew status "$worker_id" blocked "need input"
  poll_for 300 stream_has_blocked || {
    cat "$STREAM_ERR" >&2
    cat "$STREAM_OUT" >&2
    false
  }
  jq -s -e 'any(.[]; .scope == "crew" and .argv[1] == "relay")' "$SSH_REQS"
  stop_stream

  run crew reply --crew c1 "$worker_id" "hello"
  [ "$status" -eq 0 ]
  run on_remote "$wt" crew inbox "$worker_id"
  [ "$status" -eq 0 ]
  [[ "$output" == *hello* ]]
}

# pinned_remote_session — the remote tmux server and session `main` already
# exist with a state root other than the remote dispatch's.
pinned_remote_session() {
  on_remote "$RHOME" env XDG_STATE_HOME="$BATS_TEST_TMPDIR/other" tmux new-session -d -s main
}

# last_engine_root — the state root the newest engine stub launch saw.
last_engine_root() {
  tail -1 "$BATS_TEST_TMPDIR/claude-xdg"
}

@test "layout-2 dispatch pins the worker pane's state root over a tmux server that carries another" {
  l2_origin
  pinned_remote_session
  l2_dispatch
  row="$(jq -c 'select(.kind == "dispatch")' "$L2_LOG")"
  win="$(remote_tmux list-windows -t =main -F '#{window_id}|#{@crew_branch}' | awk -F'|' -v b="$(jq -r .branch <<<"$row")" '$2 == b {print $1}')"
  [ -n "$win" ]
  poll_for 100 test -s "$BATS_TEST_TMPDIR/claude-xdg"
  [ "$(last_engine_root)" = "$RHOME/state" ]
  [ "$(remote_tmux show-options -wqv -t "$win" @crew_state_home)" = "$RHOME/state" ]
}

@test "layout-2 dispatch resume pins the relaunched worker pane's state root over a tmux server that carries another" {
  l2_origin
  pinned_remote_session
  l2_dispatch
  row="$(jq -c 'select(.kind == "dispatch")' "$L2_LOG")"
  branch="$(jq -r .branch <<<"$row")"
  wt="$(remote_wt "$branch")"
  win="$(remote_tmux list-windows -t =main -F '#{window_id}|#{@crew_branch}' | awk -F'|' -v b="$branch" '$2 == b {print $1}')"
  [ -n "$win" ]
  poll_for 100 test -s "$BATS_TEST_TMPDIR/claude-xdg"
  remote_tmux kill-window -t "$win"
  : >"$BATS_TEST_TMPDIR/claude-xdg"

  run on_remote "$wt" env CREW_ID=c1 DISPATCH_TMUX_SESSION=main dispatch resume --fresh
  [ "$status" -eq 0 ] || {
    echo "$output" >&2
    false
  }
  win="$(remote_tmux list-windows -t =main -F '#{window_id}|#{@crew_branch}' | awk -F'|' -v b="$branch" '$2 == b {print $1}')"
  [ -n "$win" ]
  poll_for 100 test -s "$BATS_TEST_TMPDIR/claude-xdg"
  [ "$(last_engine_root)" = "$RHOME/state" ]
  [ "$(remote_tmux show-options -wqv -t "$win" @crew_state_home)" = "$RHOME/state" ]
}

@test "layout-2 --spawn-role pins the lazy role pane's state root from the window stamp" {
  l2_origin
  pinned_remote_session
  l2_dispatch
  worker_id="$(sed -n 's/^worker_id: //p' <<<"$output" | head -1)"
  row="$(jq -c 'select(.kind == "dispatch")' "$L2_LOG")"
  branch="$(jq -r .branch <<<"$row")"
  wt="$(remote_wt "$branch")"
  crew_dir="$REMOTE_CLONE/.git/crew"
  mkdir -p "$crew_dir/artifacts/$branch"
  printf '{"reviewer":{"agent":"claude","model":"sonnet","effort":"medium"}}\n' >"$crew_dir/artifacts/$branch/roles.json"
  poll_for 100 test -s "$BATS_TEST_TMPDIR/claude-xdg"
  : >"$BATS_TEST_TMPDIR/claude-xdg"

  # The lead's own pane: --spawn-role must run inside the pane it names.
  local out="$BATS_TEST_TMPDIR/spawn.out"
  win="$(remote_tmux list-windows -t =main -F '#{window_id}|#{@crew_branch}' | awk -F'|' -v b="$branch" '$2 == b {print $1}')"
  remote_tmux split-window -d -t "$win" -c "$wt" -e CREW_WORKER_ID="$worker_id" \
    "dispatch --spawn-role reviewer >$out 2>&1; echo rc=\$? >>$out; sleep 60"
  poll_for 300 grep -q '^rc=' "$out" || {
    cat "$out" >&2
    false
  }
  grep -qx 'rc=0' "$out" || {
    cat "$out" >&2
    false
  }
  poll_for 100 test -s "$BATS_TEST_TMPDIR/claude-xdg"
  [ "$(last_engine_root)" = "$RHOME/state" ]
}

@test "layout-2 dispatch --host recovers a dropped link through a crew-scope dispatch-row and records crew_layout 2" {
  l2_origin
  SSH_DROP_AFTER_DISPATCH=1 DISPATCH_HOST_RECOVER_TRIES=3 DISPATCH_HOST_RECOVER_SLEEP=1 \
    run --separate-stderr dispatch_remote --host mini 42 "Recover layout two"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [[ "$stderr" == *"link dropped"* ]]
  jq -s -e 'any(.[]; .scope == "crew" and .argv[0] == "crew" and .argv[1] == "dispatch-row" and .v == 2)' "$SSH_REQS"
  row="$(jq -c 'select(.kind == "dispatch")' "$L2_LOG")"
  [ "$(wc -l <<<"$row")" -eq 1 ]
  jq -e '.host == "mini" and .remote == true and .crew_layout == 2' <<<"$row"
  [ -d "$(remote_wt "$(jq -r .branch <<<"$row")")" ]
  [ -z "$(local_dispatch_rows)" ]
}

@test "layout-2 dispatch --host against a remote that speaks request v1 only fails closed and records nothing" {
  l2_origin
  cat >"$RBINS/crew" <<EOF
#!/usr/bin/env bash
if [ "\${1:-}" = remote-exec ]; then
  req="\$(cat)"
  if [ "\$(jq -r .v <<<"\$req")" = 2 ]; then
    echo 'crew: remote-exec: unsupported request version 2' >&2
    exit 2
  fi
  exec "$STUB_DIR/crew" "\$@" <<<"\$req"
fi
exec "$STUB_DIR/crew" "\$@"
EOF
  chmod +x "$RBINS/crew"
  run --separate-stderr dispatch_remote --host mini 42 "Old remote"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"speaks request v1 only"* ]]
  [[ "$stderr" == *"'mini'"* ]]
  assert_nothing_recorded_locally
  [ -z "$(jq -c 'select(.kind == "dispatch")' "$L2_LOG")" ]
  [ "$(git -C "$REMOTE_CLONE" worktree list --porcelain | grep -c '^worktree ')" -eq 1 ]
}

@test "layout-2 dispatch --host refuses a remote dispatch that does not confirm layout 2, recording nothing" {
  l2_origin
  cat >"$RBINS/dispatch" <<EOF
#!/usr/bin/env bash
"$STUB_DIR/dispatch" "\$@" | grep -vx 'crew_layout: 2'
EOF
  chmod +x "$RBINS/dispatch"
  run --separate-stderr dispatch_remote --host mini 42 "Unconfirmed"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"did not confirm layout 2"* ]]
  assert_nothing_recorded_locally
  [ -z "$(jq -c 'select(.kind == "dispatch")' "$L2_LOG")" ]
}

@test "layout-2 dispatch --host refuses a host holding a pre-layout-2 remote worker of the crew, before any ssh" {
  l2_origin
  printf '%s\n' '{"kind":"dispatch","remote":true,"host":"mini","crew_id":"c1","branch":"feat/1-old","ts":1}' >>"$L2_LOG"
  run --separate-stderr dispatch_remote --host mini 42 "Guarded"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"feat/1-old"* ]]
  [ ! -e "$SSH_LOG" ]
  assert_nothing_recorded_locally
}

@test "crew remote-run --crew-scope for a crew the remote never admitted exits 2 naming it" {
  run --separate-stderr crew remote-run --host mini --crew-scope --env CREW_ID=c9 --env DISPATCH_ORIGIN_HOST=originbox \
    -- crew dispatch-row --crew c9 --session s1-1
  [ "$status" -eq 2 ]
  [[ "$stderr" == *c9* ]]
  [[ "$stderr" == *"is not a crew on this host"* ]]
}
