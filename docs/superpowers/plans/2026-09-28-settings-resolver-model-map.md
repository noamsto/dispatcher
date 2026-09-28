# Settings resolver and model map as data — plan (#560)

Spec: `docs/superpowers/specs/2026-09-28-settings-resolver-model-map-design.md`.
Base: `main` @ 4c5ed21. All commands run from the worktree root inside the
devshell (`direnv` is active; `bats`, `jq`, `shellcheck`, `shfmt` on PATH).

## File list

| file                                                                                    | purpose                                                                                                                         |
| --------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `tests/fixtures/model-map/gate.tsv`                                                     | new — `main`'s tier-row decision per engine×tier×model (`ok`, `expected`)                                                       |
| `tests/fixtures/model-map/escalation.tsv`                                               | new — `main`'s escalation decisions per engine×tier×failed×model (dispatch out-of-row, dispatch record-only, resume out-of-row) |
| `tests/fixtures/model-map/pace.tsv`                                                     | new — `main`'s pace model downgrade per engine×model                                                                            |
| `tests/fixtures/model-map/refusals.tsv`                                                 | new — `main`'s end-to-end tier refusal line per engine×tier                                                                     |
| `adapters/core/defaults.json`                                                           | new — layer 1: `modelMap`, `escalation`, `paceDowngrades`                                                                       |
| `adapters/core/dispatch-config.sh`                                                      | new, mode 755 — the resolver                                                                                                    |
| `tests/dispatch-config.bats`                                                            | new — resolver tests                                                                                                            |
| `tests/model-map.bats`                                                                  | new — fixture-driven equivalence, e2e refusal bytes, helper parity, doc-check teeth                                             |
| `tests/helpers.bash`                                                                    | isolation: `DISPATCH_CONFIG_BIN`, per-test `XDG_CONFIG_HOME`, `unset DISPATCH_LOCKED_SETTINGS`                                  |
| `adapters/core/dispatch.sh`                                                             | `_settings_load` + lookup helpers replace the tier-gate, escalation, record-only and pace `case` arms                           |
| `adapters/core/dispatch-resume.sh`                                                      | same `_settings_load` / escalation helpers (byte-identical copies) replace its `_escalation_target` copy                        |
| `adapters/core/dispatcher.sh`                                                           | engine roster through the resolver                                                                                              |
| `adapters/core/refresh-budget.sh`                                                       | OpenRouter target + key file through the resolver                                                                               |
| `tests/dispatch.bats`                                                                   | append grant-roots invariant tests; retarget "tier map conformance" oracle (dispatcher-waived)                                  |
| `tests/dispatcher.bats`, `tests/refresh-budget.bats`                                    | append user-layer tests                                                                                                         |
| `tests/module.bats`                                                                     | append one test: built consumers bake the resolver                                                                              |
| `scripts/gen-model-map-doc.sh`                                                          | new — render/check the generated doc regions + Model map worker cells                                                           |
| `scripts/gen-adapters.sh`                                                               | call the renderer before copying protocols                                                                                      |
| `adapters/core/protocols/dispatch-orchestration.md`                                     | generated regions + prose pointing at `defaults.json`                                                                           |
| `adapters/{claude-code/plugin,codex/plugin,cursor}/protocols/dispatch-orchestration.md` | regenerated mirrors (never hand-edited)                                                                                         |
| `flake.nix`                                                                             | `dispatch-config` package, `@dispatchConfig@` substitution in 4 consumers, `checks.model-map-doc`                               |
| `README.md`                                                                             | short "Settings" section: layers, `dispatch-config`, security keys                                                              |

Nothing else. `nix/hm-module.nix` is untouched; `tests/module.bats` only gains an appended test.

## Steps

