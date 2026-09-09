# dispatch resume Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `dispatch resume`, run from inside a worker's worktree, relaunches that worker with its own engine session continued, reading every launch parameter back from `WORKER_TASK.md`.

**Architecture:** A new standalone binary `dispatch-resume` (its own `writeShellApplication`, like `pr-watch`), which `dispatch.sh` reaches by intercepting `resume` as `$1` and `exec`ing it. A separate file rather than a mode threaded through `dispatch.sh`'s 1153 linear lines: resume skips the issue-claim, branch-creation, task-document-rewrite and new-window paths entirely, and inherits only the gates it explicitly re-runs.

**Tech Stack:** bash (POSIX-ish, BSD+GNU portable), `jq`, `git`, `tmux`, `gh`, bats, Nix flake-parts + `writeShellApplication`.

## Global Constraints

- **Design doc:** `docs/superpowers/specs/2026-09-09-dispatch-resume-design.md`. Read it before Task 1.
- **`adapters/core/dispatch-resume.sh` is NOT treefmt-excluded.** `flake.nix:43-48` excludes `crew.sh`, `dispatch.sh` and `dispatch-notify.sh` only, to keep the upstream extraction byte-identical. A new file is ours, like `dispatcher.sh`, so it must be **shfmt-clean at `indent_size = 2`**.
- **No shebang and no `set -euo pipefail`** in `dispatch-resume.sh`: `writeShellApplication` prepends both. Every core script is a function body only (`dispatch.sh:6-7`).
- **`@protocolDir@`** is the build-time protocol path placeholder. The new package must go through the `sub` replacer (`flake.nix:93`) or `PROTOCOL_DIR` resolves to the literal string.
- **No apostrophes in any launch-prompt string.** All three launch lines single-quote the prompt inside a double-quoted `tmux send-keys` argument; one apostrophe silently breaks the line (`dispatch.sh:1075-1077`).
- **Bus appends go through `_bus_append`**, never bare `printf >>`: a bare append is not one `write(2)` and concurrent writers splice (`dispatch.sh:20-24`, #55/#61).
- **`ps -o ppid= -p <pid>`** is the one parent-of spelling identical on BSD and GNU (`crew.sh:658`). Use it for any ancestry walk.
- **`awk` over `git worktree list --porcelain` must read to EOF.** An early `exit` SIGPIPEs git, and under `pipefail` that kills the script (`dispatch.sh:849-851`).
- Run `shellcheck adapters/core/dispatch-resume.sh` after every task that touches it.
- **`shfmt` is NOT on the devShell PATH** (only pulled in transitively by the treefmt wrapper), so `nix develop -c shfmt` fails. Use `nix run nixpkgs#shfmt -- -d -i 2 <file>`.
- **`nix build --no-link .#dispatch-resume` and `.#dispatch` are the real gate** on the new file: `writeShellApplication` runs shellcheck at build time and fails the build on any warning. `nix flake check` does NOT cover this — it reports the packages as "build skipped". Run both after every task that touches `dispatch-resume.sh`.
- **Never background a test run, and never run the whole `bats tests/` suite.** Run targeted files in the foreground with an explicit timeout; `tests/crew.bats` alone takes minutes and four agents on this plan have wedged waiting on a background job notification that never arrived.

---

### Task 1: Stamp `mcp:` in the WORKER_TASK.md header

Without this, an `--mcp analytics` worker cannot be faithfully resumed — the header records `engine`, `model` and `effort` but not the MCP profile.

**Files:**

- Modify: `adapters/core/dispatch.sh:987-989`
- Test: `tests/dispatch.bats`

**Interfaces:**

- Consumes: nothing.
- Produces: an `mcp: <profile-or-empty>` line in every `WORKER_TASK.md` header. Task 3 reads it.

- [ ] **Step 1: Write the failing test**

Append to `tests/dispatch.bats`:

```bash
@test "stamps the mcp profile in the task header" {
  stub_launch_bins
  export DISPATCH_PROFILE=work
  mkdir -p "$HOME/.config/claude-code"
  printf '{}' >"$HOME/.config/claude-code/mcp-posthog.json"
  run run_dispatch standard sonnet --effort medium --mcp analytics --crew-id c1 "add a flag"
  [ "$status" -eq 0 ]
  doc="$(find "$TEST_REPO/.dispatch-wt" -name WORKER_TASK.md | head -1)"
  grep -qx 'mcp: analytics' "$doc"
}

@test "stamps an empty mcp line when no profile was given" {
  stub_launch_bins
  run run_dispatch standard sonnet --effort medium --crew-id c1 "add a flag"
  [ "$status" -eq 0 ]
  doc="$(find "$TEST_REPO/.dispatch-wt" -name WORKER_TASK.md | head -1)"
  grep -qE '^mcp: ?$' "$doc"
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch.bats -f 'mcp profile in the task header'`
Expected: FAIL — the header has no `mcp:` line, so `grep -qx` returns 1.

- [ ] **Step 3: Add the field to the header printf**

In `adapters/core/dispatch.sh`, the stamp block currently reads:

```bash
  printf 'tier: %s\nkind: %s\ndraft: %s\nengine: %s\nmodel: %s\neffort: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\nworker_id: %s\n' \
    "$tier" "$kind" "$draft" "$agent" "$model" "$effort" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name" "$worker_id"
```

Insert `mcp` immediately after `effort`, so the engine/model/effort/mcp launch tuple stays contiguous:

```bash
  printf 'tier: %s\nkind: %s\ndraft: %s\nengine: %s\nmodel: %s\neffort: %s\nmcp: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\nworker_id: %s\n' \
    "$tier" "$kind" "$draft" "$agent" "$model" "$effort" "$mcp_profile" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name" "$worker_id"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `bats tests/dispatch.bats -f mcp`
Expected: PASS, both tests.

- [ ] **Step 5: Record the field in the worker protocol**

`adapters/core/protocols/WORKER_PROTOCOL.md:11` lists the header fields a worker reads. Find:

```
authoritative `engine:`, `model:`, and `effort:`,
```

Replace with:

```
authoritative `engine:`, `model:`, `effort:` and `mcp:`,
```

- [ ] **Step 6: Regenerate the adapters and confirm no drift**

Run: `./scripts/gen-adapters.sh && git status --short adapters/`
Expected: the claude-code and codex `WORKER_PROTOCOL.md` copies show as modified, matching the core edit. CI fails if generated output differs from a fresh run.

- [ ] **Step 7: Run the full suite**

Run: `bats tests/`
Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add adapters/ tests/dispatch.bats
git commit -m "feat(dispatch): stamp the mcp profile in the task header

A resume reads the launch tuple back from WORKER_TASK.md, and --mcp was the
one launch flag the header did not record."
```

---

### Task 2: `crew register` records the dispatcher's pane

A reattaching worker must update `dispatcher_pane:` to the _live_ dispatcher's pane. `crew register` records only a pid, so that pane is not discoverable today.

**Files:**

- Modify: `adapters/core/crew.sh:514-534`
- Test: `tests/crew.bats`

**Interfaces:**

- Consumes: nothing.
- Produces: `$crew_dir/crews/<crew_id>/pane` holding `$TMUX_PANE` when set. Task 7 reads it.

- [ ] **Step 1: Write the failing test**

Append to `tests/crew.bats`:

```bash
@test "register records the dispatcher pane when in tmux" {
  CREW_ID=c1 TMUX_PANE='%12' run_crew register 4242
  [ "$(cat "$(crew_dir)/crews/c1/pid")" = 4242 ]
  [ "$(cat "$(crew_dir)/crews/c1/pane")" = '%12' ]
}

@test "register writes no pane file outside tmux" {
  CREW_ID=c1 run_crew register 4242
  [ -f "$(crew_dir)/crews/c1/pid" ]
  [ ! -f "$(crew_dir)/crews/c1/pane" ]
}

@test "deregister removes the pane file with the crew dir" {
  CREW_ID=c1 TMUX_PANE='%12' run_crew register 4242
  CREW_ID=c1 run_crew deregister
  [ ! -e "$(crew_dir)/crews/c1" ]
}
```

If `tests/crew.bats` has no `run_crew` / `crew_dir` helpers, define them in its `setup()` to match the file's existing invocation style before writing these — read the top 40 lines first and follow whatever is already there rather than introducing a second convention.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/crew.bats -f 'records the dispatcher pane'`
Expected: FAIL — no `pane` file is written.

- [ ] **Step 3: Write the pane alongside the pid**

In `adapters/core/crew.sh`, the register branch currently reads:

```bash
  if [ "$sub" = register ]; then
    mkdir -p "$cdir"
    printf '%s\n' "${1:-$PPID}" >"$cdir/pid"
  else
    rm -rf "$cdir"
  fi
```

Replace with:

```bash
  if [ "$sub" = register ]; then
    mkdir -p "$cdir"
    printf '%s\n' "${1:-$PPID}" >"$cdir/pid"
    # The pane, not just the pid: a worker reattaching to a live dispatcher has
    # to retarget its `dispatcher_pane:` ping, and the pid alone cannot name a
    # pane. Absent outside tmux, which readers must tolerate.
    if [ -n "${TMUX_PANE:-}" ]; then
      printf '%s\n' "$TMUX_PANE" >"$cdir/pane"
    fi
  else
    rm -rf "$cdir"
  fi
```

`deregister`'s `rm -rf "$cdir"` already removes the new file.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `bats tests/crew.bats -f 'pane'`
Expected: PASS, all three.

- [ ] **Step 5: Run the full suite and shellcheck**

Run: `bats tests/ && shellcheck adapters/core/crew.sh`
Expected: all pass, no shellcheck output.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): register records the dispatcher pane beside its pid

