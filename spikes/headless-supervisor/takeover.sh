#!/usr/bin/env bash
# Hand a running `hsup serve` session to its real TUI in a new tmux window,
# then back to headless once the TUI exits. Both sides only ever run under
# <state>/session.lock, so they cannot write the session at the same time.
set -euo pipefail

usage() {
  echo "usage: takeover.sh <state-dir> [--tmux-socket NAME]" >&2
  exit 2
}

[[ $# -ge 1 ]] || usage
state=$1
shift
tmux_args=()
while [[ $# -gt 0 ]]; do
  case $1 in
  --tmux-socket)
    [[ $# -ge 2 ]] || usage
    tmux_args=(-L "$2")
    shift 2
    ;;
  *) usage ;;
  esac
done
if [[ ${#tmux_args[@]} -eq 0 && -z ${TMUX:-} ]]; then
  echo "takeover.sh: not inside tmux; run it from a tmux pane or pass --tmux-socket" >&2
  exit 2
fi
for c in hsup flock tmux; do
  command -v "$c" >/dev/null || {
    echo "takeover.sh: $c not on PATH" >&2
    exit 2
  }
done
state=$(cd "$state" && pwd)
lockfile=$state/session.lock

die() {
  echo "takeover.sh: $*" >&2
  exit 1
}

if ! out=$(hsup ctl "$state" pause 2>&1); then
  [[ $out == *"already paused"* ]] || die "pause failed: $out"
fi
echo "takeover: paused ($out)"

# session.json is printed as key=value lines; values are never evaluated, only
# matched against the shapes below before they reach an argv.
info=$(hsup ctl "$state" session)
engine='' id='' work='' bin='' model='' sdir='' gate=''
while IFS= read -r line; do
  val=${line#*=}
  case ${line%%=*} in
  engine) engine=$val ;;
  session_id) id=$val ;;
  work) work=$val ;;
  bin) bin=$val ;;
  model) model=$val ;;
  session_dir) sdir=$val ;;
  gate) gate=$val ;;
  esac
done <<<"$info"

abs_path='^/[A-Za-z0-9._/+@-]+$'
[[ $id =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "bad session id: $id"
[[ $work =~ $abs_path && -d $work ]] || die "bad work dir: $work"
[[ $bin =~ $abs_path && -x $bin ]] || die "bad binary: $bin"
[[ $model =~ ^[A-Za-z0-9][A-Za-z0-9._/:-]*$ ]] || die "bad model: $model"
case $engine in
claude)
  tui=("$bin" --resume "$id" --setting-sources local --strict-mcp-config --model "$model")
  ;;
pi)
  [[ $sdir =~ $abs_path ]] || die "bad pi session dir: $sdir"
  [[ $gate =~ $abs_path && -f $gate ]] || die "bad pi gate: $gate"
  tui=("$bin" --session "$id" --session-dir "$sdir" --model "$model"
    --no-extensions --no-skills --no-prompt-templates --no-context-files --no-mcp -e "$gate")
  ;;
*) die "engine $engine has no TUI take-over" ;;
esac

if ! flock -n -E 75 "$lockfile" true; then
  die "$lockfile is held by another process; refusing to open a second writer (headless stays paused)"
fi

sock=$(tmux "${tmux_args[@]}" display-message -p '#{socket_path}')
chan=hsup-takeover-$$-$RANDOM
rcdir=$(mktemp -d)
trap 'rm -rf -- "$rcdir"' EXIT

# Runs in the window: strip worker identity, hold the lock for the TUI's whole
# life (-o keeps it off the TUI's own children), record its exit code, and
# wake the waiter even when the pane is killed.
# shellcheck disable=SC2016 # expanded by the window's bash, not here
inner='sock=$1 chan=$2 rcf=$3 lk=$4
shift 4
trap '\''tmux -S "$sock" wait-for -S "$chan"'\'' EXIT
unset CREW_WORKER_ID CREW_ID TMUX TMUX_PANE CLAUDECODE CLAUDE_CODE_CHILD_SESSION CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SESSION_ID CLAUDE_CODE_EXECPATH CLAUDE_CODE_SUBAGENT_EFFORT CLAUDE_CODE_SSE_PORT "${!CREW_LEAD_@}"
rc=0
flock -o -n -E 75 "$lk" "$@" || rc=$?
printf "%s\n" "$rc" >"$rcf"'

pane=$(tmux "${tmux_args[@]}" new-window -P -F '#{pane_id}' -n hsup-takeover -c "$work" -- \
  bash -c "$inner" hsup-takeover "$sock" "$chan" "$rcdir/rc" "$lockfile" "${tui[@]}")
echo "takeover: pane=$pane engine=$engine session=$id"
echo "takeover: waiting for the TUI to exit"
tmux "${tmux_args[@]}" wait-for "$chan"

rc=$(cat "$rcdir/rc" 2>/dev/null || echo unknown)
if [[ $rc == 75 ]]; then
  die "the TUI could not take $lockfile; headless stays paused"
fi
echo "takeover: TUI exited (rc=$rc); resuming headless"
hsup ctl "$state" resume
hsup ctl "$state" status