- [ ] **Step 1: capture `main`'s gate decisions as fixtures** (before any source edit)
      Files: `tests/fixtures/model-map/{gate,escalation,pace}.tsv`; a throwaway
      generator in the scratchpad (not committed).
      The generator reads `git show 4c5ed21:adapters/core/dispatch.sh` and
      `git show 4c5ed21:adapters/core/dispatch-resume.sh` (the pinned base, not
      the moving `main`) and, by `sed` range
      extraction of the real text (not retyping), defines:
  - `old_gate <agent> <tier> <model>`: the block from `  tier_ok=1` through the
    `  esac` that closes `case "$agent" in` inside `if [ -z "$ignore_map" ]`
    (dispatch.sh ~:2599-2694), wrapped in a function with `re_effort_tail` set
    exactly as :2377; prints `<tier_ok>\t<tier_expected>`.
  - `_escalation_target`, `_escalation_model_matches` (dispatch.sh functions).
  - `old_record <agent> <tier> <failed> <model>`: the record-only `case`
    (dispatch.sh ~:2738-2765) wrapped; prints `escalated_from` or nothing.
  - `old_pace <agent> <model>`: the `case "$target_agent:$target_model"` arm of
    `pace_rule_target` (~:46-51) wrapped; prints `model_downgrade` or nothing.
  - dispatch decision `old_dispatch_esc <a> <t> <failed> <model>`: if
    `old_gate` ok=0 → `_escalation_target`; when non-empty and target ≠
    `RECORD_ONLY` and `_escalation_model_matches target model` → `admit\t<baseline>`,
    else `refuse\t`; if ok=1 → `record\t<old_record output>` (empty label = no stamp).
  - resume decision `old_resume_esc` from dispatch-resume.sh's own
    `_escalation_target`/`_escalation_model_matches` copies: `admit\t<baseline>` or `none\t`.
    Model universe `M` (one list, used for gate and pace): every literal id in
    the arms and messages (`opus sonnet haiku fable`, `claude-opus-5
claude-sonnet-5 claude-haiku-4-5 claude-fable-5-1`, `gpt-5.6-sol
gpt-5.6-terra gpt-5.6-luna gpt-5.5 gpt-5.4 gpt-5.4-mini`, `kimi-k3-high`,
    `grok-4.7-{low,medium,high}` and `cursor-grok-4.6-{low,medium,high}` each
    bare and `-fast`, `composer-2.5 composer-2.5-fast`, the four pi ids) plus
    variants/near-misses: `gpt-5.6 gpt-5.7-sol gpt-5.4-nano`, `kimi-k3-high-fast
kimi-k3-medium`, `grok-4.7-xhigh grok-4.7-low-slow grok-4.6-high`,
    `cursor-grok-4.6-high[effort=high] grok-4.7-high[effort=high]
cursor-grok-4.6-lowx`, `composer-2.5[effort=high] composer-2`,
    `claude-opus-5-high claude-opus-5-high-fast claude-high claude--high
claudex-high claude-thigh claude-opus-5[effort=high]
claude-opus-5[context=1m,effort=high,fast=false] claude-opus-5[context=1m]
claude-opus-5[xeffort=high] gpt-5.6-sol-high gpt-5.5-extra-high
gpt-5.6-sol[effort=high] gpt-high`, effort-tail ids with a bracket block
    and malformed blocks `claude-opus-5-high[context=1m]
gpt-5.6-sol-high-fast[context=1m] claude-opus-5-high[effort=]
claude-opus-5-high[] claude-opus-5-high[Context=1m]
claude-opus-5-high[context=1m,] claude-opus-5[effort=high,context=1m]
claude-opus-5[context=1m,effort=high]`, `openrouter/deepseek/deepseek-v4-pro
openrouter/foo/bar`, `sonnet-5 opus-fast`. No id contains `:` (spec:
    shape-gate scope).
    `gate.tsv` = every agent∈{claude,codex,cursor,pi} × tier∈{deep,standard,trivial} × `M`.
    `pace.tsv` = every agent × `M`.
    `escalation.tsv` = agent × tier × failed∈`E[agent]` × model∈`E[agent]`, where
    `E[agent]` = the ids of `M` whose shape belongs to that engine (claude: the
    aliases + `claude-*` claude ids; codex: `gpt-*` non-cursor; cursor:
    `kimi*`/`*grok*`/`composer*`/the effort-suffixed/bracketed claude/gpt ids;
    pi: `openrouter/*`), columns `agent tier failed model dispatch_kind dispatch_label resume_kind resume_label`.
    Each file starts with `# generated from 4c5ed21 dispatch.sh/dispatch-resume.sh gate code — do not edit`.
    **Cell encoding:** every empty cell (no downgrade, no label, empty
    `expected`) is written as a single `-`, so no line ends in a tab (the
    `trim-trailing-whitespace` pre-commit hook, part of `nix flake check`) and no
    two tabs are adjacent. `IFS=$'\t' read -r a b c` is wrong for this (tab is
    IFS-whitespace, so runs collapse): tests parse each line with
    `IFS= read -r line` then `mapfile -d $'\t' -t f < <(printf '%s' "$line")`,
    skip lines starting `#`, and map a `-` cell back to empty before comparing.
    No model id is `-`.
    Proof: `wc -l tests/fixtures/model-map/*.tsv` (gate ≈ 12×|M|); spot-check
    `grep -P '^claude\tdeep\tfable\t1' gate.tsv`, `grep -P '^codex\tstandard\tgpt-5.6-terra\tgpt-5.6-sol\tadmit\tterra' escalation.tsv`,
    `grep -P '^cursor\tstandard\tcursor-grok-4.6-medium\tcursor-grok-4.6-high-fast\tadmit\tmedium' escalation.tsv`,
    `grep -P '^claude\tdeep\topus\tfable\trecord\topus \(record only\)' escalation.tsv`,
    `grep -P '^cursor\tgrok-4.7-high\[effort=high\]\tgrok-4.7-medium' pace.tsv` — each prints one line.

