# Dispatcher Background-Watch (issue #69) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Convert the dispatcher's foreground-blocking `crew watch` into an event-driven, non-blocking background watch loop with a cursor-file source of truth and PID-based locks.

**Architecture:** `crew.sh` `watch)` gains a finite long-park default, rejects a non-positive `--timeout`, persists an authoritative per-crew cursor file (self-seeds when `--since` is omitted), and guards itself with an atomic `mkdir`-gated per-watch lock; a shared lock helper also backs a new `lock-role`/`unlock-role` pair for the dispatcher role lock. `DISPATCHER_PROTOCOL.md` is then rewritten to arm `crew watch` as a `run_in_background` Bash call with token-based one-outstanding-watch discipline and an active/drained park cadence.

**Tech Stack:** bash (nix `writeShellApplication`), `jq`, POSIX `mkdir`/`mv` atomicity, Home Manager (`nh home switch`), Markdown persona doc.

## Global Constraints

- **Deploy order is load-bearing: crew.sh mechanism FIRST (built + `nh home switch`, Task 6), THEN the wiring (Tasks 5b/5c) and the persona (Task 7).** Nothing may reference behavior the CLI doesn't yet have — the fish launcher (5b) and `/dispatcher` skill (5c) call `crew lock-role`, which only exists once Task 6's rebuild lands. The fish launcher change (5b) is nix-embedded, so it needs its **own second `nh home switch`** to take effect.
- **Contract unchanged (R1.3):** exit codes `0` (batch) / `3` (timeout) / `1` (error/refuse); state set `blocked,pr_open,done,failed`; `me="dispatcher:$crew"`; strict `>` comparison; exit-0 stdout is `{"cursor":<ts>,"events":[…]}`; exit-3 has no stdout.
- **`--timeout 0` (indefinite) must exist nowhere** — a reaped indefinite watch is undetectable.
- **Never abort on a bad cursor file** — missing/corrupt → fall back to `0`.
- **Cursor is written only on exit 0, never on exit 3.**
- **crew.sh is a `writeShellApplication`:** shebang + `set -euo pipefail` are prepended by nix; the source starts with `# shellcheck shell=bash`. New code MUST be `set -u`-safe (guard every expansion with `${x:-}`).
- **`shellcheck` must pass clean on crew.sh** (repo convention) before the rebuild gate.
- Worktree: `/home/noams/nix-config-worktrees/feat-69-dispatcher-bg-watch` on branch `feat/69-dispatcher-bg-watch`. Commit after each task (commits are pre-authorized). Expect the first commit touching `.nix`/formatted files to fail on the reformat hook — `git add` the reformatted files and re-run the same commit (see repo CLAUDE.md).

## Shell-level test harness (used by Tasks 1–5)

These tests need **no live dispatcher and no nix build** — they run the edited source directly through bash, mirroring `writeShellApplication`'s flags via `SHELLOPTS`. Paste this preamble at the top of each test:

```bash
CS=/home/noams/nix-config-worktrees/feat-69-dispatcher-bg-watch/home/ai/claude-code/crew.sh
# crewrun: run crew.sh with the SAME flags nix prepends (errexit/nounset/pipefail),
# under crew_id "t". bash imports these from $SHELLOPTS at startup.
crewrun() { env SHELLOPTS=errexit:nounset:pipefail CREW_ID=t bash "$CS" "$@"; }
# fresh scratch bus in an isolated git repo; sets D (repo), CD (crew dir), L (log)
newbus() {
  D=$(mktemp -d); ( cd "$D" && git init -q )
  CD="$D/.git/crew"; mkdir -p "$CD"; L="$CD/events.jsonl"
}
# a blocked-status event with a given ts
ev() { printf '{"ts":%s,"crew_id":"t","from":"worker:b","to":"dispatcher:t","kind":"status","body":{"state":"blocked"}}\n' "$1"; }
```

All `crewrun` calls must be issued from inside `$D` (e.g. `( cd "$D" && crewrun watch … )`) so `git rev-parse --git-common-dir` resolves to `$CD`'s parent.

---

## File Structure

- **`home/ai/claude-code/crew.sh`** (293 lines) — the coordination CLI. All logic changes land here: shared lock helpers (top), the `watch)` case (currently lines 165–255), a new `lock-role|unlock-role` case, and the usage string (line 290).
- **`home/ai/claude-code/DISPATCHER_PROTOCOL.md`** (62 lines) — the baked dispatcher persona. Only the "Read the bus" section (lines 37–55) changes; the two detail reads and blocked-worker guidance (lines 51–55) are preserved.
- **`home/terminal/fish/default.nix`** — the `dispatcher` launcher fish function (lines 388–412). Task 5b adds the role-lock acquire (`crew lock-role $fish_pid` at startup) + release (`crew unlock-role` after `claude` exits) and the `CREW_ID` mint.
- **`home/ai/claude-code/commands/dispatcher.md`** (30 lines) — the `/dispatcher` promotion skill. Task 5c adds the `crew lock-role $PPID` (plain bash) acquire near the top, before role adoption.
- **`docs/superpowers/plans/2026-07-10-dispatcher-bg-watch.md`** — this plan.

---

## Task 1: Shared atomic-lock helpers in crew.sh

**Files:**

- Modify: `home/ai/claude-code/crew.sh` (insert after `_identity()` closes, currently line 24, before the `_crew_id` comment at line 26)

**Interfaces:**

- Produces:
  - `_lock_acquire <lockdir> <owner_pid>` → exit `0` acquired (writes `owner_pid` into `<lockdir>/pid`) **OR the lock is already held by this same `owner_pid`** (idempotent re-acquire — a no-op success); exit `1` held by a _different_ live PID or lost the reclaim race.
  - `_lock_release <lockdir>` → removes the lock dir (idempotent).
