# Configurable Engine Roster Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the set of dispatchable engines a per-machine setting — an engine must be both **enabled** (listed in `DISPATCH_ENGINES`) and **available** (its CLI on PATH) — without removing support for any engine.

**Architecture:** Two tiny shell helpers (`engine_cli`, `engine_enabled`) plus one `check_engine` guard replace the three hardcoded `profile != work` blocks in `dispatch.sh` and `dispatcher.sh`. A new `dispatch --engines` prints the effective roster for the dispatcher model to consult. The home-manager module gains an `engines` option that exports the variable and gates each engine's installed artifacts.

**Tech Stack:** POSIX-ish bash (`bash -euo pipefail`), bats-core tests, Nix flake + home-manager module, shellcheck in CI.

**Spec:** `docs/superpowers/specs/2026-09-17-configurable-engine-roster-design.md`
**Issue:** https://github.com/noamsto/dispatcher/issues/218

## Global Constraints

- **Unset `DISPATCH_ENGINES` means all four engines.** Never default to a narrower list in the shell scripts. This is what keeps non-Nix checkouts and the existing bats suite working.
- **Engine name ≠ CLI name.** `cursor` runs `cursor-agent`; `claude`, `codex` and `pi` match their own names. Every PATH probe goes through `engine_cli`.
- **The canonical engine order is `claude codex cursor pi`** — used for `--engines` output and for any message listing engines, so output is deterministic.
- **No engine support is removed.** `--agent` accept-lists, usage strings, model-shape cases and protocol prose keep naming all four.
- **`profile` / `$DISPATCH_PROFILE` keeps its non-engine jobs.** Do not delete `profile="${DISPATCH_PROFILE:-personal}"` from `dispatch.sh:578`: it is still read at `dispatch.sh:1683` (work+claude+deep rung). Same in `dispatch-resume.sh:248`/`:267`.
- **Two message shapes, exactly:**
  - not enabled → `<script>: <context> is not enabled here (enabled: <roster>)`
  - enabled but missing → `<script>: <context> is enabled but not installed (no '<cli>' on PATH)`

  where `<script>` is `dispatch` or `dispatcher`, matching each script's existing messages.

- **`dispatch-resume.sh` gets no engine gate.** It already delegates to `dispatch`'s precheck so there is "exactly one copy of the profile, model-shape, effort-ceiling" logic (`dispatch-resume.sh:272`). Adding a second copy there is a defect, not thoroughness.
- **Every `adapters/core/*.sh` change must pass `shellcheck`** — CI runs `nix develop -c shellcheck adapters/core/*.sh adapters/core/reviewers/*.sh scripts/*.sh`.
- **Run tests through the devshell:** `nix develop -c bats tests/<file>.bats`.

---

### Task 1: Give the dispatch suite a deterministic engine PATH

`tests/dispatch.bats` stubs `tmux`, `crew`, `gh`, `wt` and `direnv` but never the engine CLIs, so today it silently inherits whatever engines the developer has installed. CI runs bats on a bare GitHub runner with none of them. The moment Task 2 adds a PATH probe, every launch test in this file would fail in CI and pass locally. This task removes that dependency first, changing no production code.

`tests/dispatcher.bats` already stubs all four (`stub_bin claude/codex/cursor-agent/pi`) and needs no change.

**Files:**

- Modify: `tests/dispatch.bats:15-30` (the `setup()` stub block)

**Interfaces:**

- Consumes: `stub_bin <name>` from `tests/helpers.bash:70` — writes a log-and-exit-0 stub into `$STUB_DIR` and prepends that dir to `PATH`.
- Produces: every engine CLI resolves inside `$STUB_DIR` for all tests in `tests/dispatch.bats`. Tasks 2, 3 and 5 rely on this.

- [ ] **Step 1: Write the failing test**

Add at the end of `tests/dispatch.bats`:

```bash
@test "the suite resolves engine CLIs from the stub dir, not the developer's machine" {
  # Without this, a PATH probe in dispatch.sh passes locally (real engines
  # installed) and fails on a bare CI runner. Pin the dependency here.
  for cli in claude codex cursor-agent pi; do
    run command -v "$cli"
    [ "$status" -eq 0 ]
    [[ "$output" == "$STUB_DIR/$cli" ]]
  done
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `nix develop -c bats tests/dispatch.bats -f "resolves engine CLIs"`
Expected: FAIL — `command -v claude` either resolves outside `$STUB_DIR` (developer machine) or returns status 1 (CI).

- [ ] **Step 3: Stub the engines**

In `tests/dispatch.bats`, in `setup()`, immediately after `stub_bin direnv` (line 30):

```bash
  # The engine CLIs are never executed by a launch (dispatch hands tmux a
  # command string), but dispatch probes them for availability. Stub all four
  # so the suite depends on the stub dir instead of the developer's install.
  stub_bin claude
  stub_bin codex
  stub_bin cursor-agent
  stub_bin pi
```

- [ ] **Step 4: Run the new test and the whole file**

Run: `nix develop -c bats tests/dispatch.bats`
Expected: PASS, including the new test, with no change in the count of other passes.

- [ ] **Step 5: Commit**

```
git add tests/dispatch.bats
git commit -m "test(dispatch): stub engine CLIs so the suite owns its PATH"
```

---

### Task 2: Roster gate for the lead engine

Replace the two work-only blocks with the roster check and the availability probe. Both ship in one helper, so both are tested here, red-first.

**Files:**

- Modify: `adapters/core/dispatch.sh:574-587`
- Test: `tests/dispatch.bats`

**Interfaces:**

- Produces, for Tasks 3 and 5:
  - `ENGINES_ALL="claude codex cursor pi"` — canonical order, and the unset default.
  - `engine_cli <engine>` — prints the CLI name; only `cursor` differs (`cursor-agent`).
  - `engine_enabled <engine>` — returns 0 when the engine is in `$DISPATCH_ENGINES`, or when that variable is unset/empty.
  - `check_engine <engine> <context>` — prints one of the two messages to stderr and exits 1, or returns 0.

- [ ] **Step 1: Write the failing tests**

Add to `tests/dispatch.bats`:

```bash
@test "rejects an engine that is not on the roster" {
  DISPATCH_ENGINES="claude pi" run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "roster test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent codex is not enabled here (enabled: claude pi)"* ]]
}

@test "an unset roster admits every engine" {
  # The compatibility contract: a non-Nix checkout exports nothing.
  run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "roster test"
  [[ "$output" != *"not enabled here"* ]]
}

@test "the roster gate replaces the work-profile gate" {
  # codex off a work profile used to be rejected on profile alone; with a
  # roster that lists it, profile is no longer the gate.
  DISPATCH_PROFILE=personal DISPATCH_ENGINES="claude codex pi" run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "roster test"
  [[ "$output" != *"work-profile only"* ]]
  [[ "$output" != *"not enabled here"* ]]
}

@test "rejects an enabled engine whose CLI is missing" {
  # PATH keeps the stub dir (tmux, crew, gh, wt are needed to get this far)
  # but the engine stub is removed, so only the probe can fail.
  rm "$STUB_DIR/codex"
  DISPATCH_ENGINES="claude codex pi" run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "probe test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent codex is enabled but not installed (no 'codex' on PATH)"* ]]
}

