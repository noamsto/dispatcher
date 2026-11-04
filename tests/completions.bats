setup() {
  ROOT="$BATS_TEST_DIRNAME/.."
  COMP="$ROOT/completions"
  STUBS="$BATS_TEST_TMPDIR/stubs"
  mkdir -p "$STUBS"
  PATH="$STUBS:$PATH"
}

# stub <name> <script body>: a command on PATH for the dynamic helpers to call.
stub() {
  printf '#!/usr/bin/env bash\n%s\n' "$2" >"$STUBS/$1"
  chmod +x "$STUBS/$1"
}

# complete_line <file> <command line>: what fish offers after sourcing <file>.
complete_line() {
  fish --no-config -c 'source "$argv[1]"; complete -C "$argv[2]"' "$1" "$2"
}

# parser_flags <script>: the long flags of the launcher's argument parser, read
# from its case arms. dispatch.sh also holds internal parsers for the role
# panes, so only its main loop (from the `--)` arm to the title fallthrough)
# counts.
parser_flags() {
  local src="$ROOT/adapters/core/$1.sh"
  if [ "$1" = dispatch ]; then
    sed -n '/^  --)$/,/^  \*)$/p' "$src"
  else
    cat "$src"
  fi | grep -E '^\s*--[a-z][a-z-]*( \| -{1,2}[a-z][a-z-]*)*\)' | grep -oE -- '--[a-z][a-z-]*' | sort -u
}

has_flag() {
  grep -qE -- "^complete -c $1 .*-l $2( |\$)" "$COMP/$1.fish"
}

crew_script() {
  "$CREW_GO_BIN" completion fish >"$BATS_TEST_TMPDIR/crew.fish"
}

@test "every launcher has a completion file and nothing else is shipped" {
  names="dispatch dispatch-config dispatch-resume dispatcher lead-launcher overseer permission-check pr-watch refresh-budget refresh-models refresh-scores reviewer-roster"
  for n in $names; do
    [ -f "$COMP/$n.fish" ]
  done
  run bash -c 'ls "$1" | sed "s/\.fish$//" | sort | tr "\n" " "' _ "$COMP"
  [ "$output" = "$(printf '%s\n' $names | sort | tr '\n' ' ')" ]
}

