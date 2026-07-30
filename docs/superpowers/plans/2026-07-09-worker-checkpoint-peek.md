# Worker Checkpoint-Peek Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a worker cheaply check the crew bus for a dispatcher "stop/redirect" directive at each pipeline seam by adding a non-blocking `crew inbox --since <ts>` form, and document the checkpoint-peek protocol for workers.

**Architecture:** Two files change. `crew.sh` gains a `--since <ts>` flag on the existing `inbox` subcommand — a single non-blocking jq pass (no loop, no sleep — unlike `watch`/`await`) that returns only messages with `.ts > <ts>`. `WORKER_PROTOCOL.md` gains a "checkpoint-peek" subsection telling workers to poll for directives between stages using that flag, thread a seen-cursor, and fold stragglers after every `crew await` return. This is the LOW-RISK half of issue #68; the dispatcher-side background watch is a SEPARATE issue (#69) — do NOT touch `DISPATCHER_PROTOCOL.md` or the dispatcher watch logic.

**Tech Stack:** POSIX-ish bash (nix `writeShellApplication`), jq, Home Manager (Linux standalone), markdown.

## Global Constraints

- Two files only: `home/ai/claude-code/crew.sh` and `home/ai/claude-code/WORKER_PROTOCOL.md`. Nothing else.
- `crew.sh` is nix `writeShellApplication` source: the file starts at `# shellcheck shell=bash`; the shebang + `set -euo pipefail` are prepended by nix at build time — do NOT add them to the source.
- **CRITICAL REGRESSION CONSTRAINT:** omitting `--since` must be BYTE-IDENTICAL to today's `inbox` behavior (returns all msgs addressed to the agent). Do not alter the existing code path; add the filter only on the `--since` branch.
- `inbox` stays exit-0-always (empty stdout when nothing matches), non-blocking, single jq pass — no `while`/`sleep`.
- Match existing `crew.sh` style exactly: jq idioms (`.ts>$since` no spaces, `--argjson`), the `while [ $# -gt 0 ]; case … --flag) … shift 2` arg-parse idiom, errors to stderr, `exit 1` on bad args.
- `--since` integer-ms validation must mirror `watch`'s: `case "$since" in '' | *[!0-9]*) … exit 1 ;; esac`.
- `shellcheck home/ai/claude-code/crew.sh` must be clean (project rule).
- Match `WORKER_PROTOCOL.md`'s terse voice.
- Behavioral tests use a **scratchpad throwaway** git repo + `events.jsonl`, never a real crew dir.
- Deploy ordering is load-bearing: `crew.sh` must land on PATH via `nh home switch` before the protocol text is meaningful; `WORKER_PROTOCOL.md` hot-loads via the `.claude` symlink but only once merged to the main checkout. So: implement + shellcheck + test crew.sh → `nh home switch` → verify live on PATH → then the protocol edit.
- Pre-commit hooks (alejandra/deadnix/statix/prettier) reformat on first commit — prettier rewrites the `.md`. The FIRST `git commit` touching it will fail with "files were modified by this hook"; `git add` the reformatted file and re-run the SAME commit. This is expected workflow, not an error.

---

## File Structure

- `home/ai/claude-code/crew.sh` — the bus CLI. Modify the `inbox)` case (currently lines ~276-283) and the usage string in the `*)` case (line ~290). No other case changes.
- `home/ai/claude-code/WORKER_PROTOCOL.md` — the worker persona. Add a new "Checkpoint-peek" subsection; extend the `crew await` block under "Report to the bus" with the B3 straggler-fold rule.
- `<scratchpad>/test-inbox-since.sh` — throwaway behavioral test (NOT committed).

Scratchpad dir for this session: `/tmp/claude-1000/-home-noams-nix-config/448b1616-067d-467e-b17c-9e49f3d98ecc/scratchpad`

---

## Task 1: `crew inbox --since <ts>` (non-blocking peek)

**Files:**