A worker reattaching to a live dispatcher must retarget dispatcher_pane, and
a pid cannot name a pane."
```

---

### Task 3: The `dispatch-resume` binary — resolution, refusals, `--print`

**Files:**

- Create: `adapters/core/dispatch-resume.sh`
- Modify: `flake.nix` (new package + `all` paths), `nix/hm-module.nix:41`, `adapters/core/dispatch.sh:9-11` and `:30`
- Test: `tests/dispatch-resume.bats` (create)

**Interfaces:**

- Consumes: the `mcp:` header line from Task 1.
- Produces:
  - binary `dispatch-resume`, reached as `dispatch resume [...]`.
  - shell variables later tasks extend: `branch`, `wt_path`, `task_doc`, `agent`, `model`, `effort`, `mcp_profile`, `tier`, `crew_id`, `agent_name`, `prev_worker_id`, `fresh`, `do_print`. NOT `PROTOCOL_DIR`, `kind` or `plan_val` — each is declared by Task 6, the task that reads it, because a variable assigned here and read only later needs an SC2034 waiver to build (`writeShellApplication` runs shellcheck at build time) and this repo forbids escape hatches that silence a checker.
  - `_hdr <field>` — echoes the value of `<field>: ` from `$task_doc`, or empty.

- [ ] **Step 1: Write the failing tests**

Create `tests/dispatch-resume.bats`:

```bash
setup() {
  load helpers
  RESUME="$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  run_resume() { bash -euo pipefail "$RESUME" "$@"; }
  setup_repo
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID TMUX_PANE
  stub_bin tmux
  stub_bin crew
  stub_bin gh
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
  git commit --allow-empty -qm init
}

teardown() { teardown_repo; }

# A worktree that looks like a live worker's: its own branch, its own
# directory, and a task document with a full header.
setup_worker_wt() { # [extra header lines...]
  git -C "$TEST_REPO" worktree add -q -b feat/7-a-thing "$TEST_REPO/wt" HEAD
  WT="$TEST_REPO/wt"
  {
    printf 'tier: standard\nkind: implement\ndraft: false\n'
    printf 'engine: claude\nmodel: sonnet\neffort: medium\nmcp: \n'
    printf 'plan: required\ntitle: a thing\nCloses #7\n'
    printf 'dispatcher_pane: %%3\ncrew_dir: %s/.git/crew\ncrew_id: c1\n' "$TEST_REPO"
    printf 'agent_name: iris\nworker_id: worker:feat/7-a-thing#s1-99\n'
    for extra in "$@"; do printf '%s\n' "$extra"; done
    printf '\n## Task\n\nthe original body\n'
  } >"$WT/WORKER_TASK.md"
  export WT
}

@test "refuses outside a worktree carrying a task document" {
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"no WORKER_TASK.md"* ]]
  [[ "$output" == *"dispatch <tier> <model>"* ]]
}

@test "refuses in the primary worktree even with a task document" {
  printf 'engine: claude\nmodel: sonnet\neffort: medium\ncrew_id: c1\n' >"$TEST_REPO/WORKER_TASK.md"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"primary worktree"* ]]
}

@test "refuses on a detached HEAD" {
  setup_worker_wt
  git -C "$WT" checkout -q --detach
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"detached HEAD"* ]]
}

@test "refuses when the header lacks the launch tuple" {
  setup_worker_wt
  printf 'tier: standard\ncrew_id: c1\n' >"$WT/WORKER_TASK.md"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"header is missing"* ]]
  [[ "$output" == *"engine"* ]]
}

@test "--print reports the resolved launch and does not launch" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"branch: feat/7-a-thing"* ]]
  [[ "$output" == *"engine: claude"* ]]
  [[ "$output" == *"model: sonnet"* ]]
  [[ "$output" == *"effort: medium"* ]]
  [[ "$output" == *"crew_id: c1"* ]]
  run cat "$STUB_LOG"
  [[ "$output" != *send-keys* ]]
}

@test "explicit flags override the recorded header" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print --model opus --effort high
  [ "$status" -eq 0 ]
  [[ "$output" == *"model: opus"* ]]
  [[ "$output" == *"effort: high"* ]]
}

