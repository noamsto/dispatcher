# Plan — SessionEnd backstop speaks only for a session that can name itself (#69)

**Spec:** `docs/superpowers/specs/2026-08-30-notify-worker-identity-design.md`. The spec
is authoritative for the decision, the rejected alternatives, and every rationale; this
plan sequences the edits and pins the verification after each one.

**Orchestration consult:** not run. The survey did not trip — the change is a guard in
one 42-line shell script, its two mechanically generated copies, and one new test file.
There is no cross-module decomposition for a consultant to contribute, and the single
shared interface (the bus status envelope) is untouched: the hook keeps emitting the
same shape, just never under a guessed id.

**Sequencing:** step 0 establishes a clean baseline. Step 1 is red-first and must
precede step 2 (its first three tests fail today — that failure IS the reproduction).
Step 3 is mechanical and must follow step 2. Step 4 commits, because the drift gate
cannot pass against an uncommitted change. Step 5 gates. All steps are serial; none may
run as concurrent subagents, since 2 and 3 both write files 1 asserts on.

## Two landmines, pinned so nobody rediscovers them at execute time

**Unborn HEAD.** `setup_repo` (`tests/helpers.bash:4-15`) leaves HEAD unborn —
`git rev-parse --abbrev-ref HEAD` then exits 128 and the hook's `branch` falls back to
`'?'` (`dispatch-notify.sh:11`). Every test must `git commit --allow-empty -qm init`
before `git switch -qc feat/x`, or it will assert against `worker:?`.

**`CREW_WORKER_ID` leaks in from the runner.** The suite is run from a dispatched worker
session, which has `CREW_WORKER_ID` exported (`dispatch.sh:694`) — so a test that merely
_omits_ it still sees the real worker's id and passes green against the unfixed hook.
Put `unset CREW_WORKER_ID` in `setup()` after `setup_repo`, and let tests 4-6 reintroduce
it with the repo's existing prefix idiom (`tests/crew.bats:56`:
`CREW_ID=c1 run run_crew status worker working`). Never write
`env -u CREW_WORKER_ID run_notify` — `run_notify` is a shell function and `env` execs a
binary, so it would exit 127 and the red would be uninterpretable.

---

- [ ] **Step 0: clean baseline** — no edits

  `./scripts/gen-adapters.sh` then `git status --short`. It must be empty apart from
  untracked `WORKER_TASK.md` and the two doc files. `gen-adapters.sh:67-73` also
  `rm -rf`s and re-copies each plugin's `protocols/`, and 79-81 rewrite a codex skill, so
  pre-existing drift would otherwise be misattributed to this change in step 3.

