# dispatch model gate — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `dispatch` rejects a `<model>` the chosen `--agent` cannot run, before any side effect, per the accepted `SPEC.md`.

**Architecture:** One new pre-side-effect block in `adapters/core/dispatch.sh`, inserted between the profile gate and the `--effort ultra` gate. It is a `case "$agent"` with three shape-checking arms (claude / codex / cursor), wrapped in a `DISPATCH_SKIP_MODEL_CHECK` bypass whose truthiness is exact string equality with `$model`. Codex additionally cross-checks `$HOME/.codex/models_cache.json` when that file is usable. Everything else is docs and tests.

**Tech Stack:** bash 5 (`[[ =~ ]]`, `BASH_REMATCH`), `jq`, bats, shellcheck, nix flake checks.

## Global Constraints

- **`adapters/claude-code/plugin/protocols/` and `adapters/codex/plugin/protocols/` are GENERATED.** Never hand-edit them. They are `rm -rf`'d and re-copied from `adapters/core/protocols/` by `scripts/gen-adapters.sh`. Edit the core file, then run the script. CI regenerates and asserts no diff; `tests/adapters.bats` asserts idempotence.
- **`adapters/core/dispatch.sh` is excluded from treefmt** (`flake.nix` `settings.global.excludes`). shfmt will not tidy it — match the file's existing style by hand: 2-space indent, `case` labels at the same column as `case`, `>&2` on the `echo`, `exit 1` on its own line.
- **Markdown under `adapters/` is excluded from prettier** (`flake.nix`, prettier `excludes = ["\\.bats$" "^adapters/"]`). Hand-wrap protocol prose at ~80 columns to match the surrounding lines; do not reflow paragraphs you are not editing.
- **`dispatch.sh` is a function body**, not a standalone script: `#!/usr/bin/env bash` and `set -euo pipefail` are prepended by `writeShellApplication`. Line 1 is `# shellcheck shell=bash`. There is no enclosing function, so `return` is unavailable — every rejection is `echo … >&2` then `exit 1`.
- **A bare `[[ ]]` as the last statement of a branch returns 1 under `set -e` and kills dispatch silently.** Every arm is an explicit `if <reject condition>; then echo …; exit 1; fi`.
- **Paths are `"$HOME/.codex/models_cache.json"`, never `~/…`** — a tilde does not expand inside a quoted string, and the tests override `$HOME`.
- **Every rejection message ends with** `See dispatch-orchestration.md "Model gate".`
- **Comments explain non-obvious WHY only.** No restating code, no ticket/issue references, no "added for X".
- **`shellcheck adapters/core/*.sh scripts/*.sh` must be clean** after every task that touches shell.

**Model tagging:** no task in this plan is tagged `(implement: opus)`. There is no concurrency, no security logic, and no wide-blast refactor here — the two fiddly parts (the cursor regex and the two-step jq probe) are pinned by explicit bats rows, and the one launch-path edit (single-quoting `$model`) is a two-character change with an assertion on it. All tasks are sonnet.

**Ordering rationale:** Task 1 (harness hermeticity) lands first because _every_ acceptance row below fails without it — verified, not theorised. After that, each engine arm ships **with its own tests in the same task** rather than tests-then-code across task boundaries: a subagent that only writes tests hands the next subagent a red suite it did not author, which is worse for review than a task that is red for two steps and green at the end. Within a task, the test still goes first and is run red before the implementation.

---

## File Structure

| File                                                                             | Responsibility                                           | Tasks            |
| -------------------------------------------------------------------------------- | -------------------------------------------------------- | ---------------- |
| `tests/helpers.bash`                                                             | git isolation for every bats file                        | 1                |
| `tests/dispatch.bats`                                                            | env hermeticity + all gate assertions                    | 1, 2, 3, 4, 5, 6 |
| `adapters/core/dispatch.sh`                                                      | the gate; cursor launch-string quoting                   | 2, 3, 4, 5       |
| `adapters/core/protocols/dispatch-orchestration.md`                              | canonical "Model gate" doc + reworded line 81            | 7                |
| `adapters/core/protocols/DISPATCHER_PROTOCOL.md`                                 | the "Engine." bullet becomes an enforced rule            | 7                |
| `adapters/claude-code/plugin/protocols/**`, `adapters/codex/plugin/protocols/**` | **GENERATED** — output of `scripts/gen-adapters.sh` only | 7                |

---

### Task 1: Harness hermeticity

Without this the suite's result depends on whose machine runs it, and every exit-0 row below dies at status 128 in `stub_launch_bins`. `GIT_CONFIG_GLOBAL` goes in `setup_repo` so all four bats files get it, not just `dispatch.bats`.

**Files:**

- Modify: `tests/helpers.bash:4-11` (`setup_repo`)
- Modify: `tests/dispatch.bats:1-11` (`setup`)

**Interfaces:**

- Produces: `setup_repo` additionally exports `GIT_CONFIG_GLOBAL=/dev/null`. `dispatch.bats`'s `setup()` additionally exports `HOME="$TEST_REPO"` and unsets six inherited variables. Every later task assumes `$HOME` is the throwaway repo and that no `DISPATCH_*`/`CREW_ID` leaks in.

