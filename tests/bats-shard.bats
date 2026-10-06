#!/usr/bin/env bats

setup() {
  bats_require_minimum_version 1.5.0
  SHARD_SCRIPT="$BATS_TEST_DIRNAME/../scripts/bats-shard.sh"
  WEIGHTS_SCRIPT="$BATS_TEST_DIRNAME/../scripts/bats-shard-weights.sh"
  expected="$(find "$BATS_TEST_DIRNAME" -maxdepth 1 -name '*.bats' ! -name module.bats -print | sort)"
  FIXTURE="$BATS_TEST_TMPDIR/suite"
  mkdir -p "$FIXTURE"
}

write_case() {
  local file=$1 name=$2 tag=${3:-}
  if [[ -n $tag ]]; then
    printf '# bats test_tags=%s\n' "$tag" >>"$file"
  fi
  printf '@test "%s" {\n  true\n}\n' "$name" >>"$file"
}

write_weights_header() {
  printf 'file\tfamily\tfilter\tweight_ms\n' >"$FIXTURE/weights.tsv"
}

add_weight() {
  printf '%s\t%s\t%s\t%s\n' "$@" >>"$FIXTURE/weights.tsv"
}

shard() {
  run --separate-stderr "$SHARD_SCRIPT" --weights "$FIXTURE/weights.tsv" "$@"
}

@test "live suite shards exit 0 and partition every file" {
  local shard all
  all=''
  for shard in 1 2 3 4; do
    run --separate-stderr "$SHARD_SCRIPT" "$shard" 4 "$BATS_TEST_DIRNAME"
    [ "$status" -eq 0 ]
    all+=$'\n'"$output"
  done

  # Split files appear once per shard as file<TAB>regex units, so dedupe:
  # file-level coverage is "every file appears", case-level exactness is
  # --check's job (it runs in the CI lint job).
  [ "$(printf '%s\n' "$all" | sed '/^$/d; s/\t.*$//' | sort -u)" = "$expected" ]
}

@test "module bats is not sharded" {
  run --separate-stderr "$SHARD_SCRIPT" 1 4 "$BATS_TEST_DIRNAME"
  [ "$status" -eq 0 ]
  [[ $output != *module.bats* ]]
}

@test "bats shard rejects invalid arguments" {
  run "$SHARD_SCRIPT" 0 4
  [ "$status" -ne 0 ]
  run "$SHARD_SCRIPT" 5 4
  [ "$status" -ne 0 ]
  run "$SHARD_SCRIPT" a 4
  [ "$status" -ne 0 ]
  run "$SHARD_SCRIPT"
  [ "$status" -ne 0 ]
  run "$SHARD_SCRIPT" --plan --check
  [ "$status" -ne 0 ]
  run "$SHARD_SCRIPT" --weights "$FIXTURE/missing.tsv" 1 2 "$FIXTURE"
  [ "$status" -ne 0 ]
}

@test "shard assignment is weight-balanced" {
  local heavy="$FIXTURE/a100.bats" mid="$FIXTURE/b060.bats" light="$FIXTURE/c050.bats"
  write_case "$heavy" "heavy one"
  write_case "$mid" "mid one"
  write_case "$light" "light one"
  write_weights_header
  add_weight "$heavy" '*' '!timing' 100
  add_weight "$mid" '*' '!timing' 60
  add_weight "$light" '*' '!timing' 50

  local shard sum max=0 min=0 largest=100 loads
  loads=()
  for shard in 1 2; do
    shard "$shard" 2 "$FIXTURE"
    [ "$status" -eq 0 ]
    sum=0
    while IFS= read -r file; do
      [[ -n $file ]] || continue
      case $file in
      "$heavy") sum=$((sum + 100)) ;;
      "$mid") sum=$((sum + 60)) ;;
      "$light") sum=$((sum + 50)) ;;
      *)
        echo "unexpected unit $file" >&2
        return 1
        ;;
      esac
    done <<<"$output"
    loads[shard]=$sum
    if ((shard == 1 || sum < min)); then min=$sum; fi
    if ((sum > max)); then max=$sum; fi
  done

  [ "${loads[1]}" -eq 100 ]
  [ "${loads[2]}" -eq 110 ]
  [ $((max - min)) -le "$largest" ]
}

