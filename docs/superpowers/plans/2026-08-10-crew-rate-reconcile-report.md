# `crew rate` — GitHub reconcile, cost proxy and rollup: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the worker/model rating system (#36). Phase 1 (bus sweep → global store) already shipped; this plan adds the t2 GitHub reconcile, the cost proxy, and `crew rate --report` — the rollup #86 will read.

**Architecture:** The `rate)` case arm of `adapters/core/crew.sh` grows a flag parser splitting two paths. The **sweep** path keeps its existing dispatch-boundary-windowed jq fold, extends the record (shape, wall clock, cost, `owns_pr`, a total `outcome`), then reconciles each owned GitHub PR through `gh` and merges the result forward over the stored row before appending only what changed. The **report** path never touches the local bus or the network: it folds the global store last-wins by `run_id` and renders a per-`(engine, model, tier)` table (or `--json`) in which every aggregate carries its own denominator.

**Tech Stack:** bash (`writeShellApplication`-wrapped, so `set -euo pipefail` is ambient), `jq`, bats, `gh`. `nix flake check` is treefmt + git-hooks (shellcheck/prettier/statix/deadnix); bats is a separate CI step (`nix develop -c bats tests/`).

**Spec:** `docs/superpowers/specs/2026-08-10-crew-rate-reconcile-report-design.md` — authoritative. Where this plan and the spec disagree, the spec wins; where the spec is silent, "Design decisions this plan locks in" below decides.

---

## Global constraints

- **Default branch is `extract`, not `main`.** Open the PR against `extract`.
- **Scope guard — files this plan may touch, and no others:**
  - Own: `adapters/core/crew.sh` — **the `rate)` case arm**, plus **two** new helper functions (`_burn_weight`, `_gh_json`) beside the other `_`-prefixed helpers, plus the one-line `usage:` string at the bottom. Nothing else in that file. Also a **new** `tests/rate.bats`, and the two docs already written (spec + this plan).
  - **Never touch:** `crew msg`, `crew reap`, `crew id`, `_crew_id`, `crew register`, `crew roster`, `crew watch`, `crew status`, or the shared prologue (`crew.sh:226-234`) — live workers are executing every one of those right now. **`tests/crew.bats` is owned by another live worker: do not open it for editing.** Also never touch `adapters/claude-code/**`, `adapters/codex/**`, `adapters/cursor/**` (generated trees), `DISPATCHER_PROTOCOL.md`, `dispatch-orchestration.md`, or `dispatch.sh`.
  - **`WORKER_PROTOCOL.md` needs no edit** (spec §Scope boundary: the `metrics:` emit is already all-tier). Therefore **`scripts/gen-adapters.sh` does not need to run.** If you find yourself wanting to edit a protocol, stop and ask the dispatcher on the bus instead.
- **Tasks 1–4 all edit the same `rate)` region and MUST run SEQUENTIALLY** — never as concurrent subagents. Task 5 (tests) may only start once Task 4 lands, because it asserts the finished behaviour. Strict chain: 1 → 2 → 3 → 4 → 5 → 6.
- **Locate `rate)` by symbol, not line number.** It is the `rate)` case arm (currently ~line 726) and every task moves it.
- `crew.sh` has **no shebang and no `set -euo pipefail`** — `writeShellApplication` prepends them, and tests run it as `bash -euo pipefail "$CREW"`. Write code that survives `set -e`: **never end an `if`/`else` branch or a function with a bare `A && B` list** — its exit status 1 propagates and kills the script. Use explicit `if … then … fi`. Command substitutions that may legitimately fail need `|| true`.
- **The four pre-existing `crew rate` tests in `tests/crew.bats` (≈ lines 201, 504, 748, 1335) must keep passing untouched.** They run sandboxed with no network and pass no flags. Verify after every task with `nix develop -c bats tests/crew.bats -f rate`.
- Conventional commits (`feat(crew): …`). Pre-commit hooks reformat markdown (prettier); if a commit fails with "files were modified by this hook", `git add` the reformatted files and re-run the **same** commit.
- Run every command from the repo root: `/Users/noams/Data/git/.worktrees/git/dispatcher/feat-36-worker-model-rating-system-feeding-routi`.
- **No test may make a real network call.** Every test stubs `gh` on PATH.
- **Manual verify steps must not touch the developer's real state.** This worktree's `.git/crew/events.jsonl` is live and populated, so a bare `crew rate` at the repo root appends real rows to `~/.local/share/crew/ratings.jsonl` and — from Task 3 on — issues real `gh` calls against this repo's own PRs. Every hand-run verification must set a scratch `XDG_DATA_HOME` and, from Task 3 on, put a stub `gh` first on `PATH`.
- **`crew.sh` shifts the subcommand off (`crew.sh:200-201`), so inside `rate)` the first flag is `$1`.**

