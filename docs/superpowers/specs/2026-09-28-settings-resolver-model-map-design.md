# Settings resolver and model map as data — design (#560)

Part of #559. Slice 1 of 3: a settings resolver plus the model map moved out of
`dispatch.sh` `case` arms into a shipped JSON file. Pure refactor of the map's
representation: no row admits or refuses a different model, and a machine with
no settings files and today's env behaves identically.

## Problem

- The model map is hand-copied in four places: the tier-row gate in
  `dispatch.sh` (~:2596-2730, regexes + `tier_expected` strings), the
  escalation rungs in `dispatch.sh` (`_escalation_target` ~:2482 and the
  record-only `case` ~:2735) and again in `dispatch-resume.sh`
  (`_escalation_target` ~:717), the pace downgrades in `pace_rule_target`
  (`dispatch.sh` ~:43-60), and the prose tables in
  `protocols/dispatch-orchestration.md`. A ladder bump means editing all of
  them by hand; only a token-presence tripwire (`tests/dispatch.bats`
  "tier map conformance") notices drift.
- Settings reach scripts only as `DISPATCH_*` session env vars exported by
  `nix/hm-module.nix`, snapshotted at shell start. Nothing can be edited from a
  config repo or a TUI.

## Goals

1. `adapters/core/defaults.json` (layer 1) holds the model map as data, in a
   schema where each of the three consumers (tier gate, escalation, pace
   downgrade) is a table lookup. It holds no engine roster: an unset roster
   already means "every engine" (`${DISPATCH_ENGINES:-$ENGINES_ALL}`), and a
   base copy would be a third list beside the two `ENGINES_ALL` constants.
2. A resolver, `dispatch-config`, prints the merged settings
   (base → user → locked → env) and, with `--show-origin`, each leaf's layer.
3. Security-bearing keys (`grantRoots`, `openrouter.keyFile`) are honoured only
   from the locked layer or env; a user-layer (or base-layer) value is dropped
   with a stderr warning.
4. `dispatch.sh`, `dispatch-resume.sh`, `dispatcher.sh`, `refresh-budget.sh`
   read what they need through the resolver. Env precedence is unchanged.
5. The data-shaped tables in `dispatch-orchestration.md` are generated from
   `defaults.json`, and the hand-written Model map table's worker cells are
   checked against it, in `nix flake check`.

## Non-goals

- `nix/hm-module.nix` changes (next slice generates layer 3 and links layer 2).
- Any TUI.
- Budget thresholds (70/85/95, the 15-point pace margin) as data.
- Admitting or refusing any different model in any row.
- Trackers (`DISPATCH_REPO_TRACKERS`/`DISPATCH_ORG_TRACKERS`) and
  `DISPATCH_PROFILE` through the resolver: not among this slice's named
  consumers. A resolver key nothing reads would mislead a future TUI user, so
  they stay env-only here; follow-up issue.
- `crew.sh`'s burn-class `case` (~:572) and `dispatcher.sh`'s orchestrator
  default models (~:259, :279): separate model knowledge, not the worker map;
  follow-up issue.

## Layers and precedence

| #   | layer  | source                                                                                                                                                                                                                                                                    | notes                                                                                                                                         |
| --- | ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | base   | `adapters/core/defaults.json`, baked into the resolver at build (`@defaultsJson@`); a raw-source run uses the `defaults.json` beside the resolver script                                                                                                                  | required; must parse to an object                                                                                                             |
| 2   | user   | `${XDG_CONFIG_HOME:-$HOME/.config}/dispatcher/settings.json`                                                                                                                                                                                                              | optional; absent = empty layer; present but not a JSON object = exit 1 naming the file                                                        |
| 3   | locked | the path in `$DISPATCH_LOCKED_SETTINGS`                                                                                                                                                                                                                                   | optional; unset/empty = empty layer; set but unreadable or not a JSON object = exit 1 naming the path (fail closed: it is the security layer) |
| 4   | env    | `DISPATCH_ENGINES` → `engines` (split on whitespace); `DISPATCH_GRANT_ROOTS` → `grantRoots` (split on `:`, empty segments dropped); `DISPATCH_OPENROUTER_MONTHLY_USD` → `openrouter.monthlyUsd` (string, verbatim); `DISPATCH_OPENROUTER_KEY_FILE` → `openrouter.keyFile` | an empty-string var is unset — the `${VAR:-}` semantics every consumer uses today                                                             |