@test "shards prefer measured family weights over test counts" {
  local heavy="$FIXTURE/heavy.bats" grouped="$FIXTURE/grouped.bats"
  write_case "$heavy" "heavy one"
  write_case "$grouped" "medium: one"
  write_case "$grouped" "light: one"
  write_weights_header
  add_weight "$heavy" '*' '!timing' 100
  add_weight "$grouped" medium '!timing' 60
  add_weight "$grouped" light '!timing' 40

  shard 1 2 "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$output" = "$heavy" ]

  shard 2 2 "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$output" = "$grouped" ]

  shard --check "$FIXTURE"
  [ "$status" -eq 0 ]
  [ -z "$output" ]

  printf 'file\tfamily\tfilter\tweight_ms\n' >"$FIXTURE/empty.tsv"
  run --separate-stderr "$SHARD_SCRIPT" --weights "$FIXTURE/empty.tsv" 1 2 "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$output" = "$grouped" ]
}

@test "check warns on an uncovered family and schedules it" {
  local file="$FIXTURE/cases.bats"
  write_case "$file" "alpha: first"
  write_case "$file" "beta: second"
  write_weights_header
  add_weight "$file" alpha '!timing' 10

  shard --check "$FIXTURE"
  [ "$status" -eq 0 ]
  [[ $stderr == *"$file"* ]]
  [[ $stderr == *beta* ]]
  [[ $stderr == *bats-shard-weights.sh* ]]

  local shard all='' name line regex covered
  for shard in 1 2; do
    shard "$shard" 2 "$FIXTURE"
    [ "$status" -eq 0 ]
    all+=$'\n'"$output"
  done
  for name in "alpha: first" "beta: second"; do
    covered=0
    while IFS= read -r line; do
      [[ -n $line ]] || continue
      if [[ $line != *$'\t'* ]]; then
        covered=1
        break
      fi
      regex=${line#*$'\t'}
      if [[ $name =~ $regex ]]; then
        covered=1
        break
      fi
    done <<<"$all"
    [ "$covered" -eq 1 ]
  done
}

@test "check warns on a weights row that names no case" {
  local file="$FIXTURE/cases.bats"
  write_case "$file" "alpha: first"
  write_weights_header
  add_weight "$file" alpha '!timing' 10
  add_weight "$file" nope '!timing' 4

  shard --check "$FIXTURE"
  [ "$status" -eq 0 ]
  [[ $stderr == *"stale"* ]]
  [[ $stderr == *nope* ]]
}

@test "uncovered family gets the median per-case rate as fallback weight" {
  local file="$FIXTURE/cases.bats"
  write_case "$file" "alpha: one"
  write_case "$file" "gamma: one"
  write_case "$file" "gamma: two"
  write_case "$file" "gamma: three"
  write_case "$file" "beta: one"
  write_case "$file" "beta: two"
  write_weights_header
  add_weight "$file" alpha '!timing' 10
  add_weight "$file" gamma '!timing' 90

  # Rates 10 and 30. Even count, lower-middle is 10; beta has 2 cases → 20.
  shard --plan "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$(awk -F '\t' '$4 == "beta" { print $5 }' <<<"$output")" = 20 ]
}

@test "zero-padded weight strings parse as decimal fallback rates" {
  local file="$FIXTURE/cases.bats"
  write_case "$file" "alpha: one"
  write_case "$file" "gamma: one"
  write_case "$file" "gamma: two"
  write_case "$file" "gamma: three"
  write_case "$file" "beta: one"
  write_case "$file" "beta: two"
  write_weights_header
  add_weight "$file" alpha '!timing' 010
  add_weight "$file" gamma '!timing' 090

  # 010 and 090 are decimal (rates 10 and 30). Lower-middle is 10; beta → 20.
  shard --plan "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$(awk -F '\t' '$4 == "beta" { print $5 }' <<<"$output")" = 20 ]
}

