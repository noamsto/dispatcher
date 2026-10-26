# Worker protocol: ship

Phase file of `WORKER_PROTOCOL.md`: it binds like the core. References to "this protocol" or a section of it resolve through the core's Phase files table.

## Rules 4 and 5 (detail)

4. **Push through the gate.** Order: code-review gate → review seam (standard/deep) → `/deslop` → affected tests if the repo ships a selector (fast gate) → deslop seam → pre-push peek → `git push`. `/deslop` = harness `deslop` skill, every engine.
   Plain `deslop` on pi.
   Scope: the diff to push (stacked layer: `base` per Base ref; else unpushed commits) → commit its cleanup → `crew msg "$CREW_WORKER_ID" "review:$(crew id)" '{"seam":"deslop"}'` (only after it ran). standard/deep `kind: implement`: `crew status` refuses `pr_open`/`done` without this branch's deslop seam; earlier sessions' seams count, later commits don't void it, but each fix round re-runs `/deslop` before pushing. Trivial: same, unenforced.
   Cleanup, CI-failure or hook fix changed behavior → redo affected evidence and targeted review gates before re-push. The git pre-push hook (typecheck/lint/unit/build-num) binds every pusher: fail → fix, re-push, never bypass. Git is non-interactive (`GIT_EDITOR=true`, `GIT_SEQUENCE_EDITOR=:`); still pass `-m`/`--no-edit`, no editors.
5. standard/deep: push (rule 4) → `gh pr create` → `pr_open` → `done`; `crew status` refuses `pr_open`/`done` without the review and deslop seams, plus the plan seam under `plan: required` (Plan of record). `--draft` only if `WORKER_TASK.md` `draft:` is `true`. Header `base:` (`sed -nE '/^$/q; s/^base: //p' WORKER_TASK.md`, same tool call as `gh pr create`) → `--base` that value, none → no `--base`; parent branch gone on origin → block→await; never push another layer's branch or fall back to the default branch. Keep the assignee. Body per "PR body contract": every closes line from your task file, verbatim; escalations; blocking unresolved review notes; non-blocking deferrals only after create ("Deferred findings"). Skipped plan phase (plan of record) → `## Summary` plan-skip line `Plan: task doc (provided)` (`plan: provided`), `Plan: task doc (self-gate)` (self-assessed legacy doc) or `Plan: recovered (resume)` (`resume: true`).

## Pre-push tracked-scaffolding check (all tiers)

Pre-push also checks this branch's changes: `WORKER_TASK.md` staged or committed here (in `git diff --name-only "$base_ref"...HEAD` or `git diff --cached --name-only`) → stop and drop it, never push. Staged: `git restore --source=<base> --staged WORKER_TASK.md`. Committed: drop it from that commit and rewrite it (`git rm --cached WORKER_TASK.md && git commit --amend`, or an interactive rebase); never discard a commit carrying real work. `.git/info/exclude` hides only an untracked `WORKER_TASK.md`; tracked, it rides the diff into commits (#397).

## PR body contract (standard/deep)

Every visible line must help a human reviewer decide something; agent-facing state (full ledgers, harness diagnostics) stays out. Visible order, each section only if it has content:

- Header `Closes` lines, primary first (`Closes #<N>` / `Closes <TEAM>-<N>`; bundled task: one per issue).
- `## Summary`: compact what, why, needed design rationale; plan-skip line if applicable (rule 5).
- Contract/risk notes; shared-contract change: one sentence on the consumer map (full map: Agent ledger, `EVIDENCE_REVIEW.md`).
- `## Testing` (replaces Evidence/Test-plan): one line per command + result; raw output only for evidence CI cannot reproduce, trimmed to decisive lines.
- `## Review notes`: one disposition line (e.g. `deferred (reported)`, `dropped (below bar)`, `refuted`) per open, deferred or refuted item; no fixed rows, SHAs, round counts.
- `## Escalated`, `## Assumptions`, `## Follow-ups (untracked)` (worker-written, "Deferred findings"), in that order.

Always last: one collapsed `<details><summary>Agent ledger</summary>…</details>` block with full recurrence ledger (all `EVIDENCE_REVIEW.md` ledger-table fields) and acceptance ledger (all `## Acceptance` items with evidence). Add it at PR create if ledger data exists, else with the first `gh pr edit` carrying some; update via `gh pr edit`. New visible sections (e.g. Follow-ups) go before it.

## Deferred findings (standard/deep)

Non-blocking findings never ride only in the PR body: fix in place or report. Blocking correctness findings and critic escalations (`## Escalated`) follow `EVIDENCE_REVIEW.md`. A `kind: review` worker reports nothing; its findings stay in the posted review.

