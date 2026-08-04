# shellcheck shell=bash
# pr-watch — block until a GitHub PR changes, print one JSON event, exit 0.
#
# Babysitting a posted review is otherwise manual: a human tells an agent "watch
# it and approve when you're happy", every single time, and an agent that polls
# `gh pr view` in a loop burns a whole session idling and dies on compaction. The
# wake belongs in a primitive, not in a parked session.
#
# Standalone by construction: no crew, no dispatcher, no $CREW_ID, no bus — a
# plain agent session (background the call, handle the event on completion) or a
# human at a shell gets identical behaviour. `crew pr-watch` is a thin bus bridge
# layered on top; bus integration is opt-in, never the substrate.
#
# The shebang + `set -euo pipefail` are prepended by writeShellApplication, so
# this file is only the body (see crew.sh for the same pattern).

usage() {
  echo "usage: pr-watch <N> [--repo owner/name] [--timeout S] [--interval S]" >&2
}

pr="${1:-}"
shift || true
case "$pr" in '' | *[!0-9]*)
  usage
  exit 1
  ;;
esac

repo=""
timeout=1800
interval=60
while [ $# -gt 0 ]; do
  case "$1" in
  --repo)
    repo="${2:-}"
    [ -n "$repo" ] || {
      echo "pr-watch: --repo needs owner/name" >&2
      exit 1
    }
    shift 2
    ;;
  --timeout)
    timeout="${2:-}"
    shift 2
    ;;
  --interval)
    interval="${2:-}"
    shift 2
    ;;
  *)
    echo "pr-watch: unknown arg '$1'" >&2
    exit 1
    ;;
  esac
done
case "$timeout" in '' | *[!0-9]*)
  echo "pr-watch: --timeout must be a positive integer number of seconds" >&2
  exit 1
  ;;
esac
case "$interval" in '' | *[!0-9]*)
  echo "pr-watch: --interval must be a positive integer number of seconds" >&2
  exit 1
  ;;
esac
# An unbounded park is rejected for the same reason `crew watch` rejects one: a
# reaped indefinite park is undetectable, so the caller can never tell "still
# waiting" from "died".
[ "$timeout" -gt 0 ] || {
  echo "pr-watch: --timeout must be > 0 (an indefinite park would be undetectable if reaped)" >&2
  exit 1
}
[ "$interval" -gt 0 ] || {
  echo "pr-watch: --interval must be > 0" >&2
  exit 1
}

if [ -z "$repo" ]; then
  repo=$(git config --get remote.origin.url 2>/dev/null |
    sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##' || true)
fi
case "$repo" in
*/*) ;;
*)
  echo "pr-watch: pass --repo owner/name (no origin remote here to derive it from)" >&2
  exit 1
  ;;
esac

# Per-PR cursor, so a restart cannot re-deliver an event the caller already
# handled — and, symmetrically, so a change that lands while nothing is watching
# fires on the next start instead of being lost. Repo-keyed path, never a
# git dir: `pr-watch` must work with no repo checked out at all.
state_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew/pr-watch/$repo"
state_file="$state_dir/$pr.json"
mkdir -p "$state_dir"

# One fingerprint of every watched signal. Firing on "the fingerprint moved"
# rather than five bespoke comparisons is what keeps the signals from drifting
# apart, and it makes the cursor a single durable value.
#
# `checks` collapses the rollup to one aggregate conclusion on purpose: a
# babysitter cares that CI flipped, not that one of nine checks moved from
# queued to running while the rest are still pending.
_poll() { # -> fingerprint JSON, non-zero when gh could not be read
  local view threads
  view=$(gh pr view "$pr" --repo "$repo" \
    --json headRefOid,state,reviewDecision,latestReviews,statusCheckRollup,comments) || return 1
  # Review-thread replies are the whole point of the watch and live on their own
  # endpoint — no `gh pr view` field carries them. --paginate: the newest reply
  # on a long thread is on the last page.
  threads=$(gh api "repos/$repo/pulls/$pr/comments" --paginate \
    -q '.[] | (.updated_at // .created_at)') || return 1
  printf '%s' "$view" | jq -c \
    --arg thread_at "$(printf '%s\n' "$threads" | sort | tail -1)" \
    --argjson thread_n "$(printf '%s' "$threads" | grep -c . || true)" '
      ([.statusCheckRollup[]? | (.conclusion // .state // .status // "")]) as $c
      | {
          head_sha: (.headRefOid // ""),
          state: (.state // ""),
          review_decision: (.reviewDecision // ""),
          review_at: ([.latestReviews[]?.submittedAt | select(. != null)] | sort | last // ""),
          checks: (
            if ($c | length) == 0 then "NONE"
            elif ($c | any(IN("FAILURE","TIMED_OUT","CANCELLED","ACTION_REQUIRED","STARTUP_FAILURE","STALE","ERROR"))) then "FAILURE"
            elif ($c | any(IN("","PENDING","IN_PROGRESS","QUEUED","WAITING","REQUESTED","EXPECTED"))) then "PENDING"
            else "SUCCESS" end
          ),
          thread_at: ([$thread_at, (.comments[]?.createdAt)] | map(select(. != null and . != "")) | sort | last // ""),
          thread_n: ((.comments | length) + $thread_n)
        }'
}

_save() { # atomic: a half-written cursor would re-fire forever
  local tmp
  tmp=$(mktemp "$state_dir/.$pr.XXXXXX")
  printf '%s\n' "$1" >"$tmp"
  mv -f "$tmp" "$state_file"
}

# The first poll must succeed: it is the baseline, and a bad PR number or a
# missing gh auth is a caller error, not something to spin on until timeout.
# Later failures are transient by assumption and keep the park alive.
cur=$(_poll) || {
  echo "pr-watch: could not read PR $pr in $repo" >&2
  exit 1
}

prev=$(cat "$state_file" 2>/dev/null || true)
printf '%s' "$prev" | jq -e type >/dev/null 2>&1 || prev=""
[ -n "$prev" ] || {
  _save "$cur"
  prev="$cur"
}

start=$(jq -nc 'now*1000|floor')
deadline=$((start + timeout * 1000))
while :; do
  changed=$(jq -nc --argjson a "$prev" --argjson b "$cur" \
    '[$b | to_entries[] | select(.value != ($a[.key] // null)) | .key]')
  [ "$changed" != "[]" ] && {
    jq -nc --arg repo "$repo" --argjson pr "$pr" --argjson changed "$changed" \
      --argjson state "$cur" --argjson was "$prev" \
      '{ts:(now*1000|floor), repo:$repo, pr:$pr,
          url:("https://github.com/"+$repo+"/pull/"+($pr|tostring)),
          changed:$changed, state:$state,
          was:($changed | map({key:., value:($was[.] // null)}) | from_entries)}'
    _save "$cur"
    exit 0
  }
  [ "$(jq -nc 'now*1000|floor')" -ge "$deadline" ] && {
    echo "pr-watch: park ended after ${timeout}s — PR $pr unchanged" >&2
    exit 0
  }
  sleep "$interval"
  if next=$(_poll); then cur="$next"; fi
done
