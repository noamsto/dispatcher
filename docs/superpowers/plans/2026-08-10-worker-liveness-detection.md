# Worker Liveness Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a worker that is parked on an interactive prompt, or whose turn died mid-task, visible on the crew bus as a recoverable `blocked` — without ever flipping a healthy long-running worker.

**Architecture:** `crew stall-watch` stops being a startup-only silence timer and becomes a lifetime-scoped watchdog with four detectors over one pane capture per tick (D0 `stalled:`, D1 `prompt:`, D2 `turn-stall:`, D3 `quiet:`). Every detector posts `blocked` first; only `quiet:`/`turn-stall:` episodes escalate to `failed` after a second, later evidence check. Watchdog events are machine-labelled (`body.source:"watchdog"` + reserved `detail` prefixes), identity is branch-keyed and suffix-tolerant, and every bus read is scoped to the watchdog's own start timestamp. `roster` and `rate` learn to read the new field; `WORKER_PROTOCOL.md`/`DISPATCHER_PROTOCOL.md` document the contract and are projected by `scripts/gen-adapters.sh`.

**Tech Stack:** bash (`writeShellApplication`-wrapped, so `set -euo pipefail` is ambient), `jq`, bats, tmux, Nix flake (`nix flake check` = treefmt + shellcheck + bats).

**Spec:** `docs/superpowers/specs/2026-08-10-worker-liveness-detection-design.md`. **Its final section, "Post-cap critic resolutions (BINDING)", overrides any earlier text it contradicts.** This plan implements C-1…C-6, not the pre-critic text.

**Evidence:** `EVIDENCE-2026-08-10.txt` (repo root). Every fixture in Task 1 is pinned from it.

---

## Global constraints

- **Default branch is `extract`, not `main`.** Open the PR against `extract`.
- **Scope guard — files this plan may touch, and no others:**
  - Own: `adapters/core/crew.sh`, `tests/crew.bats`, the liveness/blocked-state sections of `adapters/core/protocols/WORKER_PROTOCOL.md`, the one liveness paragraph + one `crew reply` clause of `adapters/core/protocols/DISPATCHER_PROTOCOL.md`.
  - Minimal touch: `adapters/core/dispatch.sh` — **only** the two-token `--engine "$agent"` addition on the `crew stall-watch` spawn line at the very end of the file. Nothing else there, and **never** the `--pr` path.
  - Never touch: `scripts/gen-adapters.sh` (you RUN it), `tests/adapters.bats`, the `spec-plan-critic` skill body, and anything under `adapters/claude-code/`, `adapters/codex/`, `adapters/cursor/` — those trees are **generated**. Protocol edits go in `adapters/core/protocols/**` and are projected by running `scripts/gen-adapters.sh`; CI asserts no drift.
- **Tasks 2, 3 and 4 all edit `adapters/core/crew.sh` and MUST run SEQUENTIALLY — never as concurrent subagents.** Task 1 also edits `tests/crew.bats`, which Tasks 3 and 4 append to, so 1 → 2 → 3 → 4 is a strict chain.
- Parallel-safe pairs: **Task 5 (`dispatch.sh`) and Task 6 (protocols)** touch disjoint files from each other and from 1–4, so they can run concurrently with the chain and with each other. Task 7 (regenerate) must run **after** Task 6. Task 8 (gate) runs last.
- **Locate `stall-watch` by symbol, not line number** — it is the `stall-watch)` case arm. `WORKER_TASK.md` cites `crew.sh:792`; it is at line 572 in this worktree and will move as you edit. Same for `roster)`, `rate)`.
- `crew.sh` has **no shebang and no `set -euo pipefail`** — `writeShellApplication` prepends them, and the tests run it as `bash -euo pipefail "$CREW"`. Write code that survives `set -e`: **never end an `if`/`else` branch (or a function) with a bare `A && B` list** — its exit status 1 propagates and kills the script. Use explicit `if … then … fi`.
- Conventional commits (`feat(crew): …`, `fix(crew): …`, `docs(protocol): …`). Pre-commit hooks reformat markdown (prettier) — if the first `git commit` fails with "files were modified by this hook", `git add` the reformatted files and re-run the **same** commit.
- Run every command from the repo root: `/Users/noams/Data/git/.worktrees/git/dispatcher/feat-31-surface-workers-blocked-on-interactive-p`.
- Tests must not sleep for minutes: every stall-watch test drives the loop through `CREW_STALL_SAMPLE_CMD` with `--grace 0 --interval 1` and a small `--max-life`. **No tmux in any test.**

## Design decisions this plan locks in (read before Task 2)

Three places where the spec is ambiguous or self-contradicting after the binding resolutions. These are decided here; do not re-litigate them mid-implementation.

1. **`stalled:` does not escalate.** C-1 says escalation is "available only to `quiet:` and `turn-stall:` origins", which excludes `stalled:` even though §6 and criterion 7 assumed a D0s escalation path. C-1 is binding, so D0's `stalled:` episode never reaches `failed`.
2. **Therefore INV-W3 stays _same-prefix_, not any-prefix.** A permanently static pane inside the startup window posts `blocked`/`stalled:` at `--stall`; at `--idle` D3 posts `blocked`/`quiet:` over it, and _that_ episode escalates to `failed` at `--idle + --dead`. That second post is what preserves Decision 9's budget-freeing path after C-1 removed it from `stalled:`. An any-prefix INV-W3 would suppress the `quiet:` post and pin the roster ACTIVE forever.
3. **D0p is deleted (C-2).** D1/D2/D3 are armed from `grace` onward with no dependence on `--window`; `--window` bounds only D0's silence branch. D0p's intent survives as a **negative check** inside D0: on a static pane, if the frame is a prompt, D0 says nothing and D1 owns it.

---

### Task 1: stall-watch test harness, pinned fixtures, and the RED test suite

**Model:** default (sonnet). Test-writing is not high-risk.

**Files:**

- Modify: `tests/crew.bats` (append; the file currently has **zero** stall-watch coverage)

**Sequencing:** first in the chain. Task 2 turns these red tests green. **Do not commit at the end of this task** — the suite is intentionally failing; Task 2 commits both files together.

**Interfaces this task pins (Task 2 must match them exactly):**

- CLI: `crew stall-watch <worker-id|branch> --pane <id> [--engine E] [--grace S] [--stall S] [--window S] [--interval S] [--idle S] [--dead S] [--max-life S]`
- Event shape: `{kind:"status", from:"worker:<branch>", to:"dispatcher:<crew>", body:{state, detail, source:"watchdog"}}`
- Detail strings: `prompt: interactive prompt in pane <id> — worker is waiting on input nobody can give` · `stalled: no output for <stall>s` · `turn-stall: token count static at <tok> for <n>s while the pane clock advanced` · `quiet: pane unchanged for <n>s` · `dead: <origin-prefix> unchanged for <n>s` · clearance `working` with detail `<origin-prefix> cleared`

- [ ] **Step 1: Append the harness helpers to `tests/crew.bats`**

Append at the end of the file:

```bash
# ---------------------------------------------------------------------------
# stall-watch harness
# ---------------------------------------------------------------------------

# bus — the raw event log for the test repo.
bus() {
  cat "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl" 2>/dev/null || true
}

# seed_raw <from> <state> <detail> <source> [ts_ms] — append a status event
# directly, so a test can plant a `source:"watchdog"` event or one dated into a
# previous run (things `crew status` cannot express).
seed_raw() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg f "$1" --arg s "$2" --arg d "$3" --arg src "$4" \
    --argjson ts "${5:-$(($(date +%s) * 1000))}" \
    '{ts:$ts, crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status",
      body:({state:$s}
            + (if $d!="" then {detail:$d} else {} end)
            + (if $src!="" then {source:$src} else {} end))}' >>"$logf"
}

# stall_sampler <frame-file>... — install a CREW_STALL_SAMPLE_CMD that emits the
# given frames one per call and repeats the last one forever. The literal token
# GONE makes the sampler exit non-zero from that call on (pane vanished).
stall_sampler() {
  SAMPLER_DIR="$BATS_TEST_TMPDIR/sampler.$$"
  mkdir -p "$SAMPLER_DIR"
  printf '%s\n' "$@" >"$SAMPLER_DIR/frames"
  printf '0' >"$SAMPLER_DIR/n"
  cat >"$SAMPLER_DIR/sample" <<'EOS'
#!/usr/bin/env bash
d="$(dirname "$0")"
n=$(cat "$d/n")
n=$((n + 1))
printf '%s' "$n" >"$d/n"
total=$(wc -l <"$d/frames")
i="$n"
[ "$i" -gt "$total" ] && i="$total"
f=$(sed -n "${i}p" "$d/frames")
[ "$f" = GONE ] && exit 1
cat "$f"
EOS
  chmod +x "$SAMPLER_DIR/sample"
  export CREW_STALL_SAMPLE_CMD="$SAMPLER_DIR/sample"
}

# frame_file <name> — read a frame from stdin, write it, echo its path.
frame_file() {
  local f="$BATS_TEST_TMPDIR/frame.$1"
  cat >"$f"
  printf '%s' "$f"
}
```

- [ ] **Step 2: Append the pinned fixtures**

Every frame below is from `EVIDENCE-2026-08-10.txt`. The evidence file joins a frame's lines with `|`; these restore the line breaks. Append:

```bash
# ---- fixtures, pinned from EVIDENCE-2026-08-10.txt ------------------------

# A3/A4 gate capture, pane %118 — a live option-select prompt frame. The footer
# is the last non-empty line; the nearest numbered option is 2 lines above it.
fx_prompt_select() {
  frame_file prompt_select <<'EOF'
  2. Gate everything on 3.8
     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.
  3. Require 3.8, drop legacy
  4. Type something.
──────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
EOF
}

# [field] session 3 — the workspace-trust frame that wedged three healthy
# workers. Footer is `Enter to confirm`, marker is ASCII `>`, no `Esc to cancel`.
fx_prompt_trust() {
  frame_file prompt_trust <<'EOF'
Quick safety check: Is this a project you created or one you trust?
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
EOF
}

# The same trust frame scrolled into the transcript: the input box is the last
# non-empty line, so the geometry anchor must reject it (this is what keeps this
# very test file from being a false-positive source).
fx_prompt_scrollback() {
  frame_file prompt_scrollback <<'EOF'
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
EOF
}

# Same, with the option-select footer, to assert the widening is symmetric.
fx_select_scrollback() {
  frame_file select_scrollback <<'EOF'
  5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
EOF
}

# A3 — a finished/idle pane: no meter, no prompt, ends on the input box.
fx_idle_box() {
  frame_file idle_box <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents
EOF
}

# fx_meter <clock> <tokens> — D2's shape: a live meter with NO live subagent
# row. The `⎿ Done` history line is included on purpose: it is the decoy that
# must NOT read as a subagent row, or D2 would be neutered forever.
fx_meter() {
  frame_file "meter.$1.$2" <<EOF
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✳ Perusing… ($1 · ↓ $2 tokens · thinking more with high effort)
EOF
}

# fx_subbatch <parent-clock> <subagent-elapsed> — A1's measured false-positive
# driver, pane %129, verbatim shape: meter present, clock rising, parent token
# string static at 73.2k, and a LIVE subagent row painted throughout.
fx_subbatch() {
  frame_file "subbatch.$1.$2" <<EOF
  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)
✶ Hatching… ($1 · ↓ 73.2k tokens)
  ◯ general-purpose  Revise spec per critic                              $2 · ↓ 71.5k tokens
EOF
}

# [#31]'s transcription — ASCII-fied `·`/`↓`/`…`. Must NOT match the meter ERE.
fx_meter_transcribed() {
  frame_file meter_transcribed <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
Considering... 1h 20m - 28.0k tokens
EOF
}

# fx_meter_hours <clock> — the reconstructed ≥1h wire form (A2, uncaptured).
fx_meter_hours() {
  frame_file "meter_hours.$1" <<EOF
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
Considering… ($1 · ↓ 28.0k tokens)
EOF
}
```

- [ ] **Step 3: Append the positive-detection tests**

```bash
@test "stall-watch: D1 posts blocked/prompt: on the option-select frame" {
  p=$(fx_prompt_select)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|prompt: interactive prompt in pane %9 —"* ]]
}

@test "stall-watch: D1 fires on the workspace-trust frame outside the startup window" {
  # Criterion 4b — the widened footer set lives in D1's own signature set, not
  # only in D0's classifier.
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == prompt:* ]]
}

@test "stall-watch: session-3 regression — a static trust prompt is prompt:, never failed or stalled:" {
  # The measured 3/3 false positive. A pane byte-static across the whole --stall
  # window, inside --window, carrying the trust frame.
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == prompt:* ]]
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'stalled:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D0 posts blocked with a diagnosis-free stalled: detail" {
  # Full-string match on purpose: a reintroduced `(suspected …)` fails CI.
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: D2 posts blocked/turn-stall: when the clock advances and tokens do not" {
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  d=$(fx_meter "6m 14s" "25.0k")
  stall_sampler "$a" "$b" "$c" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|watchdog" ]
  run bash -c "bus | jq -r '.body.detail'"
  [[ "$output" == "turn-stall: token count static at 25.0k for "* ]]
}

@test "stall-watch: D2 reads the reconstructed 1h meter and ignores #31's transcription" {
  # The hours alternative of the meter ERE (A2 is a documented false-negative
  # risk per C-4, not a gate) — and the ASCII-fied paste must stay unmatched.
  a=$(fx_meter_hours "1h 20m")
  stall_sampler "$a" "$a" "$a" "$a"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c 'turn-stall:' || true"
  [ "$output" = "0" ] # clock never changed: a static capture is not evidence

  rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  a=$(fx_meter_hours "1h 20m")
  b=$(fx_meter_hours "1h 21m")
  c=$(fx_meter_hours "1h 22m")
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c 'turn-stall:' || true"
  [ "$output" = "1" ]

  rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  t=$(fx_meter_transcribed)
  stall_sampler "$t" "$t" "$t" "$t"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c 'turn-stall:' || true"
  [ "$output" = "0" ] # no meter matched → D2 has nothing to read
}

@test "stall-watch: D3 posts blocked/quiet: on a byte-identical pane in steady state" {
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|quiet: pane unchanged for "* ]]
}
```

- [ ] **Step 4: Append the negative tests — the regression that matters most**

