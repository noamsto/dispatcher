# dispatcher

A standalone, shell-based agent-orchestration harness: a dispatcher process that
fans work out to worker agents across multiple engines (Claude Code, Codex,
Cursor) and coordinates them over a shared, git-backed bus.

This repo is being extracted from the `nix-config` monorepo, where the harness
originated as fish functions and Home Manager modules under
`home/ai/claude-code/`. Extraction is happening incrementally across a sequence
of tasks; see the repo's issues/PRs for status.

## Install (Home Manager)

Add the flake as an input and enable the module:

```nix
# flake.nix
dispatcher = {
  url = "github:noamsto/dispatcher";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

```nix
# your home-manager config
imports = [inputs.dispatcher.homeManagerModules.default];

programs.dispatcher = {
  enable = true;
  profile = "work"; # or "personal"; gates the codex and cursor engines
};
```

That puts `crew`, `dispatch` and `dispatcher` on `PATH`, exports
`DISPATCH_PROFILE` and `DISPATCHER_PROTOCOL_DIR`, writes the cursor rule and
commands into `~/.cursor/`, and installs the Codex plugin.

For Claude Code, pass the plugin directory to `claude`:

```nix
--plugin-dir ${config.programs.dispatcher.claudePluginDir}
```

### Prerequisites the module does NOT manage

- **Codex `config.toml` stanzas.** The Codex plugin is inert until
  `~/.codex/config.toml` contains a marketplace and plugin entry. These carry no
  store path, so they are hand-managed:

  ```toml
  [marketplaces.dispatcher]
  source = "local"
  path = "/home/YOU/.codex/plugins/cache/dispatcher"

  [plugins."dispatcher@dispatcher"]
  enabled = true
  ```

- **A Codex worker profile.** `profile = "work"` dispatches Codex workers with
  `--profile worker`, which requires `~/.codex/worker.config.toml`. That file is
  owned by your Codex configuration, not by this module. There is no eval-time
  check for it — it is a runtime file — so a missing profile surfaces as a Codex
  launch failure in the worker pane.

- **Engine CLIs and auth.** `claude`, `codex`, `cursor-agent` and `wt`
  (worktrunk) resolve from the ambient session `PATH`; log each in out of band.

## Protocol resolution

`dispatch` and `dispatcher` resolve the protocol directory as
`${DISPATCHER_PROTOCOL_DIR:-<baked store path>}`. The slash command and cursor
rule — markdown an agent reads live — resolve it through the same variable, which
is why the module exports it rather than only baking it into the scripts. Each
plugin additionally ships a `protocols/` copy as the fallback for a non-Nix
install.

To iterate on a protocol without rebuilding:

```bash
export DISPATCHER_PROTOCOL_DIR=~/path/to/dispatcher/adapters/core/protocols
```

Edits then take effect on the next `dispatch`.

## Development

This repo uses a Nix flake (flake-parts) for its devshell, formatting, and
pre-commit hooks.

```bash
direnv allow      # or: nix develop
bats tests/       # run the test suite
nix flake check   # pre-commit + formatting checks
```

`nix flake check` reports `homeManagerModules` as **unchecked** — it does not
evaluate the module. `tests/module.bats` covers that gap explicitly.

After changing anything under `adapters/core/commands/`, regenerate:

```bash
./scripts/gen-adapters.sh
```

CI fails if committed adapter output differs from a fresh run.

## Layout

- `adapters/core/` — engine-neutral: the three shell CLIs, the protocols, and the
  shared command bodies that the generator projects per engine
- `adapters/claude-code/plugin/` — Claude Code plugin: commands, agents, skills,
  workflows, hooks
- `adapters/codex/plugin/` — Codex plugin: skills (Codex has no custom slash
  commands) and hooks; no agents or workflows, which Codex cannot express
- `adapters/cursor/` — loose rules and commands; Cursor has no plugin format
- `scripts/gen-adapters.sh` — regenerates every adapter tree from
  `adapters/core/commands/`
- `nix/hm-module.nix` — the Home Manager module
- `tests/` — bats suite; `tests/helpers.bash` provides `setup_repo` and
  `stub_bin`

### Formatting exclusions, deliberately

`adapters/` is excluded from prettier and the vendored shell from treefmt.
Everything under `adapters/` is vendored payload or generator output, not
hand-authored source here. Two reasons it must not be reformatted: it is content
an agent reads as instructions (prettier rewrote nested code fences inside a
prompt template), and reformatting generated output after the generator writes it
would permanently deadlock the CI drift gate.