@test "timing-tagged cases never appear in shard output" {
  local file="$FIXTURE/cases.bats"
  write_case "$file" "alpha: first"
  write_case "$file" "beta: second"
  write_case "$file" "security: slow" timing
  write_weights_header
  add_weight "$file" alpha '!timing' 10
  add_weight "$file" beta '!timing' 10
  add_weight "$file" security timing 12

  local shard all=''
  for shard in 1 2; do
    shard "$shard" 2 "$FIXTURE"
    [ "$status" -eq 0 ]
    all+=$'\n'"$output"
  done
  [[ $all != *security* ]]
  [[ $all == *"^(alpha):"* ]]
  [[ $all == *"^(beta):"* ]]

  shard --check "$FIXTURE"
  [ "$status" -eq 0 ]
}

@test "family filters escape regex metacharacters" {
  local file="$FIXTURE/cases.bats"
  write_case "$file" "a.b: one"
  write_case "$file" "c: two"
  write_weights_header
  add_weight "$file" 'a.b' '!timing' 100
  add_weight "$file" c '!timing' 1

  shard 1 2 "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$output" = "$file"$'\t''^(a\.b):' ]
}

@test "a split star family selects names with no colon" {
  local file="$FIXTURE/cases.bats"
  write_case "$file" "plain one"
  write_case "$file" "grid: two"
  write_weights_header
  add_weight "$file" '*' '!timing' 100
  add_weight "$file" grid '!timing' 1

  shard 1 2 "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$output" = "$file"$'\t''^[^:]+$' ]
  shard 2 2 "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$output" = "$file"$'\t''^(grid):' ]
}

@test "equal weights break ties by unit name" {
  local first="$FIXTURE/a.bats" second="$FIXTURE/b.bats"
  write_case "$first" "one"
  write_case "$second" "two"
  write_weights_header
  add_weight "$first" '*' '!timing' 10
  add_weight "$second" '*' '!timing' 10

  shard 1 2 "$FIXTURE"
  [ "$output" = "$first" ]
  shard 2 2 "$FIXTURE"
  [ "$output" = "$second" ]
}

@test "plan lists every unit and its shard weight" {
  local heavy="$FIXTURE/heavy.bats" grouped="$FIXTURE/grouped.bats"
  write_case "$heavy" "heavy one"
  write_case "$grouped" "medium: one"
  write_case "$grouped" "light: one"
  write_weights_header
  add_weight "$heavy" '*' '!timing' 100
  add_weight "$grouped" medium '!timing' 60
  add_weight "$grouped" light '!timing' 40

  shard --plan "$FIXTURE"
  [ "$status" -eq 0 ]
  [ "$(head -n 1 <<<"$output")" = $'shard\tunit\tfile\tfamily\tweight_ms\tshard_weight_ms' ]
  [ "$(awk -F '\t' 'NR > 1 { print $3, $4, $5, $6 }' <<<"$output" | sort)" = "$(
    printf '%s\n' \
      "$grouped light 40 40" \
      "$grouped medium 60 60" \
      "$heavy * 100 100"
  )" ]
}

@test "weights aggregate families and reject a red run" {
  local out
  out=$(
    printf '%s\n' \
      $'tests/a.bats\tok\t4\tno colon here' \
      $'tests/crew.bats\tok\t10\tstall-watch: a' \
      $'tests/crew.bats\tok\t5\tstall-watch: b' \
      $'tests/module.bats\tok\t99\tmod' \
      $'tests/secret-read-guard.bats[timing]\tok\t7\tsecret: slow' \
      | "$WEIGHTS_SCRIPT"
  )
  [ "$out" = "$(
    printf '%s\n' \
      $'file\tfamily\tfilter\tweight_ms' \
      $'tests/a.bats\t*\t!timing\t4' \
      $'tests/crew.bats\tstall-watch\t!timing\t15' \
      $'tests/secret-read-guard.bats\tsecret\ttiming\t7'
  )" ]

  printf 'tests/a.bats\tnot-ok\t3\tboom\n' >"$FIXTURE/red.tsv"
  run --separate-stderr "$WEIGHTS_SCRIPT" <"$FIXTURE/red.tsv"
  [ "$status" -ne 0 ]
  [[ $stderr == *not-ok* ]]
}