- Modify: `home/ai/claude-code/crew.sh` — `inbox)` case (~276-283) and usage string (~290)
- Test: `<scratchpad>/test-inbox-since.sh` (throwaway, not committed)

**Interfaces:**

- Consumes: nothing from other tasks.
- Produces: `crew inbox <agent> [crew] --since <ts>` — non-blocking; prints (JSONL, one obj/line) only msg events with `.crew_id==crew`, `.to==agent or .to=="*"`, and `.ts > ts`; exit 0 always. Omitting `--since` prints all msgs to the agent (unchanged). Task 3's protocol text depends on this exact invocation shape.

- [ ] **Step 1: Write the failing behavioral test**

Create `<scratchpad>/test-inbox-since.sh` (use the real scratchpad path). It builds a throwaway git repo, writes a two-message `events.jsonl`, and asserts each form. `CREW_SH` points at the worktree source; run it through `bash -euo pipefail` to mimic the nix wrapper.

```bash
#!/usr/bin/env bash
set -euo pipefail

CREW_SH="/home/noams/nix-config-worktrees/feat-68-worker-checkpoint-peek/home/ai/claude-code/crew.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

git -C "$TMP" init -q
mkdir -p "$TMP/.git/crew"
cat >"$TMP/.git/crew/events.jsonl" <<'EOF'
{"ts":100,"crew_id":"testcrew","from":"dispatcher:testcrew","to":"worker:x","kind":"msg","body":"old"}
{"ts":200,"crew_id":"testcrew","from":"dispatcher:testcrew","to":"worker:x","kind":"msg","body":"new"}
{"ts":150,"crew_id":"testcrew","from":"worker:x","to":"dispatcher:testcrew","kind":"status","body":{"state":"working"}}
EOF

run() { ( cd "$TMP" && env CREW_ID=testcrew bash -euo pipefail "$CREW_SH" "$@" ); }

fail() { echo "FAIL: $1" >&2; exit 1; }

# 1. --since filters to strictly-newer msgs (no positional crew)
out="$(run inbox worker:x --since 150)"
[ "$(printf '%s\n' "$out" | grep -c .)" = 1 ] || fail "since:count $out"
printf '%s' "$out" | jq -e '.body=="new"' >/dev/null || fail "since:body $out"

# 2. --since with positional crew present
out="$(run inbox worker:x testcrew --since 150)"
printf '%s' "$out" | jq -e '.body=="new"' >/dev/null || fail "since+crew $out"

# 3. REGRESSION: omitting --since returns ALL msgs to the agent
out="$(run inbox worker:x)"
[ "$(printf '%s\n' "$out" | grep -c .)" = 2 ] || fail "regression:count $out"

# 4. REGRESSION: existing dispatcher form unchanged (status is never in inbox)
out="$(run inbox dispatcher:testcrew)"
[ -z "$out" ] || fail "dispatcher-inbox should be empty (status is not msg): $out"

# 5. --since with no newer msgs => empty, exit 0
out="$(run inbox worker:x --since 999)"; rc=$?
[ -z "$out" ] || fail "since:empty $out"
[ "$rc" = 0 ] || fail "since:exitcode $rc"

# 6. bad --since value rejected
if run inbox worker:x --since abc >/dev/null 2>&1; then fail "since:validation accepted abc"; fi

# 7. --since with no value rejected
if run inbox worker:x --since >/dev/null 2>&1; then fail "since:novalue accepted"; fi

echo "ALL PASS"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash <scratchpad>/test-inbox-since.sh`
Expected: FAIL — case 1 fails (`FAIL: since:count …`) because today's `inbox` ignores `--since` and treats `--since`/`150` as a positional crew, so the selector returns 0 rows for crew `--since`. (Any FAIL before `ALL PASS` confirms the feature is absent.)

- [ ] **Step 3: Rewrite the `inbox)` case in `crew.sh`**

Replace the entire current `inbox)` case:

```bash
inbox)
  # messages only — `roster` owns status (every status is addressed to the
  # dispatcher, so without this the inbox is status spam).
  crew="${2:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  jq -c --arg crew "$crew" --arg me "${1:-}" \
    'select(.crew_id==$crew and .kind=="msg" and (.to==$me or .to=="*"))' "$log"
  ;;
```

with:

```bash
inbox)
  # messages only — `roster` owns status (every status is addressed to the
  # dispatcher, so without this the inbox is status spam). `--since TS` is a
  # non-blocking single pass (no loop, unlike watch/await): return only msgs
  # strictly newer than TS. Omitting it returns all msgs to the agent, unchanged.
  me="${1:-}"
  shift || true
  crew=""
  since=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --since)
      [ -n "${2:-}" ] || {
        echo "crew: --since needs a value" >&2
        exit 1
      }
      since="$2"
      shift 2
      ;;
    *)
      crew="$1"
      shift
      ;;
    esac
  done
  crew="${crew:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  if [ -n "$since" ]; then
    case "$since" in '' | *[!0-9]*)
      echo "crew: --since must be an integer ms timestamp" >&2
      exit 1
      ;;
    esac
    jq -c --arg crew "$crew" --arg me "$me" --argjson since "$since" \
      'select(.crew_id==$crew and .kind=="msg" and (.to==$me or .to=="*") and .ts>$since)' "$log"
  else
    jq -c --arg crew "$crew" --arg me "$me" \
      'select(.crew_id==$crew and .kind=="msg" and (.to==$me or .to=="*"))' "$log"
  fi
  ;;
```

Note: the `else` branch is byte-identical to today's jq call (same filter string, same `me` value from `${1:-}`), satisfying the regression constraint. The positional `crew` is now any non-flag arg after the agent, preserving both `inbox worker:x` and `inbox worker:x mycrew`.

- [ ] **Step 4: Update the usage string in the `*)` case**

Find in the `*)` case (line ~290): `inbox <agent> [crew]`
Replace with: `inbox <agent> [crew] [--since TS]`

The full line becomes:

```bash
  echo "usage: crew id | identity <branch> | status <from> <state> [detail] [pr] | msg <from> <to> <body> | reply <to> <body> | await <agent> [--timeout S] [--interval S] | watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] | roster [crew] | inbox <agent> [crew] [--since TS] | log [crew]" >&2
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `bash <scratchpad>/test-inbox-since.sh`
Expected: `ALL PASS`

- [ ] **Step 6: Run shellcheck**

Run: `shellcheck /home/noams/nix-config-worktrees/feat-68-worker-checkpoint-peek/home/ai/claude-code/crew.sh`
Expected: clean (no output, exit 0). The `# shellcheck shell=bash` directive at the top lets standalone shellcheck resolve the missing shebang.

- [ ] **Step 7: Commit the crew.sh change**

```bash
cd /home/noams/nix-config-worktrees/feat-68-worker-checkpoint-peek
git add home/ai/claude-code/crew.sh
git commit -m "feat(crew): add non-blocking inbox --since <ts> peek"
```

Note: `crew.sh` is not a prettier target, so this commit should pass on the first try. If any hook reformats a file, `git add` it and re-run the SAME commit.

---

## Task 2: Deploy crew.sh to PATH and verify live

**Files:** none (build/deploy only)

**Interfaces:**

- Consumes: the committed `crew.sh` from Task 1.
- Produces: `crew inbox --since` available on the live PATH, so the protocol text in Task 3 refers to real behavior.

- [ ] **Step 1: Rebuild Home Manager**

REQUIRED SUB-SKILL: invoke the `nix-rebuild` skill before running the rebuild (it covers the `git add` gotcha — untracked files are invisible to the flake; Task 1 already committed `crew.sh`, so this is satisfied).

Run: `nh home switch`
Expected: build succeeds, new generation activated. (This is the Linux standalone Home Manager path per `CLAUDE.local.md`.)

- [ ] **Step 2: Verify the new flag is live on PATH**

Run: `crew 2>&1 | grep -- 'inbox <agent> \[crew\] \[--since TS\]'`
Expected: the usage line prints, confirming the rebuilt `crew` is on PATH.