@test "the task body is left byte-identical" {
  setup_worker_wt
  cd "$WT"
  before="$(md5sum <"$WT/WORKER_TASK.md")"
  run run_resume --print
  [ "$status" -eq 0 ]
  [ "$(md5sum <"$WT/WORKER_TASK.md")" = "$before" ]
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch-resume.bats`
Expected: every test FAILs — `adapters/core/dispatch-resume.sh` does not exist.

- [ ] **Step 3: Create the resume binary**

Create `adapters/core/dispatch-resume.sh`:

```bash
# shellcheck shell=bash
# dispatch-resume — relaunch the worker whose worktree you are standing in,
# continuing its own engine session. Reached as `dispatch resume`, which execs
# this binary (dispatch.sh intercepts the subcommand).
#
# Its own file rather than a mode inside dispatch.sh: resume skips the
# issue claim, branch creation, task-document rewrite and new-window paths
# entirely, and re-runs only the gates it names. The shebang and
# `set -euo pipefail` are prepended by writeShellApplication.

usage() {
  echo "usage: dispatch resume [--agent claude|codex|cursor] [--model M] [--effort E] [--mcp <profile>] [--fresh] [--print] [--ignore-budget] [--ignore-map] [extra prompt...]" >&2
}

PROTOCOL_DIR="${DISPATCHER_PROTOCOL_DIR:-@protocolDir@}"

fresh=""
do_print=""
ignore_budget=""
ignore_map=""
agent_flag=""
model_flag=""
effort_flag=""
mcp_flag_val=""
extra=""

while [ $# -gt 0 ]; do
  case "$1" in
  --agent)
    agent_flag="${2:-}"
    [ -n "$agent_flag" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --model)
    model_flag="${2:-}"
    [ -n "$model_flag" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --effort)
    effort_flag="${2:-}"
    [ -n "$effort_flag" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --mcp)
    mcp_flag_val="${2:-}"
    [ -n "$mcp_flag_val" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --fresh)
    fresh=1
    shift
    ;;
  --print)
    do_print=1
    shift
    ;;
  --ignore-budget)
    ignore_budget=1
    shift
    ;;
  --ignore-map)
    ignore_map=1
    shift
    ;;
  -*)
    usage
    exit 1
    ;;
  *)
    extra="${extra:+$extra }$1"
    shift
    ;;
  esac
done

git rev-parse --git-common-dir >/dev/null 2>&1 || {
  echo "dispatch resume: not in a git repository" >&2
  exit 1
}

wt_path="$(git rev-parse --show-toplevel)"
task_doc="$wt_path/WORKER_TASK.md"

# Task document first: the overwhelmingly common mistake is running this from
# the main checkout, which has no WORKER_TASK.md, and "not a worker's worktree"
# says more there than "primary worktree" would.
[ -f "$task_doc" ] || {
  echo "dispatch resume: no WORKER_TASK.md in $wt_path — this is not a dispatched worker's worktree. To start a new worker, use 'dispatch <tier> <model> --effort <e> <title>'." >&2
  exit 1
}

# Then the primary worktree, which catches the remaining case: a stray
# WORKER_TASK.md in the main checkout must not make this look legitimate. A
# worker there would run in the main checkout, which dispatch.sh refuses on its
# own resume path for the same reason.
# awk reads to EOF on purpose — an early exit SIGPIPEs git under pipefail.
primary_wt="$(git worktree list --porcelain | awk '/^worktree /{if (!p) p=$2} END{print p}')"
if [ "$wt_path" = "$primary_wt" ]; then
  echo "dispatch resume: $wt_path is the primary worktree — a worker must not run in the main checkout. cd into the worker's worktree and retry." >&2
  exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" != HEAD ] || {
  echo "dispatch resume: detached HEAD — a worker resumes onto its own branch. Check the branch out and retry." >&2
  exit 1
}

# Header reader. `cut -d' ' -f2-` keeps values containing spaces (title), and
# -m1 pins the first occurrence so a value echoed inside the ## Task body
# cannot shadow the header.
_hdr() { grep -m1 "^$1: " "$task_doc" | cut -d' ' -f2- || true; }

agent="${agent_flag:-$(_hdr engine)}"
model="${model_flag:-$(_hdr model)}"
effort="${effort_flag:-$(_hdr effort)}"
mcp_profile="${mcp_flag_val:-$(_hdr mcp)}"
tier="$(_hdr tier)"
kind="$(_hdr kind)"
plan_val="$(_hdr plan)"
crew_id="$(_hdr crew_id)"
agent_name="$(_hdr agent_name)"
prev_worker_id="$(_hdr worker_id)"

# The launch tuple is what makes a resume faithful; without it we would be
# guessing at a model and effort the first dispatch already decided.
missing=""
[ -n "$agent" ] || missing="${missing:+$missing }engine"
[ -n "$model" ] || missing="${missing:+$missing }model"
[ -n "$effort" ] || missing="${missing:+$missing }effort"
[ -n "$crew_id" ] || missing="${missing:+$missing }crew_id"
[ -z "$missing" ] || {
  echo "dispatch resume: $task_doc header is missing: $missing — pass --agent/--model/--effort explicitly, or re-dispatch this branch." >&2
  exit 1
}

case "$agent" in
claude | codex | cursor) ;;
*)
  echo "dispatch resume: unknown engine '$agent' in the task header — pass --agent claude|codex|cursor" >&2
  exit 1
  ;;
esac

if [ -n "$do_print" ]; then
  printf 'branch: %s\nworktree: %s\nengine: %s\nmodel: %s\neffort: %s\nmcp: %s\ntier: %s\ncrew_id: %s\nagent_name: %s\nprev_worker_id: %s\ncontinue: %s\n' \
    "$branch" "$wt_path" "$agent" "$model" "$effort" "$mcp_profile" \
    "$tier" "$crew_id" "$agent_name" "$prev_worker_id" \
    "$([ -n "$fresh" ] && echo false || echo true)"
  exit 0
fi
```

- [ ] **Step 4: Intercept the subcommand in `dispatch.sh`**

In `adapters/core/dispatch.sh`, immediately before `tier="${1:-}"` (line 30), insert:

```bash
# `dispatch resume` is its own binary — resume skips the issue claim, branch
# creation, task-document rewrite and new-window paths this file is built
# around. Intercepted here so the subcommand reads as part of dispatch, and
# before the positional tier parse below, which would reject it as a tier.
if [ "${1:-}" = resume ]; then
  shift
  exec dispatch-resume "$@"
fi
```

And extend the usage line (`dispatch.sh:10`) by appending to the existing string, before the closing quote:

```
\n       dispatch resume [--agent E] [--model M] [--effort E] [--mcp P] [--fresh] [--print] [extra prompt...]
```

- [ ] **Step 5: Add the Nix package**

In `flake.nix`, after the `dispatch` package block (`flake.nix:115-120`), add:

```nix
          # `dispatch` is deliberately NOT in runtimeInputs: dispatch lists
          # dispatch-resume (for the exec below), so naming it here would be an
          # infinite recursion at eval time. Task 5 calls `dispatch` for its
          # gate precheck and resolves it from the ambient PATH, the same way
          # dispatch leaves `wt` ambient.
          dispatch-resume = pkgs.writeShellApplication {
            name = "dispatch-resume";
            runtimeInputs = (with pkgs; [gh git jq gnused gnugrep coreutils tmux]) ++ [crew];
            text = sub (builtins.readFile ./adapters/core/dispatch-resume.sh);
          };
```

Add it to `dispatch`'s `runtimeInputs` so the `exec` resolves — change `flake.nix:118` from:

```nix
            runtimeInputs = (with pkgs; [gh git jq gnused coreutils tmux direnv]) ++ [crew];
```

to:

```nix
            runtimeInputs = (with pkgs; [gh git jq gnused coreutils tmux direnv]) ++ [crew dispatch-resume];
```

And add it to the aggregate `paths` (`flake.nix:148`):

```nix
            paths = [crew dispatch dispatch-resume dispatcher refresh-scores refresh-budget refresh-models pr-watch];
```

- [ ] **Step 6: Put it on PATH via the module**

In `nix/hm-module.nix:41`, change:

```nix
      packages = [pkgsFor.crew pkgsFor.dispatch pkgsFor.dispatcher pkgsFor.refresh-scores pkgsFor.refresh-budget pkgsFor.refresh-models pkgsFor.pr-watch];
