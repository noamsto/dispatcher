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