- [ ] **Step 2: capture `main`'s end-to-end refusal lines**
      Files: `tests/fixtures/model-map/refusals.tsv`; throwaway scratch bats file.
      A scratch `.bats` that `load`s `tests/helpers` and reuses `tests/dispatch.bats`'s
      `setup()` body verbatim, with `DISPATCH` pointed at a scratch copy of
      `4c5ed21:adapters/core/dispatch.sh`, runs for each engine×tier one shape-valid
      off-row model — claude deep `haiku` / standard `haiku` / trivial `fable`;
      codex deep `gpt-5.6-luna` / standard `gpt-5.6-sol` / trivial `gpt-5.6-terra`;
      cursor deep `grok-4.7-low` / standard `grok-4.7-high` / trivial
      `grok-4.7-medium`; pi deep `openrouter/deepseek/deepseek-v4-flash` /
      standard `openrouter/foo/bar` / trivial `openrouter/z-ai/glm-5.3-flash` —
      as `run_dispatch <tier> <model> --agent <a> --effort high --crew-id c1 42 "t"`
      (`--effort high` is valid on every engine) and writes
      `<agent>\t<tier>\t<model>\t<the single stderr line containing "is not $tier's row">`
      (same header + parsing rules as step 1; no refusal line contains a tab).
      Proof: `wc -l` = 12 data lines + header; each line contains `--ignore-map`.
      Commit steps 1–2 together: `git add tests/fixtures/model-map && git commit -m "test: capture main's model-map gate decisions as fixtures (#560)"`.