```

to:

```nix
      packages = [pkgsFor.crew pkgsFor.dispatch pkgsFor.dispatch-resume pkgsFor.dispatcher pkgsFor.refresh-scores pkgsFor.refresh-budget pkgsFor.refresh-models pkgsFor.pr-watch];
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `bats tests/dispatch-resume.bats`
Expected: all seven PASS.

- [ ] **Step 8: Verify formatting, lint, and that the module still evaluates**

Run: `shellcheck adapters/core/dispatch-resume.sh`
Expected: no output.

Run: `nix run nixpkgs#shfmt -- -d -i 2 adapters/core/dispatch-resume.sh`
Expected: no diff. `dispatch-resume.sh` is not treefmt-excluded, so a diff here fails CI.

Run: `bats tests/module.bats`
Expected: PASS — it forces the module config body, which the new package reference must survive.

- [ ] **Step 9: Commit**

```bash
git add adapters/core/dispatch-resume.sh adapters/core/dispatch.sh flake.nix nix/hm-module.nix tests/dispatch-resume.bats
git commit -m "feat(dispatch): dispatch resume resolves a worker from its worktree

Reads the launch tuple back from WORKER_TASK.md instead of having it retyped,
and refuses the states where a resume is meaningless: the primary worktree, a
detached HEAD, a tree with no task document, a header without the tuple.
--print reports what a resume would launch without launching it."
```

---

### Task 4: Placement — reuse the pane already at the worktree

**Files:**

- Modify: `adapters/core/dispatch-resume.sh`
- Test: `tests/dispatch-resume.bats`

**Interfaces:**

- Consumes: `wt_path`, `branch`, `agent_name`, `do_print` from Task 3.
- Produces: `win` and `pane` (tmux ids), and `reused` (`1` when an existing pane was adopted, empty when a window was created). Task 6 sends keys to `$pane`; Task 7 reads `reused`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch-resume.bats`:

```bash
# tmux stub that reports one pane sitting at $WT, so the reuse path fires.
stub_tmux_with_pane_at_wt() { # $1=window id  $2=pane id  $3=@crew_name value
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1" in
list-panes) printf '%s\t%s\t%s\t%s\n' '$1' '$2' '$WT' '$3' ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

@test "--print names the existing pane at the worktree" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"window: @4"* ]]
  [[ "$output" == *"pane: %8"* ]]
  [[ "$output" == *"placement: reuse"* ]]
}

@test "--print reuses a pane with no worker identity (a human sitting there)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' ''
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"pane: %8"* ]]
  [[ "$output" == *"placement: reuse"* ]]
}

@test "--print reports a fresh window when nothing sits at the worktree" {
  setup_worker_wt
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-panes) : ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"placement: create"* ]]
}

@test "creating a window stamps the crew identity on it" {
  setup_worker_wt
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-panes) : ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'set-window-option -t %99 @crew_name iris' "$STUB_LOG"
}

@test "reusing a pane does not open a window" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c new-window "$STUB_LOG"
  [ "$status" -ne 0 ]
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch-resume.bats -f placement`
Expected: FAIL — `--print` emits no `window:`/`pane:`/`placement:` lines.

- [ ] **Step 3: Resolve the target pane**

In `adapters/core/dispatch-resume.sh`, insert this **before** the `if [ -n "$do_print" ]` block from Task 3:

```bash
# Placement. dispatch always opens a fresh window and refuses when anything
# already sits at the worktree — including a pane with an empty @crew_name,
# i.e. a human in a plain shell, which is exactly whoever runs this command.
# Inheriting that would refuse the primary use case, so resume reuses the pane
# that is already there and gives up the anti-stacking refusal in trade.
#
# Keyed on pane_current_path, not the window name: lazytmux renames worker
# windows, so the name dispatch assigned is long gone by now.
win=""
pane=""
reused=""
while IFS=$'\t' read -r cand_win cand_pane cand_path _cand_name; do
  [ -n "$cand_win" ] || continue
  [ "$cand_path" = "$wt_path" ] || continue
  win="$cand_win"
  pane="$cand_pane"
  reused=1
  break
done <<PANES
$(tmux list-panes -a -F '#{window_id}	#{pane_id}	#{pane_current_path}	#{@crew_name}' 2>/dev/null || true)
PANES

if [ -z "$pane" ]; then
  sanitized="${branch//\//-}"
  # Same client-geometry handling as dispatch: a detached new-window otherwise
  # inherits tmux's fallback size, and codex's startup banner never redraws.
  client_target=()
  [ -n "${TMUX_PANE:-}" ] && client_target=(-t "$TMUX_PANE")
  client_size="$(tmux display-message -p "${client_target[@]}" '#{client_width} #{client_height} #{status}' 2>/dev/null || true)"
  client_width=""
  client_height=""
  if [[ $client_size =~ ^([1-9][0-9]*)[[:space:]]+([1-9][0-9]*)[[:space:]]+(off|on|[0-9]+)$ ]]; then
    client_width="${BASH_REMATCH[1]}"
    client_height="${BASH_REMATCH[2]}"
    case "${BASH_REMATCH[3]}" in
    off) status_rows=0 ;;
    on) status_rows=1 ;;
    *) status_rows="${BASH_REMATCH[3]}" ;;
    esac
    client_height=$((client_height - status_rows))
    if ((client_height <= 0)); then
      client_width=""
      client_height=""
    fi
  fi
  read -r win pane < <(tmux new-window -d -c "$wt_path" -n "$sanitized" -P -F '#{window_id} #{pane_id}')
  if [ -n "$client_width" ]; then
    tmux resize-window -t "$win" -x "$client_width" -y "$client_height"
  fi
fi

if [ -z "$pane" ]; then
  echo "dispatch resume: could not resolve a tmux pane for $wt_path — is tmux running?" >&2
  exit 1
fi

# Identity surfaces. Re-stamped on both paths: a hand-made window carries none,
# and a reused worker window may have been renamed since.
agent_color="$(crew identity "$branch" | jq -r .tmux)"
tmux set-window-option -t "$win" @crew_name "$agent_name"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold] "
```

**Ordering — the `--print` block moves.** The read-only pane lookup runs
first; then `--print` reports and exits; and only a real run creates the window
and stamps identity. Putting creation before the dry-run exit makes
`dispatch resume --print` open a real tmux window when nothing sits at the
worktree, and restyle whatever pane it found — including a human's shell — which
contradicts what `--print` is for. So the file reads:

1. read-only lookup of a pane whose `pane_current_path` is the worktree
2. `if [ -n "$do_print" ]` → print and `exit 0`
3. `tmux new-window` + `resize-window` when the lookup found nothing
4. the `@crew_name` / `@crew_color` / border stamping
5. the "could not resolve a pane" guard

On the create path `--print` has no window or pane id to report yet, so it
reports them as `-` and relies on `placement: create` to say what would happen.

Then extend the `--print` block's printf — change its format string and arguments to add the three placement lines:

```bash
  printf 'branch: %s\nworktree: %s\nengine: %s\nmodel: %s\neffort: %s\nmcp: %s\ntier: %s\ncrew_id: %s\nagent_name: %s\nprev_worker_id: %s\ncontinue: %s\nwindow: %s\npane: %s\nplacement: %s\n' \
    "$branch" "$wt_path" "$agent" "$model" "$effort" "$mcp_profile" \
    "$tier" "$crew_id" "$agent_name" "$prev_worker_id" \
    "$([ -n "$fresh" ] && echo false || echo true)" \
    "$win" "$pane" "$([ -n "$reused" ] && echo reuse || echo create)"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `bats tests/dispatch-resume.bats`