```bash
@test "stall-watch: healthy subagent batch produces ZERO events" {
  # A1's measured false-positive driver, verbatim: meter present, clock rising,
  # parent token string static at 73.2k, live subagent row throughout. D2 is
  # vetoed by the row; D1 is vetoed by the meter; D3 by the byte changes.
  a=$(fx_subbatch "26m 57s" "3m 29s")
  b=$(fx_subbatch "27m 12s" "3m 45s")
  c=$(fx_subbatch "27m 27s" "4m 0s")
  d=$(fx_subbatch "27m 42s" "4m 15s")
  e=$(fx_subbatch "27m 58s" "4m 30s")
  stall_sampler "$a" "$b" "$c" "$d" "$e" "$e"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 6
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: the subagent-row veto survives LC_ALL=C" {
  # C-5: a single-character bracket expression consumes one BYTE under LC_ALL=C,
  # so a non-multibyte-safe class would never match `◯` and D2 would lose its
  # only measured guard — silently.
  a=$(fx_subbatch "26m 57s" "3m 29s")
  b=$(fx_subbatch "27m 12s" "3m 45s")
  c=$(fx_subbatch "27m 27s" "4m 0s")
  stall_sampler "$a" "$b" "$c" "$c" "$c"
  LC_ALL=C CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a meter with rising tokens produces ZERO events" {
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "26.1k")
  c=$(fx_meter "5m 59s" "27.4k")
  d=$(fx_meter "6m 14s" "28.8k")
  stall_sampler "$a" "$b" "$c" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a working heartbeat damps D2" {
  CREW_ID=c1 run_crew status worker:feat/x working
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 4
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a self-reported blocked suppresses every detector" {
  CREW_ID=c1 run_crew status worker:feat/x blocked "which approach?"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 2 --dead 999 --max-life 4
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a prompt frame in scrollback produces ZERO events, both footers" {
  for fx in fx_prompt_scrollback fx_select_scrollback; do
    rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
    p=$($fx)
    stall_sampler "$p" "$p" "$p" "$p"
    CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
      --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
    run bash -c "bus | grep -c . || true"
    [ "$output" = "0" ]
  done
}

@test "stall-watch: --engine codex gets no prompt or meter detector, and never failed" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 4
  # The static pane is not classifiable for codex, so it falls to D0s.
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
  run bash -c "bus | grep -c 'prompt:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a missing --engine enables no signature detector" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}
```

- [ ] **Step 5: Append the episode, clearance and escalation tests**

```bash
@test "stall-watch: one post per episode, then a working clearance that re-arms" {
  p=$(fx_prompt_trust)
  q=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$q" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 7
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 3 ]
  [[ "${lines[0]}" == blocked\|prompt:* ]]
  [ "${lines[1]}" = "working|prompt: cleared" ]
  [[ "${lines[2]}" == blocked\|prompt:* ]]
}

@test "stall-watch: a quiet: episode escalates to failed after --dead" {
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 9
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == blocked\|quiet:* ]]
  [[ "${lines[1]}" == "failed|dead: quiet: unchanged for "* ]]
}

@test "stall-watch: a clearance before --dead cancels the escalation" {
  p=$(fx_idle_box)
  q=$(fx_meter "5m 29s" "25.0k")
  stall_sampler "$p" "$p" "$p" "$q" "$q" "$q"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 3 --max-life 6
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet: cleared' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a prompt: episode NEVER escalates (C-1)" {
  # An unanswered answerable question is waiting work, not death. Escalating it
  # would reproduce session 3 with a 30-minute delay.
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 8
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'prompt:' || true"
  [ "$output" = "1" ]
}
```

- [ ] **Step 6: Append the identity, invariant and lifetime tests**

```bash
@test "stall-watch: INV-W0 a — a bare-id worker terminal state mutes a suffixed watchdog" {
  CREW_ID=c1 run_crew status worker:feat/x done
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1786338213-54181" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 b — a suffixed-id worker terminal state mutes a bare watchdog" {
  seed_raw "worker:feat/x#s99" done "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 c — a suffixed heartbeat damps a bare-invoked watchdog" {
  seed_raw "worker:feat/x#s99" working "" ""
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 4
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 d — writes normalise from, so roster shows ONE row" {
  CREW_ID=c1 run_crew status worker:feat/x working
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1786338213-54181" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x" ]
  CREW_ID=c1 run run_crew roster c1
  run bash -c "printf '%s' '$output' | jq 'length'"
  [ "$output" = "1" ]
}

@test "stall-watch: INV-W1 — a terminal state already on the bus produces zero writes" {
  CREW_ID=c1 run_crew status worker:feat/x failed "gate red"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W3 — a second watchdog does not re-post an open episode" {
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %9 — worker is waiting on input nobody can give" watchdog
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: C-3 — a previous run's terminal state does not mute a new watchdog" {
  seed_raw worker:feat/x failed "dead: quiet: unchanged for 1800s" watchdog "$((($(date +%s) - 3600) * 1000))"
  seed_raw worker:feat/x exited "" "" "$((($(date +%s) - 3500) * 1000))"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: pr_open does not exit the watchdog, done does" {
  CREW_ID=c1 run_crew status worker:feat/x pr_open "" https://example.com/pr/1
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: survives 2 sample failures and exits after the 3rd" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" GONE GONE "$p" "$p" "$p" GONE GONE GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 30
  [ "$status" -eq 0 ]
  # It survived the pair at samples 2-3 (the prompt confirmed on 4+5 and posted),
  # then exited on the triple rather than running to --max-life 30.
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: writes zero msg events" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c '\"kind\":\"msg\"' || true"
  [ "$output" = "0" ]
  CREW_ID=c1 run run_crew inbox "dispatcher:c1"
  [ -z "$output" ]
}

@test "stall-watch: rejects an unknown flag" {
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --bogus 1
  [ "$status" -eq 1 ]
  [[ "$output" == *"unknown arg"* ]]
}
```

- [ ] **Step 7: Run the suite and confirm it fails for the right reason**

Run: `nix develop -c bats tests/crew.bats`

Expected: every pre-existing test still passes; every new `stall-watch:` test **fails**. Spot-check that the failures are behavioural (wrong/absent events, `unknown arg '--engine'`), **not** harness errors like `frame_file: command not found` or a bats parse error. If a harness error appears, fix the harness before moving on — Task 2 must not be debugging this file.

**Do not commit.** Task 2 commits both files.

---

### Task 2: Rewrite the `stall-watch` case arm (implement: opus)

**Model: opus.** This is the detector and invariant core — subtle cross-process ordering (INV-W1/W2/W3), a false positive here destroys real work, and every other task depends on its contract. This is the only step in the plan that earns an opus tag.

**Files:**

- Modify: `adapters/core/crew.sh` — the `stall-watch)` case arm **only** (locate with `rg -n '^stall-watch\)' adapters/core/crew.sh`). Do not touch `status`, `watch`, `await`, `inbox`, `log`, `report`, `reap`.

**Sequencing:** strictly after Task 1. Strictly before Tasks 3 and 4 (same file).

- [ ] **Step 1: Locate the arm**

```bash
rg -n '^stall-watch\)|^reap\)' adapters/core/crew.sh
```

Expected: two hits. The arm to replace runs from `stall-watch)` through the `;;` immediately before `reap)`. Read that range with `Read` offset/limit before editing.

- [ ] **Step 2: Replace the whole arm with this implementation**

