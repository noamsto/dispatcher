# Configurable engine roster — dispatchable = enabled ∧ available

**Status:** design, awaiting implementation
**Date:** 2026-09-17
**Issue:** [#218](https://github.com/noamsto/dispatcher/issues/218)

## Problem

Which engines a machine can dispatch is hardcoded, and the rule is spelled
`profile`:

```sh
# dispatch.sh:578-587
profile="${DISPATCH_PROFILE:-personal}"
if [ "$agent" = codex ] && [ "$profile" != work ]; then …
if [ "$agent" = cursor ] && [ "$profile" != work ]; then …
```

The same rule is repeated for per-role engines in `--roles`
(`dispatch.sh:1025-1032`) and for the orchestrator engine
(`dispatcher.sh:61-69`).

That conflates two unrelated facts. `profile` describes the _machine_
(work vs personal), and it also has real non-engine jobs: the work+claude+deep
rung (`dispatch-resume.sh:267`) and the work-only `analytics` MCP profile
(`dispatch-resume.sh:252`). Overloading it as the engine gate means a consumer
who wants, say, codex but not cursor has nothing to say — the only lever is
`profile`, and moving it takes both engines at once.

The HM module has the mirror problem: `nix/hm-module.nix` installs the five
`~/.cursor/*` trees, the `spec-plan-critic` cursor skill symlink and the codex
plugin cache copy unconditionally under `enable = true`. A machine that never
dispatches cursor still gets dispatcher rules in `~/.cursor/rules`, where a
hand-run cursor session reads them.

Separately, nothing checks whether an engine's CLI is actually present.
`dispatch` validates tier, model shape, effort ceiling, budget and protocol
revision before scaffolding — but not that the binary exists. A missing install
surfaces as `codex: command not found` inside a tmux pane the worktree, window,
branch and issue have already been paid for.

## Solution

An engine is dispatchable when it is **enabled** and **available**:

| Fact      | Question                           | Source                                        |
| --------- | ---------------------------------- | --------------------------------------------- |
| enabled   | does this machine want the engine? | `DISPATCH_ENGINES`, exported by the HM module |
| available | is the engine actually installed?  | `command -v` on the engine's CLI              |

They fail differently, and the messages say which:

```
dispatch: --agent cursor is not enabled here (enabled: claude pi codex)
dispatch: --agent codex is enabled but not installed (no 'codex' on PATH)
```

No engine support is removed. All four engines stay fully implemented; a
consumer narrows the roster.

### The roster

`DISPATCH_ENGINES` is a space-separated list. **Unset means all four** — a
non-Nix checkout, an ad-hoc shell and the bats suite keep working with no
change, which is what keeps this from being a 70-test rewrite.

The Nix option:

```nix
engines = lib.mkOption {
  type = lib.types.listOf (lib.types.enum ["claude" "codex" "cursor" "pi"]);
  default = ["claude" "pi"];
  description = ''
    Engines this machine may dispatch. Exported as DISPATCH_ENGINES and
    gates the per-engine artifacts installed below. Unset at runtime, the
    CLIs allow all four.
  '';
};
```

The default is the two all-profile engines, not all four. That preserves
today's behavior for a personal machine that only sets `enable = true`: before
this change `profile` defaulted to `personal` and rejected codex/cursor, so
`["claude" "pi"]` is the same roster arrived at honestly.

### The probe

Engine name to CLI is not identity — cursor's binary is `cursor-agent`:

| engine | CLI            |
| ------ | -------------- |
| claude | `claude`       |
| codex  | `codex`        |
| cursor | `cursor-agent` |
| pi     | `pi`           |

The probe runs in the `dispatch`/`dispatcher` process before any scaffolding.

This makes the profile-based engine gate redundant rather than merely
duplicated: on the reference consumer, `home/ai/default.nix:22` imports the
codex module only when `osConfig.profile == "work"`, so off-work the binary is
genuinely absent and the probe rejects it — with a better message than the
profile gate gave. The probe also catches what `profile` never could: a work
machine where the engine was uninstalled.

### `dispatch --engines`

A new flag prints the effective roster, one engine per line — enabled ∧
available, the same predicate the gate applies:

```
$ dispatch --engines
claude
pi
```

It follows the existing `dispatch --reap-roles` precedent (`dispatch.sh:342`):
a bare flag handled before argument parsing, printing and exiting.

This exists because the dispatcher _model_ chooses the engine. Today it judges
from prose in `DISPATCHER_PROTOCOL.md:22-41`, which names four engines and
their leanings, and from a "Profile constraint" paragraph (`:213`) stating the
work-only rule. Prose cannot express a per-machine, partly dynamic roster, so
both passages instead point at this command: run it before judging, and never
propose an engine absent from its output. The protocol keeps describing all
four engines — the document stays machine-neutral, and the command supplies the
machine-local truth.

## Scope

### `adapters/core/dispatch.sh`

- Replace the two work-only blocks (`:578-587`) with one roster check plus one
  probe against the lead engine.
- Replace the role work-only case (`:1025-1032`) with the same two checks, so
  a cross-engine grid (`--roles reviewer=cursor:composer-2.5`) is gated
  identically to the lead.
- Add the `--engines` early-exit flag beside `--reap-roles` (`:342`).

### `adapters/core/dispatcher.sh`

- Replace the `codex | cursor` profile case (`:61-69`) with the roster check
  and probe.
- `--agent` accept-lists and usage strings (`:25,54,56`) keep naming all four:
  support is exposed, availability is what narrows.

### `adapters/core/dispatch-resume.sh`

No engine gate is added. It already delegates to `dispatch`'s precheck so there
is "exactly one copy of the profile, model-shape, effort-ceiling" logic
(`:272`). Its own `profile` read (`:248`) stays — it serves the rung at `:267`,
not engine selection.

### Protocols

- `DISPATCHER_PROTOCOL.md:22-41` — roster section gains a note that the
  machine-local roster comes from `dispatch --engines`.
- `DISPATCHER_PROTOCOL.md:213` — "Profile constraint" becomes an availability
  constraint. The codex/cursor work-only sentences go; what remains is that an
  engine outside `dispatch --engines` cannot be dispatched and must not be
  proposed.
- `dispatch-orchestration.md` — same correction wherever it repeats the
  work-profile rule.

Protocol edits rehash `PROTOCOL_REV` automatically (content hash of
`$PROTOCOL_DIR`, #184/#193), so long-lived shells holding a stale value abort
with the existing actionable message. There is no committed rev file to bump.

### `nix/hm-module.nix`

- New `engines` option; export `DISPATCH_ENGINES` beside `DISPATCH_PROFILE`.
- `profile`'s description drops the claim that it gates engines.
- Gate artifacts on the roster: the five `.cursor/*` `home.file` entries and
  `activation.dispatcherCursorSkills` behind `cursor`;
  `activation.dispatcherCodexPlugin` behind `codex`.
- `home.packages` stays whole. `refresh-models` is cursor-only, but nothing
  schedules it and it already warns, exits 1 and leaves any existing cache
  intact when `cursor-agent` is absent (`refresh-models.sh:21-24`), so gating
  it would buy nothing.

### Consumer (nix-config, separate PR)

```nix
dispatcher = {
  enable  = true;
  engines = ["claude" "pi" "codex"];
  inherit (osConfig) profile;
};
```

and `home/ai/cursor/default.nix` drops `dispatcherCursorNotify` plus
`activation.dispatcherCursorStopHook`. That removal is safe **only** once
cursor is off the roster: the stop hook is cursor's sole session-end signal
(cursor exposes no session-end event, so `--turn-end` posts `exited` for a
worker that stopped without reporting). Removing it while cursor is still
dispatchable would leave workers dark on the crew bus. Cursor stays installed
and configured as an interactive editor.

## Known limits

**PATH is probed in the dispatch process.** Workers launch into tmux panes on
the same machine, so the probe is a good proxy — but a tmux server that outlived
a rebuild can carry a different PATH than the shell running `dispatch`. This is
the same class of skew that already affects tmux config reloads; it is not
introduced here and is not addressed here.

**Presence is not authentication.** `command -v codex` says the CLI exists, not
that it has a usable session. An installed-but-unauthed engine still fails in
the pane, out of band, as it does today.

**The roster is advisory to the model, mandatory to the CLI.** A dispatcher that
skips `dispatch --engines` and proposes cursor anyway is rejected by the gate,
not by the protocol. The gate is the enforcement point; the command exists to
make the rejection unnecessary.

## Testing

- `tests/dispatcher.bats:27,95-118` and `tests/dispatch.bats:368` — the
  `work-profile only` assertions become roster assertions
  (`DISPATCH_ENGINES="claude pi"` rejects `--agent codex`) and probe assertions
  (an enabled engine with a stubbed-empty PATH is rejected with the
  not-installed message).
- Unset `DISPATCH_ENGINES` still admits all four — the compatibility case that
  keeps the rest of the suite untouched.
- A role-spec case: `--roles reviewer=cursor:…` is rejected when cursor is off
  the roster, proving the lead and role gates agree.
- `dispatch --engines` prints only enabled ∧ available engines, and exits 0 with
  no crew id, worktree or tmux.
- `tests/module.bats:59` — extend the eval string with `DISPATCH_ENGINES`, and
  assert the cursor artifacts and codex activation appear only when their
  engine is listed.