## Design decisions this plan locks in (read before Task 1)

1. **Two jq passes, not one.** The sweep stays a single jq program producing t1 records; the t2 reconcile is a bash loop over those records issuing `gh` calls; a second jq program merges t2 + stored rows. Do not try to do GitHub I/O inside jq.
2. **`gh` is invoked through one wrapper**, `_gh_json <args...>`, which **always returns 0** and prints either the JSON (success) or nothing (failure, including `gh` absent from PATH). Every call site tests for empty output and treats it as `last_query_ok:false`. Always-0 is the whole point: under the ambient `set -e`, `out=$(_gh_json …)` with a non-zero return would abort `crew rate` at the first failed call — exactly the merge-forward path Task 5 tests. A `|| true` inside the wrapper protects the wrapper, **not** the caller's assignment, so the return code must be 0 at the boundary.
3. **The URL gate is a bash regex, evaluated before any call**: `^https://github\.com/([^/]+)/([^/]+)/pull/([0-9]+)$`. `owner`, `name` and `number` come from `BASH_REMATCH`. A non-match means zero calls — this is what keeps the pre-existing `crew.bats` fixture (`https://example.com/pr/1`) network-free.
4. **Store reads are `jq -s` over the whole file**, folded `group_by(.run_id) | map(max_by(.swept_at))` — the `crew roster` idiom. A missing store folds to `[]`, not an error.
5. **Humanised durations**: `< 90m` → `<n>m`, else `<n.n>h`. One helper, used by `ttpr` and `ttmerge`.
6. **Rendering pads inside jq — do NOT use `column -t`.** `column` is not in `crew`'s `runtimeInputs` (`flake.nix:110`: `git jq coreutils gnugrep tmux gh gtrash` + `pr-watch`), not in the devShell, and appears nowhere in this repo — `crew report` emits raw `@tsv` (`crew.sh:713-724`). It would resolve only from the host, and BSD `column` (darwin dev box) pads differently from util-linux `column` (CI's `ubuntu-latest`), especially around the multi-byte `—`. Task 5 asserts the table byte-for-byte, so the padding must come from code under test: compute each column's width in jq and emit already-padded rows. Adding `util-linux` instead would mean editing `flake.nix`, which the scope guard forbids.
7. The `!`/`(k)` markers are produced inside jq as strings, so the table and `--json` share one aggregation program.

---

### Task 1: flag parsing, the burn-class helper, and the report path skeleton

**Model:** default (sonnet).

**Files:** Modify `adapters/core/crew.sh`.

**Why first:** it establishes the two-path split every later task edits inside, and it fixes the `[ -f "$log" ] || exit 0` short-circuit (spec §`crew rate --report`) before any code depends on the old shape.

- [ ] **Step 1: Add `_burn_weight` beside the other helpers** (near `_lock_acquire`, above the `case`). Maps a model id to `class<TAB>weight` per the spec's burn table, `""` for unknown:
  - `composer-2.5*` → `free 0`; `haiku*`/`gpt-5.6-luna`/`cursor-grok-4.5-low-fast` → `cheap 1`; `sonnet*`/`gpt-5.6-terra`/`cursor-grok-4.5-medium-fast` → `standard 2`; `opus*`/`gpt-5.6-sol`/`cursor-grok-4.5-high` → `premium 4`; `claude-fable-5`/`fable*` → `fable 8`.
  - Use a `case` with leading-wildcard globs (`*opus*`, `*sonnet*`, `*haiku*`, `*fable*`) so `claude-opus-5` and `opus` both hit `premium`. **`kimi-k3*` deliberately falls through to unknown** — the burn doc does not class it (spec §Cost). **Match the cursor rungs on their exact `cursor-grok-4.5-*` strings, never on `*high*`** — a `*high*` arm would swallow `kimi-k3-high`, destroying both the deliberate-unknown rule and the Task 5 cost test.