- Consumed by Task 4 (watch lock, `owner_pid=$$`, unique per process — the idempotent rule can only fire harmlessly there, on PID reuse of a dead prior holder whose lock dir survived; the new live process legitimately reuses the dir and its trap cleans it up) and Tasks 5/5b/5c (role lock, `owner_pid` = the dispatcher's session-stable PID — the idempotent rule makes re-running `/dispatcher` in the same session a no-op success).

- [ ] **Step 1: Write the failing test**

`/tmp/claude-1000/.../scratchpad/t1.sh` — self-contained, pastes the two functions verbatim (they cannot be sourced from crew.sh without running the dispatch case):

```bash
set -euo pipefail
LD=$(mktemp -d)/lock.d
# --- paste _lock_acquire / _lock_release verbatim from crew.sh here ---
_lock_acquire "$LD" "$$"        && echo "A_ok"          # acquire by live self
_lock_acquire "$LD" "$$"        && echo "E_idempotent"  # same owner ($$) re-acquire -> 0 (no-op success)
_lock_acquire "$LD" "424242"    || echo "B_refused"     # held by live $$ != requester -> refuse
printf '999999999\n' > "$LD/pid"                        # forge a dead, different owner
_lock_acquire "$LD" "$$"        && echo "C_reclaimed"   # dead owner -> reclaim through gate
_lock_release "$LD"; test ! -d "$LD" && echo "D_released"
```

- [ ] **Step 2: Run to verify it fails**

Run: `bash /tmp/claude-1000/.../scratchpad/t1.sh`
Expected: FAIL (the paste region is empty — no such function) until Step 3's code is pasted in.

- [ ] **Step 3: Add the helpers to crew.sh**

Insert after line 24 (`_identity` close):

```bash
# _lock_acquire <lockdir> <owner_pid> — atomic mkdir gate with dead-PID reclaim.
# mkdir is atomic on POSIX, so it is the ONLY gate: exactly one caller wins.
# Returns 0 (acquired; owner_pid written inside for liveness) or 1 (held by a
# DIFFERENT live PID, or lost the reclaim race). If the held owner equals the
# requested owner, returns 0 idempotently — re-running /dispatcher in the same
# session (same session-stable PID) is a no-op success, not a refusal. rm -rf
# here is safe — the lock dir is transient coordination state, never user data,
# so the gtrash rule doesn't apply.
_lock_acquire() {
  local ld="$1" owner="$2" held
  if mkdir "$ld" 2>/dev/null; then
    printf '%s\n' "$owner" >"$ld/pid"
    return 0
  fi
  held=$(cat "$ld/pid" 2>/dev/null || true)
  if [ "$held" = "$owner" ]; then
    return 0 # idempotent: this same owner already holds it
  fi
  if [ -n "$held" ] && kill -0 "$held" 2>/dev/null; then
    return 1
  fi
  rm -rf "$ld" # stale (owner PID dead/empty) — reclaim through the same mkdir gate
  if mkdir "$ld" 2>/dev/null; then
    printf '%s\n' "$owner" >"$ld/pid"
    return 0
  fi
  return 1
}

_lock_release() { rm -rf "$1"; }
```

Then paste these two functions into the test file's marked region.

- [ ] **Step 4: Run to verify it passes**

Run: `bash /tmp/claude-1000/.../scratchpad/t1.sh`
Expected output (order): `A_ok`, `E_idempotent`, `B_refused`, `C_reclaimed`, `D_released`.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "feat(crew): add atomic mkdir-gated lock helpers"
```

---

## Task 2: watch — mkdir the bus dir, finite default park, reject non-positive `--timeout`

**Files:**

- Modify: `home/ai/claude-code/crew.sh` `watch)` case — header comment (166–172), defaults (178–181), and a new guard after the `--since` guard (after 226)

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces: `watch)` creates `$dir` before any write; default `--timeout` is `3300`; a `--timeout` of `0`/non-integer/negative exits `1` with a message.

- [ ] **Step 1: Write the failing test**

```bash
# paste harness preamble
newbus
( cd "$D" && crewrun watch --timeout 0 ); echo "rc0=$?"      # want: message + rc0=1
( cd "$D" && crewrun watch --timeout abc ); echo "rcA=$?"    # want: rcA=1
( cd "$D" && crewrun watch --timeout -5 ); echo "rcN=$?"     # want: rcN=1
# default park is finite (3300): with no events and a 1s timeout it still times out cleanly
( cd "$D" && crewrun watch --timeout 1 ); echo "rc3=$?"      # want: rc3=3 (exit-3, no stdout)
```

- [ ] **Step 2: Run to verify it fails**

Run the script above.
Expected: FAIL — `rc0=0` (current code silently accepts `--timeout 0` and blocks / mis-behaves) rather than `rc0=1`.

- [ ] **Step 3: Edit crew.sh `watch)`**

3a. Update the header comment (166–172) — replace the parenthetical about the caller holding the cursor with the finite-park + self-seed reality (self-seed lands in Task 3; state intent now):

```bash
  # watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] — block until
  # any worker event qualifies (a status whose state is in --states, or a msg to
  # the dispatcher), print {"cursor":TS,"events":[…]} and exit 0; exit 3 on
  # timeout (no stdout). Default --timeout is a finite long park (3300s / 55min):
  # an indefinite watch is rejected, because a reaped indefinite watch is
  # undetectable. When --since is omitted the cursor is self-seeded from
  # dispatcher.cursor (see below) so a stale caller cursor can't re-deliver.
  # Zero-token held poll, like `await`. Strict `>` matches `await` (same-ms edge).
```

3b. Immediately after the crew-id guard (after line 177), create the bus dir (N2/N3 — `watch` never created it before):

```bash
  mkdir -p "$dir"
