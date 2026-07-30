# Native `dispatch` + Concurrent Crews Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `dispatch` from a fish function (needs `fish -c`) into a native PATH binary agents call directly, and lift the one-dispatcher-per-repo limit so multiple crews run concurrently.

**Architecture:** Move `dispatch`'s body out of a Nix-interpolated fish heredoc into a standalone `dispatch.sh` packaged with `writeShellApplication` (like `crew`). Crew id is delivered explicitly (`--crew-id` flag → `$CREW_ID` env → error) instead of via a `set -gx` shell-persistence trick. In `crew`, the three single-crew singletons (role lock, watch cursor, watch lock) become per-crew paths under `.git/crew/crews/<crew_id>/`, and the exclusive `lock-role` becomes a non-exclusive `register`.

**Tech Stack:** bash (nixpkgs bash 5.x via `writeShellApplication`), fish (launcher only), Nix / home-manager, `jq`, `tmux`, `gh`, `git`, worktrunk (`wt`), `crew`.

## Global Constraints

- **macOS-compatible:** rely on nixpkgs bash 5.x (never system bash 3.2) and declare GNU `coreutils`/`gnused` as `runtimeInputs`; avoid GNU-only flags where a portable form exists.
- **shellcheck-clean:** `writeShellApplication` runs shellcheck at build and fails on findings — every `.sh` must pass.
- **Crew id resolution order (verbatim):** `--crew-id <id>` → `$CREW_ID` → error. Never mint-and-persist via shell env.
- **Profile via runtime env, never baked:** the work-only engine gate reads `$DISPATCH_PROFILE`, set from `osConfig.profile` by home-manager; source must not interpolate `${osConfig.profile}` into logic.
- **One crew per dispatcher; N crews per repo.** Crew ids are unique-by-construction (`crew id` = `timestamp-pid`); no cross-crew locking.
- **Delete, don't preserve:** rename `lock-role`→`register` etc. and update all callers; leave no compat shims or old paths.
- **Commit style:** conventional commits, scope `dispatcher`; reference `#74`.
- **Rebuild command:** Linux `nh home switch`; macOS `nh darwin switch`. Untracked new files must be `git add`ed before a flake build sees them.

---

## File Structure

- **`home/ai/claude-code/crew.sh`** (modify) — bus CLI. Change `lock-role`/`unlock-role` → `register`/`deregister` (non-exclusive, per-crew dir + pid); repoint the watch cursor and watch lock to per-crew paths.
- **`home/ai/claude-code/dispatch.sh`** (create) — the ported native `dispatch`, read verbatim by `writeShellApplication`.
- **`home/ai/claude-code/default.nix`** (modify) — add the `dispatch` derivation next to `crew`, add it to `home.packages`, and set `home.sessionVariables.DISPATCH_PROFILE`.
- **`home/terminal/fish/default.nix`** (modify) — delete the `dispatch.fish` heredoc (lines ~199-405); update the `dispatcher.fish` launcher heredoc (`lock-role`→`register`, `unlock-role`→`deregister`).
- **`home/ai/claude-code/commands/dispatcher.md`** (modify) — drop the "fish function; use `fish -c`" note and the `lock-role` step; document minting a crew id, `crew register`, and `--crew-id`.
- **`home/ai/claude-code/DISPATCHER_PROTOCOL.md`** (modify) — drop "one dispatcher per repo"; the "per-crew cursor" wording now matches reality.

**Testing note (read once):** these tools are side-effect-heavy (they drive `gh`, `wt`, `tmux`) and the repo ships **no** shell-test harness for them (`crew` itself has none). Introducing bats is out of scope. So: `writeShellApplication`'s build-time shellcheck is the static gate; **pure validation/error paths** (which run before any side effect) are checked by invoking the tool with exact args and asserting exit code + stderr; **happy paths** (worktree/tmux/gh) are verified end-to-end in Task 4 with exact commands and expected observations. This is a deliberate, documented deviation from unit-test TDD, matching the existing pattern for these tools.

---

## Task 1: `crew` — per-crew registry, cursor, and watch lock

**Files:**

- Modify: `home/ai/claude-code/crew.sh` (the `lock-role | unlock-role)` case ~202-222; the `watch)` case cursor/lock ~303-311; the `usage:` string)

**Interfaces:**

