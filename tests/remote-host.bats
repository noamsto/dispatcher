bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# dispatch --host end to end, against a fake remote host: an `ssh` shim that runs
# the command in a second environment (own HOME, XDG dirs, tmux server and
# clone of the same origin) on this machine. Everything else is real: dispatch,
# crew, remote-run/remote-exec, the relay and git. Only wt, gh, direnv, the
# engine and og-remote-open are stubs.

setup() {
  load helpers
  REPO_ROOT="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  # The bins dir is setup_repo's stub dir, so its engine stubs land here too.
  STUB_DIR="$BATS_TEST_TMPDIR/bins"
  mkdir -p "$STUB_DIR"
  setup_repo
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"

  # Local side: its own HOME, so nothing of the developer's reaches dispatch.
  LHOME="$BATS_TEST_TMPDIR/lhome"
  mkdir -p "$LHOME"
  export HOME="$LHOME"
  export XDG_STATE_HOME="$BATS_TEST_TMPDIR/lstate"
  unset PI_CODING_AGENT_DIR DISPATCH_REPO_TRACKERS DISPATCH_ORG_TRACKERS

  # Remote side: its own HOME, XDG dirs and tmux server. The tmux socket dir
  # is short because a unix socket path is capped near 100 bytes.
  RHOME="$BATS_TEST_TMPDIR/rhome"
  mkdir -p "$RHOME"
  RTMUX_ROOT="$(mktemp -d)"
  RTMUX="$RTMUX_ROOT/t"
  mkdir -p "$RTMUX"
  export RHOME RTMUX

  # A bare origin whose path gives the slug acme/widget; the local repo and the
  # remote clone are two clones of it.
  ORIGIN="$BATS_TEST_TMPDIR/acme/widget.git"
  mkdir -p "${ORIGIN%/*}"
  git init -q --bare -b main "$ORIGIN"
  git -C "$TEST_REPO" commit --allow-empty -q -m init
  git -C "$TEST_REPO" remote add origin "$ORIGIN"
  git -C "$TEST_REPO" push -q origin main
  mkdir -p "$BATS_TEST_TMPDIR/remote"
  git clone -q "$ORIGIN" "$BATS_TEST_TMPDIR/remote/widget"
  REMOTE_CLONE="$(cd "$BATS_TEST_TMPDIR/remote/widget" && pwd -P)"
  git -C "$REMOTE_CLONE" config user.email test@example.com
  git -C "$REMOTE_CLONE" config user.name test

  LBUS="$(git -C "$TEST_REPO" rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CLAIMS="$XDG_DATA_HOME/crew/claims/acme/widget.jsonl"

  # What a real (unstubbed) dispatch.sh run needs, on either side.
  export DISPATCHER_PROTOCOL_DIR="$BATS_TEST_TMPDIR/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch_worker_protocols "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR"/{EVIDENCE_REVIEW.md,GRID_PROTOCOL.md,REVIEW_TASK.md}
  export DISPATCHER_SKILLS_DIR="$BATS_TEST_TMPDIR/harness-skills"
  mkdir -p "$DISPATCHER_SKILLS_DIR/spec-plan-critic"
  printf -- '---\nname: spec-plan-critic\ndescription: seeded\n---\n' \
    >"$DISPATCHER_SKILLS_DIR/spec-plan-critic/SKILL.md"
  export CROSS_REPO_HINT_LIB="$REPO_ROOT/adapters/core/cross-repo-hint.sh"
  export CLAUDE_WORKER_SETTINGS_LIB="$REPO_ROOT/adapters/core/claude-worker-settings.sh"

  # The hosts both sides read: the local user layer, and the same file in the
  # remote's config dir. `bare` has no clone mapped.
  local settings
  settings="$(jq -nc --arg r "$REMOTE_CLONE" '{hosts: {
    mini: {ssh: "mini", repos: {"acme/widget": $r}},
    chromebox: {ssh: "mini", chrome: true, repos: {"acme/widget": $r}},
    broken: {ssh: "fail-broken", repos: {"acme/widget": $r}},
    bare: {ssh: "mini", repos: {}}}}')"
  mkdir -p "$XDG_CONFIG_HOME/dispatcher" "$RHOME/config/dispatcher"
  printf '%s\n' "$settings" >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  printf '%s\n' "$settings" >"$RHOME/config/dispatcher/settings.json"

  export SSH_LOG="$BATS_TEST_TMPDIR/ssh.targets"
  export SSH_CMDS="$BATS_TEST_TMPDIR/ssh.commands"
  export SSH_COUNT="$BATS_TEST_TMPDIR/ssh.count"
  export SSH_DOWN="$BATS_TEST_TMPDIR/ssh.down"
  export SSH_PIDS="$BATS_TEST_TMPDIR/ssh.pids"
  export OG_LOG="$BATS_TEST_TMPDIR/og-remote-open.log"
  unset SSH_FAIL_AFTER SSH_DROP_AFTER_DISPATCH SSH_DOWN_AFTER_DROP
  make_bins
  write_remote_env
}