```

3c. Change the default timeout (line 180) from `timeout=110` to:

```bash
  timeout=3300
```

3d. After the existing `--since` integer guard (after line 226), add the `--timeout` guard:

```bash
  case "$timeout" in '' | *[!0-9]*)
    echo "crew: --timeout must be a positive integer number of seconds" >&2
    exit 1
    ;;
  esac
  [ "$timeout" -gt 0 ] || {
    echo "crew: --timeout must be > 0 (indefinite watch unsupported: a reaped watch would be undetectable)" >&2
    exit 1
  }
```

(The `*[!0-9]*` case rejects negatives — the `-` is a non-digit — and non-integers; the `-gt 0` check rejects `0`.)

- [ ] **Step 4: Run to verify it passes**

Run the Step 1 script.
Expected: `rc0=1`, `rcA=1`, `rcN=1`, `rc3=3`, each with the stderr message for the rejections and no stdout on the exit-3 case.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "feat(crew): finite default watch park, reject non-positive --timeout, mkdir bus dir"
```

---

## Task 3: watch — authoritative cursor file (self-seed on omitted `--since`, atomic write on exit 0)

**Files:**

- Modify: `home/ai/claude-code/crew.sh` `watch)` case — defaults + `--since` arg branch (178–191), a self-seed block (after the Task 2 `--timeout` guard), and the exit-0 match block (244–247)

**Interfaces:**

- Consumes: `$dir` exists (Task 2 `mkdir -p`).
- Produces: `"$dir/dispatcher.cursor"` — one bare integer ms ts. Omitted `--since` seeds from it (missing/corrupt → `0`). Exit 0 atomically rewrites it to the batch's `.cursor` (`.[-1].ts`); exit 3 leaves it untouched.

- [ ] **Step 1: Write the failing test**

```bash
# paste harness preamble
# --- persistence: exit-0 writes .[-1].ts as a bare int, atomically ---
newbus; { ev 1000; ev 2000; } >>"$L"
( cd "$D" && crewrun watch --since 0 --timeout 1 ) >/dev/null; echo "rc=$?"     # want rc=0
echo "cursor=$(cat "$CD/dispatcher.cursor")"                                    # want cursor=2000
# --- self-seed: omitted --since reads the cursor file, returns only ts>seed ---
newbus; printf '1500\n' >"$CD/dispatcher.cursor"; { ev 1000; ev 3000; } >>"$L"
( cd "$D" && crewrun watch --timeout 1 ) | jq -c '[.events[].ts]'              # want [3000]
# --- corrupt cursor: fall back to 0, no abort ---
newbus; printf 'garbage\n' >"$CD/dispatcher.cursor"; ev 500 >>"$L"
( cd "$D" && crewrun watch --timeout 1 ) | jq -c '.cursor'; echo "rc=$?"       # want 500 then rc=0
# --- exit-3 leaves cursor untouched ---
newbus; printf '42\n' >"$CD/dispatcher.cursor"
( cd "$D" && crewrun watch --timeout 1 ); echo "rc3=$?"                        # want rc3=3
echo "kept=$(cat "$CD/dispatcher.cursor")"                                     # want kept=42
```

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL — no `dispatcher.cursor` is written (file missing / `cat` errors), and the omitted-`--since` run returns `[1000,3000]` (current default `since=0`) instead of `[3000]`.

- [ ] **Step 3: Edit crew.sh `watch)`**

3a. Add a `since_explicit` default next to the others (after line 178 `since=0`):

```bash
  since_explicit=0
```

3b. In the `--since` arg branch (188–190), record that it was explicit — change:

```bash
      since="$2"
      shift 2
```

to:

```bash
      since="$2"
      since_explicit=1
      shift 2
```

3c. After the Task 2 `--timeout` guard (and after the existing `--states` validation at 227–231), add the cursor path + self-seed:

```bash
  cursor_file="$dir/dispatcher.cursor"
  if [ "$since_explicit" = 0 ]; then
    seed=$(cat "$cursor_file" 2>/dev/null || true)
    case "$seed" in '' | *[!0-9]*) seed=0 ;; esac
    since="$seed"
  fi
```

3d. In the exit-0 match block (244–247), atomically persist the cursor after printing the batch — replace:

```bash
      [ -n "$batch" ] && {
        printf '%s\n' "$batch"
        exit 0
      }
```

with:

```bash
      [ -n "$batch" ] && {
        printf '%s\n' "$batch"
        last=$(printf '%s' "$batch" | jq -r '.cursor')
        tmp=$(mktemp "$dir/.cursor.XXXXXX")
        printf '%s\n' "$last" >"$tmp"
        mv -f "$tmp" "$cursor_file"
        exit 0
      }
```

(`mktemp` in `$dir` keeps temp and target on one filesystem, so `mv` is an atomic rename — R2.1. The exit-3 path is untouched, so it never writes the cursor — R2.3.)

- [ ] **Step 4: Run to verify it passes**