- [ ] **Step 3: `defaults.json`**
      File: `adapters/core/defaults.json`, schema exactly as the spec (no
      `engines`). Transcription rules:
  - `modelMap` rows, in engine order claude, codex, cursor, pi and tier order
    deep, standard, trivial. `expected` = the `tier_expected` string verbatim.
    claude `models`: deep `opus claude-opus-* sonnet claude-sonnet-* fable claude-fable-*`;
    standard `opus claude-opus-* sonnet claude-sonnet-*`; trivial `opus claude-opus-* sonnet claude-sonnet-* haiku claude-haiku-*`.
    codex: deep `gpt-5.6-sol gpt-5.6-terra gpt-5.5 gpt-5.4 gpt-5.4-mini`;
    standard `gpt-5.6-terra gpt-5.6-luna` + legacy three; trivial `gpt-5.6-luna` + legacy three.
    cursor: deep `kimi-k3-high`, `grok-4.7-medium(-fast)`, `grok-4.7-high(-fast)`,
    `cursor-grok-4.6-medium(-fast)`, `cursor-grok-4.6-high(-fast)`, `composer-2.5(-fast)`
    plus `regex`:
    `^(claude|gpt)(-[a-z0-9.-]*)?-(none|low|medium|high|xhigh|max)(-fast)?(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$`
    and `^(claude|gpt)-[a-z0-9.-]*\[([a-z]+=[a-z0-9.-]+,)*effort=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\]$`;
    standard `grok-4.7-{medium,low}(-fast)`, `cursor-grok-4.6-{medium,low}(-fast)`, `composer-2.5(-fast)`;
    trivial `grok-4.7-low(-fast)`, `cursor-grok-4.6-low(-fast)`, `composer-2.5(-fast)`.
    pi: deep `openrouter/deepseek/deepseek-v4.1-flash`; standard that + `…/deepseek-v4-flash`,
    `openrouter/z-ai/glm-5.3-flash`, `openrouter/qwen/qwen3.8-flash`; trivial `…v4-flash`, `…v4.1-flash`.
    `default`: claude opus/opus/opus; codex sol/terra/luna; cursor
    `kimi-k3-high`/`grok-4.7-medium`/`grok-4.7-low`; pi v4.1/v4.1/v4-flash (full ids).
  - `escalation` (first-match order as today's arms):
    claude trivial `[{failed:[haiku,claude-haiku-*],baseline:haiku,inRow:[sonnet,claude-sonnet-*]},{failed:[sonnet,claude-sonnet-*],baseline:sonnet,inRow:[opus,claude-opus-*]}]`;
    claude standard `[{failed:[sonnet,claude-sonnet-*],baseline:sonnet,inRow:[opus,claude-opus-*]}]`;
    claude deep `[{failed:[sonnet,claude-sonnet-*],baseline:sonnet,inRow:[opus,claude-opus-*]},{failed:[opus,claude-opus-*],baseline:opus,inRow:[fable,claude-fable-*]}]`;
    codex standard `[{failed:[gpt-5.6-luna],baseline:luna,inRow:[gpt-5.6-terra]},{failed:[gpt-5.6-terra],baseline:terra,outOfRow:[gpt-5.6-sol]}]`;
    codex deep `[{failed:[gpt-5.6-terra],baseline:terra,inRow:[gpt-5.6-sol]}]`;
    cursor standard `[{failed:[cursor-grok-4.6-low*],baseline:low,inRow:[cursor-grok-4.6-medium*]},{failed:[cursor-grok-4.6-medium*],baseline:medium,outOfRow:[cursor-grok-4.6-high,cursor-grok-4.6-high-fast]}]`;
    cursor deep `[{failed:[cursor-grok-4.6-medium*],baseline:medium,inRow:[cursor-grok-4.6-high*]}]`;
    pi standard `[{failed:[openrouter/deepseek/deepseek-v4-flash],baseline:v4-flash,inRow:[openrouter/deepseek/deepseek-v4.1-flash]}]`.
  - `paceDowngrades`: claude `[{models:[opus,claude-opus-*,fable,claude-fable-*],to:sonnet}]`;
    codex `[{models:[gpt-5.6-sol],to:gpt-5.6-terra}]`;
    cursor `[{models:[grok-4.7-high,"grok-4.7-high\\[*"],to:grok-4.7-medium},{models:[cursor-grok-4.6-high,"cursor-grok-4.6-high\\[*"],to:cursor-grok-4.6-medium}]`.
    Proof: `jq -e 'has("modelMap") and has("escalation") and has("paceDowngrades") and (has("engines")|not)' adapters/core/defaults.json` → `true`;
    `jq -r '[.modelMap[][] | .default] | length' adapters/core/defaults.json` → `12`.

- [ ] **Step 4: resolver tests (red)**
      File: `tests/dispatch-config.bats` (`load helpers`; `CONFIG="$BATS_TEST_DIRNAME/../adapters/core/dispatch-config.sh"`;
      each test writes layers under `$BATS_TEST_TMPDIR` and sets `XDG_CONFIG_HOME` /
      `DISPATCH_LOCKED_SETTINGS` explicitly; `unset DISPATCH_ENGINES DISPATCH_GRANT_ROOTS DISPATCH_OPENROUTER_MONTHLY_USD DISPATCH_OPENROUTER_KEY_FILE` in setup). Cases:
  1. no user/locked file, no env → output equals `jq . defaults.json` (`jq -S` both sides); stderr empty.
  2. user `{"engines":["claude"]}` → `.engines == ["claude"]`, `.modelMap` still equals base's.
  3. deep merge: user `{"modelMap":{"pi":{"deep":{"default":"x"}}}}` → `.modelMap.pi.deep.default=="x"` and `.modelMap.pi.deep.models` equals base's; an array in user replaces the base array wholesale.
  4. locked overrides user: user engines `["claude"]`, locked `["codex"]` → `["codex"]`.
  5. env overrides all: plus `DISPATCH_ENGINES="pi  claude"` → `["pi","claude"]`; `DISPATCH_GRANT_ROOTS="/a::/b"` → `["/a","/b"]`; `DISPATCH_OPENROUTER_MONTHLY_USD=50` → `"50"`; `DISPATCH_OPENROUTER_KEY_FILE=/k` → `"/k"`.
  6. empty env var is unset: locked engines `["codex"]`, `DISPATCH_ENGINES=""` → `["codex"]`.
  7. user `grantRoots` and `openrouter.keyFile` ignored: output has no `grantRoots`, no `.openrouter.keyFile`, keeps user `openrouter.monthlyUsd`; stderr (separate capture via `run --separate-stderr`) contains `ignoring grantRoots from <user file>` and `ignoring openrouter.keyFile from <user file>`; exit 0. Same keys from locked are kept.
  8. `--show-origin`: base leaf → `{"value":…,"origin":"base"}` (`.modelMap.claude.deep.default`), user → `user`, locked → `locked`, env → `env` (e.g. `.engines.origin=="env"` with `DISPATCH_ENGINES` set); array leaf reported whole.
  9. malformed user file (`{`) → exit 1, stderr names the file; user file `[1]` (not an object) → exit 1.
  10. `DISPATCH_LOCKED_SETTINGS` pointing at a missing file → exit 1 naming it.
  11. validation: locked `grantRoots: ["/a:b"]` → exit 1 naming `grantRoots`; user `engines: "claude"` → exit 1 naming `engines`.
  12. unknown argument → exit 2 with a usage line.
      Proof: `bats tests/dispatch-config.bats` → all fail (script missing).

- [ ] **Step 5: the resolver (green)**
      File: `adapters/core/dispatch-config.sh` (`#!/usr/bin/env bash`, `set -euo pipefail`, `chmod 755`).
      `base="@defaultsJson@"`; when it still starts with `@` (raw source) use
      `"$(dirname "${BASH_SOURCE[0]}")/defaults.json"`. Read each layer with
      `jq -e 'type == "object"'` (fail → `dispatch-config: <path> is not a JSON object` exit 1).
      User file: `${XDG_CONFIG_HOME:-$HOME/.config}/dispatcher/settings.json`, skipped when absent.
      Locked: `${DISPATCH_LOCKED_SETTINGS:-}`; when non-empty the file must be readable.
      Env layer built with `jq -n --arg …` from the four vars, each only when non-empty
      (engines: `split` on whitespace dropping empties; grantRoots: `split(":")` dropping empties).
      Security strip on base and user: `del(.grantRoots)`, `del(.openrouter.keyFile)`, warning per key present:
      `dispatch-config: ignoring <key> from <path> — it is honoured only from the locked settings or the environment`.
      Merge `base * user * locked * env` in one jq program; validate per spec
      (engines array of strings; grantRoots array of strings, none containing `:`;
      keyFile string; monthlyUsd number|string) → `dispatch-config: <key> must be … (merged settings)` exit 1.
      `--show-origin`: one jq program over the four layers: for each leaf path of
      the merged tree (`paths(type != "object" or length == 0)` restricted to
      merged-leaf paths), origin = last layer where the parent is an object that
      `has` the key. Output `jq .`.
      Proof: `bats tests/dispatch-config.bats` all pass; `shellcheck adapters/core/dispatch-config.sh` clean; `shfmt -i 2 -d adapters/core/dispatch-config.sh` empty.

- [ ] **Step 6: test isolation + packaging**
      Files: `tests/helpers.bash`, `flake.nix`.
      helpers.bash (top level, beside the existing `unset` lines):
      `export DISPATCH_CONFIG_BIN="$BATS_TEST_DIRNAME/../adapters/core/dispatch-config.sh"` (resolves in every
      test file since all live in `tests/`), `export XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/config"`, `unset DISPATCH_LOCKED_SETTINGS`,
      with a one-line comment each on why.
      flake.nix: in the `packages` `let`, add `dispatchConfig = pkgs.writeShellApplication { name = "dispatch-config"; runtimeInputs = with pkgs; [jq coreutils]; text = builtins.replaceStrings ["@defaultsJson@"] ["${./adapters/core/defaults.json}"] (builtins.readFile ./adapters/core/dispatch-config.sh); };`
      and `withConfig = builtins.replaceStrings ["@dispatchConfig@"] ["${dispatchConfig}/bin/dispatch-config"];`;
      add `dispatch-config = dispatchConfig;` to the `rec` set and to `default`'s paths;
      wrap the `text` of `dispatch`, `dispatch-resume`, `dispatcher` (`withConfig (sub …)`) and `refresh-budget` (`withConfig (builtins.readFile …)`).
      Proof: `nix build .#dispatch-config .#dispatch .#dispatch-resume .#dispatcher .#refresh-budget --no-link` succeeds;
      `nix run .#dispatch-config | jq -e .modelMap` → object;
      full suite still green: `bats --jobs 16 --filter-tags '!timing' $(find tests -maxdepth 1 -name '*.bats' ! -name module.bats | sort)`.

- [ ] **Step 7: model-map equivalence tests (red)**
      File: `tests/model-map.bats`. Setup: `load helpers`;
      `DISPATCH="$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"`,
      `RESUME=…dispatch-resume.sh`; `settings="$("$DISPATCH_CONFIG_BIN")"`;
      `eval` the helpers from dispatch.sh by `sed -n '/^<fn>() {/,/^}/p'` for
      `_glob_match _model_in_row _row_expected _escalation_hop _pace_downgrade`.
      Tests (each loops its fixture, collects every mismatch into a list, prints
      them all and fails if non-empty — never stops at the first):
  1. gate: for each `gate.tsv` row, `_model_in_row a t m` status → ok bit; `_row_expected a t` == expected.
  2. dispatch escalation: per `escalation.tsv` row, compute
     ok via `_model_in_row`; ok=0 → `_escalation_hop a t failed model outOfRow` non-empty ⇒ `admit\t<it>` else `refuse\t`;
     ok=1 → `_escalation_hop … inRow` non-empty ⇒ `record\t<it> (record only)` else `record\t`; equals columns 5–6.
  3. resume escalation: `_escalation_hop` evaluated from dispatch-resume.sh's copy, `outOfRow` ⇒ `admit\t<it>` else `none\t`; equals columns 7–8.
  4. pace: `_pace_downgrade a m` equals column 3 (empty allowed).
  5. e2e refusal bytes: reuse `tests/dispatch.bats`'s `setup()` body (stub bins, protocols dir) in a local helper; for each `refusals.tsv` row run the new `dispatch.sh` and assert the stderr line containing `is not <tier>'s row` equals the fixture line byte-for-byte.
  6. parity: `_settings_load _glob_match _escalation_hop` are byte-identical between dispatch.sh and dispatch-resume.sh (same loop shape as dispatch-resume.bats:569).
     Proof: `bats tests/model-map.bats` — 1–4, 6 fail (functions absent); 5 passes (behaviour unchanged).

- [ ] **Step 8: dispatch.sh reads the map through the resolver (green)** (implement: escalated)
      File: `adapters/core/dispatch.sh`. Add, next to the engine helpers (~:541):

  ```bash
  _settings_load() {
    settings="$("${DISPATCH_CONFIG_BIN:-@dispatchConfig@}")"
    [ -n "${DISPATCH_ENGINES:-}" ] || DISPATCH_ENGINES="$(jq -r '.engines // [] | join(" ")' <<<"$settings")"
    [ -n "${DISPATCH_GRANT_ROOTS:-}" ] || DISPATCH_GRANT_ROOTS="$(jq -r '.grantRoots // [] | join(":")' <<<"$settings")"
  }
  _glob_match()      # <model> — true when <model> matches a glob read from stdin (one per line)
  _model_in_row()    # <agent> <tier> <model> — globs from .modelMap[a][t].models, EREs from .regex
  _row_expected()    # <agent> <tier> — prints .modelMap[a][t].expected // ""
  _escalation_hop()  # <agent> <tier> <failed> <model> <inRow|outOfRow> — prints the baseline of the
                     # first rule whose `failed` glob matches <failed>, when its <kind> list admits
                     # <model> (inRow: glob; outOfRow: exact id); else prints nothing
  _pace_downgrade()  # <agent> <model> — first .paceDowngrades[a][] whose models glob matches: prints .to
  ```

  A non-empty env var is left exactly as set (the resolver's env layer wins
  anyway, so this only keeps today's raw string: a whitespace-only
  `DISPATCH_ENGINES` still enables nothing, and odd spacing still prints
  verbatim in the roster refusal); the resolver fills only what env leaves
  unset. (Globs compared with an unquoted RHS, `# shellcheck disable=SC2053` on those
  lines; every jq reads `<<<"$settings"`; missing keys → empty → refuse/no-op).
  Call `_settings_load` as the first statement of the `--spawn-role` branch,
  of the `--engines` branch, and in the main path immediately before
  `profile="${DISPATCH_PROFILE:-personal}"` (~:2366). Replace:
  - in `pace_rule_target`, the `case "$target_agent:$target_model"` with
    `model_downgrade="$(_pace_downgrade "$target_agent" "$target_model")"`;
  - `_escalation_target` and `_escalation_model_matches` definitions: delete;
  - the tier-gate `case "$agent" in … esac` (including all `tiermap_*`) with
    `tier_ok=0; _model_in_row "$agent" "$tier" "$model" && tier_ok=1`, and
    `tier_expected="$(_row_expected "$agent" "$tier")"` inside the refusal branch
    before the unchanged `echo`;
  - the out-of-row branch body with `escalation_baseline="$(_escalation_hop "$agent" "$tier" "$failed_model" "$model" outOfRow)"`, then
    `if [ -n "$escalation_baseline" ] && _prior_failed_escalation_available …; then tier_ok=1; escalated_from="$escalation_baseline"; fi`;
  - the record-only `case` with
    `record_from="$(_escalation_hop "$agent" "$tier" "$failed_model" "$model" inRow)"; [ -z "$record_from" ] || escalated_from="$record_from (record only)"`.
    Update the gate's comment block to say the rows live in `defaults.json`
    (drop "Keep in sync with dispatch-orchestration.md"). Leave the Model gate
    (shape), `ENGINES_ALL`, `engine_enabled`, `check_engine`, `_add_dir_ok`,
    `write_launch_script`, and every message text untouched.
    Proof: `bats tests/model-map.bats` tests 1, 2, 4, 5 pass;
    `bats --jobs 16 tests/dispatch.bats` — only "tier map conformance" fails (retargeted in step 10);
    `shellcheck adapters/core/dispatch.sh` clean.