# write_remote_env — the remote side's whole environment, NUL-separated in
# $REMOTE_ENV for `env -i`: what sshd sets plus the harness pins a real
# dispatch.sh run needs. Nothing else of the local side (its CREW_ID, TMUX,
# DISPATCH_* overrides) reaches the remote.
write_remote_env() {
  local v
  local -a vars=(PATH="$PATH" HOME="$RHOME" TMPDIR="${TMPDIR:-/tmp}" SHELL=/bin/sh
    XDG_DATA_HOME="$RHOME/data" XDG_CONFIG_HOME="$RHOME/config" XDG_STATE_HOME="$RHOME/state"
    TMUX_TMPDIR="$RTMUX")
  for v in USER LOGNAME LANG LC_ALL GIT_CONFIG_GLOBAL CREW_GO_BIN CREW_RATE_AUTOSWEEP DISPATCH_LOADAVG_FILE \
    DISPATCHER_PROTOCOL_DIR DISPATCHER_SKILLS_DIR CROSS_REPO_HINT_LIB CLAUDE_WORKER_SETTINGS_LIB \
    DISPATCH_CONFIG_BIN GRANT_CHECK_LIB LOCAL_MODELS_LIB BUDGET_GATE_LIB WORKTREE_GIT_LIB \
    BASH_ENV BATS_COVERAGE_FILE; do
    [ -z "${!v+x}" ] || vars+=("$v=${!v}")
  done
  export REMOTE_ENV="$BATS_TEST_TMPDIR/remote.env"
  printf '%s\0' "${vars[@]}" >"$REMOTE_ENV"
}

teardown() {
  stop_stream
  # Anything an ssh call left running (a relay whose stream died unclean).
  if [ -r "${SSH_PIDS:-/nonexistent}" ]; then
    local pid cmd
    while read -r pid; do
      cmd="$(ps -o command= -p "$pid" 2>/dev/null || true)"
      case "$cmd" in
      *remote-exec*)
        pkill -P "$pid" 2>/dev/null || true
        kill "$pid" 2>/dev/null || true
        ;;
      esac
    done <"$SSH_PIDS"
  fi
  if [ -n "${RTMUX:-}" ]; then
    TMUX_TMPDIR="$RTMUX" "$REAL_TMUX" kill-server 2>/dev/null || true
  fi
  [ -z "${RTMUX_ROOT:-}" ] || rm -rf "$RTMUX_ROOT"
  teardown_repo
}

# make_bins — the dir first on PATH for BOTH sides.
make_bins() {
  local tool script
  # crew: the real binary, except the two long-lived helpers dispatch starts.
  cat >"$STUB_DIR/crew" <<EOF
#!/usr/bin/env bash
case "\${1:-}" in
stall-watch | roster-render) exit 0 ;;
esac
exec "$CREW_GO_BIN" "\$@"
EOF
  # The nix build prepends writeShellApplication's strict mode; a raw run needs it spelled.
  for tool in dispatch:dispatch.sh dispatch-resume:dispatch-resume.sh permission-check:permission-check.sh; do
    script="${tool#*:}"
    cat >"$STUB_DIR/${tool%%:*}" <<EOF
