# Dispatch Ratings — Phase 1 (Data Capture) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Capture one persistent rating record per dispatched worker run, folding the crew bus into a global append-only store — the foundation the #86 routing tally reads.

**Architecture:** A new `crew rate` subcommand slurps the current repo's `events.jsonl`, partitions each branch's events into runs by dispatch-boundary windowing, joins dispatch + status + `metrics:` msg + blocked events into one record per run, and appends it (lock-guarded, last-wins-by `run_id`) to `${XDG_DATA_HOME:-~/.local/share}/crew/ratings.jsonl`. Workers are extended to emit their `metrics:` record on **every** tier (not just deep). No GitHub reconcile and no report in this phase (Phases 2 and 3).

**Tech Stack:** Bash + jq (crew.sh is a `writeShellApplication`; runtime inputs `git jq coreutils gnugrep` already present). Prose-only edit to `WORKER_PROTOCOL.md`.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-07-22-dispatch-worker-model-ratings-design.md`. This plan implements **Phase 1** only ("data capture"). Phases 2 (GitHub reconcile) and 3 (cost + report) are separate plans.
- **Store path:** `${XDG_DATA_HOME:-$HOME/.local/share}/crew/ratings.jsonl` — global, cross-repo, created lazily with `mkdir -p`.
- **Store semantics:** append-only; readers fold **last-wins by `run_id`**. A sweep appends the current record for every run in the repo (duplicates across sweeps are expected and deduped on read).
- **`run_id` = `"<repo>:<branch>:<t0_ms>"`** where `repo` is the `owner/repo` slug from `origin`, `branch` from the dispatch event, `t0_ms` the dispatch event `ts`.
- **Windowing:** for each branch, order its dispatch timestamps `d₁<d₂<…`; a non-dispatch event with `dᵢ ≤ ts < dᵢ₊₁` belongs to run `i`. Boundaries are clean by construction (`dispatch.sh:186` `wt switch -c` fails if the branch/worktree exists, so run i+1 can't start until run i's worktree is gone and its close-emit fired).
- **Outcome ∈ {`pr_open`, `failed`, `incomplete`}** in this phase (`merged` is added in Phase 2). Classify by **terminal-status-observed**, never by null metric fields. `incomplete` = no terminal status seen (zombie/killed).
- **No test-only product code.** Tests drive the real script via `bash crew.sh` in a throwaway git repo with a planted `events.jsonl`, overriding `HOME`/`XDG_DATA_HOME` to a tmpdir. (Nix only prepends `set -euo pipefail`; run tests with `bash` directly, live-verify the nix binary post-merge.)
- **crew.sh style:** case-arm code uses plain vars (no `local` outside functions); reuse `_lock_acquire`/`_lock_release` (`crew.sh:33-54`); match the existing jq-slurp idiom used by `crew report` (`crew.sh:409-424`).

---

### Task 1: Extend worker metrics emission to all tiers

The worker's `metrics:` bus record is today gated to deep workers only. Extend it to every tier so trivial/standard runs also open a rating record. Pure `WORKER_PROTOCOL.md` prose edit; deploys on merge (read-by-path).

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (the "Deep workers — emit outcome metrics" bullet, currently the last bullet of the "When done" section, ~line 114)

**Interfaces:**

- Produces: the bus `metrics:<crew>` record shape that Task 2's fold parses —
  `{"consulted":<bool|null>,"plan_critic_first_pass":"<accept|revise|reject|null>","rework_count":<int|null>,"review_high":<int|null>,"review_mode":<str|null>}`.
  Trivial tier (no plan phase / no gate / no reviewer) emits `null` for the fields it never produced; `consulted` stays a bool.

- [ ] **Step 1: Read the current block**

Run: `sed -n '113,118p' home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: the bullet beginning `- **Deep workers — emit outcome metrics (consulted or not).**` with the `crew msg … metrics:$CREW_ID …` fenced command.

- [ ] **Step 2: Replace the block to make it all-tier**

Replace the bullet (`- **Deep workers — emit outcome metrics …` through the paragraph ending `… non-consulted deep workers form the baseline.`) with:

```markdown
- **Every worker — emit outcome metrics before you stop.** On **all** tiers, append a metrics record to the bus so this run can be rated:
```

crew msg "worker:$(git branch --show-current)" "metrics:$CREW_ID" '{"consulted":<true|false>,"plan_critic_first_pass":"<accept|revise|reject|null>","rework_count":<int>,"review_high":<int>,"review_mode":"<full|downgraded|none>"}'

```
(`$CREW_ID` is the `crew_id:` from `WORKER_TASK.md`.) `consulted` = whether the Fable consult ran (deep only; `false` otherwise). `plan_critic_first_pass` = the plan-critic's verdict on the **first** plan draft, or `null` if you skipped the plan phase (trivial). `rework_count` = execute-stage fixes the gates forced (`0` if none). `review_high` = HIGH-severity review-gate findings (`0` if no reviewer ran, e.g. trivial). `review_mode` = which review depth actually ran (`full`|`downgraded`|`none`, per the Code review gate's repo-aware scaling) — so `review_high` is read in context, never compared across mismatched depths. Emit real `null` (not the string `"null"`) for fields a tier never produces. This is a plain `msg` to a synthetic sink — it does **not** wake the dispatcher (its `watch`/`inbox` filter is `to==dispatcher:<crew>`/`*`, never `metrics:<crew>`). **Every worker emits this**, so the ratings store has one row per run.
```

- [ ] **Step 3: Verify the edit reads as all-tier**

Run: `grep -n "Every worker — emit outcome metrics" home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: one match. And `grep -c "Deep workers — emit outcome metrics" home/ai/claude-code/WORKER_PROTOCOL.md` → `0` (old wording gone).

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(dispatch): emit worker metrics on all tiers, not just deep (#102)"
```

---

### Task 2: `crew rate` sweep — fold the bus into the ratings store

Add the `crew rate` subcommand: window each branch's events into runs, build one record per run, append lock-guarded to the global store.

**Files:**

- Modify: `home/ai/claude-code/crew.sh` (add a `rate)` arm to the main `case "$sub"`, before the `*)` usage arm ~line 429; update the usage string in the `*)` arm)
- Test (throwaway, not committed): a scratchpad script under the session scratchpad dir

**Interfaces:**

- Consumes: bus `events.jsonl` (`kind` ∈ `dispatch`/`status`/`msg`); the dispatch event fields `{ts, branch, engine, model, tier, effort, title}` (`dispatch.sh:201-205`); the `metrics:` msg shape from Task 1; `_lock_acquire`/`_lock_release`.
- Produces: `ratings.jsonl` records, each object:

  ```
  {run_id, repo, branch, engine, model, tier, effort, title,
   reached_pr:bool, pr_url:str|null, time_to_pr_ms:int|null,
   outcome:"pr_open"|"failed"|"incomplete",
   rework_count:int|null, review_high:int|null, review_mode:str|null, plan_critic_first_pass:str|null,
   consulted:bool|null, blocked_count:int, reported_ok:bool, swept_at:int}
  ```

- [ ] **Step 1: Write the failing test harness**

Create `<scratchpad>/test_crew_rate.sh` (use the session scratchpad dir):