@test "the probe looks for cursor-agent, not cursor" {
  rm "$STUB_DIR/cursor-agent"
  DISPATCH_ENGINES="claude cursor pi" run run_dispatch standard composer-2.5 --agent cursor --effort medium --crew-id c1 "probe test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"no 'cursor-agent' on PATH"* ]]
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `nix develop -c bats tests/dispatch.bats -f "roster\|missing\|cursor-agent"`
Expected: all five FAIL — no `not enabled here` or `is enabled but not installed` in any output, and the third still printing `work-profile only`.

- [ ] **Step 3: Replace the work-only block**

In `adapters/core/dispatch.sh`, replace lines 574-587 (the `# Work-only engine gate.` comment and both `if` blocks) with:

```bash
# Engine gate. An engine must be enabled (on this machine's roster) and
# available (its CLI installed). $DISPATCH_ENGINES is set from
# programs.dispatcher.engines by home-manager; unset means every engine, so a
# non-Nix checkout and the test suite need no extra setup. $DISPATCH_PROFILE no
# longer gates engines — it is still read below for the work+claude+deep rung.
profile="${DISPATCH_PROFILE:-personal}"
ENGINES_ALL="claude codex cursor pi"

# engine_cli <engine> — the CLI that engine runs as. Only cursor differs.
engine_cli() {
  case "$1" in
  cursor) printf 'cursor-agent' ;;
  *) printf '%s' "$1" ;;
  esac
}

# engine_enabled <engine> — is it on this machine's roster?
engine_enabled() {
  case " ${DISPATCH_ENGINES:-$ENGINES_ALL} " in
  *" $1 "*) return 0 ;;
  esac
  return 1
}

# check_engine <engine> <context> — reject before scaffolding a worktree, so a
# missing engine is a clear message and not a later `codex: command not found`
# in a pane the branch and issue already paid for.
check_engine() {
  local cli
  engine_enabled "$1" || {
    echo "dispatch: $2 is not enabled here (enabled: ${DISPATCH_ENGINES:-$ENGINES_ALL})" >&2
    exit 1
  }
  cli="$(engine_cli "$1")"
  command -v "$cli" >/dev/null 2>&1 || {
    echo "dispatch: $2 is enabled but not installed (no '$cli' on PATH)" >&2
    exit 1
  }
}

check_engine "$agent" "--agent $agent"
```

- [ ] **Step 4: Run the tests**

Run: `nix develop -c bats tests/dispatch.bats`
Expected: PASS, whole file. The pre-existing pi test at `dispatch.bats:368` (`[[ "$output" != *"work-profile only"* ]]`) still passes — that string no longer exists anywhere.

- [ ] **Step 5: Confirm the cursor-agent test is real**

Temporarily change `engine_cli`'s `cursor)` branch to `*)`, re-run `nix develop -c bats tests/dispatch.bats -f "cursor-agent"`, confirm it FAILS with `no 'cursor' on PATH`, then revert the edit and confirm it PASSES again. Without this, that test would also pass against a broken map on a machine where a `cursor` binary happens to exist.

- [ ] **Step 6: Shellcheck**

Run: `nix develop -c shellcheck adapters/core/dispatch.sh`
Expected: no output, exit 0.

- [ ] **Step 7: Commit**

```
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): gate the lead engine on a configurable roster"
```

---

### Task 3: Gate per-role engines

`--roles reviewer=cursor:composer-2.5` picks a different engine per role (cross-engine review). It carries its own copy of the work-only rule at `dispatch.sh:1025-1032`, which must become the same check or a disabled engine slips in through a grid.

**Files:**

- Modify: `adapters/core/dispatch.sh:1025-1032`
- Test: `tests/dispatch.bats`

**Interfaces:**

- Consumes: `check_engine` from Task 2, defined well above the role loop.

- [ ] **Step 1: Write the failing test**

```bash
@test "a role cannot use an engine that is off the roster" {
  DISPATCH_ENGINES="claude pi" run run_dispatch standard sonnet --agent claude --effort medium --crew-id c1 --roles reviewer=cursor:composer-2.5 "role roster test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"role 'reviewer' uses --agent cursor is not enabled here"* ]]
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `nix develop -c bats tests/dispatch.bats -f "role cannot use"`
Expected: FAIL — the dispatch proceeds past role parsing, or rejects with the old `work-profile only` wording.

- [ ] **Step 3: Replace the role work-only case**

In `adapters/core/dispatch.sh`, replace the whole `case "$role_agent" in codex | cursor) … esac` block at lines 1025-1032 with:

```bash
    check_engine "$role_agent" "role '$role' uses --agent $role_agent"
```

- [ ] **Step 4: Run the tests**

Run: `nix develop -c bats tests/dispatch.bats`
Expected: PASS, whole file.

- [ ] **Step 5: Shellcheck and commit**

```
nix develop -c shellcheck adapters/core/dispatch.sh
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): gate per-role engines on the same roster"
```

---

### Task 4: Gate the orchestrator engine

`dispatcher.sh` launches the orchestrator itself and carries its own copy of the rule at `:61-69`. Its messages are prefixed `dispatcher:`, not `dispatch:`.

**Files:**

- Modify: `adapters/core/dispatcher.sh:61-69`
- Test: `tests/dispatcher.bats:26-30` (replace the existing work-profile test) and `:94` (adjust)

**Interfaces:**

- Produces: `ENGINES_ALL`, `engine_cli`, `engine_enabled` duplicated into `dispatcher.sh` with a `dispatcher:` message prefix. There is no shared library in `adapters/core/` — every script is standalone and bakes into its own `writeShellApplication`, so duplication is the established pattern here, not an oversight.

- [ ] **Step 1: Rewrite the existing test and add two**

In `tests/dispatcher.bats`, replace the `gates codex behind the work profile` test (lines 26-30) with:

```bash
@test "rejects an engine that is not on the roster" {
  DISPATCH_ENGINES="claude pi" run run_launcher --agent codex
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent codex is not enabled here (enabled: claude pi)"* ]]
}

