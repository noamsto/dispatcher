#!/usr/bin/env bats

setup() {
  SHARD_SCRIPT="$BATS_TEST_DIRNAME/../scripts/bats-shard.sh"
  expected="$(find "$BATS_TEST_DIRNAME" -maxdepth 1 -name '*.bats' ! -name module.bats -print | sort)"
}

@test "bats shards are exhaustive, disjoint, and balanced" {
  local shard output all min max count largest
  all=''
  min=-1
  max=0
  largest=0

  for shard in 1 2 3 4; do
    run "$SHARD_SCRIPT" "$shard" 4 "$BATS_TEST_DIRNAME"
    [ "$status" -eq 0 ]
    all+=$'\n'"$output"
    count=0
    while IFS= read -r file; do
      [ -n "$file" ] || continue
      tests=$(bats --count "$file")
      count=$((count + tests))
      [ "$tests" -gt "$largest" ] && largest=$tests
    done <<<"$output"
    if [ "$min" -lt 0 ] || [ "$count" -lt "$min" ]; then
      min=$count
    fi
    if [ "$count" -gt "$max" ]; then
      max=$count
    fi
  done

  [ "$(printf '%s\n' "$all" | sed '/^$/d' | sort)" = "$expected" ]
  [ "$(printf '%s\n' "$all" | sed '/^$/d' | sort | uniq -d)" = '' ]
  [ $((max - min)) -le "$largest" ]
}

@test "module bats is not sharded" {
  run "$SHARD_SCRIPT" 1 4 "$BATS_TEST_DIRNAME"
  [ "$status" -eq 0 ]
  [[ "$output" != *module.bats* ]]
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
