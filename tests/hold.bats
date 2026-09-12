bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
  unset CREW_ID
}

teardown() {
  teardown_repo
}

# add_valid_hold [--crew ID] <title> — `hold add` with every required flag
# filled in, --resets-at comfortably in the future. The fixture most tests
# below build on; the flag-rejection and resets-at tests below construct
# their own arg lists instead, since they need to omit or corrupt one flag.
add_valid_hold() {
  local crew=c1
  if [ "${1:-}" = --crew ]; then
    crew="$2"
    shift 2
  fi
  run_crew hold add --engine claude --window 5h \
    --resets-at "$(($(date +%s) + 100))" --agent codex --ref r1 --branch b1 \
    --tier standard --model sonnet --effort medium --crew "$crew" "$1"
}

events_log() {
  printf '%s' "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
}

@test "hold add: a hold neither wakes watch nor appears in inbox, but does appear in log" {
  id=$(add_valid_hold "quiet hold")

  # `crew watch` has nothing that matches `hold:c1` (crew.sh:917's to==me
  # predicate), so a short park should time out with empty stdout.
  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]

  run run_crew inbox dispatcher:c1 c1
  [ "$status" -eq 0 ]
  [ -z "$output" ]

  run run_crew log c1
  [ "$status" -eq 0 ]
  [[ "$output" == *"$id"* ]]
}

@test "hold add: each required flag is rejected by name when omitted" {
  local now
  now=$(date +%s)
  declare -A vals=(
    [--engine]=claude [--window]=5h [--resets-at]=$((now + 100)) [--agent]=codex
    [--ref]=r1 [--branch]=b1 [--tier]=standard [--model]=sonnet [--effort]=medium
  )
  local order=(--engine --window --resets-at --agent --ref --branch --tier --model --effort)
  local skip f args
  for skip in "${order[@]}"; do
    args=(--crew c1)
    for f in "${order[@]}"; do
      [ "$f" = "$skip" ] && continue
      args+=("$f" "${vals[$f]}")
    done
    args+=("title")
    run --separate-stderr run_crew hold add "${args[@]}"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"$skip is required"* ]]
  done
}

@test "hold add: rejects a missing title" {
  run --separate-stderr run_crew hold add --engine claude --window 5h \
    --resets-at "$(($(date +%s) + 100))" --agent codex --ref r1 --branch b1 \
    --tier standard --model sonnet --effort medium --crew c1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"a title is required"* ]]
}

@test "hold add: rejects a non-integer --resets-at" {
  run --separate-stderr run_crew hold add --engine claude --window 5h \
    --resets-at notanumber --agent codex --ref r1 --branch b1 \
    --tier standard --model sonnet --effort medium --crew c1 "title"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"must be an integer epoch-seconds timestamp"* ]]
}

@test "hold add: rejects a --resets-at in the past" {
  run --separate-stderr run_crew hold add --engine claude --window 5h \
    --resets-at "$(($(date +%s) - 100))" --agent codex --ref r1 --branch b1 \
    --tier standard --model sonnet --effort medium --crew c1 "title"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"must be in the future"* ]]
}

@test "hold add: --agent and --engine land in task.engine and wait.engine, both set and distinct" {
  id=$(add_valid_hold "title")
  run jq -e --arg id "$id" '
    (.body | fromjson) as $b
    | $b.id == $id and $b.wait.engine == "claude" and $b.task.engine == "codex"
      and ($b.wait.engine != null) and ($b.task.engine != null)
      and ($b.wait.engine != $b.task.engine)
  ' "$(events_log)"
  [ "$status" -eq 0 ]
}

@test "hold add: --spec is copied to holds/<id>.md with the path recorded in task.spec" {
  specfile="$BATS_TEST_TMPDIR/spec.md"
  printf 'the task body\n' >"$specfile"
  id=$(run_crew hold add --engine claude --window 5h \
    --resets-at "$(($(date +%s) + 100))" --agent codex --ref r1 --branch b1 \
    --tier standard --model sonnet --effort medium --spec "$specfile" --crew c1 "title")
  holddir="$(git rev-parse --path-format=absolute --git-common-dir)/crew/holds"
  [ -f "$holddir/$id.md" ]
  [ "$(cat "$holddir/$id.md")" = "the task body" ]
  run jq -e --arg id "$id" --arg p "$holddir/$id.md" '
    (.body | fromjson) as $b | $b.id == $id and $b.task.spec == $p
  ' "$(events_log)"
  [ "$status" -eq 0 ]
}

@test "hold add: an unreadable --spec errors before any write" {
  specfile="$BATS_TEST_TMPDIR/unreadable.md"
  printf 'body' >"$specfile"
  chmod 000 "$specfile"
  run --separate-stderr run_crew hold add --engine claude --window 5h \
    --resets-at "$(($(date +%s) + 100))" --agent codex --ref r1 --branch b1 \
    --tier standard --model sonnet --effort medium --spec "$specfile" --crew c1 "title"
  chmod 600 "$specfile"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"is not readable"* ]]
  [ ! -f "$(events_log)" ]
  [ ! -d "$(git rev-parse --path-format=absolute --git-common-dir)/crew/holds" ]
}

@test "hold add: prints the minted id on stdout" {
  run add_valid_hold "title"
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+-[0-9]+$ ]]
}