Merge: jq `*` — objects merge recursively, a later layer wins per key; arrays and
scalars are leaves, replaced wholesale. Before merging, `grantRoots` and
`openrouter.keyFile` are deleted from the base and user layers, each deletion
printing `dispatch-config: ignoring <key> from <file> — it is honoured only from
the locked settings or the environment` to stderr.

`--show-origin` prints the same tree with every leaf (any non-object, or an
empty object) replaced by `{"value": <v>, "origin": "base|user|locked|env"}`,
the origin being the last layer that holds that exact path.

Validation (exit 1, naming the key and layer): `engines`, when present, must
be an array of strings; `grantRoots`, when present, an array of strings none of
which contains `:` (consumers re-join it with `:`, so one would split into two
roots; a relative entry is still skipped by `_add_dir_ok`, as today);
`openrouter.keyFile`, when present, a string; `openrouter.monthlyUsd`, when
present, a number or string. Nothing else is type-checked: a malformed model
map makes the gate refuse (fail closed), never admit.

## `defaults.json` schema

Every model matcher is a list of bash `==` globs (`*` = any string; `\[` a
literal bracket) — the semantics of the `case` arms these replace. The one
exception is a tier row's optional `regex` list (bash `=~` ERE), for cursor's
cross-vendor effort-suffixed/bracketed shape, which no glob expresses.

```jsonc
{
  "modelMap": {             // tier gate: modelMap[engine][tier]
    "<engine>": { "<tier>": {
      "default": "<the row's typical launch model>",
      "models": ["<glob>", ...],
      "regex": ["<ERE>", ...],          // optional
      "expected": "<the refusal's 'expected …' text, verbatim>"
    } }
  },
  "escalation": {           // escalation[engine][tier]: first rule whose `failed` matches
    "<engine>": { "<tier>": [ {
      "failed": ["<glob>", ...],
      "baseline": "<label stamped as escalated_from>",
      "inRow": ["<glob>", ...]          // record-only hop: next rung is already in the row
      // or "outOfRow": ["<exact id>", ...]  // one-shot admission above the row;
      //    every accepted spelling listed (cursor: the id and its -fast twin)
    } ] }
  },
  "paceDowngrades": {       // paceDowngrades[engine]: first entry whose `models` matches
    "<engine>": [ { "models": ["<glob>", ...], "to": "<downgrade model>" } ]
  }
}
```

A rule carries exactly one of `inRow` / `outOfRow`. Consumers:

- **Tier gate** (`dispatch.sh`): model admitted iff it matches a `models` glob
  or a `regex` of `modelMap[agent][tier]`; a missing row admits nothing (fail
  closed, as today's `*) tier_ok=0`). The refusal line is today's `echo`,
  with `$tier_expected` = the row's `expected`.
- **Out-of-row escalation** (`dispatch.sh` when the row refuses;
  `dispatch-resume.sh` for an explicit `--model`): the first rule whose `failed`
  matches the bus-attested failed model; if it has `outOfRow` and the model
  equals one of those ids, the existing one-shot bus check
  (`_prior_failed_escalation_available`, unchanged) decides, and
  `escalated_from` = `baseline`.
- **Record-only stamp** (`dispatch.sh` when the row admits): the first rule
  whose `failed` matches; if it has `inRow` and the model matches one of those
  globs, `escalated_from` = `"<baseline> (record only)"`.
- **Pace downgrade** (`pace_rule_target`): the first `paceDowngrades[agent]`
  entry whose `models` matches gives `model_downgrade`.

Content is today's arms, transcribed. Codex, pi and cursor Grok/Kimi/Composer
rows become exact-id lists (each `-fast` variant listed); claude rows keep
`claude-<alias>-*` globs; cursor `deep` adds two ERE entries equivalent to
`tiermap_is_alt_effort` (effort-suffixed base with optional bracket block;
bracket block naming `effort=`). The cursor escalation ids stay on
`cursor-grok-4.6-*`, as today. `pi:deep`'s `_escalation_target` arm
(`RECORD_ONLY` with no record-only counterpart) is inert in both scripts and is
dropped; the fixture below proves no decision changes.

## Consumers

The resolver _feeds the variables the scripts already use_, so the grant,
launch-pinning and engine-gate code (and the byte-identical parity tests over
it) stay untouched.

| consumer             | reads                                                     | change                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| -------------------- | --------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `dispatch.sh`        | engines, grantRoots, modelMap, escalation, paceDowngrades | new `_settings_load` runs the resolver once, keeps the JSON in `settings`, and, only where the env var is empty, fills — never exports — `DISPATCH_ENGINES` (space-joined) and `DISPATCH_GRANT_ROOTS` (colon-joined) before any use. A non-empty env value is left verbatim, so today's raw-string behaviour (a whitespace-only roster enables nothing; odd spacing prints as given) cannot drift. Children that need settings (the resume precheck's `dispatch`) re-resolve; launch scripts keep pinning `DISPATCH_GRANT_ROOTS` explicitly, as today. Called at the top of `--spawn-role`, `--engines`, and the main path (before the engine gate). Not called by `--role-watch`, `--reap-roles`, `--role-exited`. The tier-gate `case`, `_escalation_target`, `_escalation_model_matches`, the record-only `case`, and the pace `case` become lookups via small helpers (below). |
| `dispatch-resume.sh` | grantRoots, escalation                                    | same `_settings_load` (byte-identical copy, parity-tested); the escalation block uses the shared out-of-row helper.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `dispatcher.sh`      | engines                                                   | runs the resolver before its engine gate and assigns `DISPATCH_ENGINES`; `engine_enabled`/`engine_cli` unchanged.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `refresh-budget.sh`  | openrouter.monthlyUsd, openrouter.keyFile                 | assigns `DISPATCH_OPENROUTER_MONTHLY_USD` / `DISPATCH_OPENROUTER_KEY_FILE` from the resolver at startup; `_or_key`/`_or_target` and their messages unchanged.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |

