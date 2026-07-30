#!/usr/bin/env bash
# Regression check for #123: one dispatched worker must produce exactly ONE tmux
# window — the managed one dispatch creates — and no undecorated shell window
# from worktrunk's post-switch tmux hook.
#
# Not a Nix check: it needs a live tmux server and the ambient `wt`, neither of
# which exists in the build sandbox. Run it from inside tmux (`just
# test-dispatch-window`).
#
# The discriminator is @crew_name: dispatch stamps it on the window it manages,
# worktrunk's hook window has no such option. Both live at the same worktree
# path, so path alone can't tell them apart.
set -euo pipefail

# CLAUDECODE is unset for every run below: with it set, worktrunk's hook
# short-circuits on its own and the control case can't reproduce the bug — which
# is the whole reason #123 only bit codex/cursor dispatchers.
unset CLAUDECODE

for cmd in tmux wt git; do
  command -v "$cmd" >/dev/null || {
    echo "dispatch-window: $cmd not on PATH" >&2
    exit 1
  }
done
[ -n "${TMUX:-}" ] || {
  echo "dispatch-window: run this from inside tmux" >&2
  exit 1
}

tmp="$(mktemp -d)"
repo="$tmp/repo"
# Keep worktrees inside $tmp instead of the real ~/Data/git/.worktrees root, so
# cleanup can't touch anything the user cares about. zoxide is blanked for the
# same reason (it would record temp paths); it is orthogonal to the tmux hook.
wt_cfg=(--config-set "worktree-path=\"$tmp/worktrees/{{ branch | sanitize }}\"" --config-set 'post-switch.zoxide=""')

created_windows=()
# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  for w in "${created_windows[@]}"; do
    tmux kill-window -t "$w" 2>/dev/null || true
  done
  git -C "$repo" worktree remove --force "$tmp/worktrees/control" 2>/dev/null || true
  git -C "$repo" worktree remove --force "$tmp/worktrees/managed" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

git init -q "$repo"
git -C "$repo" commit -q --allow-empty -m init

# Windows sitting at $1, split by whether they carry @crew_name. Panes are
# matched on cwd because a hook window is tagged with nothing else.
windows_at() {
  tmux list-windows -a -F '#{pane_current_path}	#{@crew_name}	#{window_id}' |
    awk -F'\t' -v p="$1" '$1 == p {print ($2 == "" ? "bare" : "managed") "\t" $3}'
}

# `wt` returns before its post-switch hook has finished creating the window, so
# both directions have to wait the hook out: poll until one shows up, or until
# the budget expires with none. Bare-window count, not a boolean, so an
# unexpected second leak is reported rather than folded into "at least one".
settle=5
wait_for_bare() {
  local deadline=$((SECONDS + settle)) n
  while [ "$SECONDS" -lt "$deadline" ]; do
    n="$(windows_at "$1" | grep -c '^bare' || true)"
    [ "$n" -gt 0 ] && {
      echo "$n"
      return
    }
    sleep 0.1
  done
  echo 0
}

fail=0

# Control: plain `wt switch`. Proves the hook still opens a bare window, i.e.
# that the assertion below is actually load-bearing. If worktrunk ever stops
# doing this, the fix is obsolete and should be revisited — not silently kept.
wt -C "$repo" switch -c control -y "${wt_cfg[@]}" >/dev/null
control_bare="$(wait_for_bare "$tmp/worktrees/control")"
while read -r _ id; do created_windows+=("$id"); done < <(windows_at "$tmp/worktrees/control")
if [ "$control_bare" -lt 1 ]; then
  echo "✗ control: worktrunk's post-switch tmux hook no longer opens a window — this check proves nothing; re-examine the --config-set override in dispatch.sh" >&2
  fail=1
else
  echo "✓ control: plain 'wt switch' opens $control_bare bare window(s) (bug reproduced)"
fi

# Fixed path: the override dispatch.sh uses, then dispatch's own managed window.
managed_path="$tmp/worktrees/managed"
wt -C "$repo" switch -c managed -y "${wt_cfg[@]}" --config-set 'post-switch.tmux=""' >/dev/null

# Wait the hook out BEFORE creating the managed window — creating it first masks
# the bug: once any window sits at the worktree path, the hook's matcher
# navigates to that window instead of opening its own, so the leak silently
# stops reproducing and the check passes for the wrong reason.
leaked="$(wait_for_bare "$managed_path")"
while read -r _ id; do created_windows+=("$id"); done < <(windows_at "$managed_path")
if [ "$leaked" -ne 0 ]; then
  echo "✗ post-switch hook opened $leaked window(s) at $managed_path despite the override" >&2
  fail=1
fi

win="$(tmux new-window -d -c "$managed_path" -n dispatch-window-test -P -F '#{window_id}')"
created_windows+=("$win")
tmux set-window-option -t "$win" @crew_name test-crew

bare="$(windows_at "$managed_path" | grep -c '^bare' || true)"
managed="$(windows_at "$managed_path" | grep -c '^managed' || true)"
if [ "$bare" -ne 0 ]; then
  echo "✗ $bare undecorated window(s) leaked at $managed_path (expected 0)" >&2
  fail=1
fi
if [ "$managed" -ne 1 ]; then
  echo "✗ $managed managed window(s) at $managed_path (expected exactly 1)" >&2
  fail=1
fi
# The managed window must sit in the worker's worktree, not the dispatcher's cwd.
cwd="$(tmux display-message -t "$win" -p '#{pane_current_path}')"
if [ "$cwd" != "$managed_path" ]; then
  echo "✗ managed window cwd is $cwd, expected $managed_path" >&2
  fail=1
fi

# Guard the call site itself: the override is what makes all of the above hold.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if ! grep -q 'post-switch.tmux=""' "$here/../../adapters/core/dispatch.sh"; then
  echo "✗ dispatch.sh no longer blanks post-switch.tmux on its 'wt switch' call" >&2
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "✓ exactly one managed window at the worker worktree, no hook leak"
fi
exit "$fail"