@test "rejects an enabled engine whose CLI is missing" {
  rm "$STUB_DIR/codex"
  DISPATCH_ENGINES="claude codex pi" run run_launcher --agent codex
  [ "$status" -eq 1 ]
  [[ "$output" == *"is enabled but not installed (no 'codex' on PATH)"* ]]
}

@test "an unset roster admits every engine" {
  CREW_ID=c1 run run_launcher --agent cursor
  [ "$status" -eq 0 ]
}
```

Then update the `pi is not work-profile gated as an orchestrator` test (line ~94): keep the test, drop its now-meaningless `DISPATCH_PROFILE=personal` prefix, and change its assertion to:

```bash
  [[ "$output" != *"not enabled here"* ]]
```

- [ ] **Step 2: Run them to verify they fail**

Run: `nix develop -c bats tests/dispatcher.bats -f "roster\|missing"`
Expected: FAIL — output still says `work-profile only`.

- [ ] **Step 3: Confirm nothing else reads `$profile` here**

Run: `grep -n 'profile' adapters/core/dispatcher.sh`
Expected: only the two lines being replaced. If any other reader appears, keep the `profile=` assignment in step 4.

- [ ] **Step 4: Replace the profile case**

In `adapters/core/dispatcher.sh`, replace lines 61-69 (`profile="${DISPATCH_PROFILE:-personal}"` plus the `case "$agent" in codex | cursor) … esac`) with:

```bash
# Engine gate. Enabled (this machine's roster) and available (CLI installed).
# Unset $DISPATCH_ENGINES means every engine. Duplicated from dispatch.sh on
# purpose: adapters/core has no shared library, each script bakes standalone.
ENGINES_ALL="claude codex cursor pi"

engine_cli() {
  case "$1" in
  cursor) printf 'cursor-agent' ;;
  *) printf '%s' "$1" ;;
  esac
}

engine_enabled() {
  case " ${DISPATCH_ENGINES:-$ENGINES_ALL} " in
  *" $1 "*) return 0 ;;
  esac
  return 1
}