- Belongs to this task? Ask first. No (report it; any one outranks the yes tests): pre-existing and orthogonal, needs a design decision the task doc leaves open or shared-code refactor outside the feature, touches an unrelated subsystem. Yes: diff introduced it, or it sits in changed code (or those files' direct test/doc siblings) and the task goal is visibly incomplete or wrong without it. Task doc's file list is expected scope, not a fence (a sibling test or the same function's error message belongs), unless it fences explicitly ("touch only these files").
- Fits in this run? (belonging findings only):
  - mechanical (no behavior change, fast deterministic gate covers it) → fix via that gate within its batch's round, no re-review.
  - behavioral → fix, ride the next round's targeted re-review (`EVIDENCE_REVIEW.md`), never beyond the two-round budget.
  - behavioral, no round left (both rounds' batches returned) → report, noting it belongs to the parent task, deferred only for lack of a review round.
- Report unfixed non-blocking findings, same path for every tracker (never `gh issue create`; the dispatcher decides: on `tracker: github` it files, comments, folds and closes on its own judgment, on `tracker: linear` after one human approval).
  - Bar: only a concrete defect: a failure scenario (input or state → wrong result) verified against the code (`file:line`, a reproduction, or a reviewer's confirmed probe), or a correctness/security gap with that same evidence. Nits, style, speculative hardening, "could also handle X": not reported; one `## Review notes` line `dropped (below bar)`, nothing sent.
  - Group: same subsystem/function → one entry with sub-bullets; unrelated → separate entries.
  - `same-area`: a `Closes` issue exists and the entry's `file:line` is in a file or function it names or this diff touches (`pr: N` task: first `Closes` in that PR's body) → mark the entry `same-area` (the dispatcher comments on the origin instead of minting an issue).
  - Exactly one `crew msg "$CREW_WORKER_ID" dispatcher:<crew_id>`, body opening `follow-ups (untracked):`, then self-contained entries: what, evidence (`file:line`), why deferred, `same-area` when it applies.
  - Order: list the entries under `## Follow-ups (untracked)` before sending, so the dispatcher's edit never races yours: `gh pr create` with the section in the body (existing PR: `gh pr edit` first) → `crew msg` with the PR url → `pr_open`; later `gh pr edit`s re-read the body first and keep the section or the dispatcher's `## Follow-ups`.
- `## Follow-ups (untracked)` ("PR body contract"): one line per entry, never finding text; the dispatcher replaces it with `## Follow-ups` links once filed.
- Batch sent → final `crew status "$CREW_WORKER_ID" done "follow-ups: untracked"`; else plain `done`.

## Acceptance ledger (all tiers)

Before `pr_open`, list every item of the task doc's `## Acceptance` (or equivalent list) with evidence: command run, result. Only that evidence makes an item done; `covered by unit tests` never replaces a live/manual/build step the spec names.

- Browser-check items and the `--chrome` carve-out: rule 9 (`WORKER_PROTOCOL.md` "Rules").
- Cannot run an item (no browser, network, credentials, or command won't run): post `blocked "acceptance: <item> — <why>"` → block→await. No PR while an item is unrun and unwaived.
- Only the dispatcher waives, via `crew reply` naming the item (that item only); `crew status` checks per item that `waived(dispatcher…)` has a bus reply to this session naming its id in a waive phrase (`waive AC3`; a negation like "will not waive" doesn't count). Not waivers: PR-body disclosures ("Not done", "Assumptions"), your own low-risk judgement. Every tier, `trivial` included; overrides the low-risk safe-default allowance in "Report to the bus".
- A CI acceptance item's `pass(…)` must carry a CI run id or `actions/runs/` URL on the PR's current head; local gates (pre-push, bats-affected, shellcheck, `nix flake check`) are never CI evidence. CI unfinished → wait, or block for a waiver.
- Ledger in `pr_open` detail: `crew status "$CREW_WORKER_ID" pr_open "AC1 pass(bats); AC2 pass(nix build); AC3 waived(dispatcher)" <url>`. Items exactly `<id> pass(<evidence>)` or `<id> waived(dispatcher)`, joined by `; `; id one token, no spaces (`AC1`, `AC1-3`); notes inside the parentheses (`waived(dispatcher: <note>)`). `crew status` refuses any other form (`pending`, `not run`, `skipped`, `n/a`, `partial`, a note after the parentheses); an empty detail if the task doc has a `## Acceptance` list (else accepted); a ledger omitting an id the doc lists: every id needs an item, one item may stand for a run (`AC1-3 pass(…)`), so an unrunnable item is waived, never left out. `done` is not ledger-checked (detail: `follow-ups: untracked`). Keep it short (bus clips long lines). Full ledger: Agent ledger block ("PR body contract"), not a visible `## Acceptance` heading.

## When done (PR flow)

Standard/deep, after the code review gate and its seam: pre-PR peek → open the PR → `crew status "$CREW_WORKER_ID" pr_open "<acceptance ledger>" <url>` → emit the complete metrics snapshot → pre-done peek → `crew status "$CREW_WORKER_ID" done`. Fresh-PR path: reporting and `done` detail per "Deferred findings". Ping the dispatcher pane once, unless `dispatcher_pane:` is empty (always on a remote host): `tmux display-message -t "$dispatcher_pane" -d 4000 "<agent_name> done: <branch> — PR <url>"`.