```bash
stall-watch)
  # stall-watch <worker-id|branch> --pane <id> [--engine E] [--grace S] [--stall S]
  #   [--window S] [--interval S] [--idle S] [--dead S] [--max-life S]
  #
  # Lifetime-scoped liveness watchdog, spawned per worker by `dispatch`. The bus
  # reflects only what a worker POSTS, so a worker parked on an interactive
  # prompt, or whose turn died mid-task, is indistinguishable from one that is
  # working (#31). Four detectors read one pane capture per tick:
  #   D0 stalled:    static pane inside the startup --window whose frame is NOT a prompt
  #   D1 prompt:     prompt frame at the verified geometry, no meter, 2 samples
  #   D2 turn-stall: meter clock advancing, token string static, no live subagent row
  #   D3 quiet:      byte-identical pane for --idle
  # Every detector posts `blocked` — recoverable, answerable, and cheap to be
  # wrong about. Only quiet:/turn-stall: episodes escalate to `failed`, and only
  # after a second evidence check --dead later; a prompt still on screen is
  # evidence that nobody answered, not that the worker died, so it never
  # escalates. Engine signatures are the data table below: claude only, because
  # a guessed signature is a false-positive generator.
  # CREW_STALL_SAMPLE_CMD overrides the sampler (stdout = pane text, exit code =
  # pane liveness) so the loop is testable without tmux.
  arg="${1:-}"
  shift || true
  [ -n "$arg" ] || {
    echo "crew: stall-watch <worker-id|branch> --pane <id> [--engine E] [--grace S] [--stall S] [--window S] [--interval S] [--idle S] [--dead S] [--max-life S]" >&2
    exit 1
  }
  # INV-W0 — identity is branch-keyed and suffix-tolerant. dispatch has shipped
  # BOTH `worker:<branch>#s<session>` and a bare `<branch>`, and a watchdog
  # cannot know which one launched it. Under exact-string matching every safety
  # check below silently no-ops and the roster splits into two rows for one
  # worker (measured: EVIDENCE-2026-08-10.txt).
  branch="${arg#worker:}"
  branch="${branch%%#*}"
  me="worker:$branch"
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  pane=""
  engine="unknown"
  grace=45
  stall=300
  window=900
  interval=15
  idle=1800
  dead=1800
  max_life=43200
  while [ $# -gt 0 ]; do
    case "$1" in
    --pane)
      pane="${2:-}"
      shift 2
      ;;
    --engine)
      engine="${2:-}"
      shift 2
      ;;
    --grace)
      grace="${2:-}"
      shift 2
      ;;
    --stall)
      stall="${2:-}"
      shift 2
      ;;
    --window)
      window="${2:-}"
      shift 2
      ;;
    --interval)
      interval="${2:-}"
      shift 2
      ;;
    --idle)
      idle="${2:-}"
      shift 2
      ;;
    --dead)
      dead="${2:-}"
      shift 2
      ;;
    --max-life)
      max_life="${2:-}"
      shift 2
      ;;
    *)
      echo "crew: stall-watch: unknown arg '$1'" >&2
      exit 1
      ;;
    esac
  done
  [ -n "$pane" ] || {
    echo "crew: stall-watch needs --pane <id>" >&2
    exit 1
  }
  # Signature table. Enabling an engine is DATA, not logic: capture its prompt
  # and meter frames on a work-profile host, pin them as fixtures, add a row.
  # codex/cursor deliberately get the frame-free detectors (D0/D3) only — a
  # cursor footer that permanently contained an `Enter to select`-like string
  # would pin every cursor worker at `blocked` forever.
  case "$engine" in
  claude)
    sig_prompt=1
    sig_meter=1
    ;;
  *)
    sig_prompt=0
    sig_meter=0
    ;;
  esac
  # Multibyte-safe BY CONSTRUCTION, not by ambient locale: under LC_ALL=C a
  # bracket expression consumes one BYTE, so a single-character class would
  # never match `◯` (U+25EF, 3 bytes) and D2 would silently lose its only
  # measured false-positive guard. Hence `+` on the glyph classes and an
  # alternation rather than a bracket set for `❯`.
  re_option='^[[:space:]]*(>|❯|\*)?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]'
  re_meter='^[^[:alnum:]]*[A-Za-z]+…[[:space:]]\(([0-9]+h([[:space:]][0-9]+m)?([[:space:]][0-9]+s)?|[0-9]+m([[:space:]][0-9]+s)?|[0-9]+s)[[:space:]]·[[:space:]]↓[[:space:]][0-9.]+k?[[:space:]]tokens'
  re_subrow='^[[:space:]]*[^[:alnum:][:space:]]+[[:space:]]+[a-z][a-z-]+[[:space:]][[:space:]]+.*[[:space:]](([0-9]+h[[:space:]])?([0-9]+m[[:space:]])?[0-9]+s)[[:space:]]·[[:space:]]↓'

  # Raw pane text on stdout; non-zero when the pane is gone. The CALLER hashes:
  # D0/D3 read the hash, D1/D2 read the text.
  _sample() {
    if [ -n "${CREW_STALL_SAMPLE_CMD:-}" ]; then
      eval "$CREW_STALL_SAMPLE_CMD" 2>/dev/null
    else
      tmux capture-pane -p -t "$pane" 2>/dev/null
    fi
  }

  # C-3 — every bus read is scoped to THIS run. events.jsonl is append-only per
  # repo and re-dispatch onto the same branch is a first-class flow, so an
  # unscoped read lets the PREVIOUS run's failed/exited mute a freshly started
  # watchdog for its entire life, silently, at exit 0. Since the documented
  # recovery for every watchdog event is "kill and re-dispatch", that would
  # guarantee the run after any watchdog event has no watchdog at all.
  run_start_ms=$(($(date +%s) * 1000))
  bus_ts=0
  bus_state=""
  bus_source=""
  bus_detail=""
  _bus_refresh() {
    local l
    bus_ts=0
    bus_state=""
    bus_source=""
    bus_detail=""
    [ -f "$log" ] || return 0
    l=$(jq -r --arg c "$crew" --arg m "$me" --argjson t0 "$run_start_ms" '
        select(.crew_id==$c and .kind=="status" and .ts>=$t0
               and (.from==$m or (.from|startswith($m+"#"))))
        | [(.ts|tostring), .body.state, (.body.source // ""), (.body.detail // "")]
        | @tsv' "$log" 2>/dev/null | tail -1 || true)
    [ -n "$l" ] || return 0
    IFS=$'\t' read -r bus_ts bus_state bus_source bus_detail <<BUSLINE
$l
BUSLINE
    return 0
  }

  # INV-W1 — the watchdog is a subordinate writer. Re-read the bus immediately
  # before EVERY append (the hazard lives in the gap between sampling the pane
  # and writing the line) and abort if the worker has finished. Exempt from the
  # read cadence below on purpose: appends are rare, staleness here is a lie.
  _post() { # _post <state> <detail>
    _bus_refresh
    case "$bus_state" in
    done | failed | exited) exit 0 ;;
    esac
    mkdir -p "$dir"
    jq -nc --arg crew "$crew" --arg from "$me" --arg state "$1" --arg detail "$2" \
      '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
          kind:"status", body:{state:$state, detail:$detail, source:"watchdog"}}' >>"$log"
  }

  # INV-W3 — one open watchdog episode per branch PER PREFIX, checked on the bus
  # (the only thing two watchdogs on one branch share) rather than in process
  # memory. Same-prefix, not any-prefix, on purpose: a dead pane must still be
  # able to raise `quiet:` over an open `stalled:`, because `stalled:` does not
  # escalate (C-1) and `quiet:` is then the only path that frees fan-out budget.
  _post_blocked() { # _post_blocked <prefix> <detail>; returns 1 when suppressed
    _bus_refresh
    if [ "$bus_state" = blocked ] && [ "$bus_source" = watchdog ]; then
      case "$bus_detail" in
      "$1"*) return 1 ;;
      esac
    fi
    _post blocked "$2"
  }

  # INV-W2 — a clearance never overwrites a later worker statement. `working` is
  # not in `watch`'s wake set, so this corrects the roster without a second wake.
  _post_clear() { # _post_clear <prefix>
    _bus_refresh
    if [ "$bus_state" = blocked ] && [ "$bus_source" = watchdog ]; then
      _post working "$1 cleared"
    fi
  }

  # Geometry anchor: the footer must be the pane's LAST non-empty line, with a
  # numbered option within the 6 non-empty lines above it. A pane that is not
  # parked on a prompt ends on its input box, never on transcript text (A3), so
  # a prompt frame merely scrolling through — this very repo's bats fixtures —
  # cannot satisfy this. Relaxing it to "the last 10 lines" is exactly how those
  # fixtures become a false-positive source.
  _is_prompt() {
    local tail_n last above
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -7 || true)
    last=$(printf '%s\n' "$tail_n" | tail -1)
    case "$last" in
    *"Enter to select"* | *"Enter to confirm"*) ;;
    *) return 1 ;;
    esac
    above=$(printf '%s\n' "$tail_n" | sed '$d')
    printf '%s\n' "$above" | grep -qE "$re_option"
  }
  _meter_line() { printf '%s\n' "$1" | grep -E "$re_meter" | tail -1 || true; }
  _has_subrow() { printf '%s\n' "$1" | grep -qE "$re_subrow"; }

  start=$(date +%s)
  sleep "$grace"
  fails=0
  last_hash=""
  last_change=$(date +%s)
  tick=0
  d0_at=0
  d1_hits=0
  d1_at=0
  d2_tok=""
  d2_clock=""
  d2_since=0
  d2_moved=0
  d2_at=0
  d3_at=0
  _bus_refresh
  while :; do
    now=$(date +%s)
    # --max-life exists because the watchdog is nohup-detached: without a hard
    # cap, a bug or an orphaned pane leaves a process polling forever.
    [ $((now - start)) -ge "$max_life" ] && exit 0
    # Exit on a terminal state — but NOT on pr_open: a worker in pr_open is
    # still working (#31's own prompt was rendered by a worker watching PR CI),
    # and not on `working`, which is a heartbeat.
    case "$bus_state" in
    done | failed | exited) exit 0 ;;
    esac

    if ! text=$(_sample); then
      # Pane-gone is a quorum, not a single failure: at a 12h lifetime one tmux
      # hiccup would otherwise disarm liveness for the rest of the run. Three
      # consecutive failures ≈45s at the default interval, and the `exited`
      # backstop owns the real case anyway.
      fails=$((fails + 1))
      [ "$fails" -ge 3 ] && exit 0
      sleep "$interval"
      tick=$((tick + 1))
      continue
    fi
    fails=0
    hash=$(printf '%s' "$text" | cksum)
    if [ "$hash" != "$last_hash" ]; then
      last_hash="$hash"
      last_change="$now"
    fi

    # While the worker's own latest word is a SELF-reported `blocked` it is in
    # `crew await` — the dispatcher is already awake about it, and a held await
    # leaves a static pane by construction (a D3 false positive waiting).
    suppressed=0
    if [ "$bus_state" = blocked ] && [ "$bus_source" != watchdog ]; then
      suppressed=1
    fi
    quiet_for=$((now - last_change))

    # ---- D1: interactive prompt ------------------------------------------
    # Presence, not transition: the workspace-trust frame is on screen from the
    # worker's FIRST sample, so an "appeared" conjunct could never fire on the
    # only frame with measured production occurrences.
    if [ "$suppressed" = 0 ] && [ "$sig_prompt" = 1 ] &&
      _is_prompt "$text" && [ -z "$(_meter_line "$text")" ]; then
      d1_hits=$((d1_hits + 1))
      if [ "$d1_hits" -ge 2 ] && [ "$d1_at" = 0 ]; then
        if _post_blocked "prompt:" "prompt: interactive prompt in pane $pane — worker is waiting on input nobody can give"; then
          d1_at="$now"
        fi
      fi
    else
      if [ "$d1_at" != 0 ]; then
        _post_clear "prompt:"
        d1_at=0
      fi
      d1_hits=0
    fi

    # ---- D2: dead turn ----------------------------------------------------
    # Strings, never numbers: rounding to 0.1k and multi-unit durations make
    # arithmetic fragile and every parse failure a new branch.
    if [ "$suppressed" = 0 ] && [ "$sig_meter" = 1 ]; then
      m=$(_meter_line "$text")
      if [ -z "$m" ] || _has_subrow "$text"; then
        # A live subagent row is an UNCONDITIONAL veto: a healthy deep worker in
        # a subagent batch reproduces D2's exact signature — meter present,
        # clock rising, token string static — for minutes at a stretch (A1,
        # measured). The cost is a stated blind spot: a turn that dies with a
        # row still painted is invisible to D2.
        if [ "$d2_at" != 0 ]; then
          _post_clear "turn-stall:"
          d2_at=0
        fi
        d2_since=0
        d2_tok=""
        d2_clock=""
        d2_moved=0
      else
        clock=$(printf '%s' "$m" | sed -E 's/^[^(]*\(([^·]*)·.*/\1/')
        tok=$(printf '%s' "$m" | sed -E 's/.*↓[[:space:]]*([0-9.]+k?)[[:space:]]tokens.*/\1/')
        if [ "$d2_since" = 0 ] || [ "$tok" != "$d2_tok" ]; then
          if [ "$d2_at" != 0 ]; then
            _post_clear "turn-stall:"
            d2_at=0
          fi
          d2_tok="$tok"
          d2_clock="$clock"
          d2_since="$now"
          d2_moved=0
        elif [ "$clock" != "$d2_clock" ]; then
          # A rising clock proves the capture is a LIVE frame. A static clock
          # means a frozen renderer or copy-mode scrollback — evidence we cannot
          # trust, so the rule stays silent.
          d2_clock="$clock"
          d2_moved=1
        fi
        # Any status event from the worker in the window is a sign of life.
        if [ "$bus_source" != watchdog ] && [ "$bus_ts" -ge $((d2_since * 1000)) ]; then
          d2_since="$now"
          d2_moved=0
        fi
        if [ "$d2_at" = 0 ] && [ "$d2_moved" = 1 ] && [ $((now - d2_since)) -ge "$idle" ]; then
          if _post_blocked "turn-stall:" "turn-stall: token count static at $d2_tok for $((now - d2_since))s while the pane clock advanced"; then
            d2_at="$now"
          fi
        fi
      fi
    fi

    # ---- D3: quiet pane ---------------------------------------------------
    # Byte-identity, not "no meter": a healthy claude pane repaints its spinner
    # every second, so a working worker can never satisfy D3 even if every
    # claude signature rots to garbage. This is the failsafe for signature rot
    # and the only steady-state coverage codex and cursor get.
    if [ "$suppressed" = 0 ] && [ "$d3_at" = 0 ] && [ "$quiet_for" -ge "$idle" ]; then
      if [ "$bus_source" = watchdog ] || [ "$bus_ts" -lt $((last_change * 1000)) ]; then
        if _post_blocked "quiet:" "quiet: pane unchanged for ${quiet_for}s"; then
          d3_at="$now"
        fi
      fi
    fi
    if [ "$d3_at" != 0 ] && [ "$quiet_for" -lt "$idle" ]; then
      _post_clear "quiet:"
      d3_at=0
    fi

    # ---- D0: startup silence ----------------------------------------------
    # Classify BEFORE judging. A static pane that is a prompt belongs to D1 and
    # D0 says nothing about it — that one negative check is the session-3 fix.
    # The detail carries no diagnosis: the old `(suspected startup/indexing
    # hang)` was wrong on 3/3 measured workers, and an invented cause reads to
    # the dispatcher as corroboration.
    if [ "$suppressed" = 0 ] && [ "$d0_at" = 0 ] &&
      [ $((now - start)) -lt "$window" ] && [ "$quiet_for" -ge "$stall" ]; then
      case "$bus_state" in
      "" | working)
        if [ "$sig_prompt" = 1 ] && _is_prompt "$text"; then
          : # D1 owns this frame
        elif _post_blocked "stalled:" "stalled: no output for ${stall}s"; then
          d0_at="$now"
        fi
        ;;
      esac
    fi

    # ---- Escalation --------------------------------------------------------
    # The only path to `failed`, and it is a SECOND, later, independent evidence
    # check — not a timer. `prompt:` is exempt (C-1): a frame still on screen
    # means nobody answered yet, and escalating it would reproduce session 3
    # with a 30-minute delay. `stalled:` is exempt too — it is the branch that
    # catches the classifier's misses, so it must stay recoverable; a genuinely
    # dead pane reaches `failed` through the `quiet:` episode instead.
    if [ "$d2_at" != 0 ] && [ $((now - d2_at)) -ge "$dead" ]; then
      _post failed "dead: turn-stall: unchanged for $((now - d2_at))s"
      exit 0
    fi
    if [ "$d3_at" != 0 ] && [ $((now - d3_at)) -ge "$dead" ]; then
      _post failed "dead: quiet: unchanged for $((now - d3_at))s"
      exit 0
    fi

    sleep "$interval"
    tick=$((tick + 1))
    # Bus-read cadence: a whole-file jq over a growing cross-crew log every tick
    # for 12h is ~2880 spawns per worker. Every 4th tick costs ≤60s of latency
    # against an 1800s threshold. _post's pre-write read is exempt.
    if [ $((tick % 4)) -eq 0 ]; then
      _bus_refresh
    fi
  done
  ;;
