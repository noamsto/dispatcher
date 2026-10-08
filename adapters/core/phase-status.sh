#!/usr/bin/env bash
# Phase-status hook: post a dispatched worker's phase from its own tool calls,
# so the roster moves even when the model never writes a status (#839).
#
# pi has no native subagents, so a whole run is often one long turn, and a small
# model drops a status instruction buried in an ~80 KB protocol — the roster sat
# at `plan` for two local workers while one implemented and the other had
# finished its tests. Deriving the phase mechanically from the tool stream is
# what removes the model from the loop.
#
# Two hookyard registrations of this one script:
#
#   post_tool  fire_and_forget  every phase except `awaiting`
#   pre_tool   verdict lane     `awaiting <role>` only
#
# `awaiting <role>` has to come from pre_tool: `crew await --from role:…` is a
# blocking Bash call, so pi's tool_result — the event post_tool rides — fires
# only after the verdict lands. pre_tool is a guard slot on pi (hookyard's
# verdict.HasGuardSlot), and a fire-and-forget handler is refused there, so that
# entry sits in the verdict lane and ALWAYS ABSTAINS: no stdout, exit 0, it can
# never gate the call it observes.
#
# Workers only, and leads only: a session without CREW_WORKER_ID (a human at a
# prompt, a dispatcher) exits before jq and the git call, and a grid role pane
# inherits the lead's CREW_WORKER_ID while its own tool calls are not the lead's
# phase — posting them would scramble the lead's phase and race it for one state
# file.
#
# What it posts: `crew status "$CREW_WORKER_ID" working "<phase> (auto)"`, only
# when the phase CHANGED (a loop of `bats` calls posts once). `(auto)` is the
# marker: `crew status` builds its row as {state, detail, pr_url, restamp}, so
# there is no source field to set from here.
#
# Two rules keep the worker's own words on top, and both read the bus rather
# than a mirror of observed `crew status` calls — a mirror latches on the
# refused ones (`crew status … pr_open` is routinely refused until the seams
# exist, and a premature attempt would silence the handler for the rest of the
# run, which is the bug this hook exists to fix):
#
#   * never overwrite a non-working state the worker posted itself;
#   * never post after a terminal one.
#
# That read is bounded and rare: only after the throttle says the phase changed,
# so a handful of times per run, over the last 2000 rows — the window crew.sh's
# _unread_scan accepts. A watchdog `blocked` row is not suppressing: dispatch
# spawns `crew stall-watch "$worker_id"` and crew.sh posts under that session id,
# so it would otherwise latch the handler shut on exactly the lane #839 targets
# (WORKER_PROTOCOL.md: a watchdog `blocked` means you are alive, carry on). The
# residual cuts both ways — a row older than the window reads as absent, so a
# duplicate `working` post is harmless and an aged-out terminal row can be
# overwritten — and the throttle is what keeps either rare.
#
# Accepted imprecision, on purpose: a phase names what a call was attempting,
# not its outcome (a failed `git push` still marks `pr`), and two parallel tool
# calls can both post one phase. It is a read of the command string, not a shell:
# an operand assembled from a variable (`bats $t`) is not classified, a
# here-document body is skipped rather than parsed (see classify_command), and a
# command hidden inside another program's argument is invisible — each of those
# costs a missed phase, never a wrong one, until the next transition.
#
# Portable to macOS's bash 3.2 and BSD userland: no mapfile, no ${var,,}.

set -euo pipefail

# Workers only, ahead of everything else.
[[ -n ${CREW_WORKER_ID:-} ]] || exit 0
[[ -z ${CREW_ROLE_ID:-} ]] || exit 0

command -v jq >/dev/null || exit 0
command -v crew >/dev/null || exit 0

input=$(cat)

