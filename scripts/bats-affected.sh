#!/usr/bin/env bash
# Print the bats files worth running for a set of changed files, one repo-relative
# path per line (sorted, unique). Falls back to the full suite, with the reason on
# stderr, for shared sources and for any file with no map row.
#
#   bats-affected.sh [--base REF] [--files PATH...]
#
# Without --files the changed set is the diff against the merge-base with REF
# (default: origin/HEAD, else origin/main) plus staged, unstaged and untracked
# changes. The map below is derived from the source -> test audit in
# docs/test-suite-audit.md; tests/bats-affected.bats fails when a core script
# has no row.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

base=""
files=()
use_files=0
while [[ $# -gt 0 ]]; do
  case "$1" in
  --base)
    base="${2:?--base needs a ref}"
    shift 2
    ;;
  --files)
    use_files=1
    shift
    files=("$@")
    break
    ;;
  *)
    echo "bats-affected: unknown argument: $1" >&2
    exit 2
    ;;
  esac
done

full_reason=""
selected=()

full_suite() {
  [[ -n "$full_reason" ]] || full_reason="$1"
}

add() {
  local t
  for t in "$@"; do selected+=("tests/$t.bats"); done
}

# Sets shared by several rows.
dispatch_set=(adapters crews dispatch-comment dispatch-resume dispatch model-map module)
prompt_set=(adapters crews dispatch-comment dispatch-resume dispatch dispatcher model-map module permission-check)
crew_set=(adapters crew-dash crew-id crew crews dispatch-comment dispatch-notify dispatch-resume dispatch dispatcher hold model-map module pr-watch rate-autosweep rate-sweep-all rate reap-race retro)

map_file() {
  local f="$1"
  case "$f" in
  tests/*.bats)
    if [[ -f "$root/$f" ]]; then selected+=("$f"); fi
    ;;
  tests/*) full_suite "full suite: $f is a shared test helper or fixture" ;;
  adapters/core/dispatch-config.sh | adapters/core/grant-check.sh | adapters/core/defaults.json | adapters/core/worktree-git.sh | flake.nix | flake.lock | nix/* | .github/workflows/*)
    full_suite "full suite: $f is a shared source"
    ;;
  scripts/bats-affected.sh) add bats-affected ;;
  scripts/bats-timing.sh | scripts/bats-classify.sh) ;;
  adapters/core/crew.sh) add "${crew_set[@]}" ;;
  adapters/core/cross-repo-hint.sh | adapters/core/dispatch-resume.sh | adapters/core/dispatch.sh) add "${dispatch_set[@]}" ;;
  adapters/core/dispatch-notify.sh) add adapters dispatch-notify dispatch ;;
  adapters/core/dispatcher.sh) add adapters crews dispatch dispatcher module ;;
  adapters/core/permission-check.sh) add adapters dispatch-resume dispatch module permission-check ;;
  adapters/core/pr-watch.sh) add adapters dispatch module pr-watch ;;
  adapters/core/public-leak-guard.sh) add "${dispatch_set[@]}" public-leak-guard ;;
  adapters/core/refresh-budget.sh) add adapters crew-dash dispatch module refresh-budget ;;
  adapters/core/refresh-models.sh) add adapters dispatch module refresh-models ;;
  adapters/core/refresh-scores.sh) add adapters dispatch module refresh-scores ;;
  adapters/core/secret-read-guard.sh) add adapters dispatch secret-read-guard ;;
  adapters/core/reviewers/resolve-roster.sh) add "${prompt_set[@]}" ;;
  adapters/core/commands/*.md) add adapters dispatch ;;
  adapters/core/protocols/WORKER_PROTOCOL.md) add "${prompt_set[@]}" crew ;;
  adapters/core/protocols/dispatch-orchestration.md) add "${prompt_set[@]}" crew model-map-doc ;;
  adapters/core/protocols/*.md | adapters/core/critics/*.md | adapters/core/reviewers/*.md | adapters/core/skills/*/SKILL.md) add "${prompt_set[@]}" ;;
  adapters/claude-code/* | adapters/codex/* | adapters/cursor/*) add adapters module ;;
  scripts/cache-report.sh | scripts/gen-adapters.sh) add adapters ;;
  scripts/gen-model-map-doc.sh) add adapters model-map-doc ;;
  README.md) add adapters dispatch permission-check ;;
  hookyard.json) add adapters crew public-leak-guard secret-read-guard ;;
  dash/*) add crew-dash module ;;
  docs/* | spikes/* | EVIDENCE-*.txt | LICENSE | .gitignore | .envrc | WORKER_TASK.md) ;;
  *) full_suite "full suite: $f has no map row" ;;
  esac
}

changed_files() {
  local ref="${base:-}" mb
  if [[ -z "$ref" ]]; then
    if git -C "$root" rev-parse --verify -q refs/remotes/origin/HEAD >/dev/null; then
      ref=refs/remotes/origin/HEAD
    else
      ref=refs/remotes/origin/main
    fi
  fi
  if ! mb="$(git -C "$root" merge-base "$ref" HEAD 2>/dev/null)"; then
    echo "full suite: cannot resolve a merge-base with $ref"
    return 1
  fi
  {
    git -C "$root" diff --name-only "$mb"
    git -C "$root" diff --name-only
    git -C "$root" diff --name-only --cached
    git -C "$root" ls-files --others --exclude-standard
  } | sort -u
}

if ((! use_files)); then
  if ! out="$(changed_files)"; then
    full_suite "$out"
    out=""
  fi
  files=()
  while IFS= read -r line; do
    [[ -z "$line" ]] || files+=("$line")
  done <<<"$out"
fi

for f in ${files[@]+"${files[@]}"}; do
  map_file "$f"
done

if [[ -n "$full_reason" ]]; then
  echo "bats-affected: $full_reason" >&2
  (cd "$root" && printf '%s\n' tests/*.bats)
  exit 0
fi

if ((${#selected[@]})); then
  printf '%s\n' "${selected[@]}" | sort -u | while IFS= read -r t; do
    [[ -f "$root/$t" ]] && echo "$t"
  done
fi
exit 0
