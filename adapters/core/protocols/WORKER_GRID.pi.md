# Worker protocol: grid mode

Phase file of `WORKER_PROTOCOL.md`: it binds like the core. References to "this protocol" or a section of it resolve through the core's Phase files table.

## Grid mode (role panes)

`WORKER_TASK.md` stamps `roles:` → you lead a role grid: roles already run as panes in your window, share this worktree, park on the crew bus as `role:<branch>:<role>` and follow `GRID_PROTOCOL.md` in `protocol_dir:`. `lazy: 1` also stamped → no pane exists until you spawn it on demand (Lazy grid).

Delegate only phases with a role pane in your window; the pane's presence, not your tier alone, skips the in-process path. A `spec-critic`/`plan-critic` pane replaces the `spec-plan-critic` workflow.
pi has no native batch, so the `reviewer` pane is the review gate.

The seam, per critic/review phase with a pane:

Lazy grid: before step 1, role not running → `dispatch --spawn-role <role>`. `--agent`/`--model` override the `roles.json` spec, `--effort` the task doc's `effort:`; both are written back (a later bare respawn keeps them, no re-dispatch). Failed spawn (missing `roles.json`, role not in this grid, not in tmux) → the died-pane fallback below.
On pi this also launches the review promotion in `EVIDENCE_REVIEW.md`.

1. Write `<seam>.md` (`spec`, `plan` or `review`) into the artifacts dir `<art>` = `<crew_dir>/artifacts/<branch>` (`<crew_dir>` from `WORKER_TASK.md`; create it; bare file names below live there). `refute` (`GRID_PROTOCOL.md`) carries its finding inline; `assess` uses `assess.md`. Review: `git diff "$base_ref"...HEAD > <crew_dir>/artifacts/<branch>/review.diff`, then the resolved roster: `rm -f <art>/roster.json <art>/roster.json.tmp` → `reviewer-roster --base "$base" > <art>/roster.json.tmp` (off PATH: `bash $DISPATCHER_REVIEWERS_DIR/resolve-roster.sh`, adapter-local `reviewers/resolve-roster.sh`) → exit 0 only: `mv` to `roster.json`, name its absolute path in the assignment as `"roster":"<abs path>"`. Resolver missing or non-zero: remove `roster.json.tmp`, no `roster.json`, put `"roster_skipped":"repo-local discovery skipped: <reason>"` in the assignment. The pane reads only the roster its assignment names.
2. Assign the role pane (absolute artifact path, verdict wanted). `crew msg` takes `<from> <to> <body>`; your from is `$CREW_WORKER_ID`:
   ```
   crew msg "$CREW_WORKER_ID" "role:$(git branch --show-current):<role>" \
     '{"seam":"plan","artifact":"<abs path>","question":"Is this plan sound?"}'
   ```
   The review seam carries the roster (or `roster_skipped`):
   ```
   crew msg "$CREW_WORKER_ID" "role:$(git branch --show-current):reviewer" \
     '{"seam":"review","artifact":"<abs path to review.diff>","roster":"<abs path to roster.json>","question":"Review this diff."}'
   ```
