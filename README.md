# dispatcher

A standalone, shell-based agent-orchestration harness: a dispatcher process
that fans work out to worker agents across multiple engines (Claude Code,
Codex, Cursor) and coordinates them over a shared bus.

This repo is being extracted from the `nix-config` monorepo, where the
harness originated as fish functions and Home Manager modules under
`home/ai/claude-code/`. Extraction is happening incrementally across a
sequence of tasks; see the repo's issues/PRs for status.

## Development

This repo uses a Nix flake (flake-parts) for its devshell, formatting, and
pre-commit hooks.

```bash
direnv allow      # or: nix develop
bats tests/       # run the test suite
nix flake check   # run all flake checks (build, tests, formatting)
```

## Layout

- `adapters/` — per-engine adapter scripts (added in later tasks)
- `tests/` — bats test suite; `tests/helpers.bash` provides shared fixtures
  (`setup_repo`, `stub_bin`) used by every test file
