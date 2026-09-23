tier: trivial
kind: implement
draft: false
engine: pi
model: openrouter/deepseek/deepseek-v4-flash
effort: high
mcp:
plan: provided
title: workers: set GIT_EDITOR=true so git never opens an editor in a worker
Closes #278
dispatcher_pane: %282
crew_dir: /home/noams/git/dispatcher/.git/crew
crew_id: 1790105803-650114
agent_name: nova
worker_id: worker:feat/278-workers-set-git-editor-true-so-git-never#s1790153886-429458
protocol_dir: /nix/store/s3c62skpjn193p53d4n1bwh4mixjmihb-source/adapters/core/protocols

## Task

## Problem

Workers inherit the user's interactive `$EDITOR`/git editor (nvim). Any git
command that opens an editor — `git rebase --continue`, `git commit --amend`,
`git merge` without `--no-edit`, `git rebase -i` — hangs forever inside the
engine's bash tool, which has no TTY. Observed live: a pi worker sat 7.5 min in
`git rebase --continue` → `git commit` → `nvim` until the dispatcher killed nvim
by hand. The stall watchdog only escalates after 30 min.

Nothing in `adapters/core/dispatch.sh` or `adapters/core/dispatch-resume.sh`
sets `GIT_EDITOR` today (`rg GIT_EDITOR adapters/core` is empty).

## Fix

Export a non-interactive git editor into **every** engine launch environment the
harness creates — lead workers on all engines (claude, codex, cursor, pi), grid
role panes (eager, `--status`, `--spawn-role`), and `dispatch resume`:

- `GIT_EDITOR=true` — commit messages keep git's prepared message (rebase
  --continue/merge keep the original message).
- `GIT_SEQUENCE_EDITOR=:` — `git rebase -i` accepts the todo as-is instead of
  hanging.

Find the launch points: the `tmux send-keys … <engine> …` launch lines and the
`split-window … -e CREW_WORKER_ID=… -e CREW_ID=…` role-pane splits already carry
per-worker env (`CREW_WORKER_ID`, `CREW_ID`, `CREW_ROLE_ID`) — add the two vars
the same way, ideally through one helper so a new launch path can't miss them.
Don't touch the user's global git config.

Add one sentence to `WORKER_PROTOCOL.md` (git hygiene / rule area): git runs
non-interactively in workers; pass `-m`/`--no-edit` explicitly anyway, and never
invoke an editor.

## Acceptance

1. bats: for each engine's lead launch and for a role pane, the launched
   environment/command carries `GIT_EDITOR=true` and `GIT_SEQUENCE_EDITOR=:`
   (follow how existing tests assert `CREW_WORKER_ID` in the launch line).
2. bats: `dispatch resume` launch carries them too.
3. Full bats green, shellcheck clean, `scripts/gen-adapters.sh` re-run with no diff.