```

- [ ] **Step 3: Lint**

Run: `nix develop -c shellcheck adapters/core/crew.sh`

Expected: no output, exit 0. If SC2317 (unreachable) or SC2329 (unused function) appears for a helper, it means the helper is genuinely never called — fix the call site, do **not** add a disable directive.

- [ ] **Step 4: Run the stall-watch suite**

Run: `nix develop -c bats tests/crew.bats`

Expected: **all** tests pass, including the 30 pre-existing ones. If a `set -e` surprise kills the loop early (a run producing zero events where one is expected, exit status 0, and no other diagnosis), audit for an `if`/`else` branch or function whose last command is a failing `[ … ] && …` list — that is the one bash trap this arm is exposed to.

- [ ] **Step 5: Verify the whole suite**

Run: `nix develop -c bats tests/`

Expected: `tests/crew.bats`, `tests/dispatch.bats` and `tests/adapters.bats` all green. Nothing outside `crew.bats`/`crew.sh` changed, so any failure elsewhere is another worker's in-flight change — check `git status` before assuming it is yours.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): lifetime-scoped stall-watch with prompt and dead-turn detectors"
```

---

### Task 3: `roster` carries `detail` and `source`

**Model:** default (sonnet).

**Files:**

- Modify: `adapters/core/crew.sh` — the `roster)` arm's `base=$(jq …)` block
- Modify: `tests/crew.bats` (append one test)