- [ ] **Step 1: `tests/dispatch-notify.bats`** — new file, red first

  Follow `tests/crew.bats:1-10` conventions exactly: `load helpers`, script paths from
  `$BATS_TEST_DIRNAME/..`, `setup_repo` / `teardown_repo`. Do **not** touch
  `tests/helpers.bash` (spec §Scope — it is shared with the recently merged #70/#72 work).

  Local helpers at the top of the file:
  - `NOTIFY="$BATS_TEST_DIRNAME/../adapters/core/dispatch-notify.sh"` — the tests drive
    the **core** copy, matching `tests/crew.bats:3`; the generated pair is covered by the
    drift gate in step 5, not by these tests.
  - `CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"`.
  - `run_notify()` — feeds the hook its stdin payload and runs it the way the plugin
    does: `jq -nc --arg c "$PWD" '{cwd:$c,hook_event_name:"SessionEnd",reason:"other"}' |
bash -euo pipefail "$NOTIFY"`.
  - `seed_status()` — `CREW_ID=c1 bash -euo pipefail "$CREW" status
'worker:feat/x#s1-1' "$1"`.
  - `task_doc()` — writes `WORKER_TASK.md` containing `crew_id: c1`, plus a
    `dispatcher_pane: $1` line when `$1` is given. `dispatch-notify.sh:10` parses it with
    `cut -d' ' -f2`, so the value must be the second space-separated field
    (`dispatcher_pane: %9` → `%9`). The `crew_id:` line is load-bearing:
    `dispatch-notify.sh:19-20` skips the whole bus path without it, so a fixture omitting
    it makes tests 1-3 pass **vacuously**.

  `setup()`, after `setup_repo`: `unset CREW_WORKER_ID`;
  `git commit --allow-empty -qm init`; `git switch -qc feat/x`;
  `LOG="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"`.

  **Fixture per test — no shared shape, because they genuinely differ:**

  | test    | `task_doc` | `stub_bin tmux` | seed      |
  | ------- | ---------- | --------------- | --------- |
  | 1, 2, 3 | `%9`       | yes             | `working` |
  | 4       | no pane    | no              | `working` |
  | 5       | `%9`       | yes             | `working` |
  | 6       | no pane    | no              | `done`    |

  Tests 4 and 6 must **not** get a `dispatcher_pane:` and must **not** stub tmux — with
  a pane and no stub, the hook would fire a real `tmux display-message` against the
  developer's live tmux server on every local run. Tests 4 and 5 share tests 1-3's
  `crew_id:` + `working` fixture and _do_ produce a bus write and a ping respectively;
  that is what proves 1-3 are not passing vacuously.

  After `stub_bin tmux`, `: >"$STUB_LOG"` — `stub_bin` (`tests/helpers.bash:24-35`)
  creates `$STUB_DIR` but not the log until tmux is first invoked, so a `grep` against it
  otherwise prints `No such file or directory` to stderr.

  Six tests, one per spec acceptance criterion 1-6:
  1. `"a session with no CREW_WORKER_ID writes nothing and pings nothing"` (AC1) —
     `cp "$LOG" "$LOG.before"`; `run run_notify` (setup already unset the var); assert
     `[ "$status" -eq 0 ]` — the guarded path must exit 0, matching the `|| exit 0` idiom
     at `dispatch-notify.sh:8`; a nonzero exit would surface a hook error to the engine
     on every non-worker `SessionEnd`. Then `cmp -s "$LOG" "$LOG.before"`, and
     `! grep -q display-message "$STUB_LOG"`. Assert the **absence of a
     `display-message` invocation**, never an empty `$STUB_LOG` — spec AC1, because
     roster's own `tmux list-panes` also lands in that log.
  2. `"an empty CREW_WORKER_ID is treated as absent"` (AC2) — identical, but
     `CREW_WORKER_ID= run run_notify` (prefix form, per `tests/crew.bats:56`; `run` is
     required or `$status` is unset and the assertion errors). This is the test that
     fails an implementation written as `[ -z "${CREW_WORKER_ID+x}" ]`.
  3. `"roster still reports the live session after a session-less SessionEnd"` (AC3) —
     as test 1, then `CREW_ID=c1 bash -euo pipefail "$CREW" roster c1`; assert the JSON
     has `length == 1`, `.[0].state == "working"`, `.[0].session != null`, and
     `[.[0].sessions[] | select(.session == null)] | length == 0`.
  4. `"the dispatched worker's SessionEnd records exited under its sessioned id"` (AC4) —
     `CREW_WORKER_ID='worker:feat/x#s1-1' run run_notify`; assert the last log line has
     `.from == "worker:feat/x#s1-1"` and `.body.state == "exited"`.
  5. `"the dispatched worker's SessionEnd still pings the dispatcher pane"` (AC5) —
     as test 4 but with the pane fixture and the tmux stub; assert
     `grep -c 'display-message' "$STUB_LOG"` is exactly `1` and the logged argv contains
     `-t %9`. This is the criterion that catches hoisting the guard too far up.
  6. `"a worker that already reported done is not overridden"` (AC6) — seed `done`; run
     as test 4 but with the pane fixture and the tmux stub; assert the ping fired and no
     `"state":"exited"` line exists — the ping pins that the hook ran past the guard, so
     the absent write is the done-check, not an early exit. This exercises the hook's own
     guard at `dispatch-notify.sh:34-35`, not `crew.sh`'s (`crew.sh:345-354` only
     suppresses an identical terminal re-post, and is gated on the log already existing).

  Verify: `bats tests/dispatch-notify.bats` — tests 1, 2 and 3 **fail**, 4, 5 and 6 pass.
  That red is the reproduction; do not proceed until it is observed.