```bash
#!/usr/bin/env bash
set -euo pipefail
CREW="$HOME/nix-config/home/ai/claude-code/crew.sh"   # or the worktree path
fail=0
assert_eq() { # $1=desc $2=expected $3=actual
  if [ "$2" != "$3" ]; then printf 'FAIL %s\n  expected: %s\n  actual:   %s\n' "$1" "$2" "$3"; fail=1
  else printf 'ok   %s\n' "$1"; fi
}

# --- fixture repo -----------------------------------------------------------
setup() { # writes $1 (jsonl) as the bus log of a fresh repo; echoes the repo dir
  local repo; repo=$(mktemp -d)
  git -C "$repo" init -q
  git -C "$repo" remote add origin https://github.com/noamsto/crew-smoke.git
  mkdir -p "$repo/.git/crew"
  printf '%s\n' "$1" >"$repo/.git/crew/events.jsonl"
  printf '%s' "$repo"
}
run_rate() { # $1=repo -> path to ratings.jsonl (fresh XDG each call)
  local repo="$1" home; home=$(mktemp -d)
  ( cd "$repo" && HOME="$home" XDG_DATA_HOME="$home/share" bash "$CREW" rate >/dev/null )
  printf '%s' "$home/share/crew/ratings.jsonl"
}

# --- windowing: two runs on one branch --------------------------------------
FIX_WINDOW='{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"claude","model":"claude-opus-4-8","tier":"standard","effort":"medium","title":"first"}
{"ts":1100,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}
{"ts":2000,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"pr_open","pr_url":"http://pr/1"}}
{"ts":2500,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"done"}}
{"ts":5000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"claude","model":"claude-sonnet-5","tier":"trivial","effort":"low","title":"second"}
{"ts":5100,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}
{"ts":5200,"crew_id":"c1","kind":"msg","from":"worker:feat/x","to":"metrics:c1","body":"{\"consulted\":false,\"plan_critic_first_pass\":null,\"rework_count\":0,\"review_high\":0}"}
{"ts":5300,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"failed"}}'
r=$(setup "$FIX_WINDOW"); out=$(run_rate "$r")
assert_eq "window: two records" "2" "$(jq -s 'length' "$out")"
assert_eq "run1 run_id"  "noamsto/crew-smoke:feat/x:1000" "$(jq -rs '.[]|select(.title=="first").run_id' "$out")"
assert_eq "run1 outcome" "pr_open" "$(jq -rs '.[]|select(.title=="first").outcome' "$out")"
assert_eq "run1 time_to_pr" "1000" "$(jq -rs '.[]|select(.title=="first").time_to_pr_ms' "$out")"
assert_eq "run1 reached_pr" "true" "$(jq -rs '.[]|select(.title=="first").reached_pr' "$out")"
assert_eq "run2 outcome" "failed" "$(jq -rs '.[]|select(.title=="second").outcome' "$out")"
assert_eq "run2 reached_pr" "false" "$(jq -rs '.[]|select(.title=="second").reached_pr' "$out")"
assert_eq "run2 model kept" "claude-sonnet-5" "$(jq -rs '.[]|select(.title=="second").model' "$out")"
# regression pin: consulted:false must survive (NOT collapse to null via jq //)
assert_eq "run2 consulted false" "false" "$(jq -rs '.[]|select(.title=="second").consulted' "$out")"

# --- zombie: dispatch then silence -> incomplete ----------------------------
FIX_ZOMBIE='{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/z","engine":"claude","model":"claude-opus-4-8","tier":"deep","effort":"high","title":"zzz"}
{"ts":1100,"crew_id":"c1","kind":"status","from":"worker:feat/z","body":{"state":"working"}}'
r=$(setup "$FIX_ZOMBIE"); out=$(run_rate "$r")
assert_eq "zombie outcome" "incomplete" "$(jq -rs '.[0].outcome' "$out")"
assert_eq "zombie reported_ok" "false" "$(jq -rs '.[0].reported_ok' "$out")"

# --- metrics + blocked join -------------------------------------------------
FIX_METRICS='{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/m","engine":"claude","model":"claude-opus-4-8","tier":"deep","effort":"high","title":"mmm"}
{"ts":1100,"crew_id":"c1","kind":"status","from":"worker:feat/m","body":{"state":"working"}}
{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/m","body":{"state":"blocked"}}
{"ts":1300,"crew_id":"c1","kind":"status","from":"worker:feat/m","body":{"state":"working"}}
{"ts":1400,"crew_id":"c1","kind":"msg","from":"worker:feat/m","to":"metrics:c1","body":"{\"consulted\":true,\"plan_critic_first_pass\":\"revise\",\"rework_count\":2,\"review_high\":1}"}
{"ts":1500,"crew_id":"c1","kind":"status","from":"worker:feat/m","body":{"state":"pr_open","pr_url":"http://pr/9"}}
{"ts":1600,"crew_id":"c1","kind":"status","from":"worker:feat/m","body":{"state":"done"}}'
r=$(setup "$FIX_METRICS"); out=$(run_rate "$r")
assert_eq "metrics rework_count" "2" "$(jq -rs '.[0].rework_count' "$out")"
assert_eq "metrics review_high" "1" "$(jq -rs '.[0].review_high' "$out")"
assert_eq "metrics plan_critic" "revise" "$(jq -rs '.[0].plan_critic_first_pass' "$out")"
assert_eq "metrics blocked_count" "1" "$(jq -rs '.[0].blocked_count' "$out")"
assert_eq "metrics reported_ok" "true" "$(jq -rs '.[0].reported_ok' "$out")"

# --- idempotency: sweep twice into the SAME store -> one row per run_id ------
r=$(setup "$FIX_METRICS"); home=$(mktemp -d)
( cd "$r" && HOME="$home" XDG_DATA_HOME="$home/share" bash "$CREW" rate >/dev/null )
( cd "$r" && HOME="$home" XDG_DATA_HOME="$home/share" bash "$CREW" rate >/dev/null )
store="$home/share/crew/ratings.jsonl"
assert_eq "idem: two raw lines" "2" "$(wc -l <"$store" | tr -d ' ')"
assert_eq "idem: one folded run" "1" "$(jq -s 'group_by(.run_id)|length' "$store")"

exit $fail
```