@test "hold add: a long title shrinks through _fit_line while id and task.branch survive intact" {
  big=$(head -c 8000 /dev/zero | tr '\0' x)
  id=$(run_crew hold add --engine claude --window 5h \
    --resets-at "$(($(date +%s) + 100))" --agent codex --ref r1 \
    --branch keep-this-branch --tier standard --model sonnet --effort medium \
    --crew c1 "$big")
  log="$(events_log)"
  line_bytes=$(printf '%s' "$(head -1 "$log")" | wc -c | tr -d ' ')
  [ "$line_bytes" -le 4096 ]
  run jq -e --arg id "$id" '
    (.body | fromjson) as $b
    | $b.id == $id and $b.task.branch == "keep-this-branch"
      and ($b.task.title | endswith("[elided]"))
  ' "$log"
  [ "$status" -eq 0 ]
}

@test "hold: outstanding folds as adds minus releases" {
  id1=$(add_valid_hold "hold one")
  id2=$(add_valid_hold "hold two")
  run_crew hold release "$id1" --crew c1
  run run_crew hold list --crew c1 --json
  [ "$status" -eq 0 ]
  ids=$(printf '%s' "$output" | jq -r '.[].id')
  [ "$ids" = "$id2" ]
}

@test "hold release: releasing the same id twice is idempotent" {
  id=$(add_valid_hold "title")
  run run_crew hold release "$id" --crew c1
  [ "$status" -eq 0 ]
  run run_crew hold release "$id" --crew c1
  [ "$status" -eq 0 ]
  run run_crew hold list --crew c1 --json
  [ "$output" = "[]" ]
}

@test "hold release: releasing an unknown id exits 0" {
  run run_crew hold release does-not-exist --crew c1
  [ "$status" -eq 0 ]
}

@test "hold due: exits 1 when the events log does not exist" {
  run run_crew hold due --crew c1
  [ "$status" -eq 1 ]
}

@test "hold due: exits 1 when nothing is matured" {
  add_valid_hold "title"
  run run_crew hold due --crew c1
  [ "$status" -eq 1 ]
}

@test "hold due: exits 0 when a hold is matured" {
  seed_hold c1 h1 "$(($(date +%s) - 10))"
  run run_crew hold due --crew c1
  [ "$status" -eq 0 ]
}

@test "hold due --json: prints [] when nothing is outstanding" {
  run run_crew hold due --crew c1 --json
  [ "$status" -eq 1 ]
  [ "$output" = "[]" ]
}

# Load-bearing beyond this file: `crew stream` shells out to `hold due --json`
# with `2>/dev/null || true` (crew.sh:1274), so a silently-broken shell-out
# would otherwise hide behind the stream tests' own assertions.
@test "hold due --json: reports exactly the matured hold, not an unmatured one" {
  seed_hold c1 due1 "$(($(date +%s) - 10))"
  add_valid_hold "future"
  run run_crew hold due --crew c1 --json
  [ "$status" -eq 0 ]
  ids=$(printf '%s' "$output" | jq -r '.[].id')
  [ "$ids" = "due1" ]
}

@test "hold due: matures at exactly resets_at, not one second later (<=, not <)" {
  now=$(date +%s)
  resets_at=$((now + 2))
  run_crew hold add --engine claude --window 5h --resets-at "$resets_at" \
    --agent codex --ref r1 --branch b1 --tier standard --model sonnet \
    --effort medium --crew c1 "title" >/dev/null
  # Bounded, tight poll (never a fixed sleep) so the observed maturity
  # instant pins the boundary instead of merely landing sometime after it.
  observed=""
  i=0
  while [ "$i" -lt 200 ]; do
    if run_crew hold due --crew c1 >/dev/null 2>&1; then
      observed=$(date +%s)
      break
    fi
    sleep 0.02
    i=$((i + 1))
  done
  [ -n "$observed" ]
  [ "$observed" -eq "$resets_at" ]
}

@test "hold park: returns the default when nothing is outstanding" {
  run run_crew hold park 42 --crew c1
  [ "$status" -eq 0 ]
  [ "$output" -eq 42 ]
}

@test "hold park: returns the default when the earliest hold is already matured" {
  seed_hold c1 h1 "$(($(date +%s) - 10))"
  run run_crew hold park 42 --crew c1
  [ "$status" -eq 0 ]
  [ "$output" -eq 42 ]
}

@test "hold park: returns min(default, remaining) when the earliest hold matures before the default" {
  resets_at=$(($(date +%s) + 30))
  seed_hold c1 h1 "$resets_at"
  run run_crew hold park 1000 --crew c1
  [ "$status" -eq 0 ]
  [ "$output" -lt 1000 ]
  [ "$output" -ge 25 ]
  [ "$output" -le 30 ]
}

@test "hold park: never returns 0" {
  resets_at=$(($(date +%s) + 1))
  seed_hold c1 h1 "$resets_at"
  run run_crew hold park 300 --crew c1
  [ "$status" -eq 0 ]
  [ "$output" -ge 1 ]
}

@test "hold: a crew's holds are invisible to another crew" {
  seed_hold c1 h1 "$(($(date +%s) + 100))"
  run run_crew hold list --crew c2 --json
  [ "$status" -eq 0 ]
  [ "$output" = "[]" ]
  run run_crew hold due --crew c2 --json
  [ "$status" -eq 1 ]
  [ "$output" = "[]" ]
}
