#!/usr/bin/env bash
# Automated take-over proof on a scratch tmux server: a headless turn, a TUI
# turn under the session lock (headless resume refused meanwhile), then a
# headless turn that must recall both. The last line is PASS or FAIL.
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "usage: demo-takeover.sh <claude|pi> <transcript-path> [work-dir]" >&2
  exit 2
fi
engine=$1
transcript=$2
work=${3:-}
case $engine in
claude | pi) ;;
*)
  echo "demo-takeover.sh: unknown engine $engine" >&2
  exit 2
  ;;
esac
S=$(cd "$(dirname "$0")" && pwd)
for c in hsup flock tmux; do
  command -v "$c" >/dev/null || {
    echo "FAIL demo-takeover $engine: $c not on PATH"
    exit 1
  }
done

d=$(mktemp -d)
unset TMUX TMUX_PANE
export TMUX_TMPDIR=$d/tmux
mkdir -m 700 "$TMUX_TMPDIR"
srv=hsup-demo-$$
state=$d/state
[[ -n $work ]] || work=$d/work
mkdir -p "$work" "$(dirname "$transcript")"
serve_pid=''
take_pid=''

t() { tmux -L "$srv" "$@"; }

cleanup() {
  if [[ -n $serve_pid ]] && kill -0 "$serve_pid" 2>/dev/null; then
    timeout 60 hsup ctl "$state" stop >/dev/null 2>&1 || true
    for _ in $(seq 60); do
      kill -0 "$serve_pid" 2>/dev/null || break
      sleep 1
    done
    kill "$serve_pid" 2>/dev/null || true
  fi
  if [[ -n $take_pid ]]; then
    kill "$take_pid" 2>/dev/null || true
  fi
  t kill-server >/dev/null 2>&1 || true
  rm -rf -- "$d"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

log() { echo "demo: $*"; }
fail() {
  # The full screen can show account details, so it goes to a file only on request.
  if [[ -n ${HSUP_DEMO_DUMP:-} && -n ${pane:-} ]]; then
    t capture-pane -p -J -t "$pane" >"$HSUP_DEMO_DUMP" 2>&1 || true
  fi
  echo "FAIL demo-takeover $engine: $*"
  exit 1
}

sid() { hsup ctl "$state" session | sed -n 's/^session_id=//p'; }

# Lines of the pane mentioning a codeword or a bare "ok" reply; the rest of the
# screen (banner, account line) stays out of the log.
pane_evidence() {
  t capture-pane -p -J -t "$pane" | grep -E 'HEADLESS-1|TUI-2208|(^|[^A-Za-z])[Oo][Kk]([^A-Za-z]|$)' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | head -20 || true
}

hsup serve --engine "$engine" --work "$work" --state "$state" --transcript "$transcript" >"$d/serve.log" 2>&1 &
serve_pid=$!
for _ in $(seq 120); do
  hsup ctl "$state" status >/dev/null 2>&1 && break
  kill -0 "$serve_pid" 2>/dev/null || fail "serve exited: $(tail -3 "$d/serve.log")"
  sleep 0.5
done
hsup ctl "$state" status >/dev/null 2>&1 || fail "serve never answered status"
log "serve up: $(hsup ctl "$state" status)"

r1=$(hsup ctl "$state" send "Remember codeword HEADLESS-1. Reply ok.") || fail "turn 1 failed: $r1"
log "turn 1 reply: $r1"
sid_before=$(sid) || fail "cannot read session.json"
log "session before take-over: $sid_before"

t -f /dev/null new-session -d -s demo -x 220 -y 50
# Keep a dead TUI pane readable so a failure can still be inspected.
t set-option -wg remain-on-exit on
"$S/takeover.sh" "$state" --tmux-socket "$srv" >"$d/takeover.log" 2>&1 &
take_pid=$!

pane=''
for _ in $(seq 60); do
  pane=$(sed -n 's/^takeover: pane=\(%[0-9]*\) .*/\1/p' "$d/takeover.log")
  [[ -n $pane ]] && break
  kill -0 "$take_pid" 2>/dev/null || fail "takeover.sh exited early: $(tail -3 "$d/takeover.log")"
  sleep 0.5
done
[[ -n $pane ]] || fail "takeover.sh never opened a pane"
log "TUI pane $pane"

trusted=0
ready=0
for _ in $(seq 90); do
  kill -0 "$take_pid" 2>/dev/null || fail "takeover.sh exited before the TUI was ready: $(tail -3 "$d/takeover.log")"
  cap=$(t capture-pane -p -J -t "$pane" 2>/dev/null || true)
  # The trust dialog defaults to "No, exit": move the cursor to the Yes line
  # and confirm only once it is selected.
  if [[ $engine == claude && $trusted -lt 5 ]] && grep -q 'Yes, I trust this folder' <<<"$cap"; then
    trusted=$((trusted + 1))
    if grep -Eq '❯ *(2\. *)?Yes, I trust' <<<"$cap"; then
      log "accepting claude workspace-trust dialog"
      t send-keys -t "$pane" Enter
      sleep 2
    else
      t send-keys -t "$pane" Down
      sleep 1
    fi
    continue
  fi
  if grep -q 'HEADLESS-1' <<<"$cap"; then
    ready=1
    break
  fi
  sleep 1
done
[[ $ready -eq 1 ]] || fail "TUI never showed the resumed conversation"
sleep 2
log "TUI ready (history shows HEADLESS-1)"

if out=$(hsup ctl "$state" resume 2>&1); then
  fail "headless resume succeeded while the TUI held the lock: $out"
fi
[[ $out == *"a TUI owns this session"* ]] || fail "resume refused for another reason: $out"
log "lock-held refusal: $out"

t send-keys -t "$pane" -l "Remember a second codeword TUI-2208. Reply ok."
sleep 1
t send-keys -t "$pane" C-m
replied=0
for i in $(seq 120); do
  cap=$(t capture-pane -p -J -t "$pane") || fail "TUI pane vanished"
  if sed -n '/TUI-2208/,$p' <<<"$cap" | tail -n +2 | grep -Eq '(^|[^A-Za-z])[Oo][Kk]([^A-Za-z]|$)'; then
    replied=1
    break
  fi
  # A submit swallowed while the TUI was still settling leaves the text in the editor.
  if [[ $i -eq 15 ]]; then
    t send-keys -t "$pane" C-m
  fi
  sleep 1
done
[[ $replied -eq 1 ]] || log "WARN no ok reply seen in the TUI"
log "TUI evidence: $(pane_evidence | tr '\n' '|')"
sleep 3

if [[ $engine == claude ]]; then
  t send-keys -t "$pane" -l "/exit"
  sleep 1
  t send-keys -t "$pane" C-m
else
  t send-keys -t "$pane" C-d
fi
for _ in $(seq 120); do
  kill -0 "$take_pid" 2>/dev/null || break
  sleep 1
done
kill -0 "$take_pid" 2>/dev/null && fail "TUI did not exit after quit"
take_rc=0
wait "$take_pid" || take_rc=$?
take_pid=''
while IFS= read -r line; do log "| $line"; done <"$d/takeover.log"
[[ $take_rc -eq 0 ]] || fail "takeover.sh exited $take_rc"
sid_after=$(sid) || fail "cannot read session.json"
log "session after hand-back: $sid_after"

r3=$(hsup ctl "$state" send "List both codewords you were given, nothing else.") || fail "recall turn failed: $r3"
log "recall reply: $r3"
missing=()
for w in HEADLESS-1 TUI-2208; do
  [[ $r3 == *"$w"* ]] || missing+=("$w")
done
[[ ${#missing[@]} -eq 0 ]] || fail "recall missing ${missing[*]} (session $sid_before -> $sid_after)"
echo "PASS demo-takeover $engine: resume refused under TUI lock; both codewords recalled after hand-back (session $sid_before -> $sid_after)"