- [ ] **Step 2: Parse flags at the top of `rate)`, before the `[ -f "$log" ]` guard.** Accept `--report` and `--json`; reject anything else with `crew: rate takes --report and --json` to stderr, exit 1. `--json` without `--report` is the same error class. Move the `[ -f "$log" ] || exit 0` guard so it guards **only** the sweep path.
- [ ] **Step 3: Add the report path stub** — when `--report` is set, run the report and `exit 0` without touching `$log`. For now have it print only the header row so the "empty store" behaviour is right from the start; Task 4 fills in the body.
- [ ] **Step 4: Extend the usage string** at the bottom of `crew.sh` from `rate` to `rate [--report [--json]]`.

**Verify:** `nix develop -c bats tests/crew.bats -f rate` still green; `crew rate --bogus` exits 1; `crew rate --json` exits 1; `crew rate --report` prints a header in a repo with no bus.

---

### Task 2: extend the sweep record (t1)

**Model:** default (sonnet).

**Files:** Modify `adapters/core/crew.sh` — the sweep's jq program inside `rate)`.

**Sequencing:** after Task 1. Purely local; still no network.

- [ ] **Step 1: Add `shape`, `t0_ms` and `window_end_ms`.**
  - `shape` — `($d.shape // null) | if . == "" then null else . end`. `dispatch.sh` writes `""`, not null (spec §2).
  - `t0_ms: $t0` and `window_end_ms: (if $i+1 < ($runs|length) then $runs[$i+1].ts else null end)`. **These are load-bearing, not bookkeeping.** `$t0` and `$t1` are jq-local bindings (`crew.sh:741-742`) that die with the sweep program, but Task 3 runs as a bash loop plus a _second_ jq program and needs both: `time_to_merge_ms = merged_at_ms − t0_ms`, and the Actions filter is `created_at ∈ [t0_ms, window_end_ms // +∞)`. Without them the windowed CI filter — the thing that stops a reused branch inheriting the previous run's CI — is simply not computable, and the branch-reuse test in Task 5 cannot pass.
- [ ] **Step 2: Add `wall_clock_ms`** — `(last status whose state is "done" or "failed").ts − $t0`, else null. **Narrower than the repo's terminal set on purpose** (spec §Cost): `pr_open` and `exited` do not complete a run.
- [ ] **Step 3: Add `cost_class` / `cost_proxy`.** jq cannot call `_burn_weight`, so pass the class/weight in from bash: build a `{model: [class, weight]}` object from the distinct models in the log and hand it to jq with `--argjson`. Spell the lookup **null-safely** as `($costmap[$d.model // ""] // null)` — a dispatch event with no `model` makes a bare `$costmap[$d.model]` raise "Cannot index object with null" and abort the entire sweep, the same class of failure the `try fromjson catch null` comment at `crew.sh:761-763` exists to prevent (and all four `crew.bats` fixtures set `model`, so no existing test would catch it). `cost_proxy = weight * wall_clock_ms`, null when either side is null. Note `free` has weight 0, so a `free` model yields `0`, **not** null — 0 is a measured cost.
- [ ] **Step 4: Add `owns_pr`.** Per branch, the candidates for a `pr_url` are the runs whose window contains a status carrying it; the **owner is the earliest such candidate** (spec §Branch reuse). Decide it from the bus fold — `$runs` plus each run's own windowed status set — with no network call. Note this is a _cross-run_ comparison on the branch: the current run's `$st` alone is not enough, and collapsing it to `owns_pr = ($pr != null)` silently reintroduces the double-counted merge this field exists to prevent. Single-run branches (the overwhelmingly common case) get `owns_pr: true` whenever `pr_url != null`.
- [ ] **Step 5: Null `time_to_pr_ms` when `owns_pr == false`.** Leave `reached_pr` as-is. This stops a re-announcing fix-up run from poisoning the `ttpr` median.
- [ ] **Step 6: Replace the `outcome` expression** with the spec's precedence table. **Rule 5 is the `else` arm** — the map must be total, or a run vanishes from the rollup entirely:
  1. `owns_pr and pr_state == "MERGED"` → `merged` (always false this task; t2 arrives in Task 3)
  2. last status ∈ {`working`,`blocked`} → `running`
  3. `pr_url != null` → `pr_open`
  4. last status ∈ {`done`,`failed`} → `failed`
  5. else → `incomplete`