- Produces: `crew register [pid]` (mkdir `.git/crew/crews/<crew_id>/`, write `pid`; idempotent, non-exclusive), `crew deregister` (rm the crew dir). Watch state lives at `.git/crew/crews/<crew_id>/cursor` and `.git/crew/crews/<crew_id>/watch.lock.d`.
- Consumes: existing `_crew_id()` (env `$CREW_ID` → `WORKER_TASK.md` crew_id) and `_lock_acquire`/`_lock_release` helpers, unchanged.

- [ ] **Step 1: Find every reference to the old singleton paths**

Run:

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
rg -n 'dispatcher\.lock\.d|dispatcher\.cursor|dispatcher\.watch\.lock\.d|lock-role|unlock-role' \
  home/ai/claude-code/ home/terminal/fish/default.nix
```

Expected: matches in `crew.sh` (this task), `dispatcher.fish` heredoc + `commands/dispatcher.md` (Task 3). No other files. If anything else appears (e.g. `dispatch-notify.sh`), add it to the relevant task's edit list before proceeding.

- [ ] **Step 2: Replace the exclusive role lock with a non-exclusive per-crew registry**

In `home/ai/claude-code/crew.sh`, replace the whole `lock-role | unlock-role)` case:

```sh
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

with:

```sh
register | deregister)
  # Per-crew registration (was an exclusive per-repo role lock). N crews may
  # share a repo: each is identified by its crew_id, so there is no
  # cross-crew contention and registration never refuses. The crew dir records
  # the dispatcher's long-lived PID (default $PPID) for stale reclaim, and
  # holds that crew's watch cursor + watch lock. Crew ids are unique by
  # construction (timestamp-pid), so re-registering a live crew is a no-op.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  cdir="$dir/crews/$crew"
  if [ "$sub" = register ]; then
    mkdir -p "$cdir"
    printf '%s\n' "${1:-$PPID}" >"$cdir/pid"
  else
    rm -rf "$cdir"
  fi
  ;;
```

- [ ] **Step 3: Repoint the watch cursor and watch lock to the per-crew dir**

In the `watch)` case, replace these lines (currently ~303 and ~309-313):

```sh
  cursor_file="$dir/dispatcher.cursor"
```

and

```sh
  wlock="$dir/dispatcher.watch.lock.d"
  _lock_acquire "$wlock" "$$" || {
    echo "crew: another dispatcher watch is already running for this crew (dispatcher.watch.lock.d)" >&2
    exit 1
  }
```

with (introduce `cdir` just before the cursor line, after `since` is resolved):

```sh
  cdir="$dir/crews/$crew"
  mkdir -p "$cdir"
  cursor_file="$cdir/cursor"
```

and

```sh
  wlock="$cdir/watch.lock.d"
  _lock_acquire "$wlock" "$$" || {
    echo "crew: another watch is already running for this crew ($crew)" >&2
    exit 1
  }
```

Leave the `mktemp "$dir/.cursor.XXXXXX"` + `mv -f "$tmp" "$cursor_file"` logic as-is — a rename within the same filesystem into the per-crew subdir is fine.

- [ ] **Step 4: Update the `usage:` string**

In the final `usage:` echo, change `ln [pid] | unln` (the `lock-role`/`unlock-role` tokens; they may render mangled in some tools — match on the real file text) to `register [pid] | deregister`.

- [ ] **Step 5: Static-check the edit by building the `crew` derivation**

Run:

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
git add home/ai/claude-code/crew.sh
nix build .#homeConfigurations.$(whoami)@$(hostname).activationPackage 2>&1 | tail -20 || \
  nix-instantiate --eval -E '1' >/dev/null  # fallback: ensure nix is reachable
