#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

readonly manifest=tests/harness/manifest.tsv
readonly files=(
  tests/crew-id.bats
  tests/refresh-models.bats
  tests/pr-watch.bats
  tests/dispatch.bats
)

# Optional per-file ERE limiting which tests enter the sample (empty = whole
# file). dispatch.bats is 785 tests; only this role-watch slice is sampled.
declare -A file_filter=(
  [tests/dispatch.bats]='^role-watch: (a permission dialog receives no keys|option-select, quota|an idle claude input box|queued assignments go out|a dialog raised after the text)'
)

usage() {
  echo "usage: $0 [--check]" >&2
  exit 2
}

case "${1:-}" in
"") mode="write" ;;
--check) mode="check" ;;
*) usage ;;
esac
(($# <= 1)) || usage

generate() {
  local filter_spec="" file
  for file in "${!file_filter[@]}"; do
    filter_spec+="$file"$'\t'"${file_filter[$file]}"$'\n'
  done
  printf 'case_id\tsource_file\tsource_line\tfamily\ttest_name\ttags\n'
  awk -v filter_spec="$filter_spec" '
    BEGIN {
      spec_count = split(filter_spec, pairs, "\n")
      for (i = 1; i <= spec_count; i++) {
        tab = index(pairs[i], "\t")
        if (tab > 0) filters[substr(pairs[i], 1, tab - 1)] = substr(pairs[i], tab + 1)
      }
    }

    function slugify(value,    slug) {
      slug = tolower(value)
      gsub(/[^a-z0-9]+/, "-", slug)
      gsub(/^-|-$/, "", slug)
      return slug
    }

    FNR == 1 {
      if (FILENAME == "tests/crew-id.bats") {
        family = "crew-id"
        sample_tag = "fast"
      } else if (FILENAME == "tests/refresh-models.bats") {
        family = "refresh-models"
        sample_tag = "fork-heavy"
      } else if (FILENAME == "tests/pr-watch.bats") {
        family = "pr-watch"
        sample_tag = "slow"
      } else if (FILENAME == "tests/dispatch.bats") {
        family = "role-watch"
        sample_tag = "slow"
      } else {
        printf "harness-manifest: unknown source file: %s\n", FILENAME > "/dev/stderr"
        exit 1
      }
      filter = filters[FILENAME]
      file_tags = ""
      test_tags = ""
    }

    /^# bats file_tags=/ {
      file_tags = $0
      sub(/^# bats file_tags=/, "", file_tags)
      next
    }

    /^# bats test_tags=/ {
      test_tags = $0
      sub(/^# bats test_tags=/, "", test_tags)
      next
    }

    /^@test / {
      name = $0
      if (name !~ /^@test ".*" \{$/) {
        printf "harness-manifest: unsupported @test syntax at %s:%d\n", FILENAME, FNR > "/dev/stderr"
        exit 1
      }
      sub(/^@test "/, "", name)
      sub(/" \{$/, "", name)
      if (name ~ /\t/) {
        printf "harness-manifest: tab in test name at %s:%d\n", FILENAME, FNR > "/dev/stderr"
        exit 1
      }

      if (filter != "" && name !~ filter) {
        test_tags = ""
        next
      }

      case_id = family "-" slugify(name)
      tags = sample_tag
      if (file_tags != "") tags = tags "," file_tags
      if (test_tags != "") tags = tags "," test_tags
      printf "%s\t%s\t%d\t%s\t%s\t%s\n", case_id, FILENAME, FNR, family, name, tags
      test_tags = ""
    }
  ' "${files[@]}"
}

generated="$(generate)"

if ! awk -F '\t' '
  NR == 1 {
    expected = "case_id\tsource_file\tsource_line\tfamily\ttest_name\ttags"
    if ($0 != expected) exit 1
    next
  }
  NF != 6 || $1 == "" || $2 == "" || $3 !~ /^[0-9]+$/ || $4 == "" || $5 == "" || $6 == "" { exit 1 }
  seen[$1]++ { exit 1 }
  END { if (NR == 1) exit 1 }
' <<<"$generated"; then
  echo "harness-manifest: invalid schema or duplicate case ID" >&2
  exit 1
fi

for file in "${files[@]}"; do
  if [[ -n ${file_filter[$file]:-} ]]; then
    source_count="$(bats --count --filter "${file_filter[$file]}" "$file")"
  else
    source_count="$(bats --count "$file")"
  fi
  manifest_count="$(awk -F '\t' -v file="$file" 'NR > 1 && $2 == file { count++ } END { print count + 0 }' <<<"$generated")"
  if [[ $source_count != "$manifest_count" ]]; then
    echo "harness-manifest: $file has $source_count Bats cases but $manifest_count manifest rows" >&2
    exit 1
  fi
done

if [[ $mode == check ]]; then
  if [[ ! -f $manifest ]]; then
    echo "harness-manifest: missing $manifest" >&2
    exit 1
  fi
  if ! diff -u "$manifest" <(printf '%s\n' "$generated"); then
    echo "harness-manifest: $manifest is stale" >&2
    exit 1
  fi
else
  mkdir -p "$(dirname "$manifest")"
  printf '%s\n' "$generated" >"$manifest"
fi