- [ ] **Step 7: Add t2 placeholders** so every row has a stable shape from the first sweep: `pr_state`, `last_query_ok`, `merged`, `closed_at_ms`, `merged_at_ms`, `merge_commit`, `time_to_merge_ms`, `review_rounds`, `first_ci_green`, `unresolved_notes`, `reverted` — all null.

**Verify:** `nix develop -c bats tests/crew.bats -f rate` green (those fixtures use `sonnet`, so they now carry a `standard`/`2` cost class — additive keys only, and their assertions select named fields). Then, against a swept store, `jq -e 'has("t0_ms") and has("window_end_ms")' "$XDG_DATA_HOME/crew/ratings.jsonl"` must exit 0 — Task 3 is unbuildable without both keys.

---

### Task 3: the t2 GitHub reconcile, merge-forward, and the two-window lock

**Model:** `implement: opus` — this is the one genuinely subtle step: a partial-failure merge across an append-only store, plus a lock split around network I/O where the wrong ordering silently loses reconciliation.

**Files:** Modify `adapters/core/crew.sh`.

**Sequencing:** after Task 2.

- [ ] **Step 1: Add `_gh_json`** (decision 2) — runs `gh "$@" 2>/dev/null`, prints stdout on success, prints **nothing and returns 0** on failure. The always-0 return is what lets call sites be plain `out=$(_gh_json …)` under `set -e`.
- [ ] **Step 2: First lock window** — acquire, `jq -s` fold the store to the last row per `run_id`, release. Hold nothing across the network. **Trap discipline:** `_lock_release` is an unconditional `rm -rf "$1"` with no owner check (`crew.sh:139`), so an `EXIT` trap left armed during the unlocked network phase would delete a _concurrent_ sweep's lock dir when this process exits. Arm `trap '_lock_release "$lockd"' EXIT` immediately on each acquire and `trap - EXIT` immediately after each release.
- [ ] **Step 3: For each swept record, decide what to call.** Gate: `owns_pr` **and** the `pr_url` matches the regex (decision 3). Then per-call finality (spec §Per-call finality):
  - `gh pr view "$pr_url" --json state,closedAt,mergedAt,mergeCommit,commits,reviews` — skip when stored `pr_state == "MERGED"`, or `CLOSED` with `now − closed_at_ms ≥ 30d`.
  - `gh api "repos/<owner>/<name>/actions/runs?branch=<branch>&per_page=100"` — skip when stored `first_ci_green != null`.
  - `gh api graphql -f query='query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{isResolved}}}}}' -F owner=… -F name=… -F number=…` — skip when stored `pr_state == "MERGED"` **and** stored `unresolved_notes != null`. Note `-F` (typed) not `-f` for `number`, which must be an Int. This repo has no existing `gh api graphql` call to copy — `pr-watch.sh:112` uses REST — so verify the query shape against `gh api graphql --help` before relying on it.
  - **Always pass the URL explicitly.** A bare `gh pr view` resolves the _sweeping checkout's_ branch and would write that PR into every row.
  - **Convert every gh timestamp at ingest** with `fromdateiso8601 * 1000`. `mergedAt`, `closedAt` and `created_at` are ISO-8601 _strings_ while `t0_ms`/`window_end_ms` are epoch ms. `merged_at_ms − t0_ms` would fail loudly, but the Actions window filter fails **silently**: in jq's total ordering any string sorts above any number, so `created_at >= $t0_ms` is unconditionally true and `< $window_end_ms` unconditionally false, and the window filter quietly does nothing.
- [ ] **Step 3b: Derive the three computed t2 fields** exactly as the spec pins them:
  - `review_rounds` — reviews in {`CHANGES_REQUESTED`,`COMMENTED`} sorted by `submittedAt`; a round is a maximal group followed by ≥1 commit with a later `committedDate`. **Three reviews before one fix commit is one round.**
  - `first_ci_green` — keep only runs with `created_at` inside `[t0_ms, window_end_ms // +∞)` (both read off the record, Task 2 Step 1), group by `head_sha`, take the sha with the smallest earliest `created_at`, true iff every **completed** run on it concluded `success|skipped|neutral`; no runs in window → null.
  - `unresolved_notes` — count of `isResolved == false`.
