# crew watch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `crew watch` subcommand — a zero-token held long-poll that wakes the dispatcher the instant any worker hits a watched state or asks a question — and rewire the dispatcher's monitor loop to use it.

**Architecture:** `watch` mirrors the existing `await` block in `crew.sh` (held `jq` poll loop over the append-only `events.jsonl`), but threads an explicit caller-supplied cursor (`--since`) instead of `start=now`, and returns a _batch_ (`{cursor, events}`) rather than a single message. DISPATCHER_PROTOCOL.md gains a `watch`-driven loop. Worker side and all other subcommands are untouched.

**Tech Stack:** POSIX-ish bash (under `set -euo pipefail` via `writeShellApplication`), `jq`, git. Spec: `docs/superpowers/specs/2026-06-30-crew-watch-design.md`.

## Global Constraints

- `crew.sh` is consumed via `builtins.readFile ./crew.sh` into `pkgs.writeShellApplication` (`home/ai/claude-code/default.nix:24-28`), which **runs shellcheck at build** — the added code MUST be shellcheck-clean and assume `set -euo pipefail` is active.
- Match the existing `await` block's conventions exactly: each flag requires a value, unknown flags `exit 1`, guard fallible commands with `|| true`, suppress concurrent-write `jq` errors with `2>/dev/null`.
- Event schema (from `crew.sh`): `{ts:(now*1000|floor), crew_id, from, to, kind:("status"|"msg"), body}`. For `status`, `body` is an object `{state, detail?, pr_url?}`; for `msg`, `body` is a string.
- Cursor invariant: emit a cursor **only on exit 0**. Timeout = exit 3, no stdout, no cursor.
- `msg` match is `to == "dispatcher:"+crew` OR `to == "*"` (parity with `inbox`). A dispatcher `reply` (`to == "worker:…"`) must never match.
- Smoke test is a **throwaway** verification artifact in the scratchpad (this repo commits no shell-unit-test harness — same as the `await` feature). Committed deliverables are `crew.sh` and `DISPATCHER_PROTOCOL.md` only.
- Deploy is post-merge: merge to the main checkout → `nh home switch` (workers/dispatcher read `crew` from the nix profile).

---

### Task 1: Implement the `crew watch` subcommand

**Files:**

- Modify: `home/ai/claude-code/crew.sh` (add a `watch)` case; update the two usage strings + the header comment)
- Test: `<scratchpad>/test-crew-watch.sh` (throwaway, not committed)

**Interfaces:**

- Consumes: existing `_crew_id`, `$dir`, `$log`, and the `status`/`msg`/`reply` subcommands (used by the test for setup).
- Produces: `crew watch [--since TS] [--states a,b,c] [--timeout S] [--interval S]` → stdout `{"cursor":<int ms>,"events":[<event>,…]}` + exit 0 on a hit; exit 3 (stderr notice, no stdout) on timeout; exit 1 on bad args.

- [ ] **Step 1: Write the failing test**

Write `<scratchpad>/test-crew-watch.sh` (set `<scratchpad>` to your session scratchpad dir). It runs the **work-in-progress** `crew.sh` under `set -euo pipefail` via a temp copy, so no Nix build is needed:

```bash
#!/usr/bin/env bash
# Throwaway live test of `crew watch`. Runs the WIP crew.sh (arg 1) against a
# throwaway repo (arg 2) so the real crew bus stays clean.
set -euo pipefail
WIP="${1:?usage: test-crew-watch.sh <path-to-crew.sh> <repo-dir>}"
REPO="${2:?usage: test-crew-watch.sh <path-to-crew.sh> <repo-dir>}"
TMPCREW="$(dirname "$REPO")/crew-wip.sh"
printf 'set -euo pipefail\n' >"$TMPCREW"
cat "$WIP" >>"$TMPCREW"
crew() { bash "$TMPCREW" "$@"; }
nowms() { jq -nc 'now*1000|floor'; }

rm -rf "$REPO"
mkdir -p "$REPO"
cd "$REPO"
git init -q
printf 'crew_id: smoke-watch\n' >WORKER_TASK.md

pass=0
fail=0
ok() {
  echo "PASS: $1"
  pass=$((pass + 1))
}
bad() {
  echo "FAIL: $1"
  fail=$((fail + 1))
}

# 1: event posted before the watch window is ignored -> timeout exit 3.
crew status worker:a done "" "http://pr/1" >/dev/null
set +e
out=$(crew watch --since "$(nowms)" --timeout 3 --interval 1)
rc=$?
set -e
[ "$rc" -eq 3 ] && ok "stale event ignored -> exit 3" || bad "stale: rc=$rc out=$out"

# 2: a single qualifying event -> exit 0, in events, cursor set.
start=$(nowms)
(
  sleep 1
  crew status worker:b pr_open "" "http://pr/2" >/dev/null
) &
set +e
out=$(crew watch --since "$start" --timeout 15 --interval 1)
rc=$?
set -e
wait
n=$(printf '%s' "$out" | jq '.events|length')
st=$(printf '%s' "$out" | jq -r '.events[0].body.state')
cur=$(printf '%s' "$out" | jq '.cursor')
if [ "$rc" -eq 0 ] && [ "$n" -eq 1 ] && [ "$st" = pr_open ] && [ "$cur" != null ]; then
  ok "single event returned"
else
  bad "single: rc=$rc out=$out"
fi

# 3: several events in one window -> one batched array.
start=$(nowms)
(
  sleep 1
  crew status worker:c done >/dev/null
  crew status worker:d failed >/dev/null
) &
set +e
out=$(crew watch --since "$start" --timeout 15 --interval 2)
rc=$?
set -e
wait
n=$(printf '%s' "$out" | jq '.events|length')
[ "$rc" -eq 0 ] && [ "$n" -ge 2 ] && ok "batched multi-worker ($n events)" || bad "batch: rc=$rc out=$out"

# 4: cursor advance across re-armed calls -> no miss, no dup.
start=$(nowms)
crew status worker:e done >/dev/null
out=$(crew watch --since "$start" --timeout 5 --interval 1)
cur=$(printf '%s' "$out" | jq '.cursor')
ids1=$(printf '%s' "$out" | jq -c '[.events[].from]')
crew status worker:f done >/dev/null
out2=$(crew watch --since "$cur" --timeout 5 --interval 1)
ids2=$(printf '%s' "$out2" | jq -c '[.events[].from]')
if [ "$ids1" = '["worker:e"]' ] && [ "$ids2" = '["worker:f"]' ]; then
  ok "cursor advance: no miss, no dup"
else
  bad "cursor: ids1=$ids1 ids2=$ids2"
fi

# 5: `working` is heartbeat noise -> does not wake watch.
start=$(nowms)
crew status worker:g working "step 1" >/dev/null
set +e
out=$(crew watch --since "$start" --timeout 3 --interval 1)
rc=$?
set -e
[ "$rc" -eq 3 ] && ok "working filtered out -> exit 3" || bad "working: rc=$rc out=$out"

# 6: a worker question (msg to dispatcher) and a broadcast both surface.
start=$(nowms)
crew msg worker:h dispatcher:smoke-watch "need creds?" >/dev/null
out=$(crew watch --since "$start" --timeout 5 --interval 1)
body=$(printf '%s' "$out" | jq -r '.events[0].body')
cur=$(printf '%s' "$out" | jq '.cursor')
crew msg worker:h '*' "broadcast hi" >/dev/null
out2=$(crew watch --since "$cur" --timeout 5 --interval 1)
body2=$(printf '%s' "$out2" | jq -r '.events[0].body')
if [ "$body" = "need creds?" ] && [ "$body2" = "broadcast hi" ]; then
  ok "question + broadcast surfaced"
else
  bad "msg: body=$body body2=$body2"
fi

# 7: cursor survives a timeout (the failure the cursor exists to prevent).
start=$(nowms)
crew status worker:i done >/dev/null
out=$(crew watch --since "$start" --timeout 5 --interval 1)
cur=$(printf '%s' "$out" | jq '.cursor')
set +e
crew watch --since "$cur" --timeout 2 --interval 1 >/dev/null
rc=$?
set -e
crew status worker:j done >/dev/null
out2=$(crew watch --since "$cur" --timeout 5 --interval 1)
ids2=$(printf '%s' "$out2" | jq -c '[.events[].from]')
if [ "$rc" -eq 3 ] && [ "$ids2" = '["worker:j"]' ]; then
  ok "cursor survives a timeout"
else
  bad "cursor-survive: rc=$rc ids2=$ids2"
fi

# 8: the dispatcher's own reply (to a worker) must not wake watch.
start=$(nowms)
crew reply worker:k "here is your answer" >/dev/null
set +e
out=$(crew watch --since "$start" --timeout 3 --interval 1)
rc=$?
set -e
[ "$rc" -eq 3 ] && ok "dispatcher reply does not wake watch -> exit 3" || bad "reply-echo: rc=$rc out=$out"

# 9: wakeup latency <= interval (pins the success criterion).
start=$(nowms)
(
  sleep 0.3
  crew status worker:l done >/dev/null
  date +%s.%N >"$REPO/.evat"
) &
crew watch --since "$start" --timeout 15 --interval 1 >/dev/null
ret=$(date +%s.%N)
wait
lat=$(awk -v a="$(cat "$REPO/.evat")" -v b="$ret" 'BEGIN{print (b-a)*1000}')
within=$(awk -v l="$lat" 'BEGIN{print (l<=1200)?1:0}')
[ "$within" -eq 1 ] && ok "wakeup latency ${lat}ms <= interval+slack" || bad "latency ${lat}ms too high"

echo "----"
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
shellcheck <scratchpad>/test-crew-watch.sh
bash <scratchpad>/test-crew-watch.sh ~/nix-config-worktrees/feat-53-crew-watch/home/ai/claude-code/crew.sh <scratchpad>/crew-watch-repo
```