# The envelope hookyard hands every handler (json.Marshal of
# internal/envelope.Envelope): {engine, canonical_event, native_event,
# session_id, cwd, protocol, tool_name, tool_input, native}. tool_name is
# already normalized (bash -> Bash, write -> Write); pi's `edit` has no mapping
# row and arrives as `edit`. Claude's PostToolUse carries the same keys, which
# is what lets a claude PostToolUse hook reuse this script later (#839).
# shellcheck disable=SC2016 # jq program, not a bash format string
assign=$(jq -r '
  def s: if type == "string" then . else "" end;
  if type != "object" then empty else
  (.canonical_event | s) as $ce
  | (.hook_event_name | s) as $ev
  | (if $ce == "post_tool" then "post"
     elif $ce == "pre_tool" then "pre"
     elif $ev == "PostToolUse" then "post"
     elif $ev == "PreToolUse" then "pre"
     else empty end) as $when
  | (.tool_name | s | ascii_downcase) as $tool
  | (.tool_input | if type == "object" then . else {} end) as $ti
  | (if $tool == "bash" or $tool == "shell" then ($ti.command | s) else "" end) as $cmd
  | (if $tool == "write" or $tool == "edit" or $tool == "multiedit" or $tool == "apply_patch"
     then ((($ti.path // $ti.file_path) | s)) else "" end) as $path
  | ((.cwd | s)) as $cwd
  | "when=" + ($when | @sh) + " cmd=" + ($cmd | @sh) +
    " path=" + ($path | @sh) + " cwd=" + ($cwd | @sh)
  end' <<<"$input" 2>/dev/null) || exit 0
[[ -n $assign ]] || exit 0
# Defaults first: the eval below owns these four, and shellcheck cannot see past
# it (SC2154) while a partial eval would otherwise leave one unset under -u.
when='' cmd='' path='' cwd=''
# @sh owns the quoting, so a multi-line command survives intact; nothing here
# came from outside the jq program that produced it.
eval "$assign"
# Resolve the session directory before classifying: the plan-doc match is against
# paths under it. (The git calls stay behind the "is there a phase at all" check,
# so an inert command pays neither.)
[[ -n $cwd && -d $cwd ]] || cwd=$PWD

phase=''

# Highest-precedence match wins inside one compound command: `git add && git
# commit && git push` is `pr`, not `commit`. `awaiting` and the plan-critic
# message outrank everything because they name what the call is doing rather
# than where the run has progressed to.
rank_of() {
  case "$1" in
  awaiting\ *) echo 6 ;;
  plan) echo 6 ;;
  pr) echo 4 ;;
  ci) echo 3 ;;
  commit) echo 2 ;;
  test) echo 1 ;;
  *) echo 0 ;;
  esac
}
note_phase() { # <phase>
  local r
  r=$(rank_of "$1")
  if ((r >= $(rank_of "${phase:-}"))); then phase=$1; fi
}

# Quote-aware split: one word per line, an empty line wherever a shell control
# operator or a newline ends a command. Quotes are stripped, never interpreted —
# this hook never evaluates the command it is reading. Redirections break a
# command too, so `> git` is never a command position.
split_words() { # <command>
  local s=$1 i=0 n=${#1} c d q w=''
  while ((i < n)); do
    c=${s:i:1}
    case $c in
    ' ' | $'\t')
      if [[ -n $w ]]; then
        printf '%s\n' "$w"
        w=''
      fi
      ;;
    ';' | '|' | '&' | '(' | ')' | '`' | '<' | '>' | $'\n' | $'\r')
      if [[ -n $w ]]; then
        printf '%s\n' "$w"
        w=''
      fi
      printf '\n'
      ;;
    "'" | '"')
      q=$c
      i=$((i + 1))
      while ((i < n)); do
        d=${s:i:1}
        i=$((i + 1))
        if [[ $d == "$q" ]]; then break; fi
        w+=$d
      done
      continue
      ;;
    \\)
      i=$((i + 1))
      if ((i < n)); then w+=${s:i:1}; fi
      i=$((i + 1))
      continue
      ;;
    *) w+=$c ;;
    esac
    i=$((i + 1))
  done
  if [[ -n $w ]]; then printf '%s\n' "$w"; fi
  printf '\n'
}