- [ ] **Step 4: `reverted`** — merged runs with a `merge_commit` only. First `git cat-file -e "<oid>^{commit}"`; **object absent → `null`, not `false`** (otherwise every unfetched repo reports a clean `false`). Present → `git log --all --since="@$((merged_at_ms / 1000))" --grep="This reverts commit <oid>" --max-count=1`, non-empty ⇒ true. **Note the unit.** `merged_at_ms` is epoch **milliseconds** after Step 3's ingest conversion, but `git log --since` takes an approxidate string or `@<epoch-seconds>` — and approxidate does not error on a bare 13-digit number, it silently falls back, so the traversal window would be wrong and `reverted` would read `false` for every merged run.
- [ ] **Step 5: Merge-forward, against the _fresh_ Step 6 read — not the Step 2 snapshot.** The Step 2 fold is only for deciding which calls to skip; carrying forward from it would lose a t2 upgrade another sweep landed while this one was on the network. For every field whose call did not succeed, carry the **stored** value forward rather than writing null. Set `last_query_ok:false` on any run that attempted a call and failed; `pr_state` is written **only** from a successful `gh pr view`. Recompute `outcome` rule 1 and `merged` after t2 lands.
- [ ] **Step 6: Second lock window** — re-acquire, re-read the store, append only records that differ from the stored one **ignoring `swept_at`**, release. Excluding `swept_at` is what makes a no-change sweep a byte-level no-op; without it every sweep appends every row forever.
- [ ] **Step 7: Losing the second lock exits 1 having written nothing.** Do not retry, do not fall back to an unlocked append — just let the next sweep redo the work (spec §Merge-forward).

**Verify:** `nix develop -c bats tests/crew.bats -f rate` green — and confirm no `gh` call is made by those fixtures (temporarily put a `gh` stub that `exit 1`s on PATH and re-run; the results must be identical).

---

### Task 4: the `--report` renderer

**Model:** default (sonnet).

**Files:** Modify `adapters/core/crew.sh`; create `tests/rate.bats`; regenerate the sample block in the spec (Step 6).

**Sequencing:** after Task 3.

- [ ] **Step 1: Fold the store** last-wins by `run_id`, group by `(engine, model, tier)`, sort by those three.
- [ ] **Step 2: Implement one jq aggregation** producing, per cell, every column of the spec's column contract as `{value, k, n}` (`k` = that aggregate's own denominator). Both output modes read this one structure.
  - `n` counts rows whose outcome is neither `running` nor `incomplete`. `inc`/`run` are outside `n`; **`pend` and `rev` count rows that are inside it** and are excluded only from `merge%`/`ttmerge`.
  - `high` averages only rows with `review_mode ∉ {none, null}`. `blocked` averages `blocked_count + watchdog_blocked_count` **over `n`** — those two are bus-derived and never null, so `blocked` is the one aggregate that never carries `(k)`.
  - `cost` renders weight·**hours** — divide the stored weight·ms by 3_600_000.
  - **Percentages `round`, they do not `floor`** — the golden needs `8/9 → 89` and `2/3 → 67`. **Medians on an even count take the mean of the two middle values.** Both are pinned here so the fixture author is not guessing.
- [ ] **Step 3: Render markers** — `value` when `k == n`, `value(k)` when it differs; `!` appended when `k < 5`; `n` itself takes `!` when `n < 5`; raw counts never take a marker; `—` for `k == 0`.
- [ ] **Step 4: Emit the table** with widths computed in jq (decision 6 — no `column`). **One padding rule, stated so the golden test has a reachable target:** build each cell's text by concatenating `value` + `(k)` + `!` _first_; column width = `max(len(header), max len(cell))`; `engine`/`model`/`tier` left-aligned, every other column right-aligned; two spaces between columns. Then the legend line, then a footer counting **rows** that carry at least one small-sample quantity.
- [ ] **Step 5: `--json`** emits the same per-cell structure with each aggregate as `{value, k, n}` and raw counts as plain numbers.
- [ ] **Step 6: Land the golden-fixture test now, not in Task 5.** Build the store that produces the spec's worked example and assert the rendered rows byte-for-byte in `tests/rate.bats` (create the file with just the harness + this test). The harness needs `load helpers` + `setup_repo` + `export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"`, exactly as `tests/pr-watch.bats:3-14` does — `--report` still goes through the prologue's git-repo gate. Pad in jq with `length`, which counts **codepoints**, so `—` measures as 1 and needs no byte-length workaround. `--report` with `n` visible is a whole acceptance criterion; shipping it behind "eyeball it" leaves it unverified until the last task.
  - **The spec's sample block is hand-aligned and no uniform rule reproduces it byte-for-byte** — its `!` sits outside the field in the `n` and `ttpr` columns but inside it in `pr%`. So: **regenerate the fenced block in the spec from the implementation's actual output, in this same commit**, and assert against the regenerated block. The spec is inside this plan's touchable file set, so this is in scope.
  - **The regeneration may change whitespace only.** Every `(k)`, `!`, `—` and numeric value in the example must come out identical — those are what the golden exists to pin, and a "fix" that alters one is a renderer bug, not an alignment difference. If a value changes, stop and fix the renderer instead of the spec.
  - Every value in the block **is** reachable from integer inputs — verified: `0.6(11)`=7/11, `1.1`=13/12, `0.3`=4/12, `0.2`=2/12, `0.5(2)`={0,1}, `0.7`=2/3, `1.3`=4/3, `90(10)`=9/10, `89(9)`=8/9, `67`=2/3. Build the fixture to hit them; do not adjust the block to match a fixture you found easier to write.