#!/usr/bin/env bash
exec bash -euo pipefail "$REPO_ROOT/adapters/core/$script" "\$@"
EOF
  done

  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
case "$*" in
repo\ view\ *) printf '%s\n' "${STUB_DEFAULT_BRANCH:-main}" ;;
esac
exit 0
EOF
  # wt creates the worktree under the repo it runs in, so it serves either side.
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = switch ]; then
  br="" base=""
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
    -c)
      br="$2"
      shift 2
      ;;
    -b)
      base="$2"
      shift 2
      ;;
    *) shift ;;
    esac
  done
  [ -n "$br" ] || exit 1
  root="$(git worktree list --porcelain | awk '/^worktree /{print $2; exit}')"
  mkdir -p "$root/.worktrees"
  git worktree add -q -b "$br" "$root/.worktrees/${br//\//-}" "${base:-HEAD}"
fi
exit 0
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' >"$STUB_DIR/direnv"
  # The worker pane runs this: alive until the tmux server is killed.
  cat >"$STUB_DIR/claude" <<'EOF'
#!/usr/bin/env bash
exec sleep 600
EOF
  cat >"$STUB_DIR/og-remote-open" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$OG_LOG"
exit 0
EOF

  # Real tmux for the remote's private TMUX_TMPDIR. Panes get /bin/sh: the
  # developer's shell rc must not decide whether the stub claude is found.
  TMUX_CONF="$BATS_TEST_TMPDIR/tmux.conf"
  printf 'set -g default-shell /bin/sh\n' >"$TMUX_CONF"
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
exec 3>&-
if [ -n "\${TMUX_PANE:-}" ] && [ "\${1:-}" = display-message ]; then
  case "\$*" in *'#{session_name}'*) echo main; exit 0 ;; esac
fi
exec "$REAL_TMUX" -f "$TMUX_CONF" "\$@"
EOF

  # ssh: skip -T, `-o X` pairs and `--`, take the target, run the command words
  # in the remote environment the way sshd would (cwd = the remote home, never a
  # repo; no tmux/crew identity), stdin passed through.
  cat >"$STUB_DIR/ssh" <<'EOF'
#!/usr/bin/env bash
exec 3>&-
while [ $# -gt 0 ]; do
  case "$1" in
  -T) shift ;;
  -o) shift 2 ;;
  --)
    shift
    break
    ;;
  *) break ;;
  esac
done
target="${1:?ssh shim: no target}"
shift
printf '%s\n' "$target" >>"$SSH_LOG"
printf '%s\n' "$*" >>"$SSH_CMDS"

case "$target" in
fail-*)
  [ -n "${SSH_FAIL_AFTER:-}" ] || exit 255
  n="$(cat "$SSH_COUNT" 2>/dev/null || echo 0)"
  [ "$n" -lt "$SSH_FAIL_AFTER" ] || exit 255
  echo $((n + 1)) >"$SSH_COUNT"
  ;;
esac
[ ! -e "$SSH_DOWN" ] || exit 255

# Drop mode: the link dies after the dispatch call has run, so its output is lost.
drop=""
if [ -n "${SSH_DROP_AFTER_DISPATCH:-}" ]; then
  req="$(mktemp "$BATS_TEST_TMPDIR/ssh-req.XXXXXX")"
  cat >"$req"
  if jq -e '.argv[0] == "dispatch" and .argv[1] != "--engines"' "$req" >/dev/null 2>&1; then
    drop=1
  fi
  exec <"$req"
fi

cd "$RHOME" || exit 254
mapfile -d '' -t renv <"$REMOTE_ENV"
remote_env=(env -i "${renv[@]}")
if [ -n "$drop" ]; then
  "${remote_env[@]}" sh -c "$*" >/dev/null || true
  [ -z "${SSH_DOWN_AFTER_DROP:-}" ] || : >"$SSH_DOWN"
  exit 255