```

Expected: build proceeds past `crew`'s shellcheck with no `crew.sh` findings. If shellcheck flags `crew.sh`, fix and re-run. (If the full activation build is too slow here, defer the full build to Task 4 and instead run `shellcheck` directly: `shellcheck home/ai/claude-code/crew.sh` → no output.)

- [ ] **Step 6: Behavioral check in a throwaway repo**

Run:

```bash
tmp=$(mktemp -d); git -C "$tmp" init -q; cd "$tmp"
export CREW_ID=test-$$
crew register 4242
cat "$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/$CREW_ID/pid"   # -> 4242
CREW_ID=other-$$ crew register 5555                                                        # second crew, NOT refused
ls "$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/"                  # -> two crew dirs
crew deregister
ls "$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/"                  # -> only other-$$
cd - >/dev/null; rm -rf "$tmp"; unset CREW_ID
```

Expected: first `cat` prints `4242`; the second `register` does **not** print the old "a dispatcher already holds…" refusal; two crew dirs exist concurrently; after `deregister` only the other crew remains. Use the freshly-built `crew` from the store if PATH still has the old one: `$(nix build --no-link --print-out-paths .#…)/bin/crew` — or defer this check to after Task 4's switch.

- [ ] **Step 7: Commit**

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
git add home/ai/claude-code/crew.sh
git commit -m "feat(dispatcher): per-crew registry, cursor, and watch lock in crew (#74)"
```

---

## Task 2: native `dispatch.sh` + packaging, remove the fish heredoc

**Files:**

- Create: `home/ai/claude-code/dispatch.sh`
- Modify: `home/ai/claude-code/default.nix` (let-block derivation ~24-28 area; `home.packages` line 212; add `home.sessionVariables`)
- Modify: `home/terminal/fish/default.nix` (delete the `"fish/functions/dispatch.fish".text` heredoc, ~199-405)

**Interfaces:**

- Consumes: `crew register`/`crew identity` (Task 1), `$CREW_ID`/`$DISPATCH_PROFILE`/`$DISPATCH_SPEC`/`$DISPATCH_SHAPE`/`$TMUX_PANE` env, `wt`, `gh`, `git`, `tmux`, `jq`.
- Produces: a `dispatch` binary on PATH with signature `dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh> [--agent claude|codex] [--mcp <profile>] [--crew-id <id>] [LINEAR-ID] <title...>`.

- [ ] **Step 1: Create `home/ai/claude-code/dispatch.sh`**

Write this file verbatim (it is the faithful bash port of the fish function, with the three required changes: `--crew-id`/env resolution, `$DISPATCH_PROFILE` gate, no `set -gx`):

```bash
# dispatch — scaffold a worker: issue/ticket -> worktree -> task file -> baked agent.
# Native PATH tool (was a fish autoload function). Crew id is delivered
# explicitly (--crew-id > $CREW_ID > error); a binary can't export env back to
# its caller, so the old `set -gx CREW_ID` persistence trick is gone.

usage() {
  echo "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh> [--agent claude|codex] [--mcp <profile>] [--crew-id <id>] [LINEAR-ID] <title...>" >&2
}

tier="${1:-}"
model="${2:-}"
case "$tier" in
  trivial | standard | deep) ;;
  *)
    usage
    exit 1
    ;;
esac
[ -n "$model" ] || {
  usage
  exit 1
}
shift 2

# Leading options before the free-form title, order-independent. LINEAR-ID is
# detected by shape so the bare <title...> form still works.
agent=claude
effort=""
linear_id=""
mcp_profile=""
crew_id_flag=""
while [ $# -gt 0 ]; do
  case "$1" in
    --agent)
      agent="${2:-}"
      case "$agent" in
        claude | codex) ;;
        *)
          echo "dispatch: --agent must be claude or codex" >&2
          exit 1
          ;;
      esac
      shift 2
      ;;
    --effort)
      effort="${2:-}"
      case "$effort" in
        low | medium | high | xhigh) ;;
        *)
          echo "dispatch: --effort must be low, medium, high, or xhigh" >&2
          exit 1
          ;;
      esac
      shift 2
      ;;
    --mcp)
      mcp_profile="${2:-}"
      [ -n "$mcp_profile" ] || {
        echo "dispatch: --mcp needs a profile (analytics)" >&2
        exit 1
      }
      shift 2
      ;;
    --crew-id)
      crew_id_flag="${2:-}"
      [ -n "$crew_id_flag" ] || {
        echo "dispatch: --crew-id needs a value" >&2
        exit 1
      }
      shift 2
      ;;
    *)
      if printf '%s' "$1" | grep -Eq '^[A-Z]{2,}-[0-9]+$'; then
        linear_id="$1"
        shift
      else
        break
      fi
      ;;
  esac
done

[ -n "$effort" ] || {
  echo "dispatch: --effort is required and must be judged independently from tier" >&2
  exit 1
}

# Crew id: explicit flag > inherited env > error. Launcher dispatchers inherit
# $CREW_ID from the claude process env; in-session dispatchers pass --crew-id.
crew_id="${crew_id_flag:-${CREW_ID:-}}"
[ -n "$crew_id" ] || {
  echo "dispatch: no crew id — pass --crew-id <id> (in-session) or run under a launcher/registered dispatcher (\$CREW_ID)" >&2
  exit 1
}