Run the Step 1 script.
Expected: `rc=0` then `cursor=2000`; `[3000]`; `500` then `rc=0`; `rc3=3` then `kept=42`.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "feat(crew): authoritative dispatcher.cursor with self-seed and atomic write"
```

---

## Task 4: watch — per-watch lock (`dispatcher.watch.lock.d`) with trap release + stale reclaim

**Files:**

- Modify: `home/ai/claude-code/crew.sh` `watch)` case — acquire block after the cursor self-seed (Task 3), before `me="dispatcher:$crew"` (line 232)

**Interfaces:**

- Consumes: `_lock_acquire`/`_lock_release` (Task 1), `$dir` (Task 2).
- Produces: at most one live `crew watch` per crew id; a second concurrent watch exits `1`; a stale (dead-owner) lock is reclaimed through the mkdir gate; the lock is released on exit 0 and exit 3 (via `trap … EXIT`).

- [ ] **Step 1: Write the failing test**

```bash
# paste harness preamble
# --- live owner refuses a second watch ---
newbus
( cd "$D" && crewrun watch --timeout 3 ) & W=$!; sleep 1     # W holds the lock, parked
test -d "$CD/dispatcher.watch.lock.d" && echo "held"
( cd "$D" && crewrun watch --timeout 1 ); echo "rc_second=$?" # want rc_second=1 (refused)
wait "$W"
# --- released after the holder exits (trap on both exit paths) ---
test ! -d "$CD/dispatcher.watch.lock.d" && echo "released"
# --- stale (dead-PID) lock is reclaimed ---
mkdir -p "$CD/dispatcher.watch.lock.d"; printf '999999999\n' >"$CD/dispatcher.watch.lock.d/pid"
( cd "$D" && crewrun watch --timeout 1 ); echo "rc_reclaim=$?" # want rc_reclaim=3 (acquired, then timed out)
test ! -d "$CD/dispatcher.watch.lock.d" && echo "reclaimed_released"
```

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL — no `dispatcher.watch.lock.d` is ever created (`held` line does not print), and a second concurrent watch is accepted instead of refused.

- [ ] **Step 3: Edit crew.sh `watch)`**

Insert after the Task 3 cursor self-seed block and before `me="dispatcher:$crew"` (line 232):

```bash
  wlock="$dir/dispatcher.watch.lock.d"
  _lock_acquire "$wlock" "$$" || {
    echo "crew: another dispatcher watch is already running for this crew (dispatcher.watch.lock.d)" >&2
    exit 1
  }
  trap '_lock_release "$wlock"' EXIT
```

(The trap fires on normal exit 0 and exit 3, so a cleanly-finishing watch never leaves the lock dangling. SIGKILL can't run a trap, so a reaped watch's lock is left behind — the dead-PID reclaim path above covers that. `$$` is the watch process's own PID, so its liveness exactly tracks whether the watch is still running.)

- [ ] **Step 4: Run to verify it passes**

Run the Step 1 script.
Expected: `held`, `rc_second=1`, `released`, `rc_reclaim=3`, `reclaimed_released`.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "feat(crew): per-watch lock with trap release and stale reclaim"
```

---

## Task 5: `lock-role` / `unlock-role` subcommands (`dispatcher.lock.d`)

> **Decision point — RESOLVED (R6.1 placement + wiring).** Put the role-lock _mechanism_ in crew.sh as `lock-role`/`unlock-role` subcommands. Rationale: (1) it keeps all lock logic in one file reusing the Task 1 helper, and (2) it makes acceptance criterion #4 (`dispatcher.lock.d` atomicity across two startups) testable at the shell level with no live dispatcher. The role lock records the **dispatcher's** session-stable PID, not the short-lived `crew` process — so the subcommand takes an optional PID (default `$PPID`), and callers pass an explicit stable PID. The wiring is now **IN SCOPE** and delivered here: **Task 5b** wires the fish `dispatcher` launcher (acquire `crew lock-role $fish_pid` at startup, `crew unlock-role` after `claude` exits), and **Task 5c** wires the `/dispatcher` promotion skill (acquire `crew lock-role $PPID` before adopting the role). This Task 5 ships and shell-tests the mechanism (Step 1 simulates startups with explicit PIDs); Tasks 5b/5c consume it.

**Files:**

- Modify: `home/ai/claude-code/crew.sh` — add a `lock-role | unlock-role)` case (e.g. after the `await)` case, before `watch)`), and extend the usage string (line 290)

**Interfaces:**

- Consumes: `_lock_acquire`/`_lock_release` (Task 1), `_crew_id`, `$dir`.
- Produces:
  - `crew lock-role [pid]` — acquire `"$dir/dispatcher.lock.d"` recording `pid` (default `$PPID`); exit `0` acquired (or already held by this _same_ `pid` — idempotent), exit `1` only if a _different_ live dispatcher already holds it.
  - `crew unlock-role` — release it.

- [ ] **Step 1: Write the failing test**

```bash
# paste harness preamble
newbus
sleep 30 & P=$!                                   # a stand-in "live dispatcher" PID
( cd "$D" && crewrun lock-role "$P" ); echo "rc1=$?"      # want rc1=0 (acquired)
test -d "$CD/dispatcher.lock.d" && echo "locked"
( cd "$D" && crewrun lock-role "$P" ); echo "rc_idem=$?"  # want rc_idem=0 (SAME owner re-acquire = idempotent no-op, per Task 1)
( cd "$D" && crewrun lock-role "$$" ); echo "rc2=$?"      # want rc2=1 (DIFFERENT live owner $$ != $P refuses)
kill "$P" 2>/dev/null; wait "$P" 2>/dev/null || true
( cd "$D" && crewrun lock-role "$$" ); echo "rc3=$?"      # want rc3=0 (dead owner $P reclaimed by live $$)
( cd "$D" && crewrun unlock-role ); test ! -d "$CD/dispatcher.lock.d" && echo "unlocked"
```

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL — `crew: unknown` / usage error (`lock-role` is not a recognized subcommand), exiting `1` with the usage string.

- [ ] **Step 3: Add the case to crew.sh**

Insert before `watch)` (line 165):

```bash
lock-role | unlock-role)
  # dispatcher role lock (R6.1): acquired at dispatcher startup, held for its
  # lifetime, recording the dispatcher's long-lived PID (default the caller's,
  # $PPID) — NOT this short-lived crew process. Released on clean shutdown; a
  # killed dispatcher's stale lock is reclaimed by the next startup's mkdir gate.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  mkdir -p "$dir"
  rlock="$dir/dispatcher.lock.d"
  if [ "$sub" = lock-role ]; then
    _lock_acquire "$rlock" "${1:-$PPID}" || {
      echo "crew: a dispatcher already holds this repo's crew bus (dispatcher.lock.d)" >&2
      exit 1
    }
  else
    _lock_release "$rlock"
  fi
  ;;
```