- [ ] **Step 2: Run it to verify it fails**

Run: `bash <scratchpad>/test_crew_rate.sh`
Expected: FAIL (red) — `crew rate` is unknown, hits the `*)` usage arm (exit 1); the first `out=$(run_rate …)` command substitution then fails under the harness's `set -e` and the script aborts non-zero before any `ok` line prints. (Non-green is all we need; the exact abort point is immaterial.)

- [ ] **Step 3: Add the `rate)` arm to crew.sh**

Insert immediately before the `*)` usage arm (~line 429):

```bash
rate)
  # Sweep this repo's bus into the global ratings store. One record per RUN
  # (a run = a dispatch + the branch events until the next dispatch on that
  # branch). Append-only; readers fold last-wins by run_id. No crew filter —
  # ratings are cross-run/cross-crew evidence.
  [ -f "$log" ] || exit 0
  repo=$(git config --get remote.origin.url 2>/dev/null |
    sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##' || true)
  repo="${repo:-$(basename "$(git rev-parse --show-toplevel)")}"
  records=$(jq -s --arg repo "$repo" '
    (map(select(.kind=="dispatch"))) as $disp
    | [ ($disp | map(.branch) | unique)[] as $b
        | ($disp | map(select(.branch==$b)) | sort_by(.ts)) as $runs
        | range(0; ($runs|length)) as $i
        | $runs[$i] as $d
        | ($d.ts) as $t0
        | (if $i+1 < ($runs|length) then $runs[$i+1].ts else 9999999999999 end) as $t1
        | (map(select(
              .kind!="dispatch"
              and (((.from // "") | ltrimstr("worker:")) == $b)
              and .ts >= $t0 and .ts < $t1))) as $ev
        | ($ev | map(select(.kind=="status"))) as $st
        | ($st | sort_by(.ts) | (.[-1] // null)) as $last
        | ($last.body.state) as $ls
        | ($st | map(.body.pr_url) | map(select(.!=null)) | last) as $pr
        | ($st | map(select(.body.state=="pr_open")) | sort_by(.ts) | (.[0] // null)) as $propen
        | ($st | map(select(.body.state=="blocked")) | length) as $blocked
        | ($ev | map(select(.kind=="msg" and ((.to // "") | startswith("metrics:"))))
               | sort_by(.ts) | (.[-1] // null)
               | if . == null then null else (.body | fromjson) end) as $m
        | {
            run_id: ($repo + ":" + $b + ":" + ($t0|tostring)),
            repo: $repo, branch: $b,
            engine: $d.engine, model: $d.model, tier: $d.tier,
            effort: $d.effort, title: $d.title,
            reached_pr: ($propen != null),
            pr_url: $pr,
            time_to_pr_ms: (if $propen != null then ($propen.ts - $t0) else null end),
            outcome: (
              if $pr != null then "pr_open"
              elif $ls == "failed" then "failed"
              elif $ls == "done" then "failed"
              else "incomplete" end),
            rework_count: ($m.rework_count // null),
            review_high: ($m.review_high // null),
            review_mode: ($m.review_mode // null),
            plan_critic_first_pass: ($m.plan_critic_first_pass // null),
            # NB: `$m.consulted // null` is WRONG — jq // treats false as absent,
            # collapsing consulted:false → null and destroying the baseline.
            consulted: (if $m == null then null else $m.consulted end),
            blocked_count: $blocked,
            reported_ok: (($m != null) and ($ls != null)),
            swept_at: (now*1000|floor)
          } ]' "$log")
  store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
  store="$store_dir/ratings.jsonl"
  mkdir -p "$store_dir"
  lockd="$store_dir/ratings.lock.d"
  if ! _lock_acquire "$lockd" "$$"; then
    echo "crew: ratings store busy" >&2
    exit 1
  fi
  trap '_lock_release "$lockd"' EXIT
  printf '%s' "$records" | jq -c '.[]' >>"$store"
  ;;
