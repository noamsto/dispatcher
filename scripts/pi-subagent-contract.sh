#!/usr/bin/env bash
# pi-subagent-contract.sh — check that a pi subagent extension still has the
# child-spawn contract the dispatcher relies on (argv shape, JSON event mode,
# agents dir, agent frontmatter fields). Unlisted index.ts lines that mutate or
# reassign args, set spawn options (env, shell, stdio, detached) or return a
# getPiInvocation command also fail, as drift.
#
# adapters/pi/extensions/subagent/contract.txt lists one source line per row.
# A row passes when its literal equals a whole, whitespace-trimmed line of the
# file. Rows scoped `both` are checked in every mode; `pinned` rows only
# against our patched copy, `upstream` rows only against pi's own example.
#
#   pi-subagent-contract.sh --pinned [dir]     our copy (default: the contract's dir)
#   pi-subagent-contract.sh --upstream <dir>   a pi examples/extensions/subagent dir
#   pi-subagent-contract.sh --installed        the pi on PATH
#
# --contract <file> overrides the contract file (tests, the nix check).
# Exit: 0 match, 1 contract drift, 2 usage or no installed pi to check.
set -euo pipefail

contract="$(dirname "$0")/../adapters/pi/extensions/subagent/contract.txt"
mode=""
dir=""
while (($#)); do
  case "$1" in
    --pinned | --installed) mode="${1#--}" ;;
    --upstream)
      mode=upstream
      dir="${2:?--upstream needs a directory}"
      shift
      ;;
    --contract)
      contract="${2:?--contract needs a file}"
      shift
      ;;
    -*)
      echo "unknown option: $1" >&2
      exit 2
      ;;
    *) dir="$1" ;;
  esac
  shift
done
[ -n "$mode" ] || {
  echo "usage: pi-subagent-contract.sh --pinned [dir] | --upstream <dir> | --installed [--contract <file>]" >&2
  exit 2
}
[ -r "$contract" ] || {
  echo "contract file not readable: $contract" >&2
  exit 2
}

pinned_version="$(awk -F'\t' '$1 == "version" { print $2 }' "$contract")"
scope="$mode"
notice=""

if [ "$mode" = installed ]; then
  scope=upstream
  pi_bin="$(command -v pi)" || {
    echo "pi not found on PATH" >&2
    exit 2
  }
  if grep -Iq . "$pi_bin"; then
    real="$(grep -Eo '/nix/store/[^ "'"'"']*/bin/pi' "$pi_bin" | head -n 1 || true)"
  else
    real="$(realpath "$pi_bin")"
  fi
  [ -n "$real" ] || {
    echo "cannot find the real pi binary behind $pi_bin" >&2
    exit 2
  }
  root="$(dirname "$(dirname "$real")")"
  dir="$root/libexec/pi/examples/extensions/subagent"
  [ -d "$dir" ] || {
    echo "no subagent example in the installed pi: $dir" >&2
    exit 2
  }
  installed_version="$(jq -r .version "$root/libexec/pi/package.json")"
  if [ "$installed_version" != "$pinned_version" ]; then
    notice="notice: installed pi $installed_version != pinned $pinned_version; re-pin recommended"
  fi
elif [ "$mode" = pinned ]; then
  : "${dir:=$(dirname "$contract")}"
else
  [ -n "$dir" ] || {
    echo "--upstream needs a directory" >&2
    exit 2
  }
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

failed=0
listed="$tmp/listed"
: >"$listed"
while IFS=$'\t' read -r file row_scope literal; do
  case "$file" in "" | "#"* | version) continue ;; esac
  [ "$row_scope" = both ] || [ "$row_scope" = "$scope" ] || continue
  trimmed="$tmp/$file"
  if [ ! -e "$trimmed" ]; then
    : >"$trimmed"
    if [ -r "$dir/$file" ]; then
      sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$dir/$file" >"$trimmed"
    fi
  fi
  [ "$file" != index.ts ] || printf '%s\n' "$literal" >>"$listed"
  if ! grep -Fxq -- "$literal" "$trimmed"; then
    echo "missing in $file: $literal"
    failed=1
  fi
done <"$contract"

# A spawn argument, spawn option or invocation line the contract does not list is drift too.
if [ -r "$dir/index.ts" ]; then
  while IFS= read -r line; do
    grep -Fxq -- "$line" "$listed" && continue
    echo "unlisted in index.ts: $line"
    failed=1
  done < <(sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$dir/index.ts" |
    grep -E '(^|[^A-Za-z0-9_.])args\.(push|unshift|splice)\(|(^|[^A-Za-z0-9_.])args = |^(env|shell|stdio|detached):|^return \{ command:' || true)
fi

[ -z "$notice" ] || echo "$notice" >&2
if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "ok: $mode $dir matches the subagent spawn contract (pinned version $pinned_version)"
