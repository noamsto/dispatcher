#!/usr/bin/env bash
# Run one hsup case, or every case defined for an engine, in a scratch dir.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: run.sh <case|all> <engine>" >&2
  exit 2
fi
target=$1
engine=$2

S=$(cd "$(dirname "$0")" && pwd)
d=$(mktemp -d)
trap 'rm -rf -- "$d"' EXIT

cd "$S"
go build -buildvcs=false -o "$d/bin/" ./cmd/...
export PATH="$d/bin:$PATH" HSUP_MCPECHO="$d/bin/mcpecho" HSUP_GATE="$S/pi/gate.ts"
mkdir -p "$S/transcripts"

run_case() {
  local name=$1 out="$S/transcripts/$engine-$1.jsonl"
  rm -rf -- "$d/work" "$d/outside"
  mkdir -p "$d/work" "$d/outside"
  rm -f -- "$out"
  hsup case "$name" --engine "$engine" --work "$d/work" --outside "$d/outside" --transcript "$out"
}

if [[ $target != all ]]; then
  run_case "$target"
  exit
fi

listing=$(hsup cases)
names=()
while read -r name eng; do
  [[ $eng == "$engine" ]] && names+=("$name")
done <<<"$listing"
[[ ${#names[@]} -gt 0 ]] || {
  echo "no cases for engine $engine" >&2
  exit 1
}

pass=0
fail=0
for name in "${names[@]}"; do
  if run_case "$name"; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
  fi
done
echo "SUMMARY $engine: $pass PASS, $fail FAIL"
[[ $fail -eq 0 ]]