Expected: every case FAILs (the WIP `crew.sh` has no `watch` subcommand, so `crew watch …` hits the `*)` usage branch and exits 1). `RESULT: 0 passed, 9 failed`.

- [ ] **Step 3: Implement the `watch` subcommand**

In `home/ai/claude-code/crew.sh`, add a `watch)` case to the `case "$sub" in` block — place it immediately after the `await)` block's closing `;;` (before `roster)`):

```bash
  watch)
    # watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] — block until
    # any worker event qualifies (a status whose state is in --states, or a msg to
    # the dispatcher), print {"cursor":TS,"events":[…]} and exit 0; exit 3 on
    # timeout (no stdout, no cursor — the caller re-arms with the cursor it already
    # holds). Zero-token held poll, like `await`. The caller-threaded cursor (not
    # start=now) is load-bearing: events landing while the dispatcher acts on a
    # batch must not be skipped. Strict `>` matches `await` (same same-ms edge).
    crew=$(_crew_id)
    [ -n "$crew" ] || { echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2; exit 1; }
    since=0
    states="blocked,pr_open,done,failed"
    timeout=110
    interval=2
    while [ $# -gt 0 ]; do
      case "$1" in
        --since) [ -n "${2:-}" ] || { echo "crew: --since needs a value" >&2; exit 1; }; since="$2"; shift 2 ;;
        --states) [ -n "${2:-}" ] || { echo "crew: --states needs a value" >&2; exit 1; }; states="$2"; shift 2 ;;
        --timeout) [ -n "${2:-}" ] || { echo "crew: --timeout needs a value" >&2; exit 1; }; timeout="$2"; shift 2 ;;
        --interval) [ -n "${2:-}" ] || { echo "crew: --interval needs a value" >&2; exit 1; }; interval="$2"; shift 2 ;;
        *) echo "crew: watch: unknown arg '$1'" >&2; exit 1 ;;
      esac
    done
    case "$since" in '' | *[!0-9]*) echo "crew: --since must be an integer ms timestamp" >&2; exit 1 ;; esac
    statesjson=$(printf '%s' "$states" | jq -Rc 'split(",") | map(select(length>0))')
    [ "$statesjson" = "[]" ] && { echo "crew: --states must be non-empty" >&2; exit 1; }
    me="dispatcher:$crew"
    start=$(jq -nc 'now*1000|floor')
    deadline=$((start + timeout * 1000))
    while :; do
      if [ -f "$log" ]; then
        batch=$(jq -c -s --arg crew "$crew" --arg me "$me" --argjson since "$since" --argjson states "$statesjson" '
          map(select(.crew_id==$crew and .ts>$since
                and ( (.kind=="status" and (.body.state as $s | $states | index($s)))
                      or (.kind=="msg" and (.to==$me or .to=="*")) )))
          | sort_by(.ts)
          | select(length>0)
          | {cursor:(.[-1].ts), events:.}' "$log" 2>/dev/null || true)
        [ -n "$batch" ] && { printf '%s\n' "$batch"; exit 0; }
      fi
      [ "$(jq -nc 'now*1000|floor')" -ge "$deadline" ] && {
        echo "crew: watch timed out after ${timeout}s (no events > $since)" >&2
        exit 3
      }
      sleep "$interval"
    done
    ;;
```

