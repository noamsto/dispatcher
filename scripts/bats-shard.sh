#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ] || ! [[ $1 =~ ^[1-9][0-9]*$ ]] || ! [[ $2 =~ ^[1-9][0-9]*$ ]] || [ "$1" -gt "$2" ]; then
  echo "usage: $0 <shard> <total> [tests-dir]" >&2
  exit 2
fi

shard=$1
total=$2
tests_dir=${3:-tests}

mapfile -t files < <(find "$tests_dir" -maxdepth 1 -name '*.bats' ! -name module.bats -print | sort)

if [ "${#files[@]}" -eq 0 ]; then
  exit 0
fi

declare -A weights assignments
for file in "${files[@]}"; do
  weights[$file]=$(bats --count "$file")
done

mapfile -t ordered < <(for file in "${files[@]}"; do
  printf '%08d %s\n' "${weights[$file]}" "$file"
done | sort -rn -k1,1 -k2,2 | cut -d' ' -f2-)

declare -a loads
for ((i = 1; i <= total; i++)); do
  # shellcheck disable=SC2004
  loads[$i]=0
done

for file in "${ordered[@]}"; do
  target=1
  for ((i = 2; i <= total; i++)); do
    if [ "${loads[$i]}" -lt "${loads[$target]}" ]; then
      target=$i
    fi
  done
  assignments[$file]=$target
  # shellcheck disable=SC2004
  loads[$target]=$((loads[$target] + weights[$file]))
done

for file in "${files[@]}"; do
  [ "${assignments[$file]}" -eq "$shard" ] && printf '%s\n' "$file"
done

exit 0