- [ ] **Step 9: dispatch-resume.sh through the resolver**
      File: `adapters/core/dispatch-resume.sh`. Copy `_settings_load _glob_match
_escalation_hop` byte-identically from dispatch.sh (only what resume calls)
      (replacing its `_escalation_target`/`_escalation_model_matches` copies and
      their "duplicated from dispatch.sh" comment, which then names the new set);
      call `_settings_load` right after `_check_protocol_rev "$PROTOCOL_DIR" "dispatch resume"` (~:671).
      Escalation block: `escalation_baseline=""; [ "$bus_failed_model" = "$orig_model" ] && escalation_baseline="$(_escalation_hop "$agent" "$tier" "$orig_model" "$model" outOfRow)"`;
      `if [ -n "$escalation_baseline" ] && _prior_failed_escalation_available "$branch" "$crew_dir"; then precheck+=(--ignore-map); escalated_from="$escalation_baseline"; fi`.
      Proof: `bats tests/model-map.bats` all 6 pass; `bats --jobs 16 tests/dispatch-resume.bats` green; `shellcheck adapters/core/dispatch-resume.sh` clean.

- [ ] **Step 10: security invariant + retargeted tripwire**
      File: `tests/dispatch.bats` (append two tests; one oracle retarget).
      Red first against the step-8 tree is not possible for the "cannot widen"
      half (main never read a user layer), so the control carries the teeth:
  - `add-dir: a user-layer grantRoots cannot widen --add-dir`: `mkdir -p "$T/roots/proj"`;
    user settings `{"grantRoots":["$T/roots"]}` under `$XDG_CONFIG_HOME/dispatcher/settings.json`, no env roots;
    `run run_dispatch standard sonnet --agent claude --effort medium --crew-id c1 --add-dir "$T/roots/proj" 42 "t"` →
    status 1, output contains `--add-dir '` and `refused`, and `ignoring grantRoots from`.
  - `add-dir: a locked-layer grantRoots admits the same --add-dir` (control): same dir, the roots in a file named by `DISPATCH_LOCKED_SETTINGS`,
    reached with `stub_launch_bins` like the existing `--add-dir records the canonical dir` test (:6252) → status 0 and the dir recorded in the grants file.
  - In "tier map conformance" (:2884) change only the second grep so it searches
    both files of the mechanism — `grep -qF "$token" "$DISPATCH" "$BATS_TEST_DIRNAME/../adapters/core/defaults.json"` —
    and its message to `missing from dispatch.sh and defaults.json` (dispatcher-waived, AC1).
    Grepping `defaults.json` alone would fail on `claude-fable-5-1`, which lives
    only in dispatch.sh's claude shape message (defaults.json holds the
    `claude-fable-*` glob); padding `defaults.json` with an unread literal to
    satisfy the test would be gaming it. Same token list, same doc slice.
    Proof: `bats -f 'add-dir: a (user|locked)-layer|tier map conformance' tests/dispatch.bats` → 3 pass;
    temporarily deleting the `del(.grantRoots)` strip for the user layer in the resolver makes the first test fail (observe, then restore).

