#!/usr/bin/env bash
# help-parity.sh — prove `crew --help` still prints exactly what it printed
# before the screens moved from crew.sh's heredocs to Go (#1007).
#
# Every invocation AC1 of #1007 names runs twice: once against the pre-change
# crew.sh read out of git (the working tree is never touched), once against the
# working tree's, with the current Go binary behind CREW_GO_BIN. stdout, stderr
# and the exit status must match byte for byte.
#
# Run it from a dev shell, which is where `go` and `jq` live:
#
#   nix develop -c bash scripts/help-parity.sh
#   nix develop -c bash scripts/help-parity.sh --base origin/main
set -euo pipefail

# The last commit whose crew.sh still held the heredocs — the parent of the port.
# Not origin/main: once #1007 lands, main's crew.sh has no screens left to run
# the old side against, and the script would compare nothing against nothing.
base=40f43f4061d0e48a1fa90b1d5c6d128404ff6c01
while (($#)); do
  case "$1" in
  --base)
    base="${2:?--base needs a ref}"
    shift 2
    ;;
  -h | --help)
    printf 'usage: %s [--base REF]\n' "${0##*/}"
    exit 0
    ;;
  *)
    printf '%s: unknown argument %s\n' "${0##*/}" "$1" >&2
    exit 2
    ;;
  esac
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The pre-change script, straight out of git. Its @crewGoBin@ and friends are
# never reached: every invocation below answers in the preamble, ahead of the
# repo check and of any lib it sources.
old_sh="$work/crew-old.sh"
git -C "$root" show "$base:adapters/core/crew.sh" >"$old_sh"
new_sh="$root/adapters/core/crew.sh"

# The new side execs the Go binary, so build it from the working tree.
(
  cd "$root/crew"
  go build -o "$work/crew-go" .
)
export CREW_GO_BIN="$work/crew-go"

# Help answers with no repo, so run everything from an empty dir: that is the
# shape the preamble promises (`crew <sub> --help` works in a bare temp dir).
cd "$work"

invocations=()
diffs=0
run_dir="$work"

# record <args...> — run one invocation against both scripts, compare the three
# things that must not change. run_dir is the cwd: the empty dir for everything
# the preamble answers before the repo check, a scratch repo for the two rows
# that live after it.
record() {
  local rc_old=0 rc_new=0

  (cd "$run_dir" && bash -euo pipefail "$old_sh" "$@" >"$work/old.out" 2>"$work/old.err") || rc_old=$?
  (cd "$run_dir" && bash -euo pipefail "$new_sh" "$@" >"$work/new.out" 2>"$work/new.err") || rc_new=$?

  local bad=()
  cmp -s "$work/old.out" "$work/new.out" || bad+=(stdout)
  cmp -s "$work/old.err" "$work/new.err" || bad+=(stderr)
  [ "$rc_old" -eq "$rc_new" ] || bad+=("exit ($rc_old vs $rc_new)")

  invocations+=("crew $*")
  if ((${#bad[@]})); then
    diffs=$((diffs + 1))
    printf 'DIFF  crew %s: %s\n' "$*" "${bad[*]}"
    if ! cmp -s "$work/old.out" "$work/new.out"; then
      diff -u "$work/old.out" "$work/new.out" | head -20 || true
    fi
    if ! cmp -s "$work/old.err" "$work/new.err"; then
      diff -u "$work/old.err" "$work/new.err" | head -20 || true
    fi
  fi
}

# The subcommands are the rows of the pre-change top-level help, so the list is
# the old contract and not the new one's own opinion (same rows crew.bats
# compares against the script's arms).
subs=$(bash -euo pipefail "$old_sh" --help | sed -nE 's/^  ([a-z][a-z0-9-]+) +[^ ].*$/\1/p' | sort -u)
[ -n "$subs" ] || {
  echo "no subcommand rows in $base's top-level help" >&2
  exit 1
}

# And hold's actions are the synopses of its own screen.
holds=$(bash -euo pipefail "$old_sh" hold --help |
  sed -nE 's/^ *usage: crew hold ([a-z]+).*/\1/p; s/^ +crew hold ([a-z]+) .*/\1/p' | sort -u)
[ -n "$holds" ] || {
  echo "no hold actions in $base's hold screen" >&2
  exit 1
}

for f in --help -h help; do
  record "$f"
done
while read -r n; do
  [ -n "$n" ] || continue
  record help "$n"
  record "$n" --help
done <<<"$subs"
for a in $holds; do
  record hold "$a" --help
  record help hold "$a"
done
record fleet stream --help
record help hold

# Names that were never case labels. `top` and `default` are file names the Go
# package invented, not subcommands, and a name with a slash or a dot in it names
# no screen either: none of these may answer with a screen.
for n in top default; do
  record help "$n"
  record "$n" --help
  record help hold "$n"
  record hold "$n" --help
done
record help hold/add
record help ../top

# Bare `crew` and an unknown subcommand are the two help rows that sit after the
# repo check, so they need a repo to reach them (outside one, both scripts say
# `crew: not in a git repo`, which is the check's own line and unchanged here).
git init -q "$work/repo"
run_dir="$work/repo"
record
record bogus

printf '\n%d invocations, %d diffs\n' "${#invocations[@]}" "$diffs"
((diffs == 0))