Resolver location in consumers: `"${DISPATCH_CONFIG_BIN:-@dispatchConfig@}"`,
the same env-overridable build-time idiom as `WORKTREE_GIT_LIB`. `flake.nix`
substitutes the `dispatch-config` package's bin path into the four consumers;
`tests/helpers.bash` exports `DISPATCH_CONFIG_BIN` at the repo copy for
raw-source runs. A resolver failure aborts the consumer (its own message on
stderr) — a model gate that silently used nothing would admit or refuse wrongly.

Equivalences the refactor relies on, for every id the Model gate's shape check
admits (none contains `:`): matching `failed` against a per-engine/tier glob list
equals matching `"$eng:$tier:$failed"` against the old combined arm. Only a
colon-bearing id admitted through `DISPATCH_SKIP_MODEL_CHECK` could differ —
the old concatenated `case` string let a `*` span the `:` and pair a failed id
with a model it never named; the per-field match does not. That is a
string-concatenation artifact on ids no engine accepts, so the fixture excludes
them; re-joining
`DISPATCH_ENGINES` / `DISPATCH_GRANT_ROOTS` changes only whitespace / empty
segments, which the old consumers already ignored. An empty `engines` array in a
file is refused by the resolver (review round 1): through the unchanged
`${DISPATCH_ENGINES:-$ENGINES_ALL}` it would otherwise mean "every engine",
the opposite of what a file emptying the roster intends.

## Doc drift: generate, plus one check

Chosen: **generate** the data-shaped tables, **check** the prose table.

- `scripts/gen-model-map-doc.sh` renders two regions of
  `adapters/core/protocols/dispatch-orchestration.md` between
  `<!-- BEGIN generated:… -->` / `<!-- END generated:… -->` markers: the tier-row
  table (engine, tier, `default`, `expected`) in "Tier map", and the pace
  downgrade table (replacing today's hand table, which also omits the
  `cursor-grok-4.6-high` row the code has). An engine in `modelMap` with no
  `paceDowngrades` entry (pi) renders as a fixed "— (effort only)" row, so the
  table keeps pi's line. `--check` renders to a temp file,
  diffs, and also asserts that the first bolded token of every Model map table
  cell (tier row × engine column) equals that row's `default`.
- Why generate: these tables are pure data, and a presence check cannot see an
  id left behind after it leaves the map; regeneration proves equality. Why
  check the Model map table instead: its cells carry execute/escalate ladders
  and rationale that `defaults.json` does not hold, so only the worker cell is
  data.
- `scripts/gen-adapters.sh` runs the renderer first, so the existing CI step
  "adapters are in sync" (regenerate + `git diff --exit-code`) catches drift and
  the three adapter mirrors stay byte-identical. `flake.nix` adds
  `checks.<system>.model-map-doc` running `--check` against the sources, so
  `nix flake check` fails on drift too.
- Prose that says a ladder bump needs a `dispatch.sh` edit is updated to point
  at `defaults.json` + the generator.