- [ ] **Step 3: Smoke-test the live binary against a throwaway repo**

Run:

```bash
D="$(mktemp -d)"; git -C "$D" init -q; mkdir -p "$D/.git/crew"
printf '%s\n' '{"ts":100,"crew_id":"c","from":"dispatcher:c","to":"worker:x","kind":"msg","body":"old"}' '{"ts":200,"crew_id":"c","from":"dispatcher:c","to":"worker:x","kind":"msg","body":"new"}' >"$D/.git/crew/events.jsonl"
( cd "$D" && CREW_ID=c crew inbox worker:x --since 150 )
rm -rf "$D"
```

Expected: exactly one line, the `"new"` message (`…"body":"new"…`).

No commit in this task.

---

## Task 3: Document the checkpoint-peek protocol in WORKER_PROTOCOL.md

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` — add a "Checkpoint-peek" subsection (after "Pipeline by tier", before "Code review gate"); extend the `crew await` block under "Report to the bus".

**Interfaces:**

- Consumes: `crew inbox <agent> [crew] --since <ts>` from Task 1 (live per Task 2).
- Produces: worker-facing prose only. No code depends on it.

- [ ] **Step 1: Add the "Checkpoint-peek" subsection**

Insert immediately after the "## Pipeline by tier" list (before "## Code review gate (standard/deep)"):

```markdown
## Checkpoint-peek (standard/deep)

At each pipeline **seam** — after spec, after plan, after execute, after review, **before** sinking cost into the next stage — do a non-blocking peek for a dispatcher stop/redirect directive:
```

crew inbox "worker:$(git branch --show-current)" --since <seen-cursor>

```
This is a single pass, not a held wait (unlike `crew await`): empty output ⇒ no directive ⇒ proceed to the next stage.

- **Seen-cursor:** initialize it once at start to the current ms: `seen=$(jq -n 'now*1000|floor')`. After a peek (or await) returns messages you **read and handled**, advance `seen` to the max `.ts` of *those* messages only — `seen=$(printf '%s\n' "$msgs" | jq -s 'map(.ts) | max')` — never to an unrelated max. A peek returning nothing does not move the cursor.
- **On a directive:** apply **receiving-code-review** discipline — verify the instruction before acting, don't perform agreement. Then redirect the pipeline, or on a "stop" wind down cleanly and stamp `crew status "worker:$(git branch --show-current)" <state>` appropriately (e.g. `failed "stopped by dispatcher"`).
- **Latency is honest, not instant:** a redirect surfaces only at the *next* seam, so its latency is the remaining time in the current stage. A redirect posted mid-`execute` (the longest stage for deep workers) is not seen until execute finishes. **The peek is NOT a kill switch** — for a hard abort the dispatcher uses `tmux kill-window` (→ SessionEnd `exited`), which stays the reliable stop.
```

- [ ] **Step 2: Extend the `crew await` block with the B3 straggler-fold rule**

In the "## Report to the bus (mandatory)" section, the blocked bullet currently ends its reply/timeout handling with the "reply arrives" / "times out" / "trivial/standard" sub-bullets. Add this sub-bullet at the end of that group (after the `trivial`/`standard` sub-bullet, before the "permission prompt" bullet):

```markdown
- **Always fold in stragglers after await, before advancing the cursor.** `crew await` keys off its own internal `start=now`, not your seen-cursor, so a directive posted _before_ the await started is not matched by that await. On **every** await return — reply (exit 0) **or** timeout (exit 3) — and **before** you advance the seen-cursor past the await reply's `.ts`, run `crew inbox "worker:$(git branch --show-current)" --since <seen-cursor>` to catch it. Ordering is load-bearing: advancing the cursor from the reply's `.ts` first would leapfrog a pre-await directive (the exact bug this fold prevents). Handle any directive with the same receiving-code-review discipline, then advance the seen-cursor only over messages you handled (fold results **and** the await reply).
```

- [ ] **Step 3: Commit (expect the first attempt to be reformatted by prettier)**

```bash
cd /home/noams/nix-config-worktrees/feat-68-worker-checkpoint-peek
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "docs(worker): add checkpoint-peek protocol and await straggler-fold"
```

If the commit fails with "files were modified by this hook" (prettier reflowed the markdown): re-stage and re-run the SAME commit — do not change the message.

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "docs(worker): add checkpoint-peek protocol and await straggler-fold"
```