Then extend the usage string (line 290) — insert after `... await <agent> [--timeout S] [--interval S] |`:

```
lock-role [pid] | unlock-role |
```

- [ ] **Step 4: Run to verify it passes**

Run the Step 1 script.
Expected: `rc1=0`, `locked`, `rc_idem=0`, `rc2=1`, `rc3=0`, `unlocked`.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "feat(crew): lock-role/unlock-role subcommands for the dispatcher role lock"
```

---

## Task 6: shellcheck + deploy gate (build crew.sh before the wiring + persona)

> This is the **first** `nh home switch`. It builds the crew.sh mechanism (Tasks 1–5) so that `crew watch`/`lock-role`/`unlock-role` are on PATH — a prerequisite for both the persona (Task 7) and the launcher/skill wiring (Tasks 5b/5c, which call `lock-role`). Task 5b then needs a **second** `nh home switch` for its own fish change.

**Files:**

- Verify only: `home/ai/claude-code/crew.sh`

- [ ] **Step 1: shellcheck the script (repo convention)**

Run: `shellcheck /home/noams/nix-config-worktrees/feat-69-dispatcher-bg-watch/home/ai/claude-code/crew.sh`
Expected: no warnings/errors. Fix any before proceeding (watch for SC2155 on the new `tmp=$(mktemp …)` — it is a plain, not `local`, assignment at case-body scope, so it is fine).

- [ ] **Step 2: Build + deploy via Home Manager**

Invoke the **`nix-rebuild`** skill first (it owns the `git add` gotcha for untracked files and the `just`/`nh` wrappers), then from the worktree run: `nh home switch`.
Expected: build succeeds; the `crew` wrapper on PATH now includes the new behavior.

- [ ] **Step 3: Faithful live smoke test (real built binary, real `set -euo pipefail`)**

```bash
D=$(mktemp -d); ( cd "$D" && git init -q )
CD="$D/.git/crew"; mkdir -p "$CD"
printf '{"ts":7000,"crew_id":"t","from":"worker:b","to":"dispatcher:t","kind":"status","body":{"state":"pr_open"}}\n' >>"$CD/events.jsonl"
( cd "$D" && env CREW_ID=t crew watch --timeout 1 ) | jq -c '.cursor'   # want 7000
cat "$CD/dispatcher.cursor"                                             # want 7000
( cd "$D" && env CREW_ID=t crew watch --timeout 0 ); echo "rc=$?"       # want message + rc=1
```

Expected: `7000`, `7000`, then the rejection message and `rc=1`.

- [ ] **Step 4: Commit (if the rebuild produced tracked changes, e.g. a lockfile)**

```bash
git add -A
git commit -m "chore: deploy crew.sh background-watch changes"
```

(If nothing changed, skip.)

---

## Task 5b: Wire the fish `dispatcher` launcher to the role lock

> **Ordering:** this task edits nix-embedded fish, so its runtime behavior only exists **after** a `nh home switch`, and it calls `crew lock-role` which is only on PATH after Task 6's `nh home switch`. Therefore 5b lands **after Task 6** and needs its **own second `nh home switch`** to take effect (see Step 4 and the Rollout).

**Files:**

- Modify: `home/terminal/fish/default.nix` — inside `function dispatcher` (currently lines 388–412), between the `session_name` block (ends line 410) and the `claude …` launch (line 411), plus a release block after line 411.

**Interfaces:**

- Consumes: `crew lock-role [pid]` / `crew unlock-role` (Task 5), `crew id` (existing; needs no git repo — handled before crew.sh's repo check).
- Produces: a launcher that mints+exports `CREW_ID`, acquires the repo's role lock on `$fish_pid` when inside a git repo, and releases it after `claude` exits. Refuses to launch (returns 1) if another dispatcher already holds the repo's crew bus.

Why `$fish_pid`: it is the launcher's interactive fish shell, which blocks on the **foreground** `claude` for the whole session — its liveness exactly tracks the dispatcher session, and if it dies the child `claude` dies too. That makes it the correct, sturdy liveness anchor for this path. Line 411 is a plain child launch (**not** `exec`), so fish resumes after `claude` exits (including a crash) and can run the release.

- [ ] **Step 1: Write the failing verification**

```bash
F=/home/noams/nix-config-worktrees/feat-69-dispatcher-bg-watch/home/terminal/fish/default.nix
# all three wiring lines must be present inside the dispatcher launcher (~lines 407–415)
rg -n 'set -q CREW_ID; or set -gx CREW_ID \(crew id\)' "$F"; echo "crewid=$?"   # want a hit, crewid=0
rg -n 'crew lock-role \$fish_pid' "$F"; echo "lock=$?"                          # want a hit, lock=0
rg -n 'crew unlock-role' "$F"; echo "unlock=$?"                                 # want a hit, unlock=0
```

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL — `crewid=1`, `lock=1`, `unlock=1` (the launcher does not yet mint `CREW_ID` or touch the role lock).

- [ ] **Step 3: Edit `home/terminal/fish/default.nix`**

3a. Between the `session_name` block (after line 410, the `end` closing the `if set -q argv[1]`) and the `claude …` call (line 411), insert the CREW_ID mint + guarded role-lock acquire:

```fish
          # Mint + export the crew id once at launch (mirrors `dispatch`), so
          # `crew lock-role` has an id and claude + every child `dispatch` inherit
          # the SAME crew. `crew id` needs no git repo (handled before crew.sh's
          # repo check).
          set -q CREW_ID; or set -gx CREW_ID (crew id)
          # Acquire the repo's dispatcher role lock ONLY inside a git repo — the
          # crew bus is repo-keyed, so a no-repo launch can't use the bus and must
          # not hard-fail. $fish_pid is this interactive shell; it blocks on the
          # foreground claude below, so its liveness tracks the whole session.
          if git rev-parse --git-common-dir >/dev/null 2>&1
              if not crew lock-role $fish_pid
                  echo "dispatcher: another dispatcher already holds this repo's crew bus" >&2
                  return 1
              end
          end