classify_command() { # <command>
  local words=() n i=0 w name at_cmd=1 args=() j k a b v role line=$1
  # A here-document body is data, not commands. The newline that ends the
  # opening line would otherwise put every body line in command position, so
  # `cat > f <<EOF` carrying `git push origin main` would post `pr`. Classify
  # the line that opens the heredoc and stop there: a real command written after
  # the heredoc is then a miss, never a wrong phase.
  if [[ $line == *'<<'* ]]; then
    line=${line%%$'\n'*}
  fi
  while IFS= read -r w; do words+=("$w"); done < <(split_words "$line")
  n=${#words[@]}

  while ((i < n)); do
    w=${words[i]}
    if [[ -z $w ]]; then
      at_cmd=1
      i=$((i + 1))
      continue
    fi
    if ((at_cmd)); then
      # Command position: past everything that can sit in front of a command
      # name — `FOO=bar` prefixes, timing/wrapping commands, a shell running a
      # script, leading flags, and a `timeout 300` duration — so
      # `CREW_ID=c1 timeout 300 bats tests/a.bats` is read as bats.
      if [[ $w == *=* && $w != */* ]]; then
        i=$((i + 1))
        continue
      fi
      case ${w##*/} in
      env | time | command | exec | nice | nohup | stdbuf | sudo | do | then | timeout | bash | sh | dash | zsh | fish)
        i=$((i + 1))
        continue
        ;;
      esac
      if [[ $w == -* || $w =~ ^[0-9]+(\.[0-9]+)?[smhd]?$ ]]; then
        i=$((i + 1))
        continue
      fi
      at_cmd=0
      name=${w##*/}
      # Non-flag operands of this command, up to two: `git -C /p push`,
      # `gh pr create`, `go test ./...`, `nix flake check`. A value flag whose
      # value is a separate word (-c/-C) is skipped with its value; anything
      # else in operand position is the operand.
      args=()
      j=$((i + 1))
      while ((j < n)); do
        w=${words[j]}
        [[ -n $w ]] || break
        case $w in
        -c | -C)
          j=$((j + 2))
          continue
          ;;
        -*)
          j=$((j + 1))
          continue
          ;;
        esac
        args+=("$w")
        if ((${#args[@]} >= 2)); then break; fi
        j=$((j + 1))
      done
      a=${args[0]:-}
      b=${args[1]:-}

      case $name in
      crew)
        case $a in
        await)
          # Only pre_tool may post this: on post_tool the wait is already over.
          if [[ $when == pre ]]; then
            k=$((i + 2))
            while ((k < n)); do
              if [[ ${words[k]} == --from ]]; then
                v=${words[k + 1]:-}
                if [[ $v == role:*:* ]]; then
                  role=${v##*:}
                  [[ -n $role ]] && note_phase "awaiting $role"
                fi
              fi
              k=$((k + 1))
            done
          fi
          ;;
        msg)
          # Assigning the plan critic is plan work; any other recipient is
          # control-plane.
          k=$((i + 2))
          while ((k < n)); do
            if [[ ${words[k]} == role:*:plan-critic ]]; then note_phase plan; fi
            k=$((k + 1))
          done
          ;;
        esac
        ;;
      git)
        case $a in
        push) note_phase pr ;;
        commit) note_phase commit ;;
        esac
        ;;
      gh)
        if [[ $a == pr && $b == create ]]; then note_phase pr; fi
        if [[ $a == run && $b == watch ]]; then note_phase ci; fi
        ;;
      bats | pytest | pyright | shellcheck) note_phase test ;;
      go | cargo) [[ $a == test ]] && note_phase test ;;
      npm | pnpm | yarn) [[ $a == test || $a == t ]] && note_phase test ;;
      make | just) [[ $a == test || $a == tests ]] && note_phase test ;;
      nix)
        k=$i
        while ((k < n && ${#words[k]} > 0)); do
          [[ ${words[k]} == check ]] && note_phase test
          k=$((k + 1))
        done
        ;;
      *)
        case $name in
        *bats-affected*) note_phase test ;;
        esac
        ;;
      esac
    fi
    i=$((i + 1))
  done
}