- [ ] **Step 4: Verify the branch is clean and both commits are present**

Run: `git -C /home/noams/nix-config-worktrees/feat-68-worker-checkpoint-peek log --oneline -2`
Expected: the docs commit and the `feat(crew)` commit, in that order.
Run: `git -C /home/noams/nix-config-worktrees/feat-68-worker-checkpoint-peek status --short`
Expected: empty (clean tree).

---

## Self-Review

**Spec coverage:**

- `inbox --since <ts>`, non-blocking single pass, `.ts > ts`, exit 0 always → Task 1 Steps 3, 5 (test cases 1, 5).
- Selector = today's + ts filter (`.to==$me or .to=="*"` + `.ts>$since`) → Task 1 Step 3.
- Accepts both `inbox worker:x --since` and `inbox worker:x mycrew --since` → Task 1 Step 3 (loop treats non-flag as positional crew); tested cases 1, 2.
- Integer-ms validation mirroring `watch` → Task 1 Step 3 (`case "$since" in '' | *[!0-9]*)`); tested case 6.
- Byte-identical no-`--since` path → Task 1 Step 3 `else` branch; tested regression cases 3, 4.
- Usage string updated → Task 1 Step 4; verified live Task 2 Step 2.
- shellcheck clean → Task 1 Step 6.
- Behavioral test with throwaway events.jsonl asserting newer-only + regression → Task 1 Step 1.
- Deploy ordering (crew.sh → nh home switch → verify live → protocol edit) → Tasks 1, 2, 3 order; Task 2 Steps 1-3.
- Checkpoint-peek at each seam, seen-cursor threading, no-op on empty → Task 3 Step 1.
- B3 fix: fold `crew inbox --since` on every await return (exit 0 and exit 3) → Task 3 Step 2.
- Directive handling with receiving-code-review discipline + clean stop stamp → Task 3 Step 1.
- N1 honesty: latency = remaining stage time; not a kill switch; tmux kill-window is the hard abort → Task 3 Step 1.
- Scope guard: no DISPATCHER_PROTOCOL.md / dispatcher-watch changes (#69) → Global Constraints; only two files touched.

**Placeholder scan:** No TBD/"handle edge cases"/"similar to". `<scratchpad>` and `<seen-cursor>`/`<state>` are intentional fill-ins (real scratchpad path given at top; cursor/state are runtime values the worker computes) — every code step ships literal code.

**Type consistency:** `--since <ts>` (integer ms), `seen` cursor, and the `inbox worker:$(git branch --show-current) --since <seen-cursor>` invocation are identical across Task 1 (produce), Task 3 Step 1 and Step 2 (consume). Selector filter string is the same in the `else` branch and the pre-existing code.

## Notes / flags for the implementer

- **Underspecified but resolved:** the spec doesn't name how the worker obtains its start ts. Chosen: `jq -n 'now*1000|floor'` — identical to how `crew.sh` mints timestamps internally, and jq is already a hard dependency of `crew`, so it's portable (Linux + macOS) with no new tooling.
- **Nothing in scope appears wrong.** The regression constraint and the `watch --since` validation shape both map cleanly onto the existing code. The one real design choice is the arg-parse rewrite (loop instead of positional `$2`); it's required to accept `--since` in either position and is the same idiom `watch`/`await` already use.

```

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-09-worker-checkpoint-peek.md`. Two execution options:

1. Subagent-Driven (recommended) — fresh subagent per task, review between tasks, fast iteration.
2. Inline Execution — execute tasks in this session with checkpoints for review.

Which approach?
```