```

- [ ] **Step 4: Update the usage string**

In the `*)` arm's `echo "usage: crew id | … | report [crew]"`, append ` | rate` before the closing quote.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bash <scratchpad>/test_crew_rate.sh`
Expected: all `ok` lines, exit 0 (windowing 2 records, zombie incomplete, metrics joined, idempotency one folded run).

- [ ] **Step 6: Commit**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "feat(dispatch): crew rate sweep folds bus into ratings store (#102)"
```

---

### Task 3: Build + lint gate

`crew.sh` is compiled by `writeShellApplication`, which runs `shellcheck` at build. Confirm the new arm passes the real gate before the phase is done.

**Files:** none changed (verification only).

- [ ] **Step 1: Standalone shellcheck**

Run: `nix develop -c shellcheck -s bash home/ai/claude-code/crew.sh` (invoke the `nix-rebuild` skill's guidance if the devshell/command isn't obvious)
Expected: no warnings for the `rate)` arm. Fix any (quote `$store`/`$lockd`; the big jq is a single-quoted literal so it needs no escaping).

- [ ] **Step 2: Nix build of the crew package**

Run: `nix build .#homeConfigurations` is heavy — instead build just the check: `nix flake check` (per the `nix-rebuild` skill; expect the pre-existing `shfmt` RED noted in the dispatcher-harness memory — confirm no _new_ failure from crew.sh).
Expected: crew.sh builds; `writeShellApplication`'s shellcheck passes. Any pre-existing `shfmt` reformat of `dispatch.sh`/`skeleton-guard.sh` is unrelated to this change.

- [ ] **Step 3: Commit (only if a lint fix was needed)**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "fix(dispatch): shellcheck crew rate arm (#102)"
```

---

## Phase-exit (not a task — do after all tasks pass)

- Open the PR for `feat/102-dispatch-ratings` (`gh pr create --assignee @me`), body `Closes #102` — note it is **Phase 1 of 3** and lists the deferred phases.
- **Live smoke is post-merge** (`crew.sh` is a nix PATH binary needing `nh home switch`; `WORKER_PROTOCOL.md` deploys read-by-path on merge). After merge + `nh home switch`: dispatch one real trivial worker, let it open a PR, run `crew rate`, and `jq -s 'group_by(.run_id)|map(last)' ~/.local/share/crew/ratings.jsonl` to confirm a real record with `reached_pr:true` and its `metrics` fields.

## Self-review (done while writing)

- **Spec coverage (Phase 1 rows):** all-tier emit → Task 1; `crew rate` fold + windowing + carry-forward `pr_url` + outcome classification → Task 2; global store + lock → Task 2; build/shellcheck → Task 3. Phase 2 (gh reconcile) and Phase 3 (cost/report) are explicitly out of this plan.
- **Placeholder scan:** none — full jq, full test harness, exact commands.
- **Type consistency:** the `metrics:` record shape in Task 1 (`consulted/plan_critic_first_pass/rework_count/review_high`) matches Task 2's `$m.*` reads; `run_id` format identical in constraints, Task 2 interface, and the windowing test.