```

3b. After the `claude …` call (line 411), insert the guarded release (line 411 is a child launch, not `exec`, so fish resumes here on normal exit and on a claude crash; the one edge it can miss is a hard SIGINT/kill that terminates the whole function before this line — contained: `$fish_pid` is unchanged so a same-shell relaunch is saved by the Task 1 idempotent rule, a different shell recovers when the interactive shell eventually exits, and R6.2's per-watch lock is the real INV-1 backstop):

```fish
          # claude exited (line above is a child launch, not exec) — release the
          # role lock so the next dispatcher can take the bus. Guarded the same
          # way: only released if we could have acquired it.
          if git rev-parse --git-common-dir >/dev/null 2>&1
              crew unlock-role
          end
```

- [ ] **Step 4: Verify (static now, live after rebuild)**

Static (no rebuild): rerun the Step 1 verification — expect `crewid=0`, `lock=0`, `unlock=0`. `shellcheck` does **not** apply (this is fish, not bash); the fish is validated by the nix build's fish parse when the module renders, so the real gate is `nh home switch` succeeding.

Live (after this task's own `nh home switch` — required because the launcher is nix-embedded fish and Task 6 already ran a rebuild for crew.sh). **Scope note:** this exercises the deployed `crew lock-role` _mechanism_ (reclaim of a crashed holder's stale lock + refusal of a concurrent live one); it does **not** invoke the `dispatcher` launcher itself, so the launcher's own wiring (`$fish_pid`, the git guard, `return 1`) is verified by the Step 1 `rg` grep + the nix fish-parse, not by this block. Run as bash, like the other harness blocks:

```bash
nh home switch   # invoke the nix-rebuild skill first (git-add gotcha, nh wrappers)
D=$(mktemp -d); ( cd "$D" && git init -q )
# a dead-PID role lock (a crashed dispatcher's fish_pid) is reclaimed by a live acquire
( cd "$D" && env CREW_ID=t crew lock-role 999999999 ); echo "seed=$?"    # want seed=0 (dead owner written)
sleep 60 & P=$!                                                          # a live "dispatcher" stand-in for $fish_pid
( cd "$D" && env CREW_ID=t crew lock-role "$P" ); echo "reclaim=$?"      # want reclaim=0 (stale reclaimed, P now holds)
# a second CONCURRENT live holder ($$ != the live P) is refused
( cd "$D" && env CREW_ID=t crew lock-role "$$" ); echo "refuse=$?"       # want refuse=1
kill "$P" 2>/dev/null; wait "$P" 2>/dev/null || true
```

Expected: `seed=0`, `reclaim=0`, `refuse=1`.

- [ ] **Step 5: Commit**

```bash
git add home/terminal/fish/default.nix
git commit -m "feat(dispatcher): launcher acquires the crew role lock on \$fish_pid"
```

---

## Task 5c: Wire the `/dispatcher` promotion skill to the role lock

**Files:**

- Modify: `home/ai/claude-code/commands/dispatcher.md` — add a role-lock acquisition step near the top, **before** the "adopt that role" / first `dispatch` guidance (before line 6's role-adoption paragraph).

**Interfaces:**

- Consumes: `crew lock-role [pid]` (Task 5).
- Produces: a promotion path that records the **long-lived `claude` PID** as the role-lock owner. In the Bash tool's shell `$PPID` is that `claude` process, so recording it means the lock auto-frees via stale-reclaim when the session ends — there is **no** post-session hook on the promotion path to run `unlock-role`.
- **Verified (2026-07-10, this environment):** two separate Bash-tool calls both reported `$PPID` = the same live PID with `comm` = `.claude-wrapped` (the nix-wrapped `claude` binary), i.e. the session-lived process, stable across calls. So the anchor is real here, not decorative. If a future environment wraps the Bash tool differently (a transient per-call shell as `$PPID`), the promotion-path lock degrades to the spec's documented best-effort fallback (precondition + `/dispatcher` refusal-by-prose) — Step 4 re-checks this cheaply.

Why **plain bash**, not `fish -c`: the acquisition must run in the Bash tool's own shell so `$PPID` resolves to the long-lived `claude` process (the session's liveness anchor). Wrapping it in `fish -c '…'` would record fish's transient PID instead, defeating stale-reclaim. This is the deliberate, acceptable asymmetry vs. the baked launcher (Task 5b): the launcher has a clean `unlock-role` on claude exit; the promotion path has none and relies on stale-reclaim through the same mkdir gate.

- [ ] **Step 1: Write the failing verification**

```bash
F=/home/noams/nix-config-worktrees/feat-69-dispatcher-bg-watch/home/ai/claude-code/commands/dispatcher.md
rg -n 'crew lock-role \$PPID' "$F"; echo "lock=$?"        # want a hit, lock=0
# the lock step must appear BEFORE the first `dispatch` mention (role adoption)
test "$(rg -n 'crew lock-role \$PPID' "$F" | head -1 | cut -d: -f1)" -lt \
     "$(rg -n 'dispatch <tier>' "$F" | head -1 | cut -d: -f1)" && echo "before_dispatch"