- [ ] **Step 1: Reproduce the failure `GIT_CONFIG_GLOBAL` prevents**

Overriding `HOME` alone does not detach git from the developer's global config — `XDG_CONFIG_HOME` is exported in a real shell, so git still reads `~/.config/git/config`, whose `commit.gpgSign = true` and tilde-relative `user.signingkey` re-expand against the _new_ `HOME`. Show it directly:

```bash
T=$(mktemp -d)
cd "$T" && git init -q -b main . && git config user.email t@e.com && git config user.name t
HOME=$T git commit --allow-empty -q -m init; echo "exit=$?"
```

Expected on a box with commit signing configured: `error: Couldn't load public key …: No such file or directory` and `exit=128`. That is exactly the line `stub_launch_bins` runs, so without Step 2 **every** exit-0 row in this plan dies there. Then:

```bash
HOME=$T GIT_CONFIG_GLOBAL=/dev/null git commit --allow-empty -q -m init; echo "exit=$?"
```

Expected: `exit=0`. If the _first_ command also gave `exit=0`, your box simply has no commit
signing configured — apply Step 2 anyway; CI and other developers' boxes do.

Clean up with `gtrash put "$T"` and return to the repo root.

- [ ] **Step 2: Isolate git in `setup_repo`**

Replace `tests/helpers.bash:4-11` with:

```bash
setup_repo() {
  TEST_REPO="$(mktemp -d)"
  cd "$TEST_REPO" || return 1
  # Overriding $HOME is not enough: $XDG_CONFIG_HOME survives it, so git still
  # reads ~/.config/git/config, whose commit.gpgSign and tilde-relative
  # signingkey then re-expand against the new HOME and kill every commit.
  export GIT_CONFIG_GLOBAL=/dev/null
  git init -q -b main .
  git config user.email test@example.com
  git config user.name test
  export TEST_REPO
}
```

- [ ] **Step 3: Make `dispatch.bats` hermetic**

Replace `tests/dispatch.bats:1-11` with:

```bash
setup() {
  load helpers
  DISPATCH="$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
  run_dispatch() { bash -euo pipefail "$DISPATCH" "$@"; }
  setup_repo
  # A work shell exports DISPATCH_PROFILE and any dispatcher session exports
  # CREW_ID; bats inherits both, so without this the suite passes on a work box
  # and fails on a personal one. HOME points at the throwaway repo so the codex
  # cache fixture and the --mcp config paths cannot reach the developer's own.
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE
  stub_bin tmux
  stub_bin crew
  stub_bin gh
  stub_bin wt
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
}
```

Order matters: `export HOME` must come **after** `setup_repo`, which is what creates `$TEST_REPO`.

- [ ] **Step 4: Verify the suite is green and now machine-independent**

```bash
bats tests/
```

Expected: all tests pass.

```bash
DISPATCH_PROFILE=personal CREW_ID=leaked TMUX_PANE=%99 bats tests/dispatch.bats
```

Expected: identical result — all pass.

Do **not** claim this proves the `unset` line. At this point it does not: every profile-sensitive
row sets `DISPATCH_PROFILE` inline, every row passes `--crew-id c1`, and `TMUX_PANE` /
`DISPATCH_SPEC` / `DISPATCH_SHAPE` reach no assertion — removing the `unset` leaves the suite
green. The one member that becomes load-bearing is `DISPATCH_SKIP_MODEL_CHECK`, and only once
Task 3/4 exist; Task 8 Step 3 is where that is actually demonstrated. The other five are declared
defence against a future row that forgets to set its own env, which is worth keeping but is not
under test.

- [ ] **Step 5: Commit**

```bash
git add tests/helpers.bash tests/dispatch.bats
git commit -m "test: isolate the bats suite from the developer's git config and env"
```

---

### Task 2: Gate skeleton, override wrapper, and the claude arm

**Files:**

- Modify: `adapters/core/dispatch.sh` — insert after the cursor profile gate's `fi` (currently line 134), before the comment `# claude's --effort tops out at max;` (currently line 135)
- Modify: `tests/dispatch.bats` — append tests

**Interfaces:**

- Produces: the shell variable `re_effort_tail` (shared with Task 5's cursor arm), the `if [ "${DISPATCH_SKIP_MODEL_CHECK:-}" = "$model" ]; then … else case "$agent" in … esac; fi` scaffold that Tasks 3 and 5 add arms to, and the claude arm.
- Consumes: Task 1's hermetic `setup()`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch.bats`:

```bash
@test "rejects a codex slug on --agent claude" {
  run run_dispatch standard kimi-k3-high --agent claude --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent claude"* ]]
}

@test "rejects an effort-suffixed cursor id on --agent claude" {
  run run_dispatch standard claude-opus-4-8-high --agent claude --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"effort-suffixed cursor id"* ]]
}

@test "the model gate outranks the effort-ultra gate" {
  # Ordering pin: the mistake is the engine/model pairing, not the effort, so
  # moving the gate below the ultra check masks it.
  run run_dispatch deep gpt-5.6-sol --agent claude --effort ultra --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent claude"* ]]
  [[ "$output" != *"effort ultra is codex-only"* ]]
}

@test "accepts claude aliases and full claude-* ids" {
  # Distinct titles are load-bearing: the title becomes the branch, and a reused
  # one makes the second `git worktree add -b` collide.
  stub_launch_bins
  run run_dispatch deep claude-fable-5 --agent claude --effort high --crew-id c1 42 "fable row"
  [ "$status" -eq 0 ]
  run run_dispatch trivial haiku --agent claude --effort low --crew-id c1 42 "haiku row"
  [ "$status" -eq 0 ]
}

@test "DISPATCH_SKIP_MODEL_CHECK bypasses the gate for that exact model" {
  stub_launch_bins
  DISPATCH_SKIP_MODEL_CHECK=kimi-k3-high run run_dispatch standard kimi-k3-high --agent claude --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" == *"model check skipped"* ]]
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
bats tests/dispatch.bats
```

Expected: the three rejection/ordering tests FAIL (status 1 expected, but dispatch currently either succeeds past the gate or reports `effort ultra is codex-only`); `DISPATCH_SKIP_MODEL_CHECK bypasses…` FAILS on the missing `model check skipped` string. `accepts claude aliases…` may already pass — that is fine, it is a regression pin.

- [ ] **Step 3: Insert the gate skeleton and claude arm**

In `adapters/core/dispatch.sh`, immediately after:

```bash
if [ "$agent" = cursor ] && [ "$profile" != work ]; then
  echo "dispatch: --agent cursor is work-profile only" >&2
  exit 1
fi
```

insert:

```bash

# Model gate. Reject a slug the chosen engine cannot run before anything is
# scaffolded — otherwise a wrong id surfaces as a 400 in a tmux pane the
# worktree, window and issue already paid for. Shape, not a model list: this
# file bakes into a store path, so a membership table would make every model
# bump a rebuild.
re_effort_tail='-(none|low|medium|high|xhigh|max)(-fast)?$'
if [ "${DISPATCH_SKIP_MODEL_CHECK:-}" = "$model" ]; then
  echo "dispatch: model check skipped (DISPATCH_SKIP_MODEL_CHECK) — '$model' on --agent $agent is unverified" >&2
else
  case "$agent" in
  claude)
    re_claude_id='^claude-[a-z0-9]+(-[a-z0-9]+)*$'
    if [[ $model =~ $re_claude_id ]] && [[ $model =~ $re_effort_tail ]]; then
      echo "dispatch: model '$model' is an effort-suffixed cursor id — on --agent claude pass the bare id and set intensity with --effort. Did you mean --agent cursor? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    if [[ ! $model =~ ^(opus|sonnet|haiku|fable)$ ]] && [[ ! $model =~ $re_claude_id ]]; then
      echo "dispatch: model '$model' does not match --agent claude — claude takes an alias (opus, sonnet, haiku, fable) or a full claude-* id (e.g. claude-fable-5). Did you mean --agent cursor? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
  esac
fi
```

Note the intentional truthiness: `DISPATCH_SKIP_MODEL_CHECK` bypasses only on **exact string equality with `$model`**. `=1` and empty take the normal path with no warning.

- [ ] **Step 4: Run the tests and shellcheck**

```bash
bats tests/dispatch.bats
shellcheck adapters/core/dispatch.sh
```

Expected: all bats tests PASS; shellcheck silent.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): gate the model slot for --agent claude"
```

---

### Task 3: The codex grammar arm

**Files:**

- Modify: `adapters/core/dispatch.sh` — add a `codex)` branch to the `case "$agent"` from Task 2
- Modify: `tests/dispatch.bats` — append tests

**Interfaces:**

- Consumes: the `case "$agent"` scaffold and the `DISPATCH_SKIP_MODEL_CHECK` wrapper from Task 2.
- Produces: the codex arm's grammar check, which Task 4 appends the cache cross-check to (inside the same `codex)` branch, after the grammar `fi`).

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch.bats`:

```bash
@test "rejects a bare gpt generation on --agent codex" {
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"there is no bare gpt-5.6"* ]]
}

@test "the model gate fires before any worktree is scaffolded" {
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  # Mirrors the profile-gate test: the gate rejects before any stub runs, so
  # $STUB_LOG may not exist at all. Non-vacuous for *this* gate because the row
  # reaches it (profile is work, crew id supplied) — move the gate below
  # dispatch.sh's `wt switch -c` and `switch` lands in the log.
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "rejects a claude alias on --agent codex" {
  DISPATCH_PROFILE=work run run_dispatch standard opus --agent codex --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent codex"* ]]
}

@test "rejects a cursor-shaped id on --agent codex" {
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol-high --agent codex --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent codex"* ]]
}

@test "accepts codex variant slugs and the legacy bare generations" {
  # Distinct titles: see the claude-alias test.
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 42 "terra row"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.5 --agent codex --effort medium --crew-id c1 42 "legacy row"
  [ "$status" -eq 0 ]
}

@test "DISPATCH_SKIP_MODEL_CHECK is exact-match, not a boolean" {
  DISPATCH_PROFILE=work DISPATCH_SKIP_MODEL_CHECK=1 run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"there is no bare gpt-5.6"* ]]
}

@test "an exported DISPATCH_SKIP_MODEL_CHECK does not blanket-disable the gate" {
  DISPATCH_PROFILE=work DISPATCH_SKIP_MODEL_CHECK=gpt-5.6 run run_dispatch standard opus --agent codex --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent codex"* ]]
}

@test "DISPATCH_SKIP_MODEL_CHECK lets its own model through to launch" {
  stub_launch_bins
  DISPATCH_PROFILE=work DISPATCH_SKIP_MODEL_CHECK=gpt-5.6 run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" == *"model check skipped"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
bats tests/dispatch.bats
```

Expected: the four rejection tests and the two exact-match override tests FAIL (dispatch currently accepts these models and proceeds).

- [ ] **Step 3: Add the codex arm**

In `adapters/core/dispatch.sh`, inside the `case "$agent" in` block, after the `claude)` branch's `;;` and before `esac`:

```bash
  codex)
    if [[ ! $model =~ ^gpt-[0-9]+\.[0-9]+-[a-z0-9]+$ ]] && [[ ! $model =~ ^gpt-5\.[45]$ ]]; then
      if [[ $model =~ ^gpt-[0-9]+\.[0-9]+$ ]]; then
        gen="${model#gpt-}"
        echo "dispatch: model '$model' is not a codex slug — the $gen family ships only as variants (gpt-$gen-sol, gpt-$gen-terra, gpt-$gen-luna); there is no bare $model. See dispatch-orchestration.md \"Model gate\"." >&2
        exit 1
      fi
      echo "dispatch: model '$model' does not match --agent codex — codex takes gpt-* variant slugs (e.g. gpt-5.6-sol). Did you mean --agent claude? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
```

Two notes for the reviewer:

- The variant suffix is **mandatory** — that, not a deny-list, is what rejects a bare `gpt-5.6`, so a bare `gpt-5.7` next quarter is rejected for the same structural reason.
- The bare-generation message is parameterised on `$gen` rather than hardcoded. For `gpt-5.6` it emits SPEC §4's literal byte-for-byte; hardcoding "5.6" would make the message wrong the first time someone types `gpt-5.7`.

- [ ] **Step 4: Run the tests and shellcheck**

```bash
bats tests/dispatch.bats
shellcheck adapters/core/dispatch.sh
```

Expected: all PASS; shellcheck silent.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): require a variant suffix on codex model slugs"
```

---

### Task 4: The codex cache cross-check

An opportunistic tightening on top of Task 3's floor. The probe and the membership test are **two separate `jq` calls on purpose**: conflated, a rotated or half-written cache would block every codex dispatch behind a file nobody edits by hand.

**Files:**

- Modify: `adapters/core/dispatch.sh` — append inside the `codex)` branch, after Task 3's grammar `fi`
- Modify: `tests/dispatch.bats` — append a fixture helper and five tests

**Interfaces:**

- Consumes: Task 3's `codex)` branch; Task 1's `HOME="$TEST_REPO"`.
- Produces: `write_codex_cache` helper in `tests/dispatch.bats` (no other task uses it).

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch.bats` — the helper first, next to `stub_launch_bins`, then the tests at the end of the file:

```bash
# The slug set this account actually has, as of writing.
write_codex_cache() {
  mkdir -p "$HOME/.codex"
  cat >"$HOME/.codex/models_cache.json" <<'EOF'
{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.6-terra"},{"slug":"codex-auto-review"},{"slug":"gpt-5.6-luna"},{"slug":"gpt-5.5"},{"slug":"gpt-5.4"},{"slug":"gpt-5.4-mini"}]}
EOF
}
```

```bash
@test "the codex cache rejects a well-shaped slug this account lacks" {
  write_codex_cache
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.7-sol --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"models_cache.json"* ]]
  [[ "$output" == *"gpt-5.6-sol"* ]]
  # The advertised list is filtered to what the grammar accepts, so it must not
  # suggest the internal review model.
  [[ "$output" != *"codex-auto-review"* ]]
}

@test "the codex cache admits a slug it holds" {
  stub_launch_bins
  write_codex_cache
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
}

@test "the grammar floor holds with no codex cache" {
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"there is no bare gpt-5.6"* ]]
}

@test "an absent codex cache is a skip, not a hard fail" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "an unparseable codex cache never blocks codex dispatch" {
  stub_launch_bins
  mkdir -p "$HOME/.codex"
  printf 'not json' >"$HOME/.codex/models_cache.json"
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
}
```

- [ ] **Step 2: Run them to verify the right one fails**

```bash
bats tests/dispatch.bats
```