Expected: all PASS.

Note the `crew identity` call: the generic `crew` stub from `setup()` prints nothing, so `jq -r .tmux` yields empty and `agent_color` is blank — harmless for these assertions. If a test needs a real colour, give it the `identity`-aware stub `tests/dispatch.bats:stub_launch_bins` uses.

- [ ] **Step 5: Lint and format**

Run: `shellcheck adapters/core/dispatch-resume.sh && nix run nixpkgs#shfmt -- -d -i 2 adapters/core/dispatch-resume.sh`
Expected: no output, no diff.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/dispatch-resume.sh tests/dispatch-resume.bats
git commit -m "feat(dispatch): resume reuses the pane already at the worktree

dispatch refuses when any pane sits at the target tree, a human in a plain
shell included — which is exactly this command's caller. Resume adopts that
pane instead, and only opens a window when the worktree has none."
```

---

### Task 5: Gates — reuse dispatch's own pre-scaffold checks

The spec requires a resume to re-run the profile, effort-ceiling, model-shape and budget/rung gates, and to skip the tier↔model map unless a model is named. Those live in `dispatch.sh:211-478` and must not be copied: the budget gate alone reads a cache with staleness rules, and a second copy would drift.

`dispatch.sh` runs **only pure validation** before its first side effect — `_ensure_dispatched_label` is called at `:526` and `:623`, `crew reap` at `:564`, and the `slug`/`crew_dir` work starts at `:503`. So an early exit placed just before the slug line has run every gate and touched nothing.

**Files:**

- Modify: `adapters/core/dispatch.sh` (early exit before the `# slug:` line at `:503`), `adapters/core/dispatch-resume.sh`
- Test: `tests/dispatch-resume.bats`

**Interfaces:**

- Consumes: `agent`, `model`, `effort`, `tier`, `crew_id`, `mcp_profile`, `model_flag`, `ignore_budget`, `ignore_map` from Task 3.
- Produces: `profile` (resolved `$DISPATCH_PROFILE`, default `personal`), used by Task 6's `xreview_mcp`.

Insert resume's part **before** Task 4's placement block, so a refusal costs no window.

- [ ] **Step 1: Write the failing tests**

Add a `dispatch` stub to `tests/dispatch-resume.bats`'s `setup()`, after the existing `stub_bin` calls. The gates themselves are already covered by `tests/dispatch.bats`; these tests assert that resume _invokes_ the precheck correctly and honours its verdict.

```bash
  cat >"$STUB_DIR/dispatch" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
[ -n "${DISPATCH_PRECHECK:-}" ] || exit 0
[ -z "${STUB_PRECHECK_FAIL:-}" ] || {
  echo "dispatch: --effort ultra is codex-only; claude tops out at max" >&2
  exit 1
}
exit 0
EOF
  chmod +x "$STUB_DIR/dispatch"
```

Then append:

```bash
@test "runs the dispatch precheck with the recorded tuple" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qE 'standard sonnet .*--effort medium' "$STUB_LOG"
  grep -q -- '--agent claude' "$STUB_LOG"
  grep -q -- '--crew-id c1' "$STUB_LOG"
}

@test "suppresses the tier-model map when no model was named" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- '--ignore-map' "$STUB_LOG"
}

@test "re-arms the tier-model map when --model is passed" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run grep -c -- '--ignore-map' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "forwards --ignore-budget to the precheck" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --ignore-budget
  [ "$status" -eq 0 ]
  grep -q -- '--ignore-budget' "$STUB_LOG"
}

@test "a refused precheck aborts before any launch" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  STUB_PRECHECK_FAIL=1 run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"ultra is codex-only"* ]]
  run grep -c send-keys "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "refuses an mcp profile on a non-claude engine" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^mcp: $/mcp: analytics/' "$WT/WORKER_TASK.md"
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"claude-only"* ]]
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch-resume.bats -f precheck`
Expected: FAIL — resume never invokes `dispatch`, so `$STUB_LOG` has no such line.

- [ ] **Step 3: Add the precheck exit to `dispatch.sh`**

In `adapters/core/dispatch.sh`, immediately **before** the `# slug: lowercase, non-alnum -> single dash...` comment at line 503, insert:

```bash
# Pre-scaffold gate check for `dispatch resume`, which re-runs the gates that
# are properties of now — profile, model shape, effort ceiling, quota, rung —
# rather than re-deriving them in a second copy that would drift. Everything
# above this point is pure validation: `_ensure_dispatched_label` and
# `crew reap` are below, as is the first string of scaffolding, so exiting
# here has no side effects. Resume suppresses the tier↔model gate for a pair
# the first dispatch already accepted by passing the existing --ignore-map.
if [ -n "${DISPATCH_PRECHECK:-}" ]; then
  exit 0
fi
```

- [ ] **Step 4: Call it from `dispatch-resume.sh`**

Insert into `adapters/core/dispatch-resume.sh`, after the engine validation from Task 3:

```bash
profile="${DISPATCH_PROFILE:-personal}"

# mcp is claude-only, and this is the one gate the precheck below cannot make:
# passing --mcp there would have dispatch resolve and validate the config file
# too, which Task 6 must do anyway to build the launch flag.
if [ "$agent" != claude ] && [ -n "$mcp_profile" ]; then
  echo "dispatch resume: mcp is claude-only; codex/cursor base MCP comes from their own profile" >&2
  exit 1
fi

# Every other pre-scaffold gate is dispatch's, run through its precheck exit
# so there is exactly one copy of the profile, model-shape, effort-ceiling,
# budget and rung rules. `dispatch` resolves from the ambient PATH: it lists
# dispatch-resume in runtimeInputs for the `resume` exec, so naming it in ours
# would be an eval-time cycle.
command -v dispatch >/dev/null 2>&1 || {
  echo "dispatch resume: dispatch is not on PATH — both are installed together by the home-manager module" >&2
  exit 1
}
precheck=(--effort "$effort" --agent "$agent" --crew-id "$crew_id")
[ -n "$ignore_budget" ] && precheck+=(--ignore-budget)
# The tier↔model pair was adjudicated when this worker was first dispatched;
# only an explicit --model is a fresh choice that deserves re-gating.
if [ -z "$model_flag" ] || [ -n "$ignore_map" ]; then
  precheck+=(--ignore-map)
fi
DISPATCH_PRECHECK=1 dispatch "$tier" "$model" "${precheck[@]}" "resume precheck" || exit 1
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bats tests/dispatch-resume.bats`
Expected: all PASS.

- [ ] **Step 6: Confirm the precheck really is side-effect free**

Run: `bats tests/dispatch.bats`
Expected: all PASS — nothing above the new exit changed.

Run this manual check in the worktree, which proves the claim the exit rests on:

```bash
grep -n '_ensure_dispatched_label$\|crew reap\|^slug=' adapters/core/dispatch.sh
```

Expected: every hit is at a line number **greater** than the line you inserted the exit at. If any is smaller, the exit is in the wrong place — move it up.

- [ ] **Step 7: Lint, format, full suite**

Run: `shellcheck adapters/core/dispatch-resume.sh adapters/core/dispatch.sh`
Expected: no output.