fi
echo $$ >>"$SSH_PIDS"
exec "${remote_env[@]}" sh -c "$*"
EOF
  chmod +x "$STUB_DIR"/*
}

# on_remote <dir> <cmd...> — run a command as a process on the remote host, from
# <dir>. Extra environment goes in front of the command: on_remote d env X=1 cmd.
on_remote() {
  local dir="$1"
  shift
  local -a renv
  mapfile -d '' -t renv <"$REMOTE_ENV"
  (cd "$dir" && env -i "${renv[@]}" "$@")
}

poll_for() { # <tries> <cmd...> — retry every 0.1s
  local tries="$1" i=0
  shift
  while [ "$i" -lt "$tries" ]; do
    "$@" && return 0
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

remote_tmux() { TMUX_TMPDIR="$RTMUX" "$REAL_TMUX" "$@"; }

# remote_wt <branch> — the remote clone's worktree for <branch>.
remote_wt() {
  git -C "$REMOTE_CLONE" worktree list --porcelain |
    awk -v b="branch refs/heads/$1" '/^worktree /{p = $2} $0 == b {print p}'
}

local_dispatch_rows() {
  [ -f "$LBUS" ] || return 0
  jq -c 'select(.kind == "dispatch")' "$LBUS"
}

# assert_nothing_recorded_locally — no bus row, claims-index row or dispatch lock.
assert_nothing_recorded_locally() {
  [ -z "$(local_dispatch_rows)" ]
  [ ! -s "$CLAIMS" ]
  [ -z "$(find "${LBUS%/*}" -name 'dispatch-*.lock*' 2>/dev/null)" ]
}

# Every ssh call's remote command is the fixed literal, whatever the caller sent.
assert_ssh_command_is_literal() {
  [ -s "$SSH_CMDS" ]
  run ! grep -vxF 'crew remote-exec' "$SSH_CMDS"
}

STREAM_PID=""
start_stream() {
  STREAM_OUT="$BATS_TEST_TMPDIR/stream.out"
  STREAM_ERR="$BATS_TEST_TMPDIR/stream.err"
  (cd "$TEST_REPO" && exec crew stream "$@") >"$STREAM_OUT" 2>"$STREAM_ERR" </dev/null 3>&- &
  STREAM_PID=$!
}

stop_stream() {
  [ -n "${STREAM_PID:-}" ] || return 0
  kill "$STREAM_PID" 2>/dev/null || true
  local i
  for i in $(seq 50); do
    kill -0 "$STREAM_PID" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$STREAM_PID" 2>/dev/null; then
    kill -9 "$STREAM_PID" 2>/dev/null || true
  fi
  wait "$STREAM_PID" 2>/dev/null || true
  STREAM_PID=""
}

stream_has_blocked() {
  jq -e 'select(.host == "mini") | .events[] | select(.body.state == "blocked")' \
    "$STREAM_OUT" >/dev/null 2>&1
}

dispatch_remote() { # <extra dispatch args...> — a standard dispatch to crew c1, issue 42
  dispatch standard sonnet --effort medium --crew-id c1 "$@"
}

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
  branch="$(jq -r .branch <<<"$row")"
  wt="$(remote_wt "$branch")"
  [ -d "$wt" ]
  grep -qx 'crew_id: c1' "$wt/WORKER_TASK.md"
  grep -Eq '^dispatcher_pane: ?$' "$wt/WORKER_TASK.md"
  grep -qF 'Body line.' "$wt/WORKER_TASK.md"
  remote_tmux list-windows -t =crew-c1 -F '#{@crew_branch}' | grep -qxF "$branch"
  # The dispatch that started the remote tmux server left no CREW_ or
  # DISPATCH_ var (CREW_ID among them) in the server's global environment.
  # Names only, never values.
  [ "$(remote_tmux show-environment -g | cut -d= -f1 | grep -cE '^(CREW|DISPATCH)_')" -eq 0 ]
  grep -qF 'mini crew-c1 --into main' "$OG_LOG"
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
  remote_tmux kill-window -t "$(remote_tmux list-windows -t =crew-c1 -F '#{window_id}' | head -1)"
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
  PATH="\${PATH#$STUB_DIR:}" exec uname "\$@"
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
  grep -qF 'mini crew-c1 --into main' "$OG_LOG"

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
