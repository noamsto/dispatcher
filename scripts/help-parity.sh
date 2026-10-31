#!/usr/bin/env bash
# help-parity.sh — prove the Go entry routes `--help` exactly where crew.sh did.
#
# #1007 moved the screens; #821 moved the routing, so the two sides of this
# comparison are no longer two scripts. The old side is the pre-change crew.sh
# read out of git (the working tree is never touched), run with the current Go
# binary behind CREW_GO_BIN, which is what its arms exec. The new side is that
# same binary run as the entrypoint, with no bash in front of it. stdout, stderr
# and the exit status must match byte for byte.
#
# Run it from a dev shell, which is where `go` and `jq` live:
#
#   nix develop -c bash scripts/help-parity.sh
#   nix develop -c bash scripts/help-parity.sh --base origin/main
set -euo pipefail

# The last commit whose crew.sh still held the entry — the parent of #821. Not
# origin/main: once this lands, main's crew.sh routes nothing, and the script
# would compare nothing against nothing (the same reason it pinned 40f43f4 for
# the screens).
base=0ced95458f38b59bfe059ccc16a57fa14a8357c8
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

# The pre-change script, straight out of git. Its @crewGoBin@ is the binary
# built below: every arm of it execs that one program, which is the point of
# comparing the two.
old_sh="$work/crew-old.sh"
git -C "$root" show "$base:adapters/core/crew.sh" >"$old_sh"
grep -q '^sub=' "$old_sh" || {
  echo "$base's crew.sh has no entry to compare against" >&2
  exit 1
}

# Build the working tree's Go binary once: it is the old side's CREW_GO_BIN and
# the new side's entrypoint, so a difference can only be the routing.
(
  cd "$root/crew"
  go build -o "$work/crew-go" .
)
crew_go="$work/crew-go"
export CREW_GO_BIN="$crew_go"

# Help answers with no repo, so run everything from an empty dir: that is the
# shape the preamble promises (`crew <sub> --help` works in a bare temp dir).
cd "$work"

invocations=()
diffs=0
run_dir="$work"

# record <args...> — run one invocation against both sides, compare the three
# things that must not change. run_dir is the cwd: the empty dir for everything
# the preamble answers before the repo check, a scratch repo for the rows that
# live after it.
record() {
  local rc_old=0 rc_new=0

  (cd "$run_dir" && bash -euo pipefail "$old_sh" "$@" >"$work/old.out" 2>"$work/old.err") || rc_old=$?
  (cd "$run_dir" && "$crew_go" "$@" >"$work/new.out" 2>"$work/new.err") || rc_new=$?

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
# the old contract and not the new one's own opinion (same rows as the old
# script's arms).
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

# Bare `crew`, an unknown subcommand, and --help past the first argument: the
# rows crew.sh's `*)` arm and its "only the first position counts" rule owned.
# They run in a scratch repo so the third reaches its subcommand rather than the
# repo check.
git init -q "$work/repo"
run_dir="$work/repo"
record
record bogus
record nosuchcmd
record status w1 working --help

# The last row is the one a routing table gets wrong: --help in the third
# position is detail text, so nothing here may be a help screen.
if grep -q 'file-based coordination bus' "$work/new.out" "$work/new.err"; then
  printf 'DIFF  crew status w1 working --help printed a help screen\n' >&2
  diffs=$((diffs + 1))
fi

printf '\n%d invocations, %d diffs\n' "${#invocations[@]}" "$diffs"
((diffs == 0))