Run: `nix run nixpkgs#shfmt -- -d -i 2 adapters/core/dispatch-resume.sh && bats tests/`
Expected: no diff, all pass.

- [ ] **Step 8: Commit**

````bash
git add adapters/core/dispatch-resume.sh adapters/core/dispatch.sh tests/dispatch-resume.bats
git commit -m "feat(dispatch): resume reuses dispatch's own pre-scaffold gates

Profile, model shape, effort ceiling, quota and rung are properties of now, so
a resume re-runs them — through a DISPATCH_PRECHECK exit in dispatch rather
than a second copy that would drift. The tier-model pair was adjudicated at
first dispatch, so resume passes --ignore-map unless a model is named."

### Task 6: Launch — continue the engine's own session

**Files:**

- Modify: `adapters/core/dispatch-resume.sh`
- Test: `tests/dispatch-resume.bats`

**Interfaces:**

- Consumes: `pane`, `agent`, `model`, `effort`, `mcp_profile`, `tier`, `plan_val`, `agent_name`, `profile`, `fresh`, `extra`, `PROTOCOL_DIR`.
- Produces: the launch is sent. `session` and `worker_id` (the new bus identity) for Task 7.

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch-resume.bats`:

```bash
@test "claude resume launches with --continue and the recorded tuple" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'send-keys -t %8 claude --continue' "$STUB_LOG"
  grep -q -- '--model sonnet' "$STUB_LOG"
  grep -q -- '--effort medium' "$STUB_LOG"
  grep -q -- '--append-system-prompt-file /opt/protocols/WORKER_PROTOCOL.md' "$STUB_LOG"
}

@test "--fresh drops the continue flag" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -q 'send-keys -t %8 claude ' "$STUB_LOG"
  run grep -c -- '--continue' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "codex resume launches resume --last" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'codex resume --last' "$STUB_LOG"
  grep -q -- '--profile worker' "$STUB_LOG"
}

@test "cursor resume launches with --continue" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: cursor/' -e 's/^model: sonnet/model: composer-2.5/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'cursor-agent --continue' "$STUB_LOG"
}

@test "the reorient prompt tells the worker not to trust its last plan" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'do not trust your transcript' "$STUB_LOG"
}

@test "trailing arguments are appended to the prompt" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume the review comments are the priority
  [ "$status" -eq 0 ]
  grep -q 'the review comments are the priority' "$STUB_LOG"
}

@test "no launch string contains an apostrophe" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c "send-keys.*'.*'.*'" "$STUB_LOG"
  [ "$status" -ne 0 ]
}
````

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch-resume.bats -f launch`
Expected: FAIL — nothing sends keys.

- [ ] **Step 3: Build the prompt and send the launch**

Append to `adapters/core/dispatch-resume.sh`. The first three assignments are
the launch parameters Task 3 deliberately did not declare: a variable assigned
before the task that reads it needs an SC2034 waiver to survive
`writeShellApplication`'s build-time shellcheck, and this repo forbids
silencing a checker. They land here, with their reader.

```bash
PROTOCOL_DIR="${DISPATCHER_PROTOCOL_DIR:-@protocolDir@}"
kind="$(_hdr kind)"
plan_val="$(_hdr plan)"

# Session identity. A resume gets a NEW session id and therefore a new
# worker_id: the pane, the watchdog and the bus rows are all new even when the
# conversation is not. dispatch.sh:832 owns the same shape.
session="${DISPATCH_SESSION_ID:-s$(date +%s)-$$}"
worker_id="worker:$branch#$session"

# The reorient prompt. dispatch's own resume_note sends a worker to SPEC.md and
# PLAN.md because it has no transcript to stand on; with the conversation
# restored the risk inverts, and the danger is trusting a stale last plan and
# redoing finished work. --fresh keeps dispatch's wording, since a fresh launch
# is exactly the no-transcript case that note was written for.
# No apostrophes anywhere in these strings.
if [ -n "$fresh" ]; then
  reorient=" You are resuming an interrupted run on this branch, not starting it: do not re-run the spec or plan phases. Read SPEC.md and PLAN.md (repo root or docs/superpowers/) and git status before anything else, then continue from the first unfinished step. Check whether this branch already has an open PR before you push, and push to that PR instead of opening a second one."
else
  reorient=" You were interrupted mid-task and this session has been resumed. Before anything else, establish where you actually got to from git log, git status and any open PR on this branch — do not trust your transcript's last plan as your current position. Then continue from the first genuinely unfinished step. If this branch already has an open PR, push to it rather than opening a second one."
fi
reorient="${reorient//\'/}"
[ -n "$extra" ] && reorient="$reorient ${extra//\'/}"

plan_note=""
if [ "$plan_val" = provided ]; then
  plan_note=" The task doc is your plan of record — extract the steps and implement; do not re-plan or re-critique it."
fi

push_mandate=" Push when pre-push passes; open a PR."
if [ "$kind" = review ]; then
  push_mandate=" Review only — do not edit, commit, push, or open a PR; post one COMMENT review and report to the bus."
fi

mcp_arg=""
if [ -n "$mcp_profile" ]; then
  case "$mcp_profile" in
  analytics) mcp_file="$HOME/.config/claude-code/mcp-posthog.json" ;;
  *)
    echo "dispatch resume: unknown mcp profile '$mcp_profile' (valid: analytics)" >&2
    exit 1
    ;;
  esac
  [ -f "$mcp_file" ] || {
    echo "dispatch resume: mcp $mcp_profile config not found at $mcp_file" >&2
    exit 1
  }
  mcp_arg="--mcp-config $mcp_file"
fi

xreview_mcp=""
if [ "$profile" = work ] && [ "$agent" = claude ] && [ "$tier" = deep ]; then
  xreview_mcp="--mcp-config $HOME/.config/claude-code/mcp-codex.json"
fi

if [ "$agent" = codex ]; then
  cont="resume --last"
  [ -n "$fresh" ] && cont=""
  tmux send-keys -t "$pane" \
    "codex $cont --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default --dangerously-bypass-approvals-and-sandbox 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}'" Enter
elif [ "$agent" = cursor ]; then
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  tmux send-keys -t "$pane" \
    "CURSOR_CLI_INDEXED_GREP=0 cursor-agent $cont --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}'" Enter
else
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  # Re-passing --append-system-prompt-file matters on a continue: it forces
  # --system-prompt-snapshot off, so WORKER_PROTOCOL.md is applied fresh rather
  # than replayed from the conversation's recorded prompt.
  tmux send-keys -t "$pane" \
    "claude $cont --name $agent_name --model $model --effort $effort $mcp_arg $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and continue it.${push_mandate}${plan_note}${reorient}'" Enter
fi

echo "worker_id: $worker_id"
```

- [ ] **Step 4: Assert the protocol path is substituted at build time**

`PROTOCOL_DIR` reintroduces the `@protocolDir@` placeholder, and the flake's
`sub` replacer is a silent no-op on a file that has none — so nothing would
catch a missing substitution. `tests/module.bats` already carries this
assertion for `dispatch` and `dispatcher`; add the third, mirroring them
exactly (read the two existing ones and follow their shape rather than
inventing a new one).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bats tests/dispatch-resume.bats`
Expected: all PASS.

Run: `bats tests/module.bats`
Expected: all PASS, including your new substitution assertion. This is the
test that actually compiles the derivation — `nix flake check` reports the
packages as "build skipped" — so it is also the only build-time shellcheck
gate on the new file.