```

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL — `lock=1` (no `crew lock-role $PPID` yet); the `before_dispatch` line does not print.

- [ ] **Step 3: Edit `home/ai/claude-code/commands/dispatcher.md`**

Insert immediately after the frontmatter (after line 4's closing `---`), before the "Read … and adopt that role" paragraph:

````markdown
**First, claim the dispatcher role lock (plain bash — NOT `fish -c`):**

```
crew lock-role $PPID
```

Run this **bare** in the Bash tool (no `|| …` wrapper) so `$PPID` is the long-lived
`claude` process AND a refusal surfaces as a **failed tool call** (crew.sh prints the
holder message on stderr and exits 1). If it **refuses** — a live dispatcher already
holds this repo's crew bus — **STOP**: do NOT adopt the dispatcher role; read the live
holder PID from `.git/crew/dispatcher.lock.d/pid` and tell the human. Do not wrap the
command so it "succeeds anyway" — the non-zero exit is the stop signal.

On success you hold the role lock for the rest of this session. Unlike the baked
`dispatcher` launcher, this path has **no clean `unlock-role`**: the lock is freed
automatically when this claude session ends (the recorded PID dies → the next
dispatcher startup reclaims the stale lock through the same mkdir gate). This
stale-reclaim asymmetry is deliberate and acceptable — the baked launcher is the
sturdier, recommended path.
````

- [ ] **Step 4: Run to verify it passes**

Run the Step 1 verification — expect `lock=0` and `before_dispatch` printed.

Also re-confirm the `$PPID` anchor is still the session-lived process in the current environment (cheap, catches a changed Bash-tool wrapper): in **two separate** Bash-tool calls run `echo $PPID; ps -o pid=,comm= -p $PPID` and confirm the PID is identical across calls and its `comm` is the `claude`/`.claude-wrapped` process. If it is not stable/claude, the promotion-path lock is best-effort only (spec fallback) — say so in the commit/PR, don't claim hard mutual exclusion.

The true test is a live promotion (a `/dispatcher` run refusing when a launcher already holds the bus), which is Q1-adjacent and manual — note it, don't block on it. This file is markdown that hot-loads on merge (no rebuild needed).

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/commands/dispatcher.md
git commit -m "feat(dispatcher): /dispatcher promotion claims the crew role lock on \$PPID"
```

---

## Task 7: Rewrite the "Read the bus" section of DISPATCHER_PROTOCOL.md

**Files:**

- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md` lines 37–55 (replace 39–49's loop mechanics; keep 51–55 verbatim)

**Interfaces:**

- Consumes: the deployed crew.sh contract (Tasks 2–5). This is doc-only; it takes effect once merged to main (the baked launcher / `/dispatcher` read the on-main protocol).

- [ ] **Step 1: Write the failing verification**

```bash
F=/home/noams/nix-config-worktrees/feat-69-dispatcher-bg-watch/home/ai/claude-code/DISPATCHER_PROTOCOL.md
rg -n 'run_in_background|arm-token|--timeout 270|--timeout 3300|self-seed' "$F"   # want: several hits
rg -n 'crew watch --since' "$F"; echo "since_refs=$?"                             # want: no match, since_refs=1
rg -n 'human interrupts \(Esc\)' "$F"; echo "esc_refs=$?"                         # want: no match, esc_refs=1
rg -n 'crew roster|crew inbox|awaiting your reply in-band' "$F"                   # want: still present (51-55 kept)
```

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL — the `--since` foreground loop and the "human interrupts (Esc)" note are still present; the background-loop terms are absent.

- [ ] **Step 3: Replace lines 39–49 of DISPATCHER_PROTOCOL.md**

Replace the intro paragraph + fenced foreground loop + the paragraph ending "the human interrupts (Esc)." (lines 39–49) with:

```markdown
All workers in your shell share one `crew_id`. **Arm `crew watch` as a background
Bash call** (`run_in_background`) — a held, zero-token poll that returns the instant
any worker needs you. Because it's backgrounded, your LLM loop stays **free**: the
human can add a task or ask a question with **no Esc**, and the watch keeps running.
(A background Bash task is not bound by the foreground tool timeout; `crew watch`'s
own `--timeout` is the real bound.)

**The loop is event-driven, not a foreground spin:**

1. **Arm** exactly one `crew watch` as a `run_in_background` call. **Do NOT pass
   `--since`** — `watch` self-seeds from its per-crew cursor file, so a stale
   post-compaction cursor can't re-deliver already-handled events (double-dispatch).
   Record the returned background-task id as your **arm-token**.
2. **On the watch-completion notification**, read the task's output file:
   - non-empty stdout → a batch: parse `{"cursor":<ts>,"events":[…]}` and handle the
     **entire `events[]` in ONE turn** (reply / dispatch next / intervene). **Never
     one-turn-per-event.**
   - empty stdout (exit 3, the timeout marker) → nothing to handle.
3. **Re-arm exactly one** new `crew watch`, recording its new arm-token. On the
   exit-3 path this re-arm is **near-silent**: one tool call, zero prose.

**INV-1 — exactly one outstanding watch: never two, never zero.**

- Re-arm **only** inside a watch-completion handler turn, and only if your recorded
  arm-token is absent/terminal. This is **token-based, not list-based**: do NOT
  "check the background-task list before arming" — between the list check and the arm
  a completion can land and you'd arm a _second_ watch (the B1 race).
- A **human turn must not re-arm** while a completion for the current token is
  pending-but-unhandled — arming in a human turn is a **NO-OP**. The single re-arm
  happens later, in the completion handler.
- **Never zero:** a reaped/SIGKILLed watch still delivers a completion notification,
  which re-invokes the handler → you re-arm within one park interval. No external
  supervisor is needed (G4 self-heal).