- [ ] **Step 11: dispatcher.sh + refresh-budget.sh**
      Files: `adapters/core/dispatcher.sh`, `adapters/core/refresh-budget.sh`, `tests/dispatcher.bats`, `tests/refresh-budget.bats`.
      Tests first (appended):
  - dispatcher.bats: `a user-layer engine roster applies when the env roster is unset` (user `{"engines":["claude","pi"]}` → `--agent codex` refused with `(enabled: claude pi)`); `the env roster outranks the user layer` (same file + `DISPATCH_ENGINES="claude codex pi"` → codex passes the gate, status 0 with `CREW_ID=c1`).
  - refresh-budget.bats: `a user-layer monthly target is used when the env target is unset` (mirror the existing target test at :751 with the target in the user file instead of env — same expected `target_usd`); `a user-layer keyFile is ignored` (user `openrouter.keyFile` → a readable key file; no env key → the "pi spend unknown — set OPENROUTER_API_KEY" warning, and stderr names `ignoring openrouter.keyFile`).
    Then: dispatcher.sh — after the `--agent` validation `case`, before `ENGINES_ALL`:
    `settings="$("${DISPATCH_CONFIG_BIN:-@dispatchConfig@}")"` and
    `[ -n "${DISPATCH_ENGINES:-}" ] || DISPATCH_ENGINES="$(jq -r '.engines // [] | join(" ")' <<<"$settings")"`;
    refresh-budget.sh — after `warn()` is defined, `settings="$("${DISPATCH_CONFIG_BIN:-@dispatchConfig@}")"`,
    `[[ -n ${DISPATCH_OPENROUTER_MONTHLY_USD:-} ]] || DISPATCH_OPENROUTER_MONTHLY_USD="$(jq -r '.openrouter.monthlyUsd // "" | tostring' <<<"$settings")"`,
    `[[ -n ${DISPATCH_OPENROUTER_KEY_FILE:-} ]] || DISPATCH_OPENROUTER_KEY_FILE="$(jq -r '.openrouter.keyFile // ""' <<<"$settings")"`; header comment notes the resolver.
    Proof: `bats tests/dispatcher.bats tests/refresh-budget.bats tests/crews.bats` green (new ones red before the edit); `shellcheck` both; `shfmt -i 2 -d` both empty.