- [ ] **Step 6: Lint, format**

Run: `shellcheck adapters/core/dispatch-resume.sh && nix run nixpkgs#shfmt -- -d -i 2 adapters/core/dispatch-resume.sh`
Expected: no output, no diff.

- [ ] **Step 7: Commit**

```bash
git add adapters/core/dispatch-resume.sh tests/dispatch-resume.bats
git commit -m "feat(dispatch): resume continues the engine's own session

claude --continue, codex resume --last, cursor-agent --continue. The reorient
prompt inverts dispatch's resume note: with a transcript restored the risk is
trusting a stale last plan, not lacking one. --fresh keeps the old wording."
```

---

### Task 7: The bus — new identity, resume row, and reattach or run solo

**Files:**

- Modify: `adapters/core/dispatch-resume.sh`
- Test: `tests/dispatch-resume.bats`

**Interfaces:**

- Consumes: `crew_id`, `branch`, `worker_id`, `prev_worker_id`, `session`, `agent`, `model`, `fresh`, `pane`, `task_doc`, `reused`.
- Produces: nothing for later tasks.

- [ ] **Step 1: Write the failing tests**

Append to `tests/dispatch-resume.bats`:

```bash
bus_log() { printf '%s/.git/crew/events.jsonl' "$TEST_REPO"; }

@test "writes a resume row naming both worker identities" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  row="$(jq -c 'select(.kind == "resume")' "$(bus_log)" | tail -1)"
  [ "$(jq -r .crew_id <<<"$row")" = c1 ]
  [ "$(jq -r .branch <<<"$row")" = feat/7-a-thing ]
  [ "$(jq -r .prev_worker_id <<<"$row")" = 'worker:feat/7-a-thing#s1-99' ]
  [ "$(jq -r .continued <<<"$row")" = true ]
  [ "$(jq -r .engine <<<"$row")" = claude ]
}

@test "--fresh records continued false" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  [ "$(jq -r 'select(.kind == "resume") | .continued' "$(bus_log)" | tail -1)" = false ]
}

@test "posts a working status under the NEW worker id" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qE "status worker:feat/7-a-thing#s[0-9]+-[0-9]+ working resumed" "$STUB_LOG"
}

@test "updates worker_id in the task doc and leaves the body alone" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  ! grep -q 'worker_id: worker:feat/7-a-thing#s1-99' "$WT/WORKER_TASK.md"
  grep -qE '^worker_id: worker:feat/7-a-thing#s[0-9]+-[0-9]+$' "$WT/WORKER_TASK.md"
  grep -qx 'the original body' "$WT/WORKER_TASK.md"
  grep -qx 'title: a thing' "$WT/WORKER_TASK.md"
}

@test "runs solo when the crew has no registered dispatcher" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"solo"* ]]
  run grep -c 'crew msg' "$STUB_LOG"
  [ "$status" -ne 0 ]
  grep -qx 'dispatcher_pane: %3' "$WT/WORKER_TASK.md"
}

@test "reattaches to a live dispatcher: retargets the pane and messages it" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '%s\n' "$$" >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"reattached"* ]]
  grep -qx 'dispatcher_pane: %77' "$WT/WORKER_TASK.md"
  grep -q 'msg .* dispatcher:c1' "$STUB_LOG"
}

@test "runs solo when the registered dispatcher pid is dead" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '999999999\n' >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"solo"* ]]
  grep -qx 'dispatcher_pane: %3' "$WT/WORKER_TASK.md"
}

@test "re-arms the stall watchdog on the resumed pane" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qE 'stall-watch worker:feat/7-a-thing#s[0-9]+-[0-9]+ --pane %8 --engine claude' "$STUB_LOG"
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats tests/dispatch-resume.bats -f 'resume row'`
Expected: FAIL — no bus row is written.

- [ ] **Step 3: Add the bus writes, the identity rewrite, and the liveness probe**

Insert into `adapters/core/dispatch-resume.sh`, **between** the identity/`worker_id` assignment and the prompt construction from Task 6:

```bash
crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
mkdir -p "$crew_dir"

# crew.sh's atomic-append helper, duplicated for the same reason dispatch.sh
# duplicates it: this file builds as its own writeShellApplication with no
# shared lib, and a bare `printf >>` is not one write(2) (#55, #61).
_bus_append() { printf '%s\n' "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"; }

# Dispatcher liveness. `crew register` writes the pid, and `crew deregister`
# removes the whole directory on a clean exit — so absent means gone, a dead
# pid means it crashed, and only a live pid is a dispatcher still watching.
# A resume never mints or adopts a crew: it keeps posting under the crew_id in
# the task document, which is what `crew adopt` is for on the other side.
dispatcher_live=""
dispatcher_pane_new=""
cdir="$crew_dir/crews/$crew_id"
if [ -d "$cdir" ]; then
  epid="$(cat "$cdir/pid" 2>/dev/null || true)"
  case "$epid" in
  '' | *[!0-9]* | 0) ;;
  *)
    if kill -0 "$epid" 2>/dev/null; then
      dispatcher_live=1
      dispatcher_pane_new="$(cat "$cdir/pane" 2>/dev/null || true)"
    fi
    ;;
  esac
fi

# Rewrite exactly two header lines in place, never the whole document: the
# worker may have been handed a spec, and this header is the record we just
# read. worker_id MUST move — it carries the session, so leaving the old one
# would have the worker post under a dead bus identity.
_hdr_set() { # $1=field  $2=value
  awk -v f="$1" -v v="$2" '
    !done && $0 ~ "^" f ": " { print f ": " v; done = 1; next }
    { print }
  ' "$task_doc" >"$task_doc.tmp" && mv "$task_doc.tmp" "$task_doc"
}
_hdr_set worker_id "$worker_id"
if [ -n "$dispatcher_live" ] && [ -n "$dispatcher_pane_new" ]; then
  _hdr_set dispatcher_pane "$dispatcher_pane_new"
fi

# The resume row. New kind: without it a worker resumed four times reports as
# one run, and the ratings rollup attributes the whole cost and latency to a
# single session. prev_worker_id is what chains the sessions back together.
line=$(jq -nc --arg crew "$crew_id" --arg branch "$branch" \
  --arg worker "$worker_id" --arg prev "$prev_worker_id" \
  --arg engine "$agent" --arg model "$model" --arg session "$session" \
  --argjson continued "$([ -n "$fresh" ] && echo false || echo true)" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"resume", branch:$branch,
     worker_id:$worker, prev_worker_id:$prev, engine:$engine, model:$model,
     session:$session, continued:$continued}')
_bus_append "$crew_dir/events.jsonl" "$line"

# Clears a stale exited/failed/done roster row so the crew reads as live again.
CREW_ID="$crew_id" crew status "$worker_id" working resumed || true

# A live dispatcher is told, deliberately. `crew watch` wakes on a message to
# the dispatcher but its default --states exclude `working`, so a status post
# alone would leave a dispatcher that wrote this worker off as failed still
# believing it dead — and free to re-dispatch the task onto this branch.
if [ -n "$dispatcher_live" ]; then
  CREW_ID="$crew_id" crew msg "$worker_id" "dispatcher:$crew_id" \
    "resumed on $branch (engine $agent, model $model) — this worker is live again, do not re-dispatch it" || true
  echo "dispatcher: reattached to live crew $crew_id"
else
  echo "dispatcher: none live for crew $crew_id — running solo (a later dispatcher can 'crew adopt $crew_id')"
fi
```