**Park length — chosen only at re-arm (never in a human turn).**
Partition the roster: `working`+`blocked` = **ACTIVE**; `pr_open`+`done`+`failed` =
**TERMINAL / budget-freeing**. At re-arm:

- **ACTIVE** roster → `--timeout 270`: a sub-TTL cache-warm heartbeat (270, not 300 —
  the prompt-cache TTL margin is load-bearing).
- **DRAINED** roster (nothing active) → `--timeout 3300`: bounds dark time, accepts
  cache-cold since nothing is in flight.
  A DRAINED→ACTIVE transition from a human adding a task happens in a human turn, so it
  does **not** wake the outstanding 3300s park — deliberate: the new worker first posts
  `working` (which `watch` does not match), so nothing needs the park woken until that
  worker blocks/finishes, at which point the exit-0 wake fires immediately. Costs only
  cache-warmth, never responsiveness.

`crew watch` wakes on any worker `status` in `blocked`/`pr_open`/`done`/`failed`
(not `working` heartbeats) or any question `msg` to you, returning
`{"cursor":<ts>,"events":[…]}`. The terminal states (`done`/`pr_open`/`failed`) free
fan-out budget, so the same wakeup tells you when to dispatch the next queued task.
```

Leave lines 51–55 ("Two reads remain for detail:" through the blocked-worker reply guidance) unchanged.

- [ ] **Step 4: Run to verify it passes**

Run the Step 1 verification.
Expected: the background-loop terms hit; `since_refs=1` and `esc_refs=1` (no matches); the `crew roster`/`crew inbox`/`awaiting your reply in-band` lines still present.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/DISPATCHER_PROTOCOL.md
git commit -m "docs(dispatcher): background event-driven watch loop protocol"
```

---

## Acceptance-criteria coverage (self-review)

| # (spec) | Criterion                                                                                                                               | Verified by                                                                                                                                                                                                                                                                                   |
| -------- | --------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1        | B1 interleaving: human turn before handling never arms a 2nd watch; exactly one, never zero                                             | Task 7 INV-1 (token-based, human-turn NO-OP); Task 4 lock refuses a concurrent watch                                                                                                                                                                                                          |
| 2        | Reaped watch: exit-3 re-arm cursor unchanged; SIGKILL → new watch within one park; `--timeout 0` nowhere                                | Task 3 (exit-3 leaves cursor), Task 2 (`--timeout 0` rejected), Task 4 (stale reclaim), Task 7 (G4 self-heal)                                                                                                                                                                                 |
| 3        | Cursor persistence + authoritative seed; corrupt→0; `--since` omitted seeds from file                                                   | Task 3 tests (all four cases)                                                                                                                                                                                                                                                                 |
| 4        | Lock atomicity, single winner, dead-PID reclaim, idempotent same-owner re-acquire — watch lock AND role lock, mechanism AND live wiring | **Mechanism:** Task 1 tests (incl. `E_idempotent`), Task 4 tests, Task 5 shell tests (explicit PIDs) — all via the Task 1 mkdir gate. **Live wiring:** Task 5b launcher stale-reclaim + concurrent-refuse test (after 2nd `nh home switch`), Task 5c promotion `crew lock-role $PPID` refusal |
| 5        | No-Esc: human adds task / asks with a watch outstanding                                                                                 | Task 7 (background arm keeps the LLM loop free)                                                                                                                                                                                                                                               |
| 6        | Contract unchanged: exit codes, states, JSON, `me`, `>`                                                                                 | Preserved throughout; Task 6 live smoke test confirms `{cursor,events}`/exit codes                                                                                                                                                                                                            |
| 7        | Drained→active-via-human is cache-cold but responsive                                                                                   | Task 7 (BLK-4 paragraph)                                                                                                                                                                                                                                                                      |

**Placeholder scan:** none — every code/test step carries literal content. **Type consistency:** `_lock_acquire <lockdir> <owner_pid>` and `_lock_release <lockdir>` names/arity are identical across Tasks 1, 4, 5; the idempotent same-owner rule (Task 1) is what makes the Task 5b/5c re-run-`/dispatcher` case a no-op success; `cursor_file`, `since_explicit`, `wlock`, `rlock` are each introduced once and reused consistently; `crew lock-role [pid]` / `crew unlock-role` (Task 5) are called with the exact same spelling by the launcher (Task 5b, `$fish_pid`) and the skill (Task 5c, `$PPID`).

## Rollout (ordered)

The execution order is **not** the numeric label order — the wiring tasks (5b/5c) call `lock-role`, so they must run after the crew.sh mechanism is on PATH. Document order matches execution order:

1. **Tasks 1–5** (crew.sh: lock helpers, watch changes, `lock-role`/`unlock-role`) — shell-tested, no rebuild.
2. **Task 6** — shellcheck + **first `nh home switch`** + live smoke. This deploys the mechanism so `crew watch`/`lock-role`/`unlock-role` are on PATH. **CLI before both wiring and persona.**
3. **Task 5b** — wire the fish `dispatcher` launcher (`crew lock-role $fish_pid` at startup, `crew unlock-role` after `claude` exits). Nix-embedded fish, so it needs a **second `nh home switch`** to take effect; its live reclaim/refuse test runs after that rebuild.
4. **Task 5c** — wire the `/dispatcher` promotion skill (`crew lock-role $PPID`, plain bash). Markdown that **hot-loads on merge** (no rebuild).
5. **Task 7** (DISPATCHER_PROTOCOL.md) — the background-watch persona; hot-loads once merged to main. **Kept last.**
6. Pre-merge verification note (Q1 residual, low-risk): confirm the baked `--append-system-prompt-file` launcher and `/dispatcher` promotion pick up the rewritten protocol, and that a live `/dispatcher` promotion refuses when a launcher already holds the repo's crew bus.