engine_enabled "$agent" || {
  echo "dispatcher: --agent $agent is not enabled here (enabled: ${DISPATCH_ENGINES:-$ENGINES_ALL})" >&2
  exit 1
}
cli="$(engine_cli "$agent")"
command -v "$cli" >/dev/null 2>&1 || {
  echo "dispatcher: --agent $agent is enabled but not installed (no '$cli' on PATH)" >&2
  exit 1
}
```

- [ ] **Step 5: Run the tests**

Run: `nix develop -c bats tests/dispatcher.bats`
Expected: PASS, whole file.

- [ ] **Step 6: Shellcheck and commit**

```
nix develop -c shellcheck adapters/core/dispatcher.sh
git add adapters/core/dispatcher.sh tests/dispatcher.bats
git commit -m "feat(dispatcher): gate the orchestrator engine on the roster"
```

---

### Task 5: `dispatch --engines`

The dispatcher model chooses the engine. It needs one command that answers "what can I actually dispatch here", because the roster is machine-local and the probe is dynamic.

**Files:**

- Modify: `adapters/core/dispatch.sh` (move the helpers up; new early-exit flag beside `--reap-roles` at `:342`)
- Test: `tests/dispatch.bats`

**Interfaces:**

- Consumes: `engine_cli`, `engine_enabled`, `ENGINES_ALL` from Task 2 — **which must be defined above the new flag block.** Task 2 defines them around line 578, _below_ `--reap-roles` at 342, so this task moves the `ENGINES_ALL` / `engine_cli` / `engine_enabled` / `check_engine` definitions up to sit immediately before the `--reap-roles` block. Leave `profile="${DISPATCH_PROFILE:-personal}"` and the `check_engine "$agent" "--agent $agent"` call where Task 2 put them.
- Produces: `dispatch --engines`, printing enabled ∧ available engines one per line in `ENGINES_ALL` order, exit 0. Prints nothing and still exits 0 when none qualify.

- [ ] **Step 1: Write the failing tests**

```bash
@test "--engines prints the effective roster" {
  DISPATCH_ENGINES="claude codex pi" run run_dispatch --engines
  [ "$status" -eq 0 ]
  [[ "$output" == "claude
codex
pi" ]]
}

@test "--engines omits an engine whose CLI is missing" {
  rm "$STUB_DIR/codex"
  DISPATCH_ENGINES="claude codex pi" run run_dispatch --engines
  [ "$status" -eq 0 ]
  [[ "$output" == "claude
pi" ]]
}

@test "--engines needs no crew id, worktree or tmux" {
  # A dispatcher asks it precisely when it has not yet decided anything.
  run run_dispatch --engines
  [ "$status" -eq 0 ]
  [[ "$output" != *"no crew id"* ]]
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `nix develop -c bats tests/dispatch.bats -f "engines"`
Expected: FAIL — `--engines` falls through to the tier parser and errors on usage.

- [ ] **Step 3: Move the helpers up and add the flag**

Move the four definitions named above, then add directly after the `--reap-roles` block:

```bash
# `dispatch --engines` — the effective roster: enabled AND installed, in
# canonical order. The dispatcher protocol tells the model to read this before
# judging, since the roster is machine-local and the probe is dynamic.
if [ "${1:-}" = "--engines" ]; then
  for e in $ENGINES_ALL; do
    engine_enabled "$e" || continue
    command -v "$(engine_cli "$e")" >/dev/null 2>&1 || continue
    echo "$e"
  done
  exit 0
fi
```

Note for shellcheck: `for e in $ENGINES_ALL` is an intentional unquoted word split over a space-separated list. If SC2086 fires, add `# shellcheck disable=SC2086` with that reason on the line above rather than restructuring.

- [ ] **Step 4: Run the tests**

Run: `nix develop -c bats tests/dispatch.bats`
Expected: PASS, whole file — the move must not disturb Tasks 2-3.

- [ ] **Step 5: Shellcheck and commit**

```
nix develop -c shellcheck adapters/core/dispatch.sh
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): add --engines, the effective roster"
```

---

### Task 6: The `engines` option and artifact gating

**Files:**

- Modify: `nix/hm-module.nix`
- Test: `tests/module.bats` (`setup_file` at 19-64, `setup` at 66-85, plus new tests)

**Interfaces:**

- Produces: `programs.dispatcher.engines`, a `listOf (enum ["claude" "codex" "cursor" "pi"])` defaulting to `["claude" "pi"]`, exported as `DISPATCH_ENGINES` (space-separated) and gating the per-engine artifacts.

Use `lib.optionalAttrs`, **not** `lib.mkIf`, for the gating: `module.bats` calls the module as a plain function rather than through `evalModules`, so a `mkIf` value would arrive as `{ _type; condition; content; }` and the attribute-name assertions below could not see through it. `optionalAttrs` evaluates to a plain attrset or `{}`.

- [ ] **Step 1: Write the failing tests**

In `tests/module.bats` `setup_file()`, add a third evaluated config inside the same `let`, after `configApplied`:

```nix
      cursorlessApplied = self.homeManagerModules.default {
        config = { programs.dispatcher = { enable = true; profile = \"work\"; engines = [\"claude\" \"pi\"]; }; };
        inherit lib pkgs;
      };
      c2 = cursorlessApplied.config.content;
      cursorlessLine = builtins.deepSeq [c2.home.file c2.home.activation]
        \"\${builtins.concatStringsSep \",\" (builtins.attrNames c2.home.file)}|\${builtins.concatStringsSep \",\" (builtins.attrNames c2.home.activation)}|\${c2.home.sessionVariables.DISPATCH_ENGINES}\";
```

Change the final expression from `in optionNames + \"\n\" + configLine` to:

```nix
    in optionNames + \"\n\" + configLine + \"\n\" + cursorlessLine
```

In `setup()`, after the `EVAL_CONFIG=` line, add:

```bash
  EVAL_CURSORLESS="$(sed -n '3p' "$BATS_FILE_TMPDIR/eval-out")"
```

Add these tests:

```bash
@test "the module declares the engines option" {
  [[ "$EVAL_OPTIONS" == *"engines"* ]]
}

@test "a roster without cursor installs no cursor artifacts" {
  [[ "$EVAL_CURSORLESS" != *".cursor/"* ]]
  [[ "$EVAL_CURSORLESS" != *"dispatcherCursorSkills"* ]]
}

@test "a roster without codex installs no codex plugin activation" {
  [[ "$EVAL_CURSORLESS" != *"dispatcherCodexPlugin"* ]]
}

@test "the roster is exported for the CLIs" {
  [[ "$EVAL_CURSORLESS" == *"|claude pi" ]]
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `nix develop -c bats tests/module.bats`
Expected: FAIL — the eval errors on the undeclared `engines` option, which fails the whole file in `setup_file`.

- [ ] **Step 3: Add the option and the export**

In `nix/hm-module.nix`, after the `profile` option:

```nix
    engines = lib.mkOption {
      type = lib.types.listOf (lib.types.enum ["claude" "codex" "cursor" "pi"]);
      default = ["claude" "pi"];
      description = ''
        Engines this machine may dispatch. Exported as DISPATCH_ENGINES and
        gates the per-engine artifacts installed below. An engine must also be
        installed: the CLIs probe PATH before scaffolding. Unset at runtime
        (a non-Nix checkout), the CLIs allow all four.

        Defaults to the two all-profile engines, which is the roster the
        removed work-profile gate produced on a personal machine.
      '';
    };
```

Replace the `profile` option's description, which no longer tells the truth:

```nix
      description = ''
        The machine's profile. Read by the CLIs for the work+claude+deep rung
        and the work-only analytics MCP profile. Engine availability is
        `engines`, not this.
      '';
```

In the `let` block, after `codexCache`:

```nix
  hasEngine = e: lib.elem e cfg.engines;
```

In `home.sessionVariables`, beside `DISPATCH_PROFILE`:

```nix
        DISPATCH_ENGINES = lib.concatStringsSep " " cfg.engines;
```

- [ ] **Step 4: Gate the artifacts**

Change `file = {` to:

```nix
      file = lib.optionalAttrs (hasEngine "cursor") {
```

leaving the five `.cursor/*` entries and their comments inside, unchanged.

Replace the two dotted `activation.*` keys with one merged attribute, so statix does not flag repeated `home.*` keys:

```nix
      activation =
        lib.optionalAttrs (hasEngine "codex") {
          dispatcherCodexPlugin = lib.hm.dag.entryAfter ["writeBoundary"] ''
            run rm -rf "$HOME/${codexCache}"
            run mkdir -p "$HOME/${codexCache}"
            run cp -rL ${codexPlugin} "$HOME/${codexCache}/${codexVersion}"
            run chmod -R u+w "$HOME/${codexCache}"
          '';
        }
        // lib.optionalAttrs (hasEngine "cursor") {
          dispatcherCursorSkills = lib.hm.dag.entryAfter ["writeBoundary"] ''
            skills_dir="$HOME/.cursor/skills"
            run mkdir -p "$skills_dir"
            run ln -sfn "${self}/adapters/cursor/skills/spec-plan-critic" "$skills_dir/spec-plan-critic"
          '';
        };
```

Keep both existing comment blocks with their activation scripts — they explain `cp -rL` and the shared `~/.cursor/skills` namespace, and both facts still hold.

`home.packages` stays whole: `refresh-models` is cursor-only, but nothing schedules it and it already warns and exits without touching its cache when `cursor-agent` is absent (`refresh-models.sh:21-24`).

- [ ] **Step 5: Keep the full-artifact case covered**

`configApplied` sets no `engines`, so it now takes the `["claude" "pi"]` default — which empties its `home.file` and drops its codex activation, breaking `the codex plugin is copied as a real dir, never symlinked` (module.bats:256) and the `.cursor` expectations in `the module's config body evaluates`. Add to `configApplied`'s config:

```nix
engines = [\"claude\" \"codex\" \"cursor\" \"pi\"];
```

so it keeps exercising the full artifact set, and `cursorlessApplied` remains the narrow case.

- [ ] **Step 6: Run the tests**

Run: `nix develop -c bats tests/module.bats`
Expected: PASS, whole file.

- [ ] **Step 7: Check the flake still evaluates**

Run: `nix flake check`
Expected: exit 0.

- [ ] **Step 8: Commit**

```
git add nix/hm-module.nix tests/module.bats
git commit -m "feat(nix): add programs.dispatcher.engines and gate artifacts on it"
```

---

### Task 7: Point the protocols at the roster

The dispatcher model judges engine choice from prose. Several passages state the work-only rule as fact; all must instead send it to `dispatch --engines`.

**Files:**

- Modify: `adapters/core/protocols/DISPATCHER_PROTOCOL.md` (roster note ~22-41, cursor line ~130, "Profile constraint" ~213, engine paragraph ~292)
- Modify: `adapters/core/protocols/dispatch-orchestration.md`
- Modify: `README.md`

**Interfaces:**

- Consumes: `dispatch --engines` from Task 5.

- [ ] **Step 1: Find every claim to fix**

Run:

```
grep -rn "work-profile only\|work profile only" adapters/core/protocols README.md
```

Fix every line this returns.

- [ ] **Step 2: Rewrite the roster note**

In `DISPATCHER_PROTOCOL.md` (line ~22), the text reads "Weigh **claude**, **codex**, **cursor**, and **pi** … (codex/cursor work-profile only) as equal candidates by task fit". Replace the parenthetical with a pointer:

```markdown
Weigh **claude**, **codex**, **cursor**, and **pi** as equal candidates by task
fit, not as a default plus exceptions.

> **The roster is machine-local.** Run `dispatch --engines` before judging: it
> prints the engines this machine can actually dispatch (enabled for the
> machine, and installed). Never propose an engine absent from that list —
> `dispatch` rejects it anyway, after you have already spent a turn on it.
```

- [ ] **Step 3: Replace the "Profile constraint" paragraph**

At line ~213, replace the paragraph stating codex and cursor are work-profile only with:

```markdown
**Engine constraint:** the dispatchable set is whatever `dispatch --engines`
prints — a machine's roster (`programs.dispatcher.engines`) intersected with
what is installed. `dispatch` and `dispatcher` both reject anything else before
scaffolding, with `is not enabled here` or `is enabled but not installed`.
Authentication is separate and out of band: a listed engine can still fail its
first turn if it has no session.
```

- [ ] **Step 4: Fix the remaining prose**

Rewrite each other hit from Step 1 so it refers to the roster rather than the work profile. Keep every engine's _leanings_ prose (cursor's PR-finishing lean, pi's ladder, `--effort` being a no-op on cursor): the document stays machine-neutral and describes all four engines.

- [ ] **Step 5: Verify no stale claim survives**

Run:

```
grep -rn "work-profile only\|work profile only" adapters/core/protocols README.md
```

Expected: no output.

- [ ] **Step 6: Run the whole suite**

Run: `nix develop -c bats tests/`
Expected: PASS. A protocol edit changes `PROTOCOL_REV` (a content hash of `$PROTOCOL_DIR`), which is by design; there is no committed rev file to bump.

- [ ] **Step 7: Commit**

```
git add adapters/core/protocols README.md
git commit -m "docs(dispatcher): judge engines from the roster, not the profile"
```

---

## Finishing

- [ ] Full gate, matching CI:

```
nix develop -c shellcheck adapters/core/*.sh adapters/core/reviewers/*.sh scripts/*.sh
nix develop -c bats tests/module.bats
nix develop -c bats tests/
nix flake check
```

- [ ] Run `/deslop` on the branch, then push and open the PR assigned to yourself, closing #218.

- [ ] The consumer change is a **separate nix-config PR**, after this merges: repin the `dispatcher` flake input, set `engines = ["claude" "pi" "codex"];` in `home/ai/claude-code/default.nix`, and delete `dispatcherCursorNotify` plus `activation.dispatcherCursorStopHook` from `home/ai/cursor/default.nix`. That deletion is safe only once cursor is off the roster — the stop hook is cursor's sole session-end signal. Verify with a full `nh os switch` (the module's package set and file set both change), not `nh home switch`.