3. Await the verdict from your shell tool, tool timeout 360s (360000 if it takes milliseconds):
   ```
   crew await "$CREW_WORKER_ID" --from "role:$(git branch --show-current):<role>" --timeout 300
   ```
   Reply: verdict JSON (`verdict`, `findings`, `evidence`); an await prints every due msg from the role, one per line, oldest first: handle earlier lines first, the newest is the verdict. A review-seam reply must carry `"seam":"review"` and a `verdict`.
   On pi, `crew status`'s review gate reads it; a reject blocks `pr_open` until the reviewer's next verdict.
   A re-request (any lead → reviewer msg but the final release) cancels every earlier verdict and your own earlier seam until a fresh one arrives; no push in between.
   `--from` limits the wait to that role (other roles' and stale replies neither release nor get consumed) and also returns the role's `role_exited` msg and the watcher's assignment events (same id); a line with `event`, not `verdict`, is never a verdict. Never hand-roll a poll loop over `events.jsonl` or the bus log.

   Bound the wait: one `--timeout 300` await = one cycle, separate from the far longer 24-cycle dispatcher-reply wait ("Report to the bus"); never hold a shell call or set a tool timeout over 600s. Empty return with the `ended after Ns` line (without it: no cycle, re-run the await) → read pane state in your window, `<role>` rows only:
   `tmux list-panes -t "$TMUX_PANE" -F '#{pane_id} #{@crew_role} #{@crew_state}'`.
   A respawn leaves the dead pane's `exited` row: judge the live pane; `exited`/missing counts only when none is live. Then fold stragglers (step 5); verdict → step 4. Otherwise:
   - `working` → another cycle, ≤ 3 `working` cycles (~15 min) since the role's current assignment; then `tmux kill-pane -t <pane>`, died-role path.
   - `idle`, no verdict → `tmux capture-pane -p -t <pane>`, apply "Assignment events". Text still in the input box: don't re-send (it pastes a second copy). Empty box, no assignment event explaining it: re-send the step 2 assignment once, one more cycle; `idle` again → kill the pane, died-role path.
   - `exited` or no pane → the died-role path (respawn once, else fall back). `dispatch --spawn-role` no-ops while a live pane exists (hence kill stalled panes first). After a respawn, re-send the step 2 assignment (new pane never saw it): fresh budget (3 `working` cycles, one `idle` re-send).
   **Assignment events.** The role-pane watcher posts them from the role's id (awaits and `crew inbox` return them like role replies); they describe the typed assignment, never the review:
   - `assignment_deferred` with `"delivery":"pull"`: the role never pulled it (ignore one from a role already released with `final`). Don't re-send or wait out the `working` budget: `tmux kill-pane -t <pane>` first (a late pull must not run it twice), then that phase's died-role fallback (below) now, no respawn (the same engine would defer again).
   - `assignment_deferred`, typed or `delivery` absent: the watcher still queues it, typing it once the pane idles at its input box. Never press Enter or re-send (it would run twice). Clear whatever holds the pane (a dialog is the dispatcher's to answer); await again under the `working` budget.
   - `assignment_unsubmitted`: typed, no turn start seen, watcher stopped typing. `tmux capture-pane -p -t <pane>` right before acting. Idle input box still holding the text → `tmux send-keys -t <pane> Enter`, never when the tail shows a dialog (`Do you want to proceed?`, `Enter to select`/`Enter to confirm`, a numbered option row) or a live turn. To re-send instead, `C-u` the box first (else a second copy runs after your Enter). `could not confirm` with an empty or unrecognised box: don't re-send; await again under the `working` budget, then kill the pane, died-role path.
   - `assignment_orphaned`: the watcher exited undelivered, as the pane lost this role's stamps (a tmux restore). Treat the role as died: died-role path, then re-send the step 2 assignment.
4. Ingest with receiving-code-review discipline: `accept` → proceed; `revise` → fix real findings, rewrite the artifact, re-assign once (plan/review cap of 2 unchanged); `reject` → escalate in the PR body. After a `plan-critic` pane's first verdict, post the plan seam (Plan of record); the pane's verdict msg is not the seam.
5. Fold stragglers after every await, before advancing, as in "Report to the bus": `crew inbox "$CREW_WORKER_ID" --since <seen>`.

A role is one-shot per assignment (re-parks after its verdict). Pipeline done → release each role so it exits (mandatory completion step):
`crew msg "$CREW_WORKER_ID" "role:$(git branch --show-current):<role>" '{"final":true}'`.
Missed release: panes idle until their watchers time out (`status failed`, detail `no assignment`); nothing is lost:

- Reap reclaims a finished grid window without a release or manual `tmux kill-window` (#194): terminal lead status + `--idle` threshold (default 300s) → the idle-release phase (reap's, or the lead's own `stall-watch` once its pane is provably idle) kills the window, engine commands and role panes included; `done`/`failed` lead always, `exited` lead only while no engine process is live in the tree (#69). Same pass, reclaim phase: worktree removed once the PR merged/closed and no pane is live.
- Role rows never count as failures: roles write as `role:<branch>:<role>`, not `worker:<branch>`, and `crew rate`/`crew retro` fold only `worker:` rows; an idle-timeout `no assignment` never changes a run's outcome.

A role whose engine exits before your release posts `role_exited` to you (step 3) and a `blocked` status under `role:<branch>:<role>` for the dispatcher; treat it as died: respawn once (`dispatch --spawn-role <role>`; an exited pane is not "already running") or fall back.

Died role (pane gone): fall back to that phase's normal path (native subagent / `spec-plan-critic` workflow / code-review batch) if the engine can spawn a fresh context, else the unavailable-gate path (blocked to the dispatcher).
pi cannot: take the unavailable-gate path (Code review gate), never review in the lead context.
After the pipeline you may also run `dispatch --reap-roles` (kills all role panes in your window, lazy or not), never instead of the per-role release.