Expected: `the codex cache rejects a well-shaped slug this account lacks` FAILS — but on the
`models_cache.json` assertion, not the status one. No cross-check exists yet, so the row sails
past the gate and exits 1 at `could not locate worktree for branch …` (it does not call
`stub_launch_bins`). The other four PASS already; they are the regression pins that the
cross-check must not break.

- [ ] **Step 3: Add the cross-check**

In `adapters/core/dispatch.sh`, inside the `codex)` branch, between the **outer** grammar `fi`
(the branch has two — the outer `if` and the nested bare-generation `if`) and the branch's `;;`:

```bash
    # The cache tightens the grammar and is never a prerequisite for it: probe
    # usability separately so the membership test's non-zero can only mean "not
    # on this account". Conflated, a rotated or half-written cache would block
    # every codex dispatch behind a file nobody edits by hand.
    codex_cache="$HOME/.codex/models_cache.json"
    if jq -e '.models|arrays|length > 0' "$codex_cache" >/dev/null 2>&1 &&
      ! jq -e --arg m "$model" 'any(.models[]; .slug == $m)' "$codex_cache" >/dev/null; then
      # Filtered to what the grammar accepts — the raw list advertises
      # codex-auto-review, an internal review model the gate rejects anyway.
      known="$(jq -r '[.models[].slug|select(startswith("gpt-"))]|join(", ")' "$codex_cache")"
      echo "dispatch: model '$model' is not in this account's codex model list (~/.codex/models_cache.json: $known). If it is genuinely new, set DISPATCH_SKIP_MODEL_CHECK=$model and update the model map. See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
```

`jq` is already a `runtimeInputs` entry for the `dispatch` package in `flake.nix` and is already used at `dispatch.sh:226`, so no packaging change is needed.

- [ ] **Step 4: Run the tests and shellcheck**

```bash
bats tests/dispatch.bats
shellcheck adapters/core/dispatch.sh
```

Expected: all PASS; shellcheck silent.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): cross-check codex slugs against the local models cache"
```

---

### Task 5: The cursor arm and the launch-string quoting

Cursor's id space is open by construction — it fronts Anthropic, OpenAI, Google, Moonshot and Zhipu ids — so this arm is a mismatch + missing-effort-suffix gate, never a membership gate. Blessing the bracket form obliges single-quoting `$model` in the cursor `send-keys` string, because brackets are glob-active in the worker's shell.

**Files:**

- Modify: `adapters/core/dispatch.sh` — add a `cursor)` branch; edit the cursor `tmux send-keys` string (currently line 320)
- Modify: `tests/dispatch.bats:147-155` (`cursor launch includes process authority`) + append tests

**Interfaces:**

- Consumes: `re_effort_tail` from Task 2; the `case "$agent"` scaffold.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Update the existing cursor launch assertion and write the new tests**

In `tests/dispatch.bats`, in `@test "cursor launch includes process authority"`, change the model assertion:

```bash
  [[ "$launch" == *"--model 'kimi-k3-high'"* ]]
```

(from `*'--model kimi-k3-high'*`.)

Append to `tests/dispatch.bats`:

```bash
@test "rejects a claude alias on --agent cursor" {
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent cursor"* ]]
}

@test "rejects a cursor claude-*/gpt-* id with no effort rung" {
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"effort suffix"* ]]
}

@test "a bracket block exempts the effort rule only by naming effort" {
  DISPATCH_PROFILE=work run run_dispatch standard 'gpt-5.6[detail=x]' --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"effort suffix"* ]]
}

@test "cursor accepts a no-effort-variant id and single-quotes it at launch" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard composer-2.5 --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$launch" == *"--model 'composer-2.5'"* ]]
}