- [ ] **Step 4: Re-arm the watchdog**

Append to the very end of `adapters/core/dispatch-resume.sh`, after the launch block from Task 6:

```bash
# Re-arm the stall watchdog: the original self-exited when it saw the terminal
# state, and a resumed worker can wedge exactly the same way.
CREW_ID="$crew_id" nohup crew stall-watch "$worker_id" --pane "$pane" --engine "$agent" >/dev/null 2>&1 &
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bats tests/dispatch-resume.bats`
Expected: all PASS.

- [ ] **Step 6: Lint, format, full suite**

Run: `shellcheck adapters/core/dispatch-resume.sh && nix run nixpkgs#shfmt -- -d -i 2 adapters/core/dispatch-resume.sh && bats tests/`
Expected: no output, no diff, all pass.

- [ ] **Step 7: Commit**

```bash
git add adapters/core/dispatch-resume.sh tests/dispatch-resume.bats
git commit -m "feat(dispatch): resume reports to the bus and reattaches or runs solo

A resume mints a new session, so worker_id moves and the task doc's copy is
rewritten with it — otherwise the worker posts under a dead identity. A live
dispatcher is messaged, not just status-posted: crew watch's default states
exclude working, so it would otherwise still believe this worker is dead."
```

---

### Task 8: Document the surface

**Files:**

- Modify: `README.md`, `adapters/core/protocols/DISPATCHER_PROTOCOL.md`, `adapters/core/protocols/WORKER_PROTOCOL.md`
- Test: `tests/adapters.bats` (drift gate, already present)

**Interfaces:**

- Consumes: the finished command.
- Produces: nothing.

- [ ] **Step 1: Add the command to the README usage section**

In `README.md`, the "Usage" section lists the in-session dispatch forms in a fenced block at lines 248-252. After that block (and before the "Four commands ship with the plugin" paragraph at line 254), add:

````markdown
Resuming a worker, from inside its own worktree — reads the engine, model,
effort and crew back from `WORKER_TASK.md` and continues the engine's own
session:

```bash
dispatch resume                     # continue this worktree's worker
dispatch resume --print             # show what a resume would launch
dispatch resume --fresh             # relaunch without the prior conversation
dispatch resume the review comments are the priority
```
````

- [ ] **Step 2: Add it to the dispatcher protocol's recovery guidance**

In `adapters/core/protocols/DISPATCHER_PROTOCOL.md`, find the paragraph describing `dispatch` as the dumb mechanism (line 115) and add after it:

```markdown
To restart a worker that died mid-task, prefer `dispatch resume` run in that
worker's worktree over a fresh `dispatch` on the same title: it continues the
engine's own session instead of making the worker rebuild its position from
`SPEC.md`, `PLAN.md` and `git status`, and it reads the engine/model/effort
tuple back from `WORKER_TASK.md` rather than having you restate it. A resumed
worker keeps its crew and posts a `resume` row to the bus; if you are live it
also messages you, so a worker you had written off as `failed` will tell you it
is back.
```

- [ ] **Step 3: Tell the worker its identity can change**

In `adapters/core/protocols/WORKER_PROTOCOL.md`, after the header-fields paragraph (line 11), add:

```markdown
If `resume: true` is set, this session is a continuation: your `worker_id:` was
rewritten when you were resumed, so re-read it from this file rather than
reusing one you remember from earlier in the conversation. Post your status
under the current value.
```

- [ ] **Step 4: Regenerate the adapters**

Run: `./scripts/gen-adapters.sh && git status --short adapters/`
Expected: the claude-code, codex and cursor protocol copies show as modified.

- [ ] **Step 5: Verify the drift gate**

Run: `bats tests/adapters.bats`
Expected: PASS. This is the check CI runs to prove committed adapter output matches a fresh generator run.

- [ ] **Step 6: Full verification**

Run: `bats tests/`
Expected: all pass.

Run: `nix flake check`
Expected: pass — formatting and pre-commit hooks over the whole tree.

- [ ] **Step 7: Commit**

```bash
git add README.md adapters/
git commit -m "docs: document dispatch resume in the README and both protocols

Includes the one thing a resumed worker must know: worker_id is rewritten on
resume, so it re-reads its bus identity rather than reusing a remembered one."
```

---

## Self-Review

**Spec coverage.** Every section of `2026-09-09-dispatch-resume-design.md` maps to a task: surface and resolution → Task 3; the `mcp:` gap → Task 1; "the task document is not rewritten" → Task 7 (in-place two-line edit, with the body asserted byte-identical in Task 3); placement → Task 4; gates (including the budget/rung gate, via dispatch's own precheck) → Task 5; dispatcher liveness and the `crew register` pane gap → Tasks 2 and 7; launch and the reorient prompt → Task 6; bus rows and the watchdog → Task 7; testing → each task's own steps; documentation → Task 8.

**One spec correction, made here.** The spec allowed rewriting only `dispatcher_pane:` and `resume:`. That is wrong: `worker_id="worker:$branch#$session"` (`dispatch.sh:832`), so a new session means a new `worker_id`, and leaving the recorded one stale would have the resumed worker post every status under a dead bus identity. Task 7 rewrites `worker_id:` too, Task 8 tells the worker this happens, and the spec should be amended to match. `resume:` needs no write — it is already `true` on any tree that reached here through a branch resume, and a tree that never did is not made more truthful by the flag.

**Two spec items deliberately not implemented.** The spec's refusal list includes "the default branch"; the primary-worktree check in Task 3 subsumes it without a `gh` call, since a worker's branch is never checked out in the main checkout. And the spec's "refuse when a _different_ live engine holds the tree" is left out: the reliable form of that check is the bus state, which `dispatch.sh:699-711` already reasons about at length, and reproducing it here would duplicate the trickiest gate in the codebase for a case Task 4's pane reuse already handles benignly (the launch lands in that pane, where the operator can see it). Worth revisiting if it bites.

**Placeholder scan.** No `TBD`/`TODO`/"handle edge cases"/"similar to Task N". Every code step carries its actual code; two steps (Task 2 Step 1, Task 8 Steps 1-3) point at existing file conventions to follow, and both name the exact file and line region to read first.

**Type consistency.** `_hdr` (Task 3) and `_hdr_set` (Task 7) are the only helpers; `_bus_append` matches `dispatch.sh:24` byte-for-byte. Variables introduced in Task 3 (`agent`, `model`, `effort`, `mcp_profile`, `tier`, `kind`, `plan_val`, `crew_id`, `agent_name`, `prev_worker_id`, `fresh`, `do_print`, `ignore_budget`, `ignore_map`, `model_flag`) are consumed under those exact names in Tasks 4-7. `win`/`pane`/`reused` (Task 4) and `session`/`worker_id`/`profile` (Tasks 5-6) likewise. The launch block uses `mcp_arg`, deliberately not `mcp_flag` — `dispatch.sh` calls its equivalent `mcp_flag`, but here that name would collide with the `--mcp` flag's own variable `mcp_flag_val`.

**Ordering constraint.** Tasks 3-7 all edit one file and must be done in order: Task 5's gates go before Task 4's placement block in the file (refuse before opening a window), and Task 7's bus writes go between Task 6's identity assignment and its prompt construction. Each task's insertion point is named relative to code the prior tasks created.