# Work-only engine gate. $DISPATCH_PROFILE is set from osConfig.profile by
# home-manager (was a source-baked literal in the fish heredoc). Reject before
# scaffolding a worktree, so the failure is a clear message not a later
# `codex: command not found`.
profile="${DISPATCH_PROFILE:-personal}"
if [ "$agent" = codex ] && [ "$profile" != work ]; then
  echo "dispatch: --agent codex is work-profile only (no personal codex account)" >&2
  exit 1
fi
if [ "$agent" = codex ] && [ -n "$mcp_profile" ]; then
  echo "dispatch: --mcp is claude-only; codex base MCP comes from --profile worker" >&2
  exit 1
fi

# Map an additive --mcp profile to its generated config (claude-only).
mcp_flag=""
if [ -n "$mcp_profile" ]; then
  case "$mcp_profile" in
    analytics) mcp_file="$HOME/.config/claude-code/mcp-posthog.json" ;;
    *)
      echo "dispatch: unknown --mcp profile '$mcp_profile' (valid: analytics)" >&2
      exit 1
      ;;
  esac
  [ -f "$mcp_file" ] || {
    echo "dispatch: --mcp $mcp_profile config not found at $mcp_file" >&2
    exit 1
  }
  mcp_flag="--mcp-config $mcp_file"
fi

title="$*"
[ -n "$title" ] || {
  usage
  exit 1
}

# slug: lowercase, non-alnum -> single dash, first 40 chars, strip edge dashes.
slug=$(printf '%s' "$title" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g' | cut -c1-40 | sed -E 's/^-+//; s/-+$//')

# Identity + closes line. Linear mode derives both from the ticket (no gh).
# GitHub mode mints an issue and aborts cleanly if that fails (issues disabled)
# rather than scaffolding a half-broken worker off an empty number.
if [ -n "$linear_id" ]; then
  branch="$(printf '%s' "$linear_id" | tr '[:upper:]' '[:lower:]')-$slug"
  closes="Closes $linear_id"
else
  url=$(gh issue create --assignee @me --title "$title" --body "Dispatched worker task." 2>/dev/null || true)
  num=$(printf '%s' "$url" | sed -nE 's#.*/([0-9]+)$#\1#p')
  [ -n "$num" ] || {
    echo "dispatch: could not create a GitHub issue (issues disabled?). Pass a Linear id, e.g. dispatch $tier $model ENG-1234 $title" >&2
    exit 1
  }
  branch="feat/$num-$slug"
  closes="Closes #$num"
fi

# Branch path must match worktrunk's {{ repo_path }}-worktrees/{{ branch |
# sanitize }} template (slash -> dash) — wt won't tell us where it put it.
sanitized="${branch//\//-}"
wt_path="$(git rev-parse --show-toplevel)-worktrees/$sanitized"

# post-switch tmux hook is disabled under CLAUDECODE, so wt only creates the
# worktree here — we drive tmux ourselves below.
wt switch -c "$branch" -y

crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
mkdir -p "$crew_dir"

# Log the dispatch decision to the crew bus for later `crew report`.
dispatch_shape="${DISPATCH_SHAPE:-}"
jq -nc --arg crew "$crew_id" --arg branch "$branch" \
  --arg engine "$agent" --arg model "$model" --arg tier "$tier" --arg effort "$effort" \
  --arg shape "$dispatch_shape" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"dispatch", branch:$branch, engine:$engine, model:$model, tier:$tier, effort:$effort, shape:$shape}' \
  >>"$crew_dir/events.jsonl"

# FleetView-style codename+color, derived from the branch (deterministic).
ident=$(crew identity "$branch")
agent_name=$(printf '%s' "$ident" | jq -r .name)
agent_color=$(printf '%s' "$ident" | jq -r .tmux)

# Stamp the task file: header fields the worker protocol reads, the closes
# line, and the full task body from $DISPATCH_SPEC (falls back to the title).
{
  printf 'tier: %s\neffort: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\n' \
    "$tier" "$effort" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name"
  if [ -n "${DISPATCH_SPEC:-}" ] && [ -f "${DISPATCH_SPEC:-}" ]; then
    printf '\n## Task\n\n'
    cat "$DISPATCH_SPEC"
  fi
} >"$wt_path/WORKER_TASK.md"

read -r win pane < <(tmux new-window -d -c "$wt_path" -n "$sanitized" -P -F '#{window_id} #{pane_id}')

# Identity surfaces: codename on the pane border + the CC prompt box (--name).
# lazytmux owns the tab text; @crew_* tint the status-bar tab.
tmux set-window-option -t "$win" @crew_name "$agent_name"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold] "