@test "cursor accepts the parameterised bracket form" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep 'claude-opus-4-8[context=1m,effort=high,fast=false]' --agent cursor --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  # Single-quoted, so the glob-active brackets never reach the worker's shell.
  [[ "$launch" == *"--model 'claude-opus-4-8[context=1m,effort=high,fast=false]'"* ]]
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
bats tests/dispatch.bats
```

Expected: the three rejection tests FAIL (dispatch accepts and proceeds); both quoting assertions and the amended `cursor launch includes process authority` FAIL on the unquoted `--model kimi-k3-high` / `--model composer-2.5`.

- [ ] **Step 3: Add the cursor arm**

In `adapters/core/dispatch.sh`, inside the `case "$agent" in` block, after the `codex)` branch's `;;` and before `esac`:

```bash
  cursor)
    # Cursor fronts other vendors, so membership is unknowable offline and only
    # id shape is checked. BASH_REMATCH is clobbered by the next [[ =~ ]], so
    # both groups are captured on the spot.
    re_cursor='^([a-z0-9][a-z0-9.-]*)(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$'
    cursor_base=""
    cursor_params=""
    if [[ $model =~ $re_cursor ]]; then
      cursor_base="${BASH_REMATCH[1]}"
      cursor_params="${BASH_REMATCH[2]}"
    fi
    if [ -z "$cursor_base" ] || [[ $cursor_base =~ ^(opus|sonnet|haiku|fable)$ ]]; then
      echo "dispatch: model '$model' does not match --agent cursor — cursor needs a full model id (e.g. kimi-k3-high, cursor-grok-4.5-medium-fast, composer-2.5, claude-opus-4-8-high). Did you mean --agent claude? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    # cursor has no --effort knob, so its claude-*/gpt-* ids carry the rung in
    # the id itself; a bracket block exempts only by naming effort= there.
    if [[ $cursor_base =~ ^(claude|gpt)- ]] && [[ ! $cursor_base =~ $re_effort_tail ]] && [[ ! $cursor_params =~ (\[|,)effort= ]]; then
      echo "dispatch: model '$model' is not a cursor id — cursor's claude-*/gpt-* ids carry an effort suffix (gpt-5.6-sol-high, gpt-5.6-sol-high-fast) because cursor has no --effort knob. Live list: cursor-agent --list-models. See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
```

Three things a reviewer should check and not "simplify":

- `cursor_base` is empty **exactly** when the shape match failed (group 1 requires at least one character), which is why the shape failure and the alias rejection share one `if`. Do not restructure into `[[ ! $model =~ $re_cursor ]] || [[ ${BASH_REMATCH[1]} =~ … ]]` — the second `[[ =~ ]]` overwrites `BASH_REMATCH` before the captures are read.
- The `effort=` test is anchored to a pair boundary — `(\[|,)effort=` — so `gpt-5.6[noeffort=x]` does not sneak through a substring match.
- `re_effort_tail` is deliberately left-unanchored at its front: `gpt-5.5-extra-high` is a real cursor id and matches on its trailing `-high`.

- [ ] **Step 4: Single-quote the model in the cursor launch string**

In `adapters/core/dispatch.sh`, in the `elif [ "$agent" = cursor ]; then` branch, change `--model $model` to `--model '$model'`:

```bash
    "CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.${plan_note}${process_authority}'" Enter
```

The surrounding string is double-quoted, so the single quotes are literal and land in the text `tmux send-keys` types. Nothing else on that line changes.

**One occurrence only.** `--model $model` also appears in the claude launch at `dispatch.sh:323`; that one keeps its unquoted form (claude ids have no glob-active characters, and the existing claude assertions expect it). Do not sed both.

- [ ] **Step 5: Run the tests and shellcheck**

```bash
bats tests/dispatch.bats
shellcheck adapters/core/dispatch.sh
```

Expected: all PASS; shellcheck silent.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): gate cursor model ids by shape and quote the launch slot"
```

---

### Task 6: Map-conformance test

One table-driven test asserting every model the canonical docs name passes its engine's arm. The list is **hand-copied**, so it does not make contradiction impossible — it makes the common drift loud. Do not claim more than that in the comment.

**Files:**

- Modify: `tests/dispatch.bats` — append one helper and one test

**Interfaces:**

- Consumes: all three arms (Tasks 2, 3, 5).

- [ ] **Step 1: Write the test**

Append to `tests/dispatch.bats`:

```bash
# Asserts only that the gate stayed silent — a full launch per model would need
# a distinct branch per row and buys nothing the acceptance tests do not cover.
assert_gate_silent() { # <engine> <model>
  DISPATCH_PROFILE=work run run_dispatch standard "$2" --agent "$1" --effort medium --crew-id c1 42 "map row $2"
  if [[ "$output" == *"Model gate"* ]]; then
    printf 'gate rejected %s/%s: %s\n' "$1" "$2" "$output" >&2
    return 1
  fi
}

@test "every model the docs name passes its engine's arm" {
  # Hand-copied from dispatch-orchestration.md: the model map, the cursor
  # alternatives prose, the codex legacy generations, and the orchestrator
  # table. Copied, so it makes drift loud rather than impossible.
  for m in opus sonnet haiku claude-fable-5; do
    assert_gate_silent claude "$m"
  done
  for m in gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-5.5 gpt-5.4 gpt-5.4-mini; do
    assert_gate_silent codex "$m"
  done
  for m in kimi-k3-high cursor-grok-4.5-high cursor-grok-4.5-medium-fast \
    cursor-grok-4.5-low-fast composer-2.5 composer-2.5-fast \
    claude-opus-4-8-high gpt-5.6-sol-high; do
    assert_gate_silent cursor "$m"
  done
}
```

`assert_gate_silent` deliberately does not assert an exit status: with only the generic stubs on `PATH`, an accepted model runs on and exits 1 at `dispatch: could not locate worktree for branch …`. The property under test is "the gate did not fire", and every gate message carries the `Model gate` doc pointer.

- [ ] **Step 2: Run it**

```bash
bats tests/dispatch.bats -f "every model the docs name"
```

Expected: PASS.

- [ ] **Step 3: Prove it is non-vacuous**

Temporarily add `sonnet` to the cursor loop and re-run.

Expected: FAIL with `gate rejected cursor/sonnet: …`. Remove `sonnet` again and confirm the test is green.

- [ ] **Step 4: Commit**

```bash
git add tests/dispatch.bats
git commit -m "test(dispatch): pin every documented model against its engine's arm"
```

---

### Task 7: Protocol docs and regeneration

**Files:**

- Modify: `adapters/core/protocols/dispatch-orchestration.md:81` (the `--model` is free sentence) and `:84-85` (the "does not validate" claim); insert a new `### Model gate` subsection after line 102, before `## Orchestrator engines`
- Modify: `adapters/core/protocols/DISPATCHER_PROTOCOL.md:76` (the "Engine." bullet)
- Run: `scripts/gen-adapters.sh`
- **Generated, never hand-edited:** `adapters/claude-code/plugin/protocols/**`, `adapters/codex/plugin/protocols/**`

Out of scope, and the new text must not re-assert it: `dispatch-orchestration.md` lines 78-79 claim `kimi-k3-high` is kimi-k3's only Cursor slug; `kimi-k3-low` and `kimi-k3-max` also exist. Leave the claim alone, and do not repeat it.

Also leave alone: `adapters/cursor/rules/dispatcher.mdc` (asserts nothing about validation) and `docs/superpowers/specs/*.md` / `docs/superpowers/plans/*.md` (dated historical records — do not rewrite history).

**Interfaces:**

- Consumes: the finished gate behaviour, so the prose describes what exists.

- [ ] **Step 1: Reword the cursor `--model` sentence**

In `adapters/core/protocols/dispatch-orchestration.md`, replace:

```
is the point of reaching for cursor. `--model` is free, so a cursor worker can
still front **`composer-2.5`** / **`composer-2.5-fast`** (no effort variants) as
an alternative, or an effort-suffixed `claude-opus-4-8-*` / `gpt-5.6-sol-*`.
`dispatch` does not validate the model slot — the map is enforced by the
dispatcher's judgment, not by code.
```

with:

```
is the point of reaching for cursor. `--model` is open across cursor's whole
multi-vendor id space (the gate checks id _shape_, not membership of this
table), so a cursor worker can still front **`composer-2.5`** /
**`composer-2.5-fast`** (no effort variants) as an alternative, or an
effort-suffixed `claude-opus-4-8-*` / `gpt-5.6-sol-*`. `dispatch` validates the
model slot against `--agent` before scaffolding — see **Model gate** below.
```

Only "is free" was false; the enumeration is exactly what the cursor arm is shaped to keep passing, so it is preserved verbatim.

- [ ] **Step 2: Add the "Model gate" subsection**

Insert into `adapters/core/protocols/dispatch-orchestration.md` after the "Orchestration consult (worker-side, deep)" paragraph (currently ending line 102) and before `## Orchestrator engines (dispatcher session)`:

```markdown
### Model gate

`dispatch` validates `<model>` against `--agent` **before** it scaffolds
anything — no issue, no branch, no worktree, no window. It checks per-engine id
_shape_, not membership of the table above, so a model bump needs no
`dispatch.sh` edit:

- **claude** — an alias (`opus`, `sonnet`, `haiku`, `fable`) or a full
  `claude-*` id. An effort suffix is rejected: `claude-opus-4-8-high` is a
  _cursor_ id, and claude takes intensity through `--effort`.
- **codex** — `gpt-<gen>-<variant>`, variant mandatory, which is what rejects a
  bare `gpt-5.6`; `gpt-5.5` / `gpt-5.4` pass as legacy bare generations. When
  `$HOME/.codex/models_cache.json` is readable and holds a non-empty `.models`
  array, the slug must also appear in it — an absent or unusable cache is
  skipped, never fatal.
- **cursor** — an open multi-vendor id space, so shape only: claude CLI aliases
  are rejected, and a `claude-*` / `gpt-*` id must carry an effort suffix
  (`gpt-5.6-sol-high`) or name one in a bracket block
  (`claude-opus-4-8[context=1m,effort=high,fast=false]`). The bracket rule is a
  conservative guess — `cursor-agent` calls the pairs "overrides", so a block
  that omits `effort=` may well be legitimate and still get rejected. That is
  what the override below is for.

The gate enforces **dispatchability, not tier-appropriateness**. Picking the
rung that fits the task stays the dispatcher's judgment, and the map above stays
the source of truth for it.

**Override.** `DISPATCH_SKIP_MODEL_CHECK=<the exact model id>` skips the gate for
that one id and warns on stderr. Truthiness is exact string equality with the
model, not "is set" — exporting it for a session still gates every _other_
model. Reaching for it means **the map above is stale**: update the map in the
same session. The map is a protocol file, hot-reloadable through
`DISPATCHER_PROTOCOL_DIR`, so the doc fix lands immediately; the `dispatch.sh`
grammar follows on the next rebuild.
```

- [ ] **Step 3: Turn the "Engine." bullet into an enforced rule**

In `adapters/core/protocols/DISPATCHER_PROTOCOL.md`, in the `- **Engine.**` bullet, replace:

```
a cursor model id for `--agent cursor` (model map in `dispatch-orchestration.md`).
```

with:

```
a cursor model id for `--agent cursor` (model map in `dispatch-orchestration.md`) — `dispatch` rejects a mismatched or unsupported model there before scaffolding, by per-engine id shape; `DISPATCH_SKIP_MODEL_CHECK=<the exact model id>` overrides one id at a time (see `dispatch-orchestration.md` → "Model gate").
```

The bullet is one long line — keep it one long line, matching its neighbours.

- [ ] **Step 4: Regenerate the projections**

```bash
bash scripts/gen-adapters.sh
git status --short
```

Expected: `adapters/claude-code/plugin/protocols/` and `adapters/codex/plugin/protocols/` show modified copies of both protocol files, and nothing else changed. Never hand-edit those trees.

- [ ] **Step 5: Verify idempotence and the suite**

Stage first, then diff the index — Steps 1-4 left the whole Task 7 diff unstaged, so a bare
`git diff --exit-code` here reports _those_ edits and exits 1, which reads as a false
regeneration failure:

```bash
git add -A
bash scripts/gen-adapters.sh
git diff --exit-code
```

Expected: no output, exit 0 — the second run changes nothing on top of what is staged.

```bash
bats tests/
```

Expected: all pass, `tests/adapters.bats` included.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/protocols adapters/claude-code/plugin/protocols adapters/codex/plugin/protocols
git commit -m "docs(protocols): document the dispatch model gate and its override"
```

---

### Task 8: Full verification

**Files:** none modified — this task only runs the repo's gates and reports.

- [ ] **Step 1: Shellcheck every edited shell script**

```bash
shellcheck adapters/core/*.sh scripts/*.sh
```

Expected: no output, exit 0.

- [ ] **Step 2: Full bats suite**

```bash
bats tests/
```

Expected: every test passes across `adapters.bats`, `crew.bats`, `dispatch.bats`, `dispatcher.bats`, `module.bats`, `smoke.bats`.

- [ ] **Step 3: Confirm the suite is still machine-independent**

```bash
DISPATCH_PROFILE=personal CREW_ID=leaked DISPATCH_SKIP_MODEL_CHECK=gpt-5.6 TMUX_PANE=%99 bats tests/dispatch.bats
```

Expected: identical to Step 2 — all pass. An inherited override in particular must not leak into the gate tests.

- [ ] **Step 4: Confirm no generated drift**

```bash
bash scripts/gen-adapters.sh
git diff --exit-code
```

Expected: no output, exit 0.

- [ ] **Step 5: Flake check**

```bash
nix flake check
```

Expected: passes. This runs the pre-commit hooks (statix, deadnix, alejandra, shellcheck, treefmt) and builds the `dispatch` package, which is what proves the edited `dispatch.sh` still assembles under `writeShellApplication`.

- [ ] **Step 6: Report**

State plainly which commands were run and their outcomes. If anything failed, say so with the output rather than describing the work as complete.

---

## Self-Review

**Spec coverage.** §1 decision → Tasks 2/3/4/5. §2 placement → Task 2 Step 3 (insertion point) plus the `the model gate outranks the effort-ultra gate` test. §3 all three arms → Tasks 2, 3, 4, 5; every accept/reject row in §3 was executed against the exact regexes in this plan before it was written, and all match. §4 messages → the literal strings appear in Tasks 2/3/4/5. §5 override → Task 2 (wrapper + skip message) and Task 3 (the three truthiness rows). §6 docs → Task 7, all three claims, plus the regeneration and the explicitly-unchanged list. §7.1 → Task 1. §7.2 invocation shape → every row supplies `--crew-id c1`, every codex/cursor row sets `DISPATCH_PROFILE=work`, every exit-0 row calls `stub_launch_bins` and passes issue `42` plus a title. §7.3 → Tasks 2–6, one bats row per bullet. §7.3 repo gates → Task 8. §8 non-goals respected: no scaffolding change beyond the one blessed launch-string quote, no bus-schema field, no CLI flag, no `cursor-agent --list-models` call, `dispatcher.sh` untouched, kimi-k3 drift left alone.

**Two deliberate deviations from the spec's literal text**, both flagged in-task:

1. §4's bare-generation message is parameterised on the generation (`$gen`) rather than hardcoded. For `gpt-5.6` the output is byte-identical to the spec literal; hardcoded, it would say "there is no bare gpt-5.6" when the user typed `gpt-5.7`.
2. §4 shows an unfiltered slug list in the cache-miss literal but its own next paragraph mandates `select(startswith("gpt-"))` filtering, because the raw list advertises `codex-auto-review`. The plan implements the filter; the test asserts `codex-auto-review` is absent.

**Type consistency.** Shell variable names are used identically wherever they cross task boundaries: `re_effort_tail` (Task 2, reused Task 5), `re_claude_id` (Task 2), `codex_cache` / `known` / `gen` (Tasks 3–4), `re_cursor` / `cursor_base` / `cursor_params` (Task 5). Test helpers: `setup_repo`, `stub_bin`, `stub_launch_bins`, `$STUB_LOG` (existing); `write_codex_cache` (Task 4), `assert_gate_silent` (Task 6).