if [[ $when == post ]]; then
  if [[ -n $path ]]; then
    # Plan work is the plan artifact and the plan doc, not any file that happens
    # to be named plan.md: the crew artifact
    # <git-common-dir>/crew/artifacts/<branch>/plan.md, or PLAN.md at the
    # session's own directory (where spec-plan-critic writes and where a resume
    # reads), or under docs/superpowers/. Every other edit is implementation.
    lc_cwd=$(printf '%s' "$cwd" | tr '[:upper:]' '[:lower:]')
    lc=$(printf '%s' "$path" | tr '[:upper:]' '[:lower:]')
    case $lc in
    /*) ;;
    *) lc="$lc_cwd/$lc" ;;
    esac
    case $lc in
    */crew/artifacts/*/plan.md | "$lc_cwd"/plan.md | "$lc_cwd"/docs/superpowers/plan.md)
      phase=plan
      ;;
    *) phase=implement ;;
    esac
  elif [[ -n $cmd ]]; then
    classify_command "$cmd"
  fi
elif [[ -n $cmd ]]; then
  classify_command "$cmd"
fi

[[ -n $phase ]] || exit 0

common=$(git -C "$cwd" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)
[[ -n $common && -d $common ]] || exit 0

# Throttle: the last phase this handler posted for THIS session, in a file
# keyed by the worker id (the crew.sh _await_state idiom: a sanitized key plus a
# cksum so two ids that sanitize alike cannot collide).
key=$(printf '%s' "$CREW_WORKER_ID" | tr -c 'A-Za-z0-9._-' '_')
key="${key}.$(printf '%s' "$CREW_WORKER_ID" | cksum | cut -d' ' -f1)"
stfile="$common/crew/phase-status/$key"
posted=''
if [[ -f $stfile ]]; then
  posted=$(sed -n 's/^phase=//p' "$stfile" 2>/dev/null | tail -1 || true)
fi
[[ $posted != "$phase" ]] || exit 0

# The crew id, resolved the way crew.sh's _crew_id resolves it: the task doc
# first, then the environment.
crew_id=''
if [[ -f $cwd/WORKER_TASK.md ]]; then
  crew_id=$(grep -m1 '^crew_id:' "$cwd/WORKER_TASK.md" | cut -d' ' -f2 || true)
fi
[[ -n $crew_id ]] || crew_id=${CREW_ID:-}
[[ -n $crew_id ]] || exit 0

# Gate: the last state this session posted itself. A watchdog `blocked` row is
# skipped (it means alive), so the last remaining row decides.
state=''
if [[ -f $common/crew/events.jsonl ]]; then
  state=$(tail -n 2000 "$common/crew/events.jsonl" 2>/dev/null |
    jq -Rr --arg c "$crew_id" --arg m "$CREW_WORKER_ID" '
      (fromjson? // empty)
      | select(.crew_id == $c and .kind == "status" and .from == $m)
      | select((.body.state == "blocked" and (.body.source // "") == "watchdog") | not)
      | .body.state' 2>/dev/null | tail -1 || true)
fi
case $state in
'' | working) ;;
*) exit 0 ;;
esac

cd "$cwd" 2>/dev/null || exit 0
crew status "$CREW_WORKER_ID" working "$phase (auto)" >/dev/null 2>&1 || true

# Record the phase whether or not the post landed: a bus that is down must not
# turn every later tool call into a retry. Written atomically — mktemp then mv,
# because two of these can run at once on parallel tool calls and a bare
# `printf >` is not one write(2).
mkdir -p "$common/crew/phase-status" 2>/dev/null || exit 0
tmp=$(mktemp "$common/crew/phase-status/.st.XXXXXX" 2>/dev/null) || exit 0
if printf 'phase=%s\n' "$phase" >"$tmp" 2>/dev/null; then
  mv "$tmp" "$stfile" 2>/dev/null || rm -f "$tmp"
else
  rm -f "$tmp"
fi