# Deep claude workers get the read-only codex MCP for cross-model review
# (work profile only — mcp-codex.json is generated work-gated).
xreview_mcp=""
if [ "$profile" = work ] && [ "$agent" = claude ] && [ "$tier" = deep ]; then
  xreview_mcp="--mcp-config $HOME/.config/claude-code/mcp-codex.json"
fi

if [ "$agent" = codex ]; then
  tmux send-keys -t "$pane" \
    "codex --profile worker -m $model -c model_reasoning_effort=$effort --dangerously-bypass-approvals-and-sandbox 'Read ~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.'" Enter
else
  tmux send-keys -t "$pane" \
    "claude --name $agent_name --model $model --effort $effort $mcp_flag $xreview_mcp --append-system-prompt-file ~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.'" Enter
fi
```

- [ ] **Step 2: Package `dispatch` in `home/ai/claude-code/default.nix`**

In the `let` block, immediately after the `crew = pkgs.writeShellApplication { … };` binding (ends ~line 28), add:

```nix
  # native dispatch on PATH (was a fish autoload fn needing `fish -c`).
  # writeShellApplication pins nixpkgs bash + runs shellcheck at build.
  # `wt` (worktrunk) resolves from the interactive session PATH — runtimeInputs
  # are prepended, ambient PATH is preserved.
  dispatch = pkgs.writeShellApplication {
    name = "dispatch";
    runtimeInputs = [pkgs.gh pkgs.git pkgs.jq pkgs.gnused pkgs.coreutils pkgs.tmux crew];
    text = builtins.readFile ./dispatch.sh;
  };
```

- [ ] **Step 3: Install `dispatch` and set `DISPATCH_PROFILE`**

Change `home.packages = [crew];` (line ~212) to:

```nix
  home.packages = [crew dispatch];
  home.sessionVariables.DISPATCH_PROFILE = osConfig.profile;
```

(If `home.sessionVariables` is already set elsewhere in this module, merge the key in rather than redeclaring the attr.)

- [ ] **Step 4: Delete the fish `dispatch` heredoc**

In `home/terminal/fish/default.nix`, delete the entire `"fish/functions/dispatch.fish".text = ''` … `'';` block (the `function dispatch` … `end` body, ~lines 199-405). Leave `dispatcher.fish` (Task 3) and every other function intact.

Verify nothing else references the removed function:

```bash
rg -n 'fish -c .*dispatch|functions/dispatch\.fish' home/ | rg -v 'dispatcher\.fish'
```

Expected: no matches (the `/dispatcher` command's `fish -c` note is handled in Task 3).

- [ ] **Step 5: Static-check both shell scripts**

Run:

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
shellcheck home/ai/claude-code/dispatch.sh
```

Expected: no output (exit 0). Fix any finding. Common ones to expect and handle properly (never `# shellcheck disable` to silence): quote every expansion; the `read -r win pane < <(…)` process substitution is fine under bash.

- [ ] **Step 6: Test the side-effect-free validation/error paths**