- [ ] **Step 12: doc renderer + generated regions + flake check**
      Files: `scripts/gen-model-map-doc.sh`, `scripts/gen-adapters.sh`, `adapters/core/protocols/dispatch-orchestration.md`, `flake.nix`, `tests/model-map.bats` (append), mirrors.
      Test first (append to model-map.bats): `doc check passes on the repo`
      (`bash scripts/gen-model-map-doc.sh --check` exit 0); `doc check fails on a stale generated row` (copy doc to tmp, edit one generated cell, `--check <defaults> <tmpdoc>` exit 1);
      `doc check fails on a worker cell that disagrees with default` (tmp copy, change `**opus** → **sonnet**` in the `deep` row's first bold → exit 1).
      Renderer `scripts/gen-model-map-doc.sh [--check] [<defaults.json> <doc.md>]` (defaults = repo paths):
      region `tier-rows`: `| engine | tier | typical launch model | the gate accepts |` then one row per engine×tier in data order, launch model in backticks, accepts = `expected` with `*` escaped as `\*`;
      region `pace-downgrades`: `| engine | premium | downgrade target |`, one row per `paceDowngrades` entry (globs backticked, `\[` shown as `[`), and `| pi | — (effort only: \`max\`→\`xhigh\`→\`high\`) | — |`for each modelMap engine with no entry.
Regions are delimited by`<!-- BEGIN generated:<name> from adapters/core/defaults.json by scripts/gen-model-map-doc.sh -->`/`<!-- END generated:<name> -->`.
Write mode rewrites the doc in place; `--check`renders to a temp file and`diff -u`s, then checks the Model map table (the pipe-table under `## Model map`: header row engine names = first word of each column after `Tier`; rows `` | `deep` ``/`standard`/`trivial`): first `**…**`token of each cell, backticks stripped, equals`.modelMap[engine][tier].default`; prints each mismatch; exit 1 on any.
Doc edits: insert the `tier-rows`region in "Tier map" after the acceptance-rule paragraph; replace the pace table with the`pace-downgrades`region; line 3 "Keep it in sync with …`dispatch.sh`(the mechanism)" → name`defaults.json`as the map's data; Model map intro: after "prose elsewhere says \"the tier-appropriate model from the model map\"." add
"The rows the Tier map gate enforces are data in`adapters/core/defaults.json`; the tables under \"Tier map\" are generated from it and the first model in each cell below is checked against it."
Model gate :215-217, exact before → after:
"…so a model bump needs no `dispatch.sh`edit — true for this gate; the Tier map gate below hand-copies the same table and *does* need a`dispatch.sh`edit on a ladder bump (see \"Tier map\" below):"
→ "…so a model bump needs no`dispatch.sh`edit — true for this gate; the Tier map gate below reads its rows from`adapters/core/defaults.json`, so a ladder bump is a `defaults.json`edit plus a rebuild, with`scripts/gen-adapters.sh`regenerating the tables (see \"Tier map\" below):".
Override :276-277 before → after: "…needs`--ignore-map`until the Tier map's table and`dispatch.sh`are updated."
→ "…needs`--ignore-map`until its row in`adapters/core/defaults.json` is updated and rebuilt (`scripts/gen-adapters.sh`regenerates the tables)."
Leave :273 ("the`dispatch.sh`grammar follows on the next rebuild") alone — the shape grammar stays in dispatch.sh. No other doc change.`gen-adapters.sh`: run `bash "$root/scripts/gen-model-map-doc.sh"`before the protocols are copied.`flake.nix`perSystem:`checks.model-map-doc = pkgs.runCommand "model-map-doc" {nativeBuildInputs = with pkgs; [bash jq gawk diffutils coreutils gnused];} "bash ${./scripts/gen-model-map-doc.sh} --check ${./adapters/core/defaults.json} ${./adapters/core/protocols/dispatch-orchestration.md} && touch $out";`.
Then `./scripts/gen-adapters.sh`(regenerates the doc regions and the three mirrors).
Proof:`bats tests/model-map.bats`all pass;`./scripts/gen-adapters.sh && git diff --exit-code -- adapters/`clean on a second run;`nix build .#checks.x86_64-linux.model-map-doc --no-link`succeeds;`shfmt -i 2 -d scripts/gen-model-map-doc.sh`empty; editing one`expected`in defaults.json without regenerating makes it fail (observe, restore);`bats tests/adapters.bats`green;`shellcheck scripts/\*.sh` clean.

- [ ] **Step 12b: prove the built consumers use the baked resolver**
      File: `tests/module.bats` (append one test; setup_file already builds
      `dispatch`, `dispatch-resume`, `dispatcher`, `refresh-budget`).
      `@test "built consumers bake the settings resolver"`: `grep -c '@dispatchConfig@'` is `0` for each of the four built bins;
      each contains `/bin/dispatch-config`; and `env -u DISPATCH_CONFIG_BIN -u DISPATCH_ENGINES -u DISPATCH_LOCKED_SETTINGS XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/cfg" "$OUT_DISPATCH/bin/dispatch" --engines` exits 0
      (the baked resolver ran; prints whichever engine CLIs are installed).
      Proof: `bats tests/module.bats` green; with the `withConfig` wrap removed from `dispatch` in flake.nix the new test fails (observe, restore).

- [ ] **Step 13: README**
      File: `README.md`. After the env-var paragraph (~:256) add a short "Settings" subsection: the four layers and their paths, `dispatch-config` / `--show-origin`, that `grantRoots` and `openrouter.keyFile` are honoured only from the locked layer or env, and that the model map lives in `adapters/core/defaults.json` (edit it, then run `scripts/gen-adapters.sh`).
      Proof: `grep -n 'dispatch-config' README.md` shows the section; `nix flake check` green (prettier on README.md, treefmt on the new script).

- [ ] **Step 14: full gate**
      `shellcheck adapters/core/*.sh adapters/core/reviewers/*.sh scripts/*.sh`;
      `./scripts/gen-adapters.sh && git diff --exit-code`;
      `bats tests/module.bats`;
      `bats --jobs 16 --filter-tags '!timing' $(find tests -maxdepth 1 -name '*.bats' ! -name module.bats | sort)`;
      `bats --filter-tags timing tests/secret-read-guard.bats`;
      `nix flake check`. All green.

## Acceptance

- [ ] **AC1** every existing test passes unchanged (except "tier map conformance", oracle retargeted — `waived(dispatcher)`) and `nix flake check` green → step 14 commands; `git diff main -- tests/*.bats` shows only appended tests plus that one-line retarget.
- [ ] **AC2** table-driven fixture test from `main` gives identical accept/reject and escalation results → `bats tests/model-map.bats` (tests 1–4 over fixtures from steps 1–2).
- [ ] **AC3** resolver tests (absent files, user>base, locked>user, env>all, user grantRoots ignored + warning, `--show-origin`) → `bats tests/dispatch-config.bats`; user grantRoots cannot widen `--add-dir` → step 10 tests.
- [ ] **AC4** byte-identical refusal messages with no settings files + today's env → `bats -f 'e2e refusal' tests/model-map.bats`.
- [ ] (built path) the baked resolver reaches every consumer → step 12b.
- [ ] **AC5** shellcheck clean on new/edited scripts → `shellcheck adapters/core/dispatch-config.sh adapters/core/dispatch.sh adapters/core/dispatch-resume.sh adapters/core/dispatcher.sh adapters/core/refresh-budget.sh scripts/gen-model-map-doc.sh scripts/gen-adapters.sh`.