**Verify:** `nix develop -c bats tests/rate.bats` green.

---

### Task 5: `tests/rate.bats`

**Model:** default (sonnet).

**Files:** Extend `tests/rate.bats` (created in Task 4 Step 6). **Do not open `tests/crew.bats`.**

**Sequencing:** after Task 4. Four themed steps rather than one bulk step, so each subagent turn stays reviewable.

- [ ] **Step 1: Extend the harness** — add a `stub_gh` that serves canned JSON per call kind (`pr view` / `api …/actions/runs` / `api graphql`) from files the test rewrites, logging argv to `$STUB_LOG`, modelled on `tests/pr-watch.bats`'s `stub_gh`. Keep `export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"` so the developer's real store is never touched.
- [ ] **Step 2: Store semantics** — idempotency (store byte-identical after a second sweep); last-wins fold; t1→t2 upgrade; merge-forward under a `gh` that fails (stored t2 survives, `last_query_ok:false`, `pr_state` unchanged); `--report` on an empty store prints a header and exits 0; `--json` without `--report` errors.
- [ ] **Step 3: Reconcile behaviour** — per-call finality (MERGED skips `pr view` but still calls Actions while `first_ci_green` is null; CLOSED inside 30d re-queries, outside does not); call targeting across two repos, and **`gh pr view` never invoked without a URL argument**; branch reuse (`owns_pr` on the earliest candidate, t2 null and `time_to_pr_ms` null on the later, windowed CI); no `gh` call at all for a non-GitHub `pr_url`.
- [ ] **Step 4: Derived fields and the report** — `review_rounds` batching (3 reviews + 1 commit ⇒ 1; APPROVED ⇒ not a round; trailing reviews ⇒ 0); `first_ci_green` (earliest-sha, mixed ⇒ false, none in window ⇒ null); `unresolved_notes`; `reverted` null when the merge commit is absent locally; `outcome` precedence, one fixture per row of the table; cost (known ⇒ weight×wall-clock, `kimi-k3-high` ⇒ null class and proxy); the report's `!` firing on a small-denominator quantity inside a large-`n` row.
- [ ] **Step 5: The `crew watch` test** — emit the `metrics:` msg **first**, then run `crew watch --since 0 --timeout 1 --interval 1` synchronously and assert empty stdout. `watch` is a poll over `events.jsonl`, so ordering it this way asserts the same property without backgrounding (bats `run` cannot background, and `watch` needs `CREW_ID`). Split streams with `run --separate-stderr` as `tests/pr-watch.bats` does.

**Verify:** `nix develop -c bats tests/rate.bats` fully green.

---

### Task 6: gate

**Model:** default (sonnet).

- [ ] **Step 1:** `nix develop -c shellcheck adapters/core/crew.sh` clean.
- [ ] **Step 2:** `nix develop -c bats tests/` — green **except** the two known-red reap-release tests in `tests/crew.bats` (#54, #58), a pre-existing macOS-host failure owned by another worker under #49. Do not fix them; do not report them as this branch's failures.
- [ ] **Step 3:** Confirm `git status` shows only: `adapters/core/crew.sh`, `tests/rate.bats`, the spec, this plan — **plus the untracked `WORKER_TASK.md`, which is this worker's own dispatch artifact and must never be removed** (`_crew_id` at `crew.sh:188-198` falls back to reading `crew_id:` out of it whenever `CREW_ID` is unset, so deleting it silently breaks every remaining `crew status`/`crew msg` this session makes). Any _other_ path is a scope-guard violation — revert it.