- [ ] **Step 2: the guard** — `adapters/core/dispatch-notify.sh`

  Two edits, no others:

  a. After the `[[ -f "$cwd/WORKER_TASK.md" ]] || exit 0` precondition (line 8) and
  **before** the `pane=` read (line 10), add the guard with a comment saying why, in
  the file's existing voice (the spec's §Decision is the source; do not paste it
  wholesale). Substance the comment must carry: `dispatch` puts `CREW_WORKER_ID` in
  the worker window's environment, so a session without one is something else in the
  same worktree — another engine's subagent session, or a human's auxiliary pane — and
  cannot know which worker it belongs to; guessing `worker:$branch` posted a
  session-less `exited` that masked a live worker (#69); the hook cannot tell them
  apart and cannot probe liveness, because the worker's own SessionEnd blocks on its
  hooks. Then:

         [[ -n ${CREW_WORKER_ID:-} ]] || exit 0

  `${CREW_WORKER_ID:-}` (not `+x`) is load-bearing: unset and empty must both take
  the silent path (spec §Decision), and the `:-` form is also what keeps `set -u`
  happy. Placing it above the pane read is what makes AC1's "pings nothing" true.

  b. At line 28, replace `me="${CREW_WORKER_ID:-worker:$branch}"` with
  `me="$CREW_WORKER_ID"`. Keep the first two sentences of the comment above it — the
  "never from WORKER_TASK.md … would post THIS session's `exited` against its
  successor (#17)" rationale is still true and still load-bearing. Delete only its
  last sentence ("A session launched before this change has no `CREW_WORKER_ID` and
  keeps the old id"), which described the fallback being removed — nothing in the gate
  catches a comment that outlives its code.

  Nothing else in the file changes — not the envelope, not the dedupe `case`, not the
  `done|failed|pr_open` set.

  Verify: `shellcheck adapters/core/dispatch-notify.sh` clean;
  `bats tests/dispatch-notify.bats` all six green.

- [ ] **Step 3: regenerate the adapters** — mechanical

  `./scripts/gen-adapters.sh`, which copies the hook into the claude-code and codex
  plugin trees (`scripts/gen-adapters.sh:67-73`). Do not hand-edit either copy.

  Verify: `git status --short` shows exactly the core file and its two copies as
  modified (step 0 guaranteed the baseline); both copies are byte-identical to
  `adapters/core/dispatch-notify.sh` (`cmp`). For idempotence, compare the tree to
  itself across a second run rather than to `HEAD`:
  `git status --porcelain >/tmp/a; ./scripts/gen-adapters.sh; git status --porcelain >/tmp/b; cmp /tmp/a /tmp/b`.
  `git diff --exit-code` is **wrong** here — the intended change is uncommitted, so it
  always exits 1.

- [ ] **Step 4: commit** — before the gate, not after

  `git add` the hook, both generated copies, `tests/dispatch-notify.bats`, and the two
  doc files. Do **not** stage `WORKER_TASK.md` (untracked, and not in `.gitignore`).
  Commit conventionally, e.g. `fix(crew): only a session that can name itself posts a
terminal status (#69)`.

  Staging is also a prerequisite for step 5: `nix flake check` evaluates from the git
  tree and ignores untracked files, so `tests/dispatch-notify.bats` is invisible to it
  until added.

- [ ] **Step 5: full gate** (AC7)
  - `shellcheck adapters/core/*.sh scripts/*.sh`
  - `./scripts/gen-adapters.sh && git diff --exit-code` — CI's drift gate
    (`.github/workflows/ci.yml:20-23`). This now passes because step 4 committed.
  - `bats tests/`
  - `nix flake check`

  `tests/dispatch-notify.bats` needs no runner registration — CI runs `bats tests/` by
  directory (`.github/workflows/ci.yml:25`) — and `.bats` is excluded from the shellcheck
  hook (`flake.nix:70`), so the new file is linted by neither. Keep it shellcheck-clean
  by eye anyway.
