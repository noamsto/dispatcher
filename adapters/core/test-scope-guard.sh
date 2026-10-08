#!/usr/bin/env bash
# Pre-tool guard: stop a dispatched worker from spending wall time on a full
# bats run — the whole suite, a directory, `bats-affected`, a glob over
# `tests/*.bats` — when its task needs a handful of targeted files. CI runs
# the full suite on every push (#841).
#
# Workers only: the rule is about a fleet burning its own CPU, so a session
# without CREW_WORKER_ID (a human at a prompt, a dispatcher) exits before jq,
# the parse and the git call, and the hook costs a human nothing. Role panes
# inherit the lead's CREW_WORKER_ID, so they are guarded too.
#
# Opt out per task: a `tests: full` line in the WORKER_TASK.md header (the
# block before the first blank line) allows everything, for the rare task that
# really does need the suite.
#
#   engine        event                  command at             verdict shape
#   claude        PreToolUse Bash        tool_input.command     hookSpecificOutput deny
#   pi(hookyard)  canonical_event        tool_input.command     hookSpecificOutput deny
#                 pre_tool Bash
#
# Allow is no stdout, exit 0. The deny hands the reason back to the agent, so
# it retries with targeted files instead of stopping on an unexplained refusal.
#
# This is a speed bump over the command string, not a sandbox: an operand
# built from a variable (`bats $f`) is not classified, and neither is a runner
# that shells out from inside another program. It only has to catch the habit,
# which is typing `bats tests/`.
#
# Portable to macOS's bash 3.2 and BSD userland: no mapfile, no ${var,,}.

set -euo pipefail

# Workers only, and before anything else runs.
[[ -n ${CREW_WORKER_ID:-} ]] || exit 0

command -v jq >/dev/null || {
  echo "test-scope-guard: jq not found; guard NOT enforcing" >&2
  exit 1
}

input=$(cat)

# shellcheck disable=SC2016
normalise='
def s: if type == "string" then . else "" end;
if type != "object" then empty else
(.tool_input | if type == "object" then . else {} end) as $ti
| (.hook_event_name | s) as $ev
| (.tool_name | s) as $tool
| (if (.canonical_event | type) == "string" then "hookyard"
   elif $ev == "PreToolUse" and has("turn_id") then "codex"
   elif $ev == "PreToolUse" then "claude"
   elif ($ev == "preToolUse" or $ev == "beforeShellExecution") then "cursor"
   else "" end) as $shape
| (if $shape == "cursor" and $ev == "beforeShellExecution" then .command | s
   elif $shape == "cursor" and $tool == "Shell" then $ti.command | s
   elif $shape != "" and $shape != "cursor" and $tool == "Bash" then $ti.command | s
   else "" end) as $command
| select($command != "")
| [$shape, $command, (first(.cwd, $ti.cwd, .workspace_roots[0]? | select(type == "string" and . != "")) // "")]
end'

parsed=$(jq -c "$normalise" <<<"$input" 2>/dev/null) || {
  echo "test-scope-guard: could not parse hook payload; guard NOT enforcing" >&2
  exit 1
}
[[ -n $parsed ]] || exit 0
shape=$(jq -r '.[0]' <<<"$parsed")
command=$(jq -r '.[1]' <<<"$parsed")
cwd=$(jq -r '.[2]' <<<"$parsed")
cwd=${cwd:-$PWD}

# Quote-aware split of the command line: one word per line, and an empty line
# wherever a shell control operator (`; && || | ( )` and newlines) ends a
# command. Quotes are stripped, never interpreted — the guard never evaluates
# the command it is checking. Words containing a literal newline are split,
# which is fine for the habit this catches.
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
    ';' | '|' | '&' | '(' | ')' | '`' | $'\n')
      # A newline ends a command just as `;` does: without the empty line the
      # next line's words are still arguments of the first command, so a
      # `bats tests/` on line 2 reads as an argument of `git status` on line 1.
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
    '#')
      # A word-initial `#` comments out the rest of the line; mid-word it is
      # an ordinary character.
      if [[ -z $w ]]; then
        while ((i < n)); do
          if [[ ${s:i:1} == $'\n' ]]; then break; fi
          i=$((i + 1))
        done
        continue
      fi
      w+=$c
      ;;
    '<' | '>')
      # A redirection and its target are never operands: `2>&1`, `> log`,
      # `< /dev/null`, `<<EOF`. A bare fd number glued to the operator goes
      # with it; anything else before the operator was a word.
      if [[ -n $w ]]; then
        if [[ $w == *[0-9] && $w != *[!0-9]* ]]; then
          w=''
        else
          printf '%s\n' "$w"
        fi
      fi
      i=$((i + 1))
      if [[ ${s:i:1} == "$c" ]]; then i=$((i + 1)); fi
      while ((i < n)); do
        if [[ ${s:i:1} != ' ' && ${s:i:1} != $'\t' ]]; then break; fi
        i=$((i + 1))
      done
      if [[ ${s:i:1} == '&' ]]; then i=$((i + 1)); fi
      # The target is one word, quotes and escapes included: `> "/tmp/o f"`,
      # `2> 'error log'`, `> /tmp/o\ f`. Stopping at the space inside the
      # quotes would leave the tail behind as an operand.
      while ((i < n)); do
        d=${s:i:1}
        case $d in
        ' ' | $'\t' | $'\n' | ';' | '|' | '&' | '(' | ')' | '<' | '>' | '`') break ;;
        "'" | '"')
          q=$d
          i=$((i + 1))
          while ((i < n)); do
            d=${s:i:1}
            i=$((i + 1))
            if [[ $d == "$q" ]]; then break; fi
          done
          continue
          ;;
        \\)
          i=$((i + 1))
          ;;
        esac
        i=$((i + 1))
      done
      continue
      ;;
    *) w+=$c ;;
    esac
    i=$((i + 1))
  done
  if [[ -n $w ]]; then printf '%s\n' "$w"; fi
  printf '\n'
}