**Sequencing:** strictly after Task 2 (same two files). Not parallel-safe with Tasks 2 or 4.

- [ ] **Step 1: Add the two fields**

In the `roster)` arm, inside the `map(… (max_by(.ts)) as $latest | { … })` object, insert after the `state:` line:

```jq
             # `detail` is unbounded free text and the roster is an LLM-read
             # dashboard, so an untruncated row can crowd out the table. 120
             # fits every reserved prefix plus its payload; the full string
             # stays in the log. `source` tells a watchdog-posted `blocked`
             # (nobody is listening) from a worker's own (someone is in await).
             detail: (($latest.body.detail // null) | if . == null then null else .[0:120] end),
             source: ($latest.body.source // null),
```

The surrounding object is:

```jq
          | {from: $latest.from,
             state: $latest.body.state,
             detail: (($latest.body.detail // null) | if . == null then null else .[0:120] end),
             source: ($latest.body.source // null),
             title: ($titles[$latest.from] // null),
```

- [ ] **Step 2: Append the test to `tests/crew.bats`**

```bash
@test "roster: carries source and truncates detail to 120 chars" {
  long=$(printf 'quiet: %0.sx' $(seq 1 200))
  seed_raw worker:feat/x blocked "$long" watchdog
  CREW_ID=c1 run run_crew roster c1
  [ "$status" -eq 0 ]
  run bash -c "printf '%s' '$output' | jq -r '.[0] | \"\(.source)|\(.detail|length)\"'"
  [ "$output" = "watchdog|120" ]
}

@test "roster: a worker-posted row has a null source" {
  CREW_ID=c1 run_crew status worker:feat/x blocked "which approach?"
  CREW_ID=c1 run run_crew roster c1
  run bash -c "printf '%s' '$output' | jq -r '.[0] | \"\(.source)|\(.detail)\"'"
  [ "$output" = "null|which approach?" ]
}
```

- [ ] **Step 3: Verify**

Run: `nix develop -c bats tests/crew.bats && nix develop -c shellcheck adapters/core/crew.sh`

Expected: all green, shellcheck silent.

- [ ] **Step 4: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): roster carries detail and source per row"
```

---

### Task 4: `rate` splits watchdog blocks out of `blocked_count`

**Model:** default (sonnet).

**Files:**

- Modify: `adapters/core/crew.sh` — the `rate)` arm's `records=$(jq …)` block
- Modify: `tests/crew.bats` (append one test)

**Sequencing:** strictly after Task 3 (same two files).

- [ ] **Step 1: Narrow `$blocked` and add `$wblocked`**

Replace this line in the `rate)` jq program:

```jq
        | ($st | map(select(.body.state=="blocked")) | length) as $blocked