@test "weights --run rejects an incomplete JUnit artifact inventory" {
  local bin="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$bin"
  cat >"$bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
name=
dir=
while (($#)); do
  case $1 in
    -n) name=$2; shift 2 ;;
    --dir) dir=$2; shift 2 ;;
    *) shift ;;
  esac
done
if [[ $name == bats-timing-shard-1 ]]; then
  mkdir -p "$dir/$name"
  printf '%s\n' \
    '<testsuites><testsuite name="crew-id.bats" failures="0" errors="0">' \
    '<testcase name="crew id: resolves from WORKER_TASK.md with no CREW_ID in the environment at all" time="0.1"/>' \
    '</testsuite></testsuites>' >"$dir/$name/report.xml"
fi
GH
  chmod +x "$bin/gh"

  run --separate-stderr env PATH="$bin:$PATH" "$WEIGHTS_SCRIPT" --run 123
  [ "$status" -ne 0 ]
  [[ $stderr == *"does not match the current !timing test inventory"* ]]
}

ci_module_bats_job_ok() {
  local ci="$1"
  local line current="" in_jobs=0 hits=0 owner="" trimmed
  local in_job=0 saw_steps=0
  local in_check=0 collecting=0 needs="" rest item found=0

  [ -f "$ci" ] || return 1

  while IFS= read -r line || [ -n "$line" ]; do
    if [ "$in_jobs" -eq 0 ]; then
      [ "$line" = "jobs:" ] && in_jobs=1
      continue
    fi
    # A new top-level document key ends the jobs map.
    if [[ "$line" =~ ^[^[:space:]#] ]]; then
      break
    fi
    if [[ "$line" =~ ^[[:space:]]{2}([A-Za-z0-9_-]+):[[:space:]]*$ ]]; then
      current="${BASH_REMATCH[1]}"
    fi
    trimmed="${line#"${line%%[![:space:]]*}"}"
    trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
    case "$trimmed" in
      "nix develop -c bats tests/module.bats" | *" nix develop -c bats tests/module.bats")
        hits=$((hits + 1))
        owner="$current"
        ;;
    esac
  done <"$ci"

  [ "$hits" -eq 1 ] || return 1
  [ "$owner" = "bats-module" ] || return 1

  while IFS= read -r line || [ -n "$line" ]; do
    if [ "$in_job" -eq 0 ]; then
      [[ "$line" =~ ^[[:space:]]{2}bats-module:[[:space:]]*$ ]] && in_job=1
      continue
    fi
    if [[ "$line" =~ ^[[:space:]]{2}[A-Za-z0-9_-]+:[[:space:]]*$ ]]; then
      return 1
    fi
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
      strategy:*) return 1 ;;
      steps:*)
        saw_steps=1
        break
        ;;
    esac
  done <"$ci"
  [ "$saw_steps" -eq 1 ] || return 1

  while IFS= read -r line || [ -n "$line" ]; do
    if [[ "$line" =~ ^[[:space:]]{2}check:[[:space:]]*$ ]]; then
      in_check=1
      continue
    fi
    [ "$in_check" -eq 1 ] || continue
    if [[ "$line" =~ ^[[:space:]]{2}[A-Za-z0-9_-]+:[[:space:]]*$ ]]; then
      break
    fi
    trimmed="${line#"${line%%[![:space:]]*}"}"
    if [ "$collecting" -eq 0 ]; then
      case "$trimmed" in
        needs:*)
          rest="${trimmed#needs:}"
          rest="${rest#"${rest%%[![:space:]]*}"}"
          if [ -z "$rest" ]; then
            collecting=1
          else
            needs="$rest"
            case "$rest" in
              *"["*"]"*) ;;
              *"["*) collecting=1 ;;
            esac
          fi
          ;;
      esac
      continue
    fi
    needs+=" $trimmed"
    case "$trimmed" in
      *"]"*) collecting=0 ;;
    esac
  done <"$ci"

  needs="${needs//[\[\]\",]/ }"
  for item in $needs; do
    [ "$item" = "bats-module" ] && found=1
  done
  [ "$found" -eq 1 ]
}

@test "module bats runs once on unsharded bats-module and check needs it" {
  run ci_module_bats_job_ok "$BATS_TEST_DIRNAME/../.github/workflows/ci.yml"
  [ "$status" -eq 0 ]
}