# bats options that take the following word. Their value is a pattern, a
# directory for reports, a job count — never a test operand — and a pattern
# like `tests` or `failed` would otherwise read as one.
bats_value_flags=' --code-quote-style --line-reference-format -f --filter --filter-status --filter-tags -F --formatter --gather-test-outputs-in -j --jobs --parallel-binary-name --report-formatter -o --output '

verdict=''
bats_files=0
at_cmd=1
in_bats=0
skip_val=0
expect_duration=0
skip_shell_flag=0

while IFS= read -r w; do
  if [[ -z $w ]]; then
    at_cmd=1
    in_bats=0
    continue
  fi

  if ((at_cmd)); then
    # Past any `FOO=bar` prefixes and interpreter wrappers, so a guard sees
    # `bash scripts/bats-affected.sh` and `timeout 300 bats tests/` too.
    if [[ $w == *=* && $w != */* ]]; then continue; fi
    if ((skip_shell_flag)); then
      skip_shell_flag=0
      if [[ $w == -* ]]; then continue; fi
    fi
    if ((expect_duration)) && [[ $w =~ ^[0-9]+(\.[0-9]+)?[smhd]?$ ]]; then
      expect_duration=0
      continue
    fi
    expect_duration=0
    case ${w##*/} in
    bash | sh | dash | zsh | fish)
      skip_shell_flag=1
      continue
      ;;
    env | time | command | exec | nice | nohup | stdbuf | xargs | do | then) continue ;;
    timeout)
      expect_duration=1
      continue
      ;;
    esac
    at_cmd=0
    case ${w##*/} in
    *bats-affected*)
      verdict=affected
      break
      ;;
    bats)
      in_bats=1
      bats_files=0
      skip_val=0
      ;;
    *) in_bats=0 ;;
    esac
    continue
  fi

  if ((in_bats == 0)); then continue; fi

  if ((skip_val)); then
    skip_val=0
    continue
  fi

  case $w in
  -*)
    # `-r`, `--recursive`, and any short cluster holding an r (`-rT`): no
    # other bats short flag carries an r, and a long one (`--report-formatter`)
    # is excluded by the `--` prefix. The r probe is a separate test because
    # bash will not backtrack `[A-Za-z]*` to zero width, so one pattern for
    # the cluster and the bare flag silently misses `-r`.
    if [[ $w == --recursive || $w != --* && $w == *r* ]]; then
      verdict=recursive
      break
    fi
    # `--filter=x` carries its value inline; a known value flag eats the
    # next word.
    if [[ $w != *=* ]]; then
      case $bats_value_flags in
      *" ${w} "*) skip_val=1 ;;
      esac
    fi
    continue
    ;;
  esac

  # An operand assembled from a variable is not classified here.
  if [[ $w == *'$'* ]]; then continue; fi

  # A glob reaches the whole directory: the shell has not expanded it yet, the
  # guard sees the pattern.
  case $w in
  *'*'* | *'?'* | *'['*)
    verdict=glob
    break
    ;;
  esac

  # bats takes `.bats` files or directories, so an operand with no extension
  # is a directory; the `-d` probe catches one that has a dot in its name.
  path=$w
  [[ $path == /* ]] || path=$cwd/$w
  if [[ ${w##*/} != *.* || -d $path ]]; then
    verdict=dir
    break
  fi

  if [[ $w == *.bats ]]; then
    bats_files=$((bats_files + 1))
    if ((bats_files > 3)); then
      verdict=many
      break
    fi
  fi
done < <(split_words "$command")

[[ -n $verdict ]] || exit 0

# Per-task opt-out, read only once a run looks like a full one.
toplevel=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null || true)
if [[ -n $toplevel && -f $toplevel/WORKER_TASK.md ]]; then
  if awk '
    NF == 0 { exit }
    $0 ~ /^tests:[ \t]*full[ \t]*$/ { found = 1; exit }
    END { exit !found }
  ' "$toplevel/WORKER_TASK.md"; then exit 0; fi
fi

case $verdict in
affected) what='bats-affected runs the affected set, which is thousands of tests' ;;
recursive) what='-r/--recursive walks a directory tree' ;;
dir) what="a directory operand ($w) runs every .bats file under it" ;;
glob) what="a glob operand ($w) expands to the whole suite" ;;
many) what="$bats_files .bats files in one call" ;;
esac

reason="This is a full bats run: $what. Workers run targeted tests only: run targeted files (bats tests/<file>.bats --filter <pattern>); CI runs the full suite.
If this task genuinely needs a full run, stamp a 'tests: full' line in the WORKER_TASK.md header and rerun."

case $shape in
cursor) jq -cn --arg r "$reason" '{permission: "deny", user_message: $r, agent_message: $r}' ;;
*) jq -cn --arg r "$reason" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}' ;;
esac