Then update the **header comment** (line 1) and **both usage strings** (the inline one and the `*)` branch) to list `watch`. Change the usage text to include:

```
watch [--since TS] [--states a,b,c] [--timeout S] [--interval S]
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
shellcheck ~/nix-config-worktrees/feat-53-crew-watch/home/ai/claude-code/crew.sh
bash <scratchpad>/test-crew-watch.sh ~/nix-config-worktrees/feat-53-crew-watch/home/ai/claude-code/crew.sh <scratchpad>/crew-watch-repo
```

Expected: shellcheck clean; `RESULT: 9 passed, 0 failed`.

- [ ] **Step 5: Commit**

```bash
cd ~/nix-config-worktrees/feat-53-crew-watch
git add home/ai/claude-code/crew.sh
PRE_COMMIT_ALLOW_NO_CONFIG=1 git commit -m "feat(crew): add watch subcommand — dispatcher-side await-any (#53)"
```

(The fresh worktree has no flake-generated `.pre-commit-config.yaml`; the `crew.sh` change is shell-only, which the repo's hooks — alejandra/deadnix/statix/prettier — don't format. `writeShellApplication` shellchecks it at build instead.)

---

### Task 2: Rewire the dispatcher monitor loop in DISPATCHER_PROTOCOL.md

**Files:**

- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md` (the "Read the bus" section, ~lines 33-39)

**Interfaces:**

- Consumes: the `crew watch` contract from Task 1.
- Produces: prose only — no code depends on it.

- [ ] **Step 1: Rewrite the "Read the bus" section**

Replace the paragraph at `DISPATCHER_PROTOCOL.md:35` (`All workers in your shell share one crew_id. Check the bus when a worker pings your pane … and before dispatching the next task:`) and keep the `crew roster` / `crew inbox` bullets, but insert the watch loop as the primary mechanism. The section should read:

```markdown
## Read the bus (not `gh`/`tmux` scraping)

All workers in your shell share one `crew_id`. **Block on the bus instead of polling it** — `crew watch` is a held, zero-token call that returns the instant any worker needs you:

​`
cur=0
loop:
  out = crew watch --since $cur --timeout 110     # run with a Bash tool timeout ≥ 120000ms
  exit 0 → cur = (out | .cursor); handle each of out.events (reply / dispatch next / intervene); re-loop
  exit 3 → re-loop with the SAME cur (a timeout never advances the cursor)
​`

`crew watch` wakes on any worker `status` in `blocked`/`pr_open`/`done`/`failed` (not `working` heartbeats) or any question `msg` to you, returning `{"cursor":<ts>,"events":[…]}`. Thread `cur` across calls so an event landing while you act on a batch is never skipped. The terminal states (`done`/`pr_open`/`failed`) free fan-out budget, so the same wakeup tells you when to dispatch the next queued task. To take a brand-new task while parked in `watch`, the human interrupts (Esc).

Two reads remain for detail:

- `crew roster` — at-a-glance dashboard: every worker's latest state + age, plus a `name`/`color` codename derived from its branch (FleetView-style — `dispatch` colors the matching tmux window the same). **Refer to workers by codename** (e.g. "sage is blocked, atlas opened a PR") so it tracks the colored windows.
- `crew inbox dispatcher:$CREW_ID` — worker **questions** in full (messages only; status lives in the roster).
- A worker that's `blocked` has posted its question and is **awaiting your reply in-band** (a bounded ~300s wait). Answer promptly with `crew reply worker:<branch> "<answer>"` — it resumes in place, no tmux, no re-dispatch. Check `age_s` in the roster: if the wait already elapsed (stale `blocked`), the worker has stopped — then **intervene in its window** or **re-dispatch** with the context baked in. A reply you post after it stopped isn't lost (durable in the log); the worker picks it up on its next activation.
```

(The leading `​` characters before each `` in the fenced block above are zero-width-space placeholders so this plan renders — when editing the real file, use plain ` `` ` fences.)

- [ ] **Step 2: Verify consistency**

```bash
cd ~/nix-config-worktrees/feat-53-crew-watch
rg -n "crew watch|cur=0|--since" home/ai/claude-code/DISPATCHER_PROTOCOL.md
```

Expected: the watch loop appears once; flag names (`--since`/`--timeout`) and the exit-3 rule match Task 1's contract.

- [ ] **Step 3: Commit**

```bash
git add home/ai/claude-code/DISPATCHER_PROTOCOL.md
PRE_COMMIT_ALLOW_NO_CONFIG=1 git commit -m "docs(crew): dispatcher monitor loop blocks on crew watch (#53)"
```

---

### Task 3: Push, open PR, deploy, live-verify

**Files:** none (integration task).

- [ ] **Step 1: Push and open the PR**

```bash
cd ~/nix-config-worktrees/feat-53-crew-watch
git push -u origin feat/53-crew-watch
gh pr create --assignee @me --title "feat(crew): crew watch — dispatcher-side await-any (#53)" --body "Closes #53. Adds a zero-token held long-poll so the dispatcher wakes on worker events instead of polling the roster at LLM cadence. Spec: docs/superpowers/specs/2026-06-30-crew-watch-design.md. Tested 9/9 against a throwaway repo (stale-ignored, single, batched, cursor-advance, working-filtered, question+broadcast, cursor-survives-timeout, reply-not-echoed, latency≤interval)."
```

- [ ] **Step 2: After PR merges — deploy**

```bash
cd /home/noams/nix-config
git switch main
git pull
nh home switch
```

- [ ] **Step 3: Live-verify against the deployed `crew`**

Re-run the smoke test but pointing at the deployed binary instead of the WIP file (replace the `crew()` shim so it calls the real `crew`), or hand-check: in a real worktree, `crew status worker:x done` in one pane while `crew watch --since 0 --timeout 30 --interval 1` runs in another → confirm it returns the event and exits 0 within ~1s.

- [ ] **Step 4: Update project memory**

Update `project_dispatcher_harness.md`: note `crew watch` shipped (the dispatcher-side counterpart to `await`), closing the "dispatcher polls at LLM cadence" gap.

---

## Self-Review

**Spec coverage:** command + defaults (Task 1 Step 3) ✓; match rule incl. broadcast & reply-exclusion (Step 3 jq + tests 6/8) ✓; batched `{cursor,events}` output (Step 3 + test 2/3) ✓; cursor invariant / exit-3-no-cursor (Step 3 + test 7) ✓; `start=now` rationale (covered by cursor threading + test 4) ✓; known same-ms edge (code comment) ✓; flag parsing incl. `--states` tokenize + unknown-flag exit 1 + `--since` integer (Step 3) ✓; dispatcher loop + scheduler coexistence (Task 2) ✓; "worker side unchanged" (no worker edits) ✓; all 9 test cases (Task 1) ✓; latency success criterion (test 9) ✓; deploy gotcha (Task 3) ✓.

**Placeholder scan:** no TBD/TODO; all code shown in full; the zero-width-space note in Task 2 is an intentional rendering aid, called out explicitly.

**Type consistency:** flag names (`--since`/`--states`/`--timeout`/`--interval`), output keys (`cursor`/`events`), exit codes (0 hit / 3 timeout / 1 bad-arg), and the `dispatcher:<crew>`/`worker:<branch>` address forms are identical across Task 1's code, the tests, and Task 2's protocol loop.