## Tests

- **Fixtures from `main`.** Before refactoring, a throwaway generator runs
  `main`'s own gate code (the tier-gate block wrapped in a function,
  `_escalation_target`, `_escalation_model_matches`, the record-only `case`,
  the pace `case`) over an enumerated input set and writes
  `tests/fixtures/model-map/*.tsv` (committed, provenance in a header line).
  Inputs: every id/glob instance in any arm, each with `-fast`, bracket-block
  and near-miss variants (e.g. `gpt-5.6`, `claude-opus-5-high`, `claude--high`,
  `claudex-high`, `gpt-5.5-extra-high`, `composer-2.5[effort=high]`,
  `grok-4.7-xhigh`), crossed with every engine × tier for the gate; for
  escalation, engine × tier × failed × model over each engine's own ids. The
  end-to-end refusal lines are captured by running `main`'s `dispatch.sh` under
  the bats harness once per engine × tier with an off-row model.
- `tests/model-map.bats`: table-driven over the fixtures using the new helpers
  (extracted with `sed` like the existing `_add_dir_ok` unit tests) and the
  resolver's output — identical accept/reject + `expected`, identical
  out-of-row and record-only `escalated_from`, identical pace downgrade; the
  12 end-to-end refusal lines byte-identical through the new `dispatch.sh`;
  the doc `--check` passes on the repo and fails on a tampered copy (a row
  edit, a worker-cell edit); the new helpers are byte-identical between
  `dispatch.sh` and `dispatch-resume.sh`.
- `tests/dispatch-config.bats`: absent user/locked files; user overrides base;
  locked overrides user; env overrides all; empty env var is unset; deep merge
  keeps sibling keys and replaces arrays; user-layer `grantRoots` and
  `openrouter.keyFile` ignored with the warning; `--show-origin` names each
  layer; malformed user file and unreadable locked path exit 1.
- `tests/dispatch.bats` (appended): a user-layer `grantRoots` cannot make
  `--add-dir` succeed (refused, warning printed); the same roots in a locked
  file do (control). `tests/dispatcher.bats` (appended): a user-layer `engines`
  applies when env is unset and loses to env. `tests/refresh-budget.bats`
  (appended): user-layer `monthlyUsd` is used when env is unset; user-layer
  `keyFile` is ignored.
- Existing tests: bodies unchanged, with one necessary exception —
  "tier map conformance" greps model ids inside `dispatch.sh`, which by design
  no longer holds them; its second grep searches `dispatch.sh` and `defaults.json` together (the
  mechanism now spans both; `claude-fable-5-1` stays only in dispatch.sh's
  shape message)
  (dispatcher confirmed and waived AC1 for that test only). `tests/helpers.bash` gains isolation only:
  `DISPATCH_CONFIG_BIN`, a per-test `XDG_CONFIG_HOME`, `unset
DISPATCH_LOCKED_SETTINGS`.

## Packaging

`flake.nix`: new `dispatch-config` `writeShellApplication` (runtimeInputs
`jq coreutils`, `@defaultsJson@` substituted), added to `default`'s
`symlinkJoin`; `@dispatchConfig@` substituted into `dispatch`,
`dispatch-resume`, `dispatcher`, `refresh-budget`; `checks.model-map-doc`.
`adapters/core/dispatch-config.sh` is executable and shfmt/shellcheck clean.
`tests/module.bats` only gains an appended test (the built consumers bake the
resolver): `hm-module.nix` does not install the new package in this slice (the
next slice does), so its package-list assertion holds.

## Follow-ups (issues, not this slice)

- Trackers and `DISPATCH_PROFILE` through the resolver.
- `crew.sh` burn classes and `dispatcher.sh` orchestrator defaults as data.
- Budget thresholds as data.
- hm slice: `DISPATCH_LOCKED_SETTINGS` is chosen by env, so the locked layer is
  exactly as trusted as env (which already wins for `grantRoots`); decide there
  whether to bake its path instead of exporting it.

## Risks

- **Gate drift in transcription** — mitigated by the fixture captured from
  `main`'s own code and the byte-identity e2e checks.
- **Resolver cost per dispatch** — one extra bash + a few jq processes;
  negligible next to dispatch's git/gh work.
- **Host leakage into tests** — a developer's real
  `~/.config/dispatcher/settings.json` would change test outcomes;
  `helpers.bash` pins `XDG_CONFIG_HOME` per test and unsets
  `DISPATCH_LOCKED_SETTINGS`.