```

with:

```jq
        # Split rather than replace. `blocked_count` feeds an append-only,
        # cross-run ratings store whose rows are compared against rows written
        # before the watchdog existed; changing that field's POPULATION in place
        # would make old and new rows silently non-comparable in the same field.
        # A new field leaves historical rows simply absent (null), which every
        # reader there already tolerates — and watchdog_blocked_count is direct
        # evidence of how often a model/tier wedges.
        | ($st | map(select(.body.state=="blocked" and (.body.source // "") != "watchdog")) | length) as $blocked
        | ($st | map(select(.body.state=="blocked" and (.body.source // "") == "watchdog")) | length) as $wblocked
```

- [ ] **Step 2: Emit the new field**

Replace:

```jq
            blocked_count: $blocked,
```

with:

```jq
            blocked_count: $blocked,
            watchdog_blocked_count: $wblocked,
```

- [ ] **Step 3: Append the test to `tests/crew.bats`**

```bash
@test "rate: blocked_count excludes watchdog blocks, watchdog_blocked_count counts them" {
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc '{ts:1,crew_id:"c1",kind:"dispatch",branch:"feat/x",engine:"claude",
           model:"opus",tier:"deep",effort:"high",title:"t"}' >>"$logf"
  seed_raw worker:feat/x blocked "which approach?" "" 2
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %9 — waiting" watchdog 3
  seed_raw worker:feat/x blocked "quiet: pane unchanged for 1800s" watchdog 4
  store="$BATS_TEST_TMPDIR/xdg"
  XDG_DATA_HOME="$store" CREW_ID=c1 run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '"\(.blocked_count)|\(.watchdog_blocked_count)"' "$store/crew/ratings.jsonl"
  [ "$output" = "1|2" ]
}
```

- [ ] **Step 4: Verify**

Run: `nix develop -c bats tests/crew.bats && nix develop -c shellcheck adapters/core/crew.sh`

Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): rate splits watchdog_blocked_count out of blocked_count"
```

---

### Task 5: Pass the engine to the watchdog (`dispatch.sh`, two tokens)

**Model:** default (sonnet).

**Files:**

- Modify: `adapters/core/dispatch.sh` — the **last line** of the file only

**Sequencing:** parallel-safe. Touches a file no other task in this plan touches. **Do not touch anything else in `dispatch.sh`, and never the `--pr` path** — another live worker owns that file.

- [ ] **Step 1: Locate the spawn line**

```bash
rg -n 'crew stall-watch' adapters/core/dispatch.sh
```

Expected: exactly one hit, the final line of the file.

- [ ] **Step 2: Add `--engine "$agent"`**

Replace:

```bash
CREW_ID="$crew_id" nohup crew stall-watch "$branch" --pane "$pane" >/dev/null 2>&1 &
```

with:

```bash
CREW_ID="$crew_id" nohup crew stall-watch "$branch" --pane "$pane" --engine "$agent" >/dev/null 2>&1 &
```

The engine cannot be inferred from tmux — `pane_current_command` reads `.claude-wrapped`, not `claude` — so it has to be passed. A missing `--engine` resolves to `unknown` and takes the conservative row, so the flag's absence can never _enable_ a detector.

- [ ] **Step 3: Verify the diff is exactly two tokens**

```bash
git diff --stat adapters/core/dispatch.sh
git diff adapters/core/dispatch.sh
```

Expected: `1 file changed, 1 insertion(+), 1 deletion(-)`, and the diff body differs only by `--engine "$agent"`. If more than one line changed, revert and redo.

- [ ] **Step 4: Lint and test**

Run: `nix develop -c shellcheck adapters/core/dispatch.sh && nix develop -c bats tests/dispatch.bats`

Expected: shellcheck silent, dispatch suite green.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/dispatch.sh
git commit -m "feat(dispatch): pass the engine to the stall watchdog"
```

---

### Task 6: Protocol edits (source of truth only)

**Model:** default (sonnet). Doc work is not high-risk.

**Files:**

- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md` — the "Report to the bus (mandatory)" section
- Modify: `adapters/core/protocols/DISPATCHER_PROTOCOL.md` — the `stalled:` liveness paragraph, plus one clause on the `crew reply` bullet

**Sequencing:** parallel-safe with Task 5 and with the 1→4 chain. **Must run before Task 7.** Never hand-edit `adapters/claude-code/**`, `adapters/codex/**` or `adapters/cursor/**` — Task 7 regenerates them.

- [ ] **Step 1: Extend the worker's bus contract**

In `WORKER_PROTOCOL.md`, find the "Report to the bus (mandatory)" list (`rg -n 'on terminal failure' adapters/core/protocols/WORKER_PROTOCOL.md`). Insert these bullets immediately after the `on terminal failure:` bullet and before `## Rules`:

```markdown
- **Never use an interactive question tool** (Claude Code's `AskUserQuestion`, or any
  engine's option-select prompt). **Nobody watches your pane** — a rendered prompt is
  invisible to the bus, and it waits for input that can never arrive. The only ask-path
  is the `crew status blocked` + `crew msg` + `crew await` sequence above; it is
  durable, it wakes the dispatcher, and it resumes you in place.
- **Heartbeat at the seams.** Re-stamp
  `crew status "worker:$(git branch --show-current)" working "<stage>"` at each pipeline
  seam the checkpoint peek already defines. It costs nothing, it does **not** wake the
  dispatcher (`crew watch` ignores `working`), it keeps `roster`'s `age_s` meaning "time
  since last sign of life", and it damps the liveness watchdog below. Its limit, stated
  so you don't rely on it: a seam heartbeat **cannot** fire from inside a long tool call,
  so it does not protect a subagent batch — the watchdog's own conjuncts do.
- **A watchdog may post on your behalf.** `dispatch` spawns `crew stall-watch` per
  worker; it samples your pane and can append `blocked` with `body.source:"watchdog"`
  and a reserved `detail` prefix (`prompt:`, `turn-stall:`, `quiet:`, `stalled:`), or
  `failed` with a `dead:` prefix when the same evidence still holds 30 minutes later. It
  never posts a `msg` and never answers a prompt for you. If you find a watchdog
  `blocked` in your own history, you are by definition alive: re-stamp
  `crew status worker:<branch> working` and carry on — no reply is owed, and none is
  waiting for you in `crew await`.
- **Two things a fresh worktree does to you.** Claude Code may draw its workspace-trust
  question (`Quick safety check: Is this a project you created or one you trust?`) before
  anything else runs — nothing proceeds until it is answered, and it is answered at the
  pane, not by you. And `.envrc` may come up blocked
  (`direnv: error .envrc is blocked. Run `direnv allow``), which leaves the pane with no
devshell — no `bats`, no `yq-go`, no `jq`. If your tools are missing, run
`direnv allow` in the worktree root before concluding anything is broken.
```

- [ ] **Step 2: Rewrite the dispatcher's liveness paragraph**

In `DISPATCHER_PROTOCOL.md`, find the paragraph beginning "A `failed` whose detail starts with `stalled:`" (`rg -n 'stalled:' adapters/core/protocols/DISPATCHER_PROTOCOL.md`). Replace **that paragraph and the one after it** ("It infers liveness from **pane output**…" through "…not a wedge.") with:

```markdown
A `status` carrying `body.source: "watchdog"` was posted **on the worker's behalf** by
the per-worker liveness watchdog (`crew stall-watch`, spawned by `dispatch`), not
self-reported. Its `detail` always begins with one of five reserved prefixes:

- `prompt:` — the pane is parked on an interactive prompt (commonly the workspace-trust
  question a fresh worktree draws). Answer it **in the pane**; the worker resumes and the
  watchdog clears the state itself. This never escalates: an unanswered answerable
  question is waiting work, not a dead worker.
- `turn-stall:` — the pane's clock advanced for 30 min against a static token count with
  no live subagent row. A dead turn.
- `quiet:` — the pane has been byte-identical for 30 min.
- `stalled:` — a static pane inside the startup window whose frame the watchdog could
  **not** classify. Deliberately its weakest claim: an unrecognised prompt family, a
  shell waiting on `direnv allow`, and a dead process all arrive under this prefix.
- `dead:` — a `turn-stall:`/`quiet:` episode whose evidence still held a further 30 min.
  This is the **only** watchdog `failed`.

**Every watchdog state except `dead:` is `blocked`, not `failed`.** This replaces the old
rule that a `stalled:` `failed` was a recovery trigger — that instruction, followed
literally, would have killed three healthy workers parked on a trust prompt. Recovery is
**verify, then act**:

1. `tmux capture-pane -p -t %<id>` on the pane named in the `detail`. **Always** — the
   `detail` exists to make this one command possible.
2. The pane confirms a prompt → answer it in place.
3. The pane confirms a dead turn or a dead pane → kill the window, then re-dispatch.
4. The pane shows work in flight (a live meter, advancing subagent rows) → it is a
   **false positive. Do not kill.** Post nothing; the watchdog clears itself on the next
   sample. The glance **is** the guard: "kill and re-dispatch" as an unconditional
   instruction turns every false positive into destroyed work.

Pane scraping only tells the truth for an engine that streams, and only claude has
verified frame signatures. **codex and cursor get liveness coverage, not prompt
coverage** — `stalled:` and `quiet:` only, by decision, until someone pastes a real
capture of their frames.
```

- [ ] **Step 3: Correct the `crew reply` bullet**

Find the bullet beginning "A worker that's `blocked` has posted its question". Append this sentence to the end of that bullet (do not restructure the rest of it):

```markdown
**This applies only to a worker's own `blocked`.** A `blocked` carrying
`source: "watchdog"` has no question behind it and nobody in `crew await` — `crew reply`
there is a no-op that looks like an answer. Go to the pane instead (verify, then act,
above).
```

- [ ] **Step 4: Verify the edits are confined**

```bash
git diff --stat adapters/core/protocols/
```

Expected: exactly two files changed, both under `adapters/core/protocols/`. Nothing under `adapters/claude-code/`, `adapters/codex/` or `adapters/cursor/` — those are Task 7's output, and touching them by hand is a drift failure.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/protocols/WORKER_PROTOCOL.md adapters/core/protocols/DISPATCHER_PROTOCOL.md
git commit -m "docs(protocol): ban the interactive question tool and document the watchdog contract"
```

---

### Task 7: Regenerate the adapter trees

**Model:** default (sonnet).

**Files:**

- Run (never edit): `scripts/gen-adapters.sh`
- Generated output: `adapters/claude-code/**`, `adapters/codex/**`, `adapters/cursor/**`

**Sequencing:** strictly after Task 6. Independent of Tasks 1–5 (they touch no projected file — `crew.sh` and `dispatch.sh` are not projected; only `adapters/core/commands/`, `adapters/core/protocols/` and `dispatch-notify.sh` are).

- [ ] **Step 1: Regenerate**

Run: `nix develop -c ./scripts/gen-adapters.sh`

Expected: exit 0.

- [ ] **Step 2: Confirm the only changes are projections of Task 6**

```bash
git status --short adapters/
```

Expected: modified files under `adapters/claude-code/plugin/protocols/` and `adapters/codex/plugin/protocols/` mirroring the two protocol files. If a `commands/` file or the skill body shows up, something outside this plan's scope drifted — stop and check with the dispatcher before committing.

- [ ] **Step 3: Verify the drift gate is clean**

```bash
nix develop -c ./scripts/gen-adapters.sh
git diff --exit-code adapters/claude-code adapters/codex adapters/cursor
```

Expected: exit 0 with no output after committing in Step 4 — this is the exact assertion CI runs. (Before the commit it will print the pending diff; that is fine.)

- [ ] **Step 4: Commit**

```bash
git add adapters/claude-code adapters/codex adapters/cursor
git commit -m "chore(adapters): regenerate protocol trees"
```

---

### Task 8: Full gate

**Model:** default (sonnet).

**Files:** none — verification only.

**Sequencing:** last. Runs after every other task.

- [ ] **Step 1: shellcheck the changed shell**

Run: `nix develop -c shellcheck adapters/core/*.sh scripts/*.sh`

Expected: no output, exit 0. (This is CI's exact invocation.)

- [ ] **Step 2: Full bats suite**

Run: `nix develop -c bats tests/`

Expected: every file green. The stall-watch tests should complete in well under two minutes total — if one hangs, it is a `--max-life` that never trips, not a slow machine.

- [ ] **Step 3: Adapter drift**

```bash
nix develop -c ./scripts/gen-adapters.sh
git diff --exit-code
```

Expected: exit 0, no output.

- [ ] **Step 4: Flake check**

Run: `nix flake check`

Expected: exit 0 (treefmt + shellcheck + the suites). A treefmt complaint on the markdown means prettier wants a reflow — apply it and amend the protocol commit.

- [ ] **Step 5: Confirm the scope guard held**

```bash
git diff --stat extract...HEAD
```

Expected files, and no others: `adapters/core/crew.sh`, `tests/crew.bats`, `adapters/core/dispatch.sh` (1 insertion / 1 deletion), `adapters/core/protocols/WORKER_PROTOCOL.md`, `adapters/core/protocols/DISPATCHER_PROTOCOL.md`, `docs/superpowers/plans/2026-08-10-worker-liveness-detection.md`, `docs/superpowers/specs/2026-08-10-worker-liveness-detection-design.md`, and the generated `adapters/{claude-code,codex}/plugin/protocols/*`. If `scripts/gen-adapters.sh`, `tests/adapters.bats`, `tests/dispatch.bats` or the skill body appears, it is another worker's file — do not include it.

---

## PR body checklist (not a task, but required by the acceptance criteria)

The PR must carry, verbatim:

- `Closes #31`
- **`## Escalated`** — the spec's post-cap critic resolutions C-1…C-6, surfaced rather than silently dropped, plus this plan's three derived decisions: `stalled:` does not escalate (from C-1), INV-W3 stays same-prefix so `quiet:` can still free fan-out budget, and D0p is deleted (C-2).
- The **`dispatch.sh` callout**: two tokens (`--engine "$agent"`) on the stall-watch spawn line, the `--pr` path untouched.
- **A2 is a documented false-negative risk, not a ship gate** (C-4): the ≥1 h meter rendering is still uncaptured, so an hours-form meter that differs from the reconstructed `Considering… (1h 20m · ↓ 28.0k tokens)` would make D2 quiet — never eager.
- The **direnv callout** (§11): ``direnv: error .envrc is blocked. Run `direnv allow` `` was present in 3/3 session-3 worktrees. No detector here; whoever owns `dispatch`'s worktree setup has the evidence without re-deriving it.
- The **#32 note**: this change makes #32 mildly easier (`body.source` gives every reader a way to tell machine-posted states from worker-posted ones) and touches `reap`'s candidate filter not at all.
- **D2's stated blind spot**: a turn that dies with a subagent row still painted is invisible to D2, accepted because the alternative flips healthy deep workers.

## Spec-coverage map

| Spec acceptance criterion                                                       | Task / test                                                                                                 |
| ------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| 1 — prompt reaches the bus as `blocked` ≤75 s, first-sample case included       | T1 D1 option-select, D1 trust frame                                                                         |
| 2 — dead turn reaches the bus as `blocked`                                      | T1 D2 turn-stall                                                                                            |
| 3a–3f — six healthy-worker negatives, zero events                               | T1 negatives: subagent batch, LC_ALL=C, rising tokens, heartbeat damping, self-reported blocked, scrollback |
| 4 — footer in scrollback fires nothing, both footers                            | T1 `prompt frame in scrollback`                                                                             |
| 4a — session-3 regression: `prompt:`, never `failed`, never `stalled:`          | T1 `session-3 regression`                                                                                   |
| 4b — trust frame fires D1 with `--window 0`                                     | T1 `outside the startup window`                                                                             |
| 5 — clearance re-stamp, re-arm, one post per episode                            | T1 `one post per episode`                                                                                   |
| 6 — INV-W1 / INV-W2                                                             | T1 INV-W1 test; INV-W2 exercised by the clearance tests                                                     |
| 6a — INV-W0 in both directions, bare `from`, one roster row                     | T1 INV-W0 a/b/c/d                                                                                           |
| 6b — INV-W3                                                                     | T1 INV-W3                                                                                                   |
| 7 — escalation after `--dead`, cancelled by clearance (C-1 narrows the origins) | T1 escalation, clearance-cancels, prompt-never-escalates                                                    |
| 8 — `--engine codex` gets D0s/D3 only, never `failed`                           | T1 codex, missing-engine                                                                                    |
| 9 — 3-failure sample quorum                                                     | T1 `survives 2 sample failures`                                                                             |
| 10 — exits on terminal, not on `pr_open`/`working`                              | T1 `pr_open does not exit`, INV-W1                                                                          |
| 11 — zero `msg` events, inbox unaffected                                        | T1 `writes zero msg events`                                                                                 |
| 12 — roster `detail` ≤120 + `source`                                            | Task 3                                                                                                      |
| 13 — `rate` split                                                               | Task 4                                                                                                      |
| 14 — protocols regenerated, no drift                                            | Tasks 6, 7, 8 step 3                                                                                        |
| 15 — bats / flake / shellcheck green                                            | Task 8                                                                                                      |
| C-3 — previous-run terminal state does not mute                                 | T1 `C-3`                                                                                                    |
| C-5 — multibyte-safe EREs under the suite locale                                | T1 `LC_ALL=C`                                                                                               |
| C-6 — `crew reply` clause                                                       | Task 6 step 3                                                                                               |