@test "every completion file passes fish -n" {
  for f in "$COMP"/*.fish; do
    run fish -n "$f"
    [ "$status" -eq 0 ]
  done
}

@test "the generated crew completions pass fish -n" {
  crew_script
  run fish -n "$BATS_TEST_TMPDIR/crew.fish"
  [ "$status" -eq 0 ]
}

@test "every flag a launcher parses has a completion" {
  for n in dispatch dispatch-resume dispatcher lead-launcher permission-check pr-watch refresh-budget; do
    flags="$(parser_flags "$n")"
    [ -n "$flags" ]
    for f in $flags; do
      has_flag "$n" "${f#--}" || { echo "$n: no completion for $f"; return 1; }
    done
  done
}

# dispatch-config compares its argument inline rather than in a case arm.
@test "dispatch-config completes its two modes" {
  for f in show-origin layers; do
    has_flag dispatch-config "$f" || { echo "dispatch-config: no completion for --$f"; return 1; }
  done
}

@test "reviewer-roster completes the flags resolve-roster.sh parses" {
  flags="$(grep -oE -- '^  --[a-z]+\) ' "$ROOT/adapters/core/reviewers/resolve-roster.sh" | grep -oE -- '--[a-z]+')"
  [ -n "$flags" ]
  for f in $flags; do
    has_flag reviewer-roster "${f#--}" || { echo "reviewer-roster: no completion for $f"; return 1; }
  done
}

@test "dispatch completes its mode flags" {
  for f in engines in-budget models profiles json recover; do
    has_flag dispatch "$f" || { echo "dispatch: no completion for --$f"; return 1; }
  done
}

@test "overseer offers only flags dispatcher also parses" {
  parsed="$(parser_flags dispatcher)"
  for f in $(grep -oE -- '-l [a-z-]+' "$COMP/overseer.fish" | cut -d' ' -f2); do
    grep -qx -- "--$f" <<<"$parsed" || { echo "dispatcher.sh has no --$f"; return 1; }
  done
}

# dispatch copies dispatch-resume's flags into its resume block; this keeps the
# copy and dispatch-resume.sh's parser equal, in both directions.
@test "dispatch resume's flags match dispatch-resume's parser" {
  parsed="$(parser_flags dispatch-resume)"
  for f in $parsed; do
    grep -qE -- "^complete -c dispatch -n '__fish_seen_subcommand_from resume' .*-l ${f#--}( |\$)" "$COMP/dispatch.fish" || { echo "dispatch resume: no $f"; return 1; }
  done
  for f in $(grep "^complete -c dispatch -n '__fish_seen_subcommand_from resume'" "$COMP/dispatch.fish" | grep -oE -- '-l [a-z-]+' | cut -d' ' -f2); do
    grep -qx -- "--$f" <<<"$parsed" || { echo "dispatch-resume.sh has no --$f"; return 1; }
  done
}

@test "the model helper is identical wherever it is repeated" {
  body() { fish --no-config -c 'source "$argv[1]"; functions __dispatch_models' "$COMP/$1.fish" | grep -v '^#'; }
  want="$(body dispatch)"
  [ -n "$want" ]
  for n in dispatch-resume dispatcher overseer; do
    [ "$(body "$n")" = "$want" ]
  done
}

@test "dispatch completes a flag prefix" {
  run complete_line "$COMP/dispatch.fish" "dispatch --ag"
  [ "$status" -eq 0 ]
  [[ $output == *--agent* ]]
}

@test "dispatch completes the tier first and the model after it" {
  stub dispatch 'echo "{\"engines\":{\"claude\":{\"models\":[\"opus\",\"sonnet\"]}}}"'
  run complete_line "$COMP/dispatch.fish" "dispatch "
  [[ $output == *standard* ]]
  [[ $output == *resume* ]]
  run complete_line "$COMP/dispatch.fish" "dispatch standard "
  [[ $output == *sonnet* ]]
  [[ $output == *@premium* ]]
}

@test "dispatch --agent takes its engines from dispatch --engines" {
  stub dispatch '[ "$*" = --engines ] && printf "claude\ncodex\n"'
  run complete_line "$COMP/dispatch.fish" "dispatch --agent "
  [[ $output == *claude* ]]
  [[ $output == *codex* ]]
}

@test "dispatch --profile takes its names from dispatch --profiles" {
  stub dispatch '[ "$*" = "--profiles --json" ] && echo "{\"profiles\":{\"quick\":{},\"thorough\":{}}}"'
  run complete_line "$COMP/dispatch.fish" "dispatch --profile "
  [[ $output == *quick* ]]
  [[ $output == *thorough* ]]
}

@test "dispatch --effort and --plan complete their fixed values" {
  run complete_line "$COMP/dispatch.fish" "dispatch --effort "
  [[ $output == *ultra* ]]
  run complete_line "$COMP/dispatch.fish" "dispatch --plan "
  [[ $output == *provided* ]]
}

@test "dynamic helpers print nothing when the command fails" {
  stub dispatch 'echo noise >&2; exit 1'
  stub crew 'echo noise >&2; exit 1'
  run complete_line "$COMP/dispatch.fish" "dispatch --agent "
  [ -z "$output" ]
  run complete_line "$COMP/dispatch.fish" "dispatch --profile "
  [ -z "$output" ]
  run complete_line "$COMP/dispatcher.fish" "dispatcher --relaunch "
  [ -z "$output" ]
}

@test "dynamic helpers give up on a hung command" {
  stub dispatch 'sleep 10'
  start=$SECONDS
  run complete_line "$COMP/dispatch.fish" "dispatch --agent "
  [ -z "$output" ]
  [ $((SECONDS - start)) -lt 8 ]
}

@test "dispatcher --relaunch takes crew ids from crew crews" {
  stub crew 'printf "crew_id\tlast_event_s\n1700-42\t3\n1700-43\t9\n"'
  run complete_line "$COMP/dispatcher.fish" "dispatcher --relaunch "
  [[ $output == *1700-42* ]]
  [[ $output == *1700-43* ]]
  [[ $output != *crew_id* ]]
}

@test "overseer completes dispatcher's launch flags" {
  run complete_line "$COMP/overseer.fish" "overseer --ef"
  [[ $output == *--effort* ]]
}

@test "dispatch resume offers dispatch-resume's flags, not dispatch's" {
  run complete_line "$COMP/dispatch.fish" "dispatch resume --pr"
  [[ $output == *--print* ]]
  run complete_line "$COMP/dispatch.fish" "dispatch resume --gr"
  [[ $output != *--grid* ]]
}

@test "dispatcher --overseer restricts --agent to claude and pi" {
  run complete_line "$COMP/dispatcher.fish" "dispatcher --overseer --agent "
  [[ $output == *claude* ]]
  [[ $output == *pi* ]]
  [[ $output != *codex* ]]
  [[ $output != *cursor* ]]
  run complete_line "$COMP/dispatcher.fish" "dispatcher --agent "
  [[ $output == *codex* ]]
}

@test "model lists drop glob patterns" {
  stub dispatch 'echo "{\"engines\":{\"pi\":{\"models\":[\"opus\",\"lemonade/*\",\"x?y\",\"a[b]\"]}}}"'
  run complete_line "$COMP/dispatch.fish" "dispatch standard "
  [[ $output == *opus* ]]
  [[ $output != *lemonade* ]]
  [[ $output != *x?y* ]]
  [[ $output != *a\[b* ]]
}

@test "pr-watch takes the PR number before any flag" {
  run complete_line "$COMP/pr-watch.fish" "pr-watch --"
  [ -z "$output" ]
  run complete_line "$COMP/pr-watch.fish" "pr-watch 5 --"
  [[ $output == *--repo* ]]
}

@test "crew completes a subcommand" {
  crew_script
  run complete_line "$BATS_TEST_TMPDIR/crew.fish" "crew wh"
  [ "$status" -eq 0 ]
  [[ $output == *where* ]]
}

@test "crew where completes roster names and branches" {
  crew_script
  stub crew '[ "$1" = roster ] && echo "[{\"name\":\"amber\",\"branch\":\"feat/x\"}]"'
  run complete_line "$BATS_TEST_TMPDIR/crew.fish" "crew where "
  [[ $output == *amber* ]]
  [[ $output == *feat/x* ]]
}