These all exit **before** any `gh`/`wt`/`tmux` call, so they are safe to run directly against the built binary. Build it and exercise:

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
git add home/ai/claude-code/dispatch.sh home/ai/claude-code/default.nix home/terminal/fish/default.nix
bin=$(nix build --no-link --print-out-paths .#homeConfigurations.$(whoami)@$(hostname).activationPackage 2>/dev/null | head -1)
# If the activation attr name differs, resolve it: `nix flake show 2>/dev/null | rg homeConfigurations`
d=$(command -v dispatch)  # after Task 4 switch; before that, build the dispatch pkg alone and point $d at it

"$d" bogus-tier m --effort low x;            echo "exit=$?"   # usage;                 exit=1
"$d" standard;                               echo "exit=$?"   # usage (no model);      exit=1
DISPATCH_PROFILE=personal CREW_ID=c1 "$d" standard claude-sonnet-5 --agent codex --effort low "t"; echo "exit=$?"  # codex work-only; exit=1
CREW_ID= "$d" standard claude-sonnet-5 --effort low "t";  echo "exit=$?"  # no crew id; exit=1
"$d" standard claude-sonnet-5 --effort bogus "t";         echo "exit=$?"  # bad effort; exit=1
"$d" standard claude-sonnet-5 "t";                        echo "exit=$?"  # effort required; exit=1
```

Expected: every case prints `exit=1` with the matching stderr message. (Building `dispatch` in isolation before the Task 4 switch: `nix build --no-link --print-out-paths --expr 'with import <nixpkgs> {}; writeShellApplication { name="dispatch"; runtimeInputs=[gh git jq gnused coreutils tmux]; text=builtins.readFile ./home/ai/claude-code/dispatch.sh; }'` then use `$out/bin/dispatch` — `crew` is on the ambient PATH.)

- [ ] **Step 7: Commit**

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
git add home/ai/claude-code/dispatch.sh home/ai/claude-code/default.nix home/terminal/fish/default.nix
git commit -m "feat(dispatcher): native dispatch bash tool on PATH, drop fish fn (#74)"
```

---

## Task 3: launcher, `/dispatcher` command, and protocol doc

**Files:**

- Modify: `home/terminal/fish/default.nix` (the `dispatcher.fish` heredoc — `lock-role`/`unlock-role`)
- Modify: `home/ai/claude-code/commands/dispatcher.md`
- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md`

**Interfaces:**

- Consumes: `crew register`/`crew deregister` (Task 1), native `dispatch` + `--crew-id` (Task 2).

- [ ] **Step 1: Update the `dispatcher.fish` launcher**

In `home/terminal/fish/default.nix`, inside the `"fish/functions/dispatcher.fish".text` heredoc, change the two crew calls:

- `if not crew lock-role $fish_pid` → `if not crew register $fish_pid`
- `crew unlock-role` → `crew deregister`

Update the adjacent comment that says "Acquire the repo's dispatcher role lock" to reflect non-exclusive registration, and the error string `"dispatcher: another dispatcher already holds this repo's crew bus"` — since `register` no longer refuses, this branch is now effectively unreachable; simplify to just call `crew register $fish_pid` without the `if not … return 1` guard:

```fish
    # Register this crew in the repo bus (non-exclusive — N crews per repo).
    # $fish_pid is this interactive shell; it blocks on the foreground claude
    # below, so its liveness tracks the whole session (stale-reclaim key).
    if git rev-parse --git-common-dir >/dev/null 2>&1
        crew register $fish_pid
    end
```

Keep `set -q CREW_ID; or set -gx CREW_ID (crew id)` (line 28) — env inheritance is exactly how launcher dispatchers hand the crew id to the native `dispatch`. Keep the `crew deregister` on exit inside its existing `git rev-parse` guard.

- [ ] **Step 2: Rewrite the crew-mechanics section of `/dispatcher` command**

In `home/ai/claude-code/commands/dispatcher.md`:

Replace the lock step (lines ~6-11, the `crew lock-role $PPID` block intro) so it mints + registers a crew id first:

```markdown
**First, mint a crew id and register this dispatcher (plain bash — NOT `fish -c`):**
```

CREW_ID=$(crew id)
crew register $PPID

```

Registration is **non-exclusive** — several crews can share a repo. `crew register`
records `$PPID` (the long-lived `claude` process) under `.git/crew/crews/<crew_id>/`
for stale reclaim; it never refuses. Note the `$CREW_ID` value — you pass it to every
`dispatch` and `crew` call below (this session's claude was not launched with it in
its environment, so it is not inherited).
```

Delete the entire "If it **refuses** … STOP … holder PID" paragraph and the "no clean `unlock-role`" paragraph (registration doesn't refuse; a dead crew dir is reclaimed by the next `register`, and `crew deregister` is the clean release).

Replace line ~30-32 (`dispatch` is a fish function; invoke it as `fish -c '…'`…) with:

```markdown
`dispatch` and `crew` are both CLIs on your PATH — call them **directly** (no `fish -c`).
Because this in-session claude has no `$CREW_ID` in its environment, pass the crew id
explicitly: `dispatch --crew-id $CREW_ID <tier> <model> …` and prefix crew reads with
`CREW_ID=$CREW_ID crew …`.
```

Update the dispatch usage line (~45) to `dispatch --crew-id $CREW_ID <tier> <model> [--agent claude|codex] <title…>`.

- [ ] **Step 2b: Verify the "fish -c" instruction is fully gone**

Run:

```bash
rg -n 'fish -c|lock-role|unlock-role|fish function' home/ai/claude-code/commands/dispatcher.md
```

Expected: no matches.

- [ ] **Step 3: Update `DISPATCHER_PROTOCOL.md`**

- Line ~53: change `All workers in your shell share one crew_id.` to reflect multi-crew and the delivery contract:
  ```markdown
  Each dispatcher owns one `crew_id`; several dispatchers (crews) may share a repo.
  Launcher sessions inherit `$CREW_ID` from the environment; an in-session `/dispatcher`
  passes `--crew-id $CREW_ID` to `dispatch` and prefixes `CREW_ID=$CREW_ID` on `crew` reads.
  ```
- Line ~64 already says the watch "self-seeds from its per-crew cursor file" — no change needed; the code now matches it.
- Grep the file for any remaining single-crew phrasing:

  ```bash
  rg -ni 'one dispatcher per repo|holds this repo|lock-role|unlock-role' home/ai/claude-code/DISPATCHER_PROTOCOL.md
  ```

  Expected: no matches (fix any that appear).

- [ ] **Step 4: Commit**

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
git add home/terminal/fish/default.nix home/ai/claude-code/commands/dispatcher.md home/ai/claude-code/DISPATCHER_PROTOCOL.md
git commit -m "docs(dispatcher): register-based launcher + command, drop fish -c and role lock (#74)"
```

---

## Task 4: rebuild, then end-to-end concurrent-crew verification

**Files:** none (verification only)

- [ ] **Step 1: Stage everything and rebuild (the `git add` gotcha)**

Run:

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
git add -A
nh home switch
```

Expected: build succeeds; `crew`'s and `dispatch`'s shellcheck pass. If a flake build errors with "path does not exist" for `dispatch.sh`, it's the untracked-file gotcha — `git add` it and re-run.

- [ ] **Step 2: `dispatch` is now bash-visible on PATH (no `fish -c`)**

Run:

```bash
command -v dispatch      # -> a /nix/store/.../bin/dispatch path
type dispatch            # bash: "dispatch is /nix/store/.../bin/dispatch" (NOT "not found")
dispatch                 # -> usage line, exit 1
```

Expected: `dispatch` resolves as a binary directly from bash; the old fish function is gone (`fish -c 'type dispatch'` shows the PATH binary, not "a function").

- [ ] **Step 3: Two concurrent crews in one repo (the core new capability)**

In a scratch git repo with GitHub issues disabled (use Linear-id mode to avoid real issue creation), open two dispatcher fish shells (or simulate with explicit ids). Minimal simulation without launching claude:

```bash
tmp=$(mktemp -d); git -C "$tmp" init -q; cd "$tmp"
git commit -q --allow-empty -m init
# crew A
CREW_ID=crewA crew register 1111
# crew B — must NOT be refused
CREW_ID=crewB crew register 2222
ls "$(git rev-parse --git-common-dir)/crew/crews"   # -> crewA crewB
# each watch uses its own cursor + lock: arm both briefly, confirm no lock refusal
CREW_ID=crewA crew watch --timeout 1 --states done >/tmp/a.out 2>/tmp/a.err &
CREW_ID=crewB crew watch --timeout 1 --states done >/tmp/b.out 2>/tmp/b.err &
wait
grep -c 'already running' /tmp/a.err /tmp/b.err   # -> 0 for both (no cross-crew watch-lock refusal)
ls "$(git rev-parse --git-common-dir)/crew/crews/crewA" "$(git rev-parse --git-common-dir)/crew/crews/crewB"  # each has cursor + watch.lock.d
cd - >/dev/null; rm -rf "$tmp"
```

Expected: both `register`s succeed; `crews/` holds both; neither watch prints "another watch is already running"; each crew dir has its own `cursor` and `watch.lock.d`. This is the exact scenario the old role lock forbade.

- [ ] **Step 4: Cursor isolation (no event-stealing)**

Append one `done` status for crew A only, then confirm A's watch consumes it and B's does not:

```bash
tmp=$(mktemp -d); git -C "$tmp" init -q; cd "$tmp"; git commit -q --allow-empty -m init
log="$(git rev-parse --git-common-dir)/crew/events.jsonl"; mkdir -p "$(dirname "$log")"
CREW_ID=crewA crew register 1; CREW_ID=crewB crew register 2
jq -nc '{ts:(now*1000|floor),crew_id:"crewA",kind:"status",from:"worker:x",to:"dispatcher:crewA",body:{state:"done"}}' >>"$log"
CREW_ID=crewA crew watch --timeout 2 --states done   # -> prints a batch with the crewA event; exit 0
CREW_ID=crewB crew watch --timeout 2 --states done; echo "B exit=$?"   # -> no stdout; B exit=3 (timeout)
cd - >/dev/null; rm -rf "$tmp"
```

Expected: crew A's watch prints `{"cursor":…,"events":[…]}` and exits 0; crew B's watch times out (exit 3) with empty stdout — B never sees A's event.

- [ ] **Step 5: In-session parity (real dispatch via `--crew-id`)**

From a normal (non-dispatcher) claude/bash session inside this repo's worktree, with a Linear-style id to avoid creating a GH issue:

```bash
cid=$(crew id)
crew register $$          # register a crew for this shell
dispatch --crew-id "$cid" trivial claude-haiku-4-5-20251001 --effort low ENG-9999 smoke test native dispatch
```

Expected: a new tmux window opens named `eng-9999-smoke-test-native-dispatch`, a worktree exists at `…-worktrees/eng-9999-smoke-test-native-dispatch`, `WORKER_TASK.md` there has `crew_id: <cid>` and `Closes ENG-9999`, and `events.jsonl` has a `kind:"dispatch"` line with that crew id. Then clean up: close the window, `wt remove` the worktree, `crew deregister`.

- [ ] **Step 6: macOS parity (if a Mac is available)**

On `mbp-m4-pro`: `git add -A && nh darwin switch`, then repeat Steps 2 and 5. Expected: identical behavior; confirm GNU `sed`/`date` are used (no BSD `sed -E` divergence in the slug) — e.g. `dispatch … "Weird Title!! 99"` yields slug `weird-title-99`.

- [ ] **Step 7: Push and open the PR**

```bash
cd ~/nix-config-worktrees/feat-74-dispatch-native-multicrew
git push -u origin feat/74-dispatch-native-multicrew
gh pr create --assignee @me --title "dispatch: native PATH tool + concurrent crews per repo" \
  --body "Closes #74.

Makes \`dispatch\` a native bash PATH tool (no more \`fish -c\`) and lifts the one-dispatcher-per-repo limit so multiple crews run concurrently.

- \`dispatch.sh\` (bash, writeShellApplication) replaces the Nix-interpolated fish heredoc; crew id via \`--crew-id\` > \`\$CREW_ID\` > error; profile gate reads \`\$DISPATCH_PROFILE\` (fixes the baked \`test \"work\" = work\").
- \`crew\`: \`register\`/\`deregister\` (non-exclusive) replace \`lock-role\`/\`unlock-role\`; watch cursor + watch lock are now per-crew under \`.git/crew/crews/<id>/\`.
- Launcher, \`/dispatcher\` command, and \`DISPATCHER_PROTOCOL.md\` updated.

Spec + plan: \`docs/superpowers/{specs,plans}/2026-07-12-dispatch-native-multicrew*.md\`.

Verified: two concurrent crews register + watch without contention; cursor isolation (no event-stealing); in-session \`--crew-id\` dispatch scaffolds a worker; validation/error paths."
```

---

## Self-Review

**Spec coverage:**

- Native PATH `dispatch` (no `fish -c`) → Task 2 (create + package), Task 4 Step 2. ✓
- Crew id `--crew-id` → `$CREW_ID` → error → Task 2 Step 1 (resolution block), Task 2 Step 6 (error test). ✓
- Profile gate via `$DISPATCH_PROFILE` (not baked) → Task 2 Steps 1+3. ✓
- Per-crew watch cursor → Task 1 Step 3, Task 4 Step 4. ✓
- Per-crew watch lock → Task 1 Step 3, Task 4 Step 3. ✓
- Role lock → non-exclusive `register`/`deregister` → Task 1 Step 2, Task 3 Step 1. ✓
- `.git/crew/crews/<id>/{pid,cursor,watch.lock.d}` layout → Task 1 Steps 2-3. ✓
- Launcher + `/dispatcher` command + `DISPATCHER_PROTOCOL.md` updates → Task 3. ✓
- Delete fish heredoc → Task 2 Step 4. ✓
- macOS compatibility → Global Constraints + Task 4 Step 6. ✓
- Migration (no data migration; restart) → covered by Task 4 rebuild; events.jsonl schema unchanged. ✓

**Placeholder scan:** no TBD/TODO; every code step shows complete code; error handling is concrete (exact messages + exit codes). The one non-literal is the activation-package attr name in build commands, which includes the exact `rg`/`nix flake show` command to resolve it — not a logic placeholder.

**Type/name consistency:** `register`/`deregister` used identically in crew.sh (Task 1), launcher, and command (Task 3). `--crew-id`, `$CREW_ID`, `$DISPATCH_PROFILE`, `crews/<crew_id>/{pid,cursor,watch.lock.d}` spelled consistently across tasks. `agent_name`/`agent_color`/`crew_id`/`crew_dir` match the `WORKER_TASK.md` fields the worker protocol reads.
