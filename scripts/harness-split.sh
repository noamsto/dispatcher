#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

readonly manifest=tests/harness/manifest.tsv
readonly parser=tests/harness/profile/parse_trace.py
readonly repo_root=$PWD

usage() {
  echo "usage: $0 [--output DIR] [--case CASE_ID]..." >&2
  exit 2
}

output="${TMPDIR:-/tmp}/dispatcher-harness-profile"
declare -a requested=()
while (($#)); do
  case "$1" in
  --output)
    (($# >= 2)) || usage
    output=$2
    shift 2
    ;;
  --case)
    (($# >= 2)) || usage
    requested+=("$2")
    shift 2
    ;;
  *) usage ;;
  esac
done

for command in bats strace python3; do
  command -v "$command" >/dev/null || {
    echo "harness-split: missing required command: $command" >&2
    exit 1
  }
done

if [[ -x /usr/bin/time ]]; then
  time_bin=/usr/bin/time
else
  time_bin="$(type -P time || true)"
fi
if [[ -z $time_bin ]]; then
  echo "harness-split: GNU time is required (/usr/bin/time on FHS systems)" >&2
  exit 1
fi

mkdir -p "$output/cases"
results="$output/cases.tsv"
printf '%s\n' 'case_id	source_file	family	status	wall_s	user_s	sys_s	bucket_harness_s	bucket_production_s	bucket_wait_s	bucket_residual_s	diagnostic_production_wait_s	diagnostic_sleep_s	diagnostic_timeout_s	diagnostic_git_s	diagnostic_jq_s	diagnostic_tmux_s	diagnostic_shell_script_s' >"$results"

selected() {
  local case_id=$1 requested_id
  ((${#requested[@]} == 0)) && return 0
  for requested_id in "${requested[@]}"; do
    [[ $requested_id == "$case_id" ]] && return 0
  done
  return 1
}

escape_ere() {
  python3 -c 'import re, sys; print(re.escape(sys.argv[1]))' "$1"
}

failures=0
seen=0
while IFS=$'\t' read -r case_id source_file _ family test_name _; do
  [[ $case_id == case_id ]] && continue
  selected "$case_id" || continue
  seen=$((seen + 1))
  trace="$output/cases/$case_id.strace"
  timing="$output/cases/$case_id.time"
  stdout="$output/cases/$case_id.tap"
  stderr="$output/cases/$case_id.stderr"
  filter="^$(escape_ere "$test_name")$"

  set +e
  "$time_bin" -q -f '%e\t%U\t%S' -o "$timing" \
    strace -f -ttt -T -s 4096 \
    -e trace=process,execve,wait4,waitid,nanosleep,clock_nanosleep \
    -o "$trace" bats --filter "$filter" "$source_file" >"$stdout" 2>"$stderr"
  status=$?
  set -e

  IFS=$'\t' read -r wall user system <"$timing"
  python3 "$parser" \
    --trace "$trace" \
    --repo-root "$repo_root" \
    --case-id "$case_id" \
    --source-file "$source_file" \
    --family "$family" \
    --status "$status" \
    --wall "$wall" \
    --user "$user" \
    --sys "$system" >>"$results"
  if ((status != 0)); then
    echo "harness-split: $case_id failed with status $status (see $stderr)" >&2
    failures=$((failures + 1))
  fi
done <"$manifest"

if ((seen == 0)); then
  echo "harness-split: no requested case IDs matched $manifest" >&2
  exit 1
fi

python3 "$parser" --summary "$results" >"$output/summary.tsv"
printf 'case splits: %s\nsummary: %s\n' "$results" "$output/summary.tsv"
((failures == 0))
