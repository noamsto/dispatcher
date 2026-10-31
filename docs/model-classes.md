# Model rungs by class

Design and phased plan for issue #1272: resolve dispatch model rungs by class
against a discovered model catalog. This is a proposal for owner approval; no
code changes ship with it.

## 1. Summary

- Each engine gets classes `cheap|standard|premium`, each an ordered list of family patterns the owner writes.
- A class resolves deterministically against a cached per-engine catalog. No clock, no scores, no network at dispatch time.
- The incumbent stays the class default. A newer match is admitted as a trial and promoted only on an owner-confirmed decision.
- No catalog cache means today's behaviour: the pinned rows are the seed.
- Ladders (`escalation`, `paceDowngrades`) and burn weights stay explicit owner data. Seven phases, each a separate PR.

## 2. Owner decisions

Recommended answers are pre-filled. Tick or amend each before P1 starts.

- [ ] **D1 class vocabulary.** Reuse `cheap|standard|premium` from `burnClasses`. `free` and `fable` stay burn-only classes, never selection classes.
- [ ] **D2 `DISPATCH_IGNORE_RUNG`.** Stays exact-match: the typed alias/ID or the resolved ID. Never a class name or `@class` sigil (Q1).
- [ ] **D3 unpriced models.** A pattern match admits them. Burn weight comes from `burnClasses`, else the class floor (cheap 1, standard 2, premium 4). Never free (Q2).
- [ ] **D4 ordered pattern lists.** One ordered list per class; a one-element list is the common case (Q3).
- [ ] **D5 promotion.** The dispatcher proposes after at least 5 rated runs; the owner or dispatcher confirms with one command. No silent auto-promotion.
- [ ] **D6 class as positional `<model>`.** Accept a sigil form such as `@premium`, phase-gated after the gate work lands. Until then the dispatcher passes resolved IDs.
- [ ] **D7 refresh host.** A home-manager systemd/launchd timer. `crew stream` runs only while a dispatcher runs, which is the staleness the issue reports.
- [ ] **D8 claude catalog source.** Aliases plus full-ID admission by pattern. No Anthropic Models API, no key.
- [ ] **D9 class membership source.** Owner-authored patterns. The AA/arena prior and price only order trials and feed the promotion report. This departs from #1272's sketch ("any catalogued model is dispatchable, cost class from pricing"). Rejected: price or score membership. A fetched number would then decide what the gates admit, and subscription engines publish no price.

## 3. Today's mechanism

Every claim below cites the base commit.

### Where model IDs are pinned

- `adapters/core/defaults.json` holds `modelMap` (`adapters/core/defaults.json:2`; rows claude `adapters/core/defaults.json:3`, codex `adapters/core/defaults.json:41`, cursor `adapters/core/defaults.json:75`, pi `adapters/core/defaults.json:126`), `escalation` (`adapters/core/defaults.json:157`), `paceDowngrades` (`adapters/core/defaults.json:313`), `burnClasses` (`adapters/core/defaults.json:350`) and `orchestratorDefaults` (`adapters/core/defaults.json:377`).
- Settings merge base, user, locked, then env. Objects merge recursively; arrays are replaced whole (`adapters/core/dispatch-config.sh:3-12`).
- `adapters/core/protocols/dispatch-orchestration.md` repeats the IDs: the hand-written Model map (`adapters/core/protocols/dispatch-orchestration.md:38`, table `adapters/core/protocols/dispatch-orchestration.md:73-78`) and two generated regions, `generated:tier-rows` (`adapters/core/protocols/dispatch-orchestration.md:454-471`) and `generated:pace-downgrades` (`adapters/core/protocols/dispatch-orchestration.md:524-534`). `scripts/gen-model-map-doc.sh:39` and `scripts/gen-model-map-doc.sh:58` render them; `scripts/gen-model-map-doc.sh:124` and `scripts/gen-model-map-doc.sh:204` check the hand-written table against `default`. The flake check is `flake.nix:388`.
- `scripts/gen-adapters.sh` hard-codes two IDs: the claude critic agent `model: "opus"` (`scripts/gen-adapters.sh:216`) and pi's deep default plus effort suffix (`scripts/gen-adapters.sh:239`).

### Where they are enforced

- **Model gate.** `model_gate` (`adapters/core/dispatch.sh:4763`, called `adapters/core/dispatch.sh:4865`) checks shape per engine. `DISPATCH_SKIP_MODEL_CHECK=<id>` skips it (`adapters/core/dispatch.sh:4765`). Codex also checks `~/.codex/models_cache.json` `.models[].slug` (`adapters/core/dispatch.sh:4797-4806`); cursor checks `cursor-models-cache.json` within 24h (`adapters/core/dispatch.sh:4840-4851`, bound at `adapters/core/dispatch.sh:4846`). Claude and pi are shape-only (`adapters/core/dispatch.sh:4770-4777`, `adapters/core/dispatch.sh:4854-4859`).
- **Tier gate and `--ignore-map`.** Fail-closed membership of (engine, tier, model) in the `modelMap` row (`adapters/core/dispatch.sh:4947-4983`, row test `adapters/core/dispatch.sh:4963-4965`, refusal `adapters/core/dispatch.sh:4979-4982`). `--ignore-map` is parsed at `adapters/core/dispatch.sh:3576`. A configured `localModels` ID is admitted by its own entry instead (`adapters/core/dispatch.sh:4732-4735`). `refuse_claude_sonnet_cap` refuses sonnet at xhigh/max (`adapters/core/dispatch.sh:139-163`).
- **One-rung escalation.** `_escalation_hop` (`adapters/core/dispatch.sh:1689-1707`, duplicate `adapters/core/dispatch-resume.sh:1006`) picks the first rule whose `failed` glob matches the failed model. `_prior_failed_model` (`adapters/core/dispatch.sh:4878`) and `_prior_failed_escalation_available` (`adapters/core/dispatch.sh:4905`) read `events.jsonl` (duplicates `adapters/core/dispatch-resume.sh:1070`, `adapters/core/dispatch-resume.sh:1097`). A hop to a model already in the row is stamped `"<from> (record only)"` (`adapters/core/dispatch.sh:4986-4997`); a genuine hop sets `escalated_from` on the event (`adapters/core/dispatch.sh:6111-6115`).
- **Pace gate.** `pace_rule_target` (`adapters/core/dispatch.sh:172-250`) refuses a premium launch target when its 7d (or month) window is at least 70% used and more than 15 points ahead of pace. It tries the model rung, then the effort rung. `_pace_downgrade` (`adapters/core/dispatch.sh:1709-1722`) matches a `paceDowngrades` glob; the downgrade applies only if `_pace_burn_weight` (`adapters/core/dispatch.sh:1724-1750`) says it burns less. `DISPATCH_IGNORE_RUNG` is compared exactly against the model (`adapters/core/dispatch.sh:218`) and the effort (`adapters/core/dispatch.sh:235`). `--host` omits the escape (`adapters/core/dispatch.sh:167`).
- **At least 95% stop.** `budget_stop` (`adapters/core/dispatch.sh:252`) uses `_budget_windows` (`adapters/core/budget-gate.sh:38`) and `_budget_limit` for `limit_reached` (`adapters/core/budget-gate.sh:57`); `absolute_limit_stop` is `adapters/core/dispatch.sh:272`. These are engine-level and read no model ID.

### Refresh, records and local models

- **`refresh-models`** probes cursor only (`adapters/core/refresh-models.sh:30`) and writes `cursor-models-cache.json` (`adapters/core/refresh-models.sh:19`). No claude, codex or pi catalog is written.
- **`refresh-scores`** reads LMArena (`adapters/core/refresh-scores.sh:15`) and OpenRouter (`adapters/core/refresh-scores.sh:59`) into `model-scores.json` (`adapters/core/refresh-scores.sh:22`). Its 14-day trigger is protocol text only (`adapters/core/protocols/DISPATCHER_PROTOCOL.md:63`); no timer runs it.
- **`crew rate`** prices each model and effort with a first-match glob table (`crew/internal/rate/burn.go:33`, `crew/internal/rate/burn.go:77`), then groups ratings by engine, model and tier (`crew/internal/rate/report.jq:113`).
- **`crew hold`** stores the exact model: `--model` is required (`crew/internal/hold/hold.go:290`) and persisted as `holdTask.Model` (`crew/internal/hold/hold.go:411`).
- **`localModels`** IDs bypass `modelMap` and are budgeted by endpoint slots, not quota (`adapters/core/local-models.sh:14`, `adapters/core/dispatch.sh:4732-4735`).

## 4. Consumer map

One row per consumer of a model ID. "Change under classes" follows this design: ladders and burn weights stay explicit owner data.

| consumer                        | path:line                                                                                                                                                                                              | what it does with the ID                                                                                   | change under classes                                                |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| Pinned: `modelMap`              | `adapters/core/defaults.json:2`                                                                                                                                                                        | per engine and tier: `default`, `models` globs, `regex`, `expected`                                        | rows become the `seed`, kept verbatim; patterns added beside them   |
| Pinned: `escalation`            | `adapters/core/defaults.json:157`                                                                                                                                                                      | `failed` globs, `baseline`, `inRow`/`outOfRow` models                                                      | unchanged; entries may name a class                                 |
| Pinned: `paceDowngrades`        | `adapters/core/defaults.json:313`                                                                                                                                                                      | premium globs mapped to a `to` model                                                                       | unchanged; `to`/`inRow` may name a class; no pi entry added         |
| Pinned: `burnClasses`           | `adapters/core/defaults.json:350`                                                                                                                                                                      | first-match glob to class and weight, `byEffort` for opus                                                  | unchanged; still the weight source                                  |
| Pinned: `orchestratorDefaults`  | `adapters/core/defaults.json:377`, `adapters/core/dispatcher.sh:410`                                                                                                                                   | dispatcher-session model and effort                                                                        | may name a class, resolved at launch                                |
| Config validators               | `adapters/core/dispatch-config.sh:116`, `adapters/core/dispatch-config.sh:129`, `adapters/core/dispatch-config.sh:148`, `adapters/core/dispatch-config.sh:161`, `adapters/core/dispatch-config.sh:189` | reject wrong-shaped leaves per layer                                                                       | add the `classes` schema and the overlap warning                    |
| Reserved providers              | `adapters/core/dispatch-config.sh:306`                                                                                                                                                                 | takes pi provider prefixes from `modelMap.pi[].models[]`                                                   | keep reading the seed rows                                          |
| Generated agent pins            | `scripts/gen-adapters.sh:216`, `scripts/gen-adapters.sh:239`                                                                                                                                           | static `opus` and pi deep default in agent frontmatter                                                     | stay pinned; frontmatter cannot hold a class                        |
| Gate: `valid_role_model`        | `adapters/core/dispatch.sh:125-134`                                                                                                                                                                    | shape check for `--roles` and `--spawn-role` models                                                        | shape applies to the resolved ID                                    |
| Gate: sonnet cap                | `adapters/core/dispatch.sh:139-163`                                                                                                                                                                    | refuses `sonnet`/`claude-sonnet-*` at xhigh/max                                                            | stays an ID rule                                                    |
| Gate: `model_gate`              | `adapters/core/dispatch.sh:4763`                                                                                                                                                                       | shape and cache membership per engine                                                                      | resolved IDs re-pass it as if typed; catalog adds membership        |
| Gate: tier gate                 | `adapters/core/dispatch.sh:4947-4983`                                                                                                                                                                  | fail-closed row membership                                                                                 | admit by seed or class pattern; refusal names classes (P3)          |
| Gate: `_model_in_row`           | `adapters/core/dispatch.sh:1667-1683`                                                                                                                                                                  | glob and ERE test of the row; `expected` text                                                              | the resolver supplies the admitted set                              |
| Gate: escalation                | `adapters/core/dispatch.sh:1689-1707`, `adapters/core/dispatch-resume.sh:1006`                                                                                                                         | first matching rule gives the baseline                                                                     | rules may name a class; edit both copies                            |
| Gate: prior-failure readers     | `adapters/core/dispatch.sh:4878`, `adapters/core/dispatch.sh:4905`; `adapters/core/dispatch-resume.sh:1070`, `adapters/core/dispatch-resume.sh:1097`                                                   | read event `.model` and `escalated_from`                                                                   | classify an old ID through the seed                                 |
| Gate: pace                      | `adapters/core/dispatch.sh:172-250`, `adapters/core/dispatch.sh:1709-1750`                                                                                                                             | model rung then effort rung, exact `DISPATCH_IGNORE_RUNG`                                                  | `to` may name a class; text names class and resolved ID (P4)        |
| Gate: stops                     | `adapters/core/dispatch.sh:252`, `adapters/core/dispatch.sh:272`                                                                                                                                       | engine-level quota stops                                                                                   | none                                                                |
| Gate: call sites                | `adapters/core/dispatch.sh:5176-5181`, `adapters/core/dispatch.sh:3716-3718`                                                                                                                           | per lead, eager role and spawned role                                                                      | receive the resolved ID                                             |
| Gate: `dispatch resume`         | `adapters/core/dispatch-resume.sh:1131-1146`                                                                                                                                                           | re-gates only an explicit `--model`; escalation via `outOfRow`                                             | the stamped concrete ID stays authoritative                         |
| Gate: local lead roles          | `adapters/core/dispatch.sh:5128-5131`                                                                                                                                                                  | hosted pi default for a local lead's bare roles                                                            | class-resolved default                                              |
| Gate: `dispatch --models`       | `adapters/core/dispatch.sh:3862-3866`                                                                                                                                                                  | publishes default, models, regex per engine and tier                                                       | add classes, resolved IDs and trial marks (P3)                      |
| Roster: `--roles` strings       | `adapters/core/dispatch.sh:42`, `adapters/core/dispatch.sh:5133-5146`                                                                                                                                  | `name=agent:model[@effort]`, shape-checked                                                                 | accept resolved IDs; a class token only under D6                    |
| Roster: `roles.json`            | `adapters/core/dispatch.sh:6412-6414`, `adapters/core/dispatch.sh:3659`, `adapters/core/dispatch.sh:3740`                                                                                              | persists the launched role model for respawn                                                               | store the resolved ID                                               |
| Roster: default roles           | `adapters/core/protocols/dispatch-orchestration.md:177-181`                                                                                                                                            | `plan-critic,reviewer` by tier                                                                             | none; roles name engines and IDs                                    |
| Refresh: `refresh-models`       | `adapters/core/refresh-models.sh:30`                                                                                                                                                                   | cursor slugs only                                                                                          | extend to all engines, write `model-catalog.json` (P1)              |
| Refresh: codex and cursor reads | `adapters/core/dispatch.sh:4797-4806`, `adapters/core/dispatch.sh:4840-4851`                                                                                                                           | best-effort membership checks                                                                              | same data via the catalog                                           |
| Refresh: `refresh-scores`       | `adapters/core/refresh-scores.sh:22`                                                                                                                                                                   | writes `model-scores.json` for the dispatcher LLM                                                          | feeds trial ordering hints only; never a gate                       |
| Budget: `refresh-budget`        | `adapters/core/refresh-budget.sh:61`                                                                                                                                                                   | writes `engine-budget.json`; claude windows `7d_opus`, `7d_sonnet` (`adapters/core/refresh-budget.sh:109`) | none; catalog sits beside it                                        |
| Budget: `engine-budget.json`    | `adapters/core/dispatch.sh:1519`, `adapters/core/dispatch.sh:5033`                                                                                                                                     | cache path read by the gates                                                                               | none                                                                |
| Budget: `budget-gate.sh`        | `adapters/core/budget-gate.sh:19`, `adapters/core/budget-gate.sh:38`, `adapters/core/budget-gate.sh:57`                                                                                                | engine-level queries; no model ID                                                                          | none                                                                |
| Records: dispatch event         | `adapters/core/dispatch.sh:6145-6146`                                                                                                                                                                  | writes `model` and `escalated_from`                                                                        | `model` stays concrete; add `class` later                           |
| Records: `WORKER_TASK.md`       | `adapters/core/dispatch.sh:6319-6321`; `adapters/core/dispatch-resume.sh:1136`                                                                                                                         | header `model:` and `escalated_from:`, read on resume                                                      | optional `model_class:`                                             |
| Records: `crew hold`            | `crew/internal/hold/hold.go:290`, `crew/internal/hold/hold.go:411`                                                                                                                                     | stores the exact model                                                                                     | concrete ID; optional `task.class`                                  |
| Records: `crew rate`            | `crew/internal/rate/burn.go:77`, `crew/internal/rate/records.jq:47`, `crew/internal/rate/records.jq:83`, `crew/internal/rate/report.jq:113`                                                            | prices by glob, groups by engine, model, tier                                                              | add class to the grouping and the promotion report (P5)             |
| Records: roster and retro       | `crew/internal/roster/roster_base.jq:8`, `crew/internal/retro/retro.jq:118`                                                                                                                            | display the concrete model                                                                                 | optional class tag                                                  |
| Records: dash settings          | `dash/internal/data/settings.go:94`                                                                                                                                                                    | generic view of resolved settings                                                                          | new keys show; goldens change                                       |
| Protocol: Model map, Tier map   | `adapters/core/protocols/dispatch-orchestration.md:38`, `adapters/core/protocols/dispatch-orchestration.md:431`                                                                                        | concrete versions, generated rows                                                                          | shrink to class names (P7)                                          |
| Protocol: Model gate            | `adapters/core/protocols/dispatch-orchestration.md:212-229`, `adapters/core/protocols/dispatch-orchestration.md:278`                                                                                   | shape rules, caches, skip override                                                                         | rewrite around the catalog (P3)                                     |
| Protocol: pace refusal          | `adapters/core/protocols/dispatch-orchestration.md:483-506`                                                                                                                                            | premium refusal, `DISPATCH_IGNORE_RUNG`                                                                    | refusal text and example (P4)                                       |
| Protocol: dispatcher text       | `adapters/core/protocols/DISPATCHER_PROTOCOL.md:25-37`, `adapters/core/protocols/DISPATCHER_PROTOCOL.md:63`, `adapters/core/protocols/DISPATCHER_PROTOCOL.md:77`                                       | tier and model choice, escalation, scores rule, overrides                                                  | class wording (P7)                                                  |
| Protocol: worker text           | `adapters/core/protocols/WORKER_PLAN.md:25-28`, `adapters/core/protocols/WORKER_PROTOCOL.md:285-287`                                                                                                   | literal consult and escalate IDs                                                                           | "premium for the engine" (P7)                                       |
| Protocol: generated copies      | `scripts/gen-adapters.sh:38`                                                                                                                                                                           | regenerates plugin protocol trees; CI diffs them                                                           | regenerate in P3, P4, P7                                            |
| Nix: locked layer               | `nix/hm-module.nix:30-31`                                                                                                                                                                              | `localModels`, `laneProfiles`; no map option                                                               | none; overrides go through `userSettings` (`nix/hm-module.nix:594`) |
| Nix: packages and timers        | `nix/hm-module.nix:641`, `flake.nix:284`                                                                                                                                                               | puts `refresh-models` on PATH; no timer anywhere                                                           | add timers (P6)                                                     |
| Nix: doc drift check            | `flake.nix:388`                                                                                                                                                                                        | `model-map-doc` check                                                                                      | extended in P7                                                      |
| Tests: gate and pace            | `tests/dispatch.bats:4746`, `tests/dispatch.bats:5001`, `tests/dispatch.bats:5855`                                                                                                                     | pin IDs per cell; pace across models; rung override                                                        | behaviour tests unchanged; fixtures move                            |
| Tests: tripwires                | `tests/dispatch.bats:5149`, `tests/dispatch.bats:5168`                                                                                                                                                 | grep doc and code for literal IDs                                                                          | rewritten in P7                                                     |
| Tests: model map                | `tests/model-map.bats:135`, `tests/model-map-doc.bats:17`                                                                                                                                              | helpers byte-identical across both scripts; generator contract                                             | keep the byte-identity pin; adapt generator test                    |
| Tests: config and resume        | `tests/dispatch-config.bats:425`, `tests/dispatch-resume.bats:2603`                                                                                                                                    | schema refusals; resume escalation                                                                         | add class cases; old cases unchanged                                |
| Tests: refresh and launch       | `tests/refresh-models.bats:44`, `tests/dispatcher.bats:275`, `tests/module.bats:925`                                                                                                                   | cursor cache shape, orchestrator `--model`, locked layer                                                   | extend                                                              |
| Tests: Go                       | `crew/internal/rate/burn_test.go:38`                                                                                                                                                                   | opus `byEffort` weights                                                                                    | unchanged                                                           |

## 5. Design

All schemas below are proposed.

### Two axes

- **Selection class** (`cheap|standard|premium`): the rung the map picks a model from.
- **Burn class and weight**: unchanged. `burnClasses` matches the resolved ID by first-match glob, with `byEffort` (opus low is standard w2; medium and up is premium w4 to w8; `adapters/core/defaults.json:351-360`).
- **Effort** is its own rung. `xhigh` and `max` are premium effort and downgrade to `high` (`adapters/core/dispatch.sh:190-192`). Classes do not touch it.

### Ladders stay explicit

`escalation` and `paceDowngrades` stay in the seed as owner data. Their entries may also name a class: `"to": "@standard"`, `"inRow": ["@premium"]`, resolved through the same resolver. Nothing is derived as "next class down". Derived steps would not reproduce today's ladders: fable placement, cursor's family-preserving `grok-4.7-high` to `grok-4.7-medium` against `cursor-grok-4.6-*` (`adapters/core/defaults.json:333-347`), or codex's `outOfRow` sol.

An ID matching patterns in more than one class takes the most expensive matching class, which is fail-closed for the pace gate. Schema validation warns on the overlap.

### `classes` schema

```json
{
  "classes": {
    "claude": {
      "cheap": ["haiku", "claude-haiku-*"],
      "standard": ["sonnet", "claude-sonnet-*"],
      "premium": ["opus", "claude-opus-*"],
      "seed": "<today's modelMap.claude rows, default and models, verbatim>"
    },
    "pi": {
      "premium": [
        "anthropic/claude-opus-*",
        "openrouter/anthropic/claude-opus-*"
      ],
      "standard": [
        "openrouter/deepseek/deepseek-v*-flash",
        "openrouter/z-ai/glm-*-flash",
        "openrouter/qwen/qwen*-flash"
      ],
      "seed": "<today's modelMap.pi rows, verbatim>"
    }
  }
}
```

Patterns are globs. A `regex` entry exists only where today's row has one (cursor, `adapters/core/defaults.json:91-92`).

Each `modelMap` row names its classes beside the seed: `class`, the selection class its default comes from, and `admits`, the classes whose candidates the row accepts. For example claude `standard` is `{"class": "standard", "admits": ["cheap", "standard", "premium"]}` and claude `deep` is `{"class": "premium", "admits": ["standard", "premium"]}`. Fable stays admitted on claude `deep` through the seed's `claude-fable-*` glob, not through a class (D1).

### Catalog cache

`model-catalog.json` sits beside `engine-budget.json` (`adapters/core/dispatch.sh:1519`):

```json
{
  "pi": {
    "fetched_epoch": 1790000000,
    "source": "pi --list-models; openrouter /api/v1/models",
    "models": [
      {
        "id": "openrouter/deepseek/deepseek-v4.1-flash",
        "created": 1789000000,
        "price": { "in": 0.1, "out": 0.4 }
      }
    ]
  }
}
```

`created` and `price` are optional. Absent `price` means unknown, never free. A class-state file (`model-class-state.json`, same directory) records each class's incumbent default, seeded from today's `modelMap.*.default`.

### Resolution

A pure function of the `classes` map, one catalog snapshot and the class-state file.

1. No parseable snapshot for the engine: return the seed. This is today's behaviour.
2. Candidates: catalog IDs that pass `model_gate`'s shape regex and match a pattern of a class in the row's `admits`. A candidate's rank is the index of the first pattern it matches.
3. Admitted set: candidates plus the row's seed `models`. Admission never depends on price.
4. Default: the incumbent from the class-state file (see Determinism).
5. Trial head: the best candidate by the first pattern with any match, newest within it. It is admitted and offered as a trial, never made the default.
6. Class of an ID: the most expensive class whose patterns match it.

Admission and default selection are separate outputs. A model can be admitted and dispatchable without ever being a default.

### Worked example: pi `anthropic/claude-opus-5-5`

- Today pi's rows hold OpenRouter models only (`adapters/core/defaults.json:126-155`). The pi shape check accepts a provider-qualified ID (`adapters/core/dispatch.sh:4856`), and the tier gate refuses it (`adapters/core/dispatch.sh:4979-4982`). The issue reports `--ignore-map` plus `DISPATCH_SKIP_MODEL_CHECK` were both needed.
- With the pi `premium` patterns above and a pi catalog that lists the model, it is admitted at `deep` with no flag.
- Burn weight comes from `burnClasses` `*opus*` by effort (`adapters/core/defaults.json:351-360`), not from price.
- **Residual.** Direct Anthropic spend is invisible to `engines.pi`, which tracks the OpenRouter month. P4 therefore adds no pi `paceDowngrades` entry. That rung is left to the at-least-95% stop and the owner (D9), because gating it on a window that cannot see its spend would be false assurance.

## 6. Open questions

### Q1. How do the pace gate and `DISPATCH_IGNORE_RUNG` name a refused rung?

**Recommendation.**

- The model-rung trigger is unchanged. A `paceDowngrades` entry matches the resolved ID, by glob or by class, and the downgrade burns less (today's weight compare).
- Cursor `-fast` variants with no entry, such as `grok-4.7-high-fast` (burn w8, `adapters/core/defaults.json:373`), stay unrefused as today. Refusing them is a separate owner call, kept out of P4.
- Refusal text names engine, class, the typed alias or ID, and the resolved ID: "premium rung (claude premium: opus → claude-opus-5-5)". The downgrade is named as class plus resolved ID.
- `DISPATCH_IGNORE_RUNG` stays exact-match. It matches the typed alias/ID or the resolved ID, never a class name or `@class` sigil. Under D6 a sigil resolves before the gate, so the escape names the resolved ID.
- The effort refusal and `DISPATCH_IGNORE_RUNG=xhigh|max` are unchanged, still checked after the model refusal (`adapters/core/dispatch.sh:235-241`).

**Rejected.**

- A class-name value (`DISPATCH_IGNORE_RUNG=premium`). One launch has several targets: the lead plus eager roles, possibly on different engines, each run through `pace_rule_target` (`adapters/core/dispatch.sh:5176-5181`). A class value would lift every premium target without naming which model was authorized.
- Resolved-ID-only. The human types the alias.

**Why.** Exact-match keeps "name what you lift". Accepting the typed form keeps a retry stable across a catalog refresh. An ID that drifts only re-refuses, which fails closed.

### Q2. A model that is catalogued but unpriced

**Recommendation.**

- Class membership comes from the map's patterns, never from price (D9). Price only orders trial candidates and can move a model up a burn class, never down.
- Unpriced and pattern-matched: admitted. Burn class and weight come from `burnClasses` (resolved ID and effort) when a glob names it, else the selection class's floor weight (cheap 1, standard 2, premium 4). Never 0 or free unless `burnClasses` says `free` (`adapters/core/defaults.json:361`).
- Unpriced and no pattern match: refused at the tier gate as today; `--ignore-map` is the escape.
- Local models stay on the `localModels` path and its slot budget (`adapters/core/dispatch.sh:4732-4735`).
- Subscription engines are unpriced by nature. That is the normal case. Null means unknown, never free.

**Rejected.**

- Treat unpriced as cheap or free. An unknown expensive model would escape the pace gate, a fail-open spend.
- Refuse every unpriced model. That refuses all of claude, codex and cursor, which publish no per-token price.

**Why.** The gates must stay fail-closed on spend. Price is advisory input; the owner's patterns are the trust boundary.

### Q3. One pattern per class, or an ordered list?

**Recommendation.** An ordered list per class. The first pattern with any catalog match wins, newest within it. A one-element list is the common case. Lists are needed for cursor (grok-4.7, then cursor-grok-4.6; composer is free), for the pi standard rotation (deepseek, glm, qwen), and to pin an incumbent by putting an exact ID first.

**Rejected.**

- A single pattern. It cannot express a cross-family preference or a pin.
- Free-form regex per class. Cursor already needs an ERE row (`adapters/core/defaults.json:91-92`), but regex ordering is opaque and harder to audit against an untrusted catalog. Keep globs; allow `regex` only where today's row has one.

**Why.** Ordered globs stay deterministic and auditable, and cover every row in the current map.

## 7. Determinism and trust

Resolution is a function of the `classes` map, the catalog snapshot and the class-state file. It reads no clock, no scores and no network.

### Total order

Candidates sort by version tuple parsed per engine (descending), then catalog `created` (descending) where present, then ID byte order (ascending). Ties cannot survive the last key.

### "Newest" per engine

| engine | "newest" means                                                                                                                        |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| claude | The alias is the class default; Claude Code resolves it. Full `claude-<family>-<maj>-<min>` IDs order by numeric version.             |
| codex  | `gpt-<maj>.<min>-<variant>` by generation. The variant is the class.                                                                  |
| cursor | No dates. Numeric version tokens in the ID. `-fast` and effort suffixes are separate rungs, not newer models.                         |
| pi     | OpenRouter `created` when priced, else numeric version tokens.                                                                        |
| all    | Dated snapshot suffixes (`-1017`, `-20261017`) and `preview`, `exp`, `beta` tags never win default selection. They are admitted only. |

### Trial and incumbent

- The incumbent is the class default recorded in the class-state file, seeded from today's `modelMap.*.default`.
- A newer catalogued match is admitted as a trial: dispatchable when named, and the dispatcher may A/B it. It never becomes the default until promotion (D5): at least 5 rated runs per engine, model and tier in `crew rate`, then an owner decision.
- **Incumbent absent from the catalog.** Keep the incumbent if it is in the seed (the engine CLI is the final judge). Otherwise use the seed default. Warn either way. A trial is never promoted implicitly.

### Clock-free

Resolution ignores cache age. A stale but parseable cache still resolves; the worst case admits a retired ID the engine CLI rejects. Staleness is surfaced by `refresh-models` and `dispatch --models` warnings, not by a resolution fallback. Cursor's 24h membership refusal (`adapters/core/dispatch.sh:4846`) stays as a refusal, not a resolution input.

### Untrusted catalog and scores

| attack                                 | bound                                                                                                                                                                                                                              |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Crafted ID in the catalog              | Every resolved ID re-passes `model_gate`'s shape checks as if typed (`adapters/core/dispatch.sh:4763`); catalog text echoed in messages is control-stripped, as the codex list already is (`adapters/core/dispatch.sh:4800-4804`). |
| Widen spend with a cheap-looking price | Only IDs matching an owner-authored pattern are admitted. Price can only tighten a burn class.                                                                                                                                     |
| Hijack "newest" with a fake version    | Lands as a trial only, never the default. Omitting the incumbent cannot promote it either; the incumbent and seed fallback applies.                                                                                                |
| Garbled or oversized file              | Ignored with a warning; fall back to the seed. A failed probe keeps that engine's previous snapshot and never writes an empty list.                                                                                                |
| Poisoned scores cache                  | The scores cache never feeds a gate. It only orders trial hints and the promotion report.                                                                                                                                          |

**Residual.** A malicious catalog can add a trial ID matching a pattern, which a dispatcher could pick explicitly. The engine CLI still rejects IDs it cannot run.

## 8. Backward compatibility

- **No cache.** Resolution returns the seed, so every gate behaves as today. Proof: the generated tier table regenerates byte-identical (`scripts/gen-model-map-doc.sh:124`) and existing bats pass unchanged. Every current `paceDowngrades` and `escalation` entry resolves to the same target, and the pace and escalation bats (`tests/dispatch.bats:5001`, `tests/dispatch-resume.bats:2603`) pass unchanged. The helper byte-identity pin (`tests/model-map.bats:135`) also holds.
- **Pinned IDs on the command line.** Still admitted when they match a class pattern or the seed list.
- **`crew hold`.** `task.model` is re-passed on release (`crew/internal/hold/hold.go:411`, `adapters/core/protocols/DISPATCHER_PROTOCOL.md:88`). The seed keeps old IDs admitted. An optional `task.class` can be added later so release re-resolves a retired ID.
- **`escalated_from`.** It stores the escalation rule's `baseline` label (`sonnet`, `terra`, `medium`), an opaque string. Consumers only test it for non-empty (`adapters/core/dispatch.sh:4927`, `adapters/core/dispatch-resume.sh:1119`), so old rows stay valid. New rows also record the class.
- **`"<from> (record only)"` suffix.** `adapters/core/dispatch.sh` string-matches it to keep record-only hops off the event (`adapters/core/dispatch.sh:6113`, set at `adapters/core/dispatch.sh:4993`). That suffix contract is kept. Resume never writes the suffix: it stamps the bare baseline (`adapters/core/dispatch-resume.sh:1144`).
- **`_prior_failed_model`.** Classifies an old event's ID through the seed.

## 9. Phased plan

Each phase is a separate PR. Tiers are proposed. The CI column uses the labels below; every command is copied from `.github/workflows/ci.yml` (line cited) or is a flake check.

- **K1** `nix flake check` (`.github/workflows/ci.yml:171`; includes `model-map-doc`, `flake.nix:388`)
- **K2** `nix develop -c shellcheck adapters/core/*.sh adapters/core/reviewers/*.sh scripts/*.sh` (`.github/workflows/ci.yml:22`)
- **K3** `nix develop -c sh -c 'cd crew && go vet ./... && golangci-lint run ./...'` (`.github/workflows/ci.yml:24`)
- **K4** `nix develop -c sh -c 'cd crew && CREW_DRIFT_REQUIRED=1 go test ./... && CREW_DRIFT_REQUIRED=1 go test -count=1 -v -run "TestDriftAgainstBash|TestRowsMatchBash|TestGoldenOracle|TestPoolsAreGolden|TestEngineCommandsMatchLocalModels|TestBurnTableOracle|TestSweepCostIsWeightTimesWallClock|TestShReal|TestDashFixtureMatches" ./internal/frame ./internal/identity ./internal/roster ./internal/stall ./internal/stall/probe ./internal/rate ./internal/invisible'` (`.github/workflows/ci.yml:31`)
- **K5** `nix develop -c ./scripts/gen-adapters.sh`, then `git diff --exit-code` (`.github/workflows/ci.yml:33-35`)
- **K6** `nix develop -c bash scripts/bats-shard.sh --check` (`.github/workflows/ci.yml:40`)
- **K7** `nix develop -c bash scripts/harness-manifest.sh --check` and `nix develop -c bash scripts/harness-inventory.sh --check` (`.github/workflows/ci.yml:47-49`)
- **K8** `nix develop -c bats tests/module.bats` (`.github/workflows/ci.yml:66`)
- **L1** `bash scripts/bats-affected.sh --base origin/main`: local only, not a CI command (CI runs the full sharded suite)

K6 and K7 apply to any phase that adds or edits bats cases. Phases touching the dispatcher protocol (P3, P4, P7) route to a non-claude engine, because the claude permission classifier blocks protocol edits.

| phase                        | tier     | files                                                                                                                                                                                                                                                      | acceptance                                                                                                                                                                                                                                                                                                                                                                      | CI commands        | protocol?                                     |
| ---------------------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------ | --------------------------------------------- |
| P1 catalog discovery         | standard | `adapters/core/refresh-models.sh`, `flake.nix` (its `runtimeInputs` gain `curl` and `pi`), `tests/refresh-models.bats`                                                                                                                                     | `refresh-models` writes `model-catalog.json` for all engines, nothing reads it. Sources: claude has no list command, so aliases plus pattern (D8); codex `~/.codex/models_cache.json`; cursor `cursor-agent --list-models`; pi `pi --list-models` plus OpenRouter `/api/v1/models` pricing. A failed probe keeps that engine's previous snapshot and never writes an empty list | K1 K2 K6 K7, L1    | no                                            |
| P2 schema and resolver       | standard | `adapters/core/defaults.json`, `adapters/core/dispatch-config.sh`, a pure resolver script, `tests/dispatch-config.bats`, new resolver bats                                                                                                                 | `classes` schema validates, overlap warns; resolver passes golden cases (order, trial, incumbent-absent, garbled cache) with no gate wired. The helper stays single-copy or byte-identical across `dispatch.sh` and `dispatch-resume.sh` (`tests/model-map.bats:135`)                                                                                                           | K1 K2 K6 K7, L1    | no                                            |
| P3 model and tier gate       | deep     | `adapters/core/dispatch.sh`, `adapters/core/dispatch-resume.sh`, `adapters/core/protocols/dispatch-orchestration.md`, generated protocol copies, `tests/dispatch.bats`                                                                                     | a catalogued model matching a class dispatches without `--ignore-map` or `DISPATCH_SKIP_MODEL_CHECK`; with no cache, tier table regenerates byte-identical and gate bats pass unchanged; `dispatch --models` shows classes and trials                                                                                                                                           | K1 K2 K5 K6 K7, L1 | yes (Model gate, Tier map); non-claude engine |
| P4 pace gate and ladders     | deep     | `adapters/core/dispatch.sh`, `adapters/core/dispatch-resume.sh`, `adapters/core/defaults.json`, `adapters/core/protocols/dispatch-orchestration.md`, `adapters/core/protocols/DISPATCHER_PROTOCOL.md`, `tests/dispatch.bats`, `tests/dispatch-resume.bats` | `escalation` and `paceDowngrades` entries may name a class; refusals name class and resolved ID; `DISPATCH_IGNORE_RUNG` stays exact-match for the typed or resolved ID; no pi `paceDowngrades` entry; at-least-95% stop untouched; every current entry resolves to the same target with no cache                                                                                | K1 K2 K5 K6 K7, L1 | yes (Budget-aware refusal); non-claude engine |
| P5 trial and promotion       | standard | `crew/internal/rate/` and its `_test.go` fixtures, class-state file code                                                                                                                                                                                   | class-state seeded from `modelMap.*.default`; a trial never becomes default without the confirm command; `crew rate` reports at least 5-run candidates per engine, model and tier                                                                                                                                                                                               | K1 K3 K4, L1       | no                                            |
| P6 scheduled refresh         | standard | `nix/hm-module.nix`, `tests/module.bats`                                                                                                                                                                                                                   | a systemd user timer and a launchd agent run `refresh-models` and `refresh-scores` with no dispatcher session                                                                                                                                                                                                                                                                   | K1 K8, L1          | no                                            |
| P7 protocol shrink and sigil | standard | `scripts/gen-model-map-doc.sh`, `adapters/core/protocols/`, generated protocol copies, `tests/dispatch.bats:5149`, `tests/dispatch.bats:5168`, `tests/model-map-doc.bats`                                                                                  | model tables shrink to class names; the generator emits the class table; tripwires rewritten; `@class` sigil lands only if D6 is accepted                                                                                                                                                                                                                                       | K1 K2 K5 K6 K7, L1 | yes (all protocol text); non-claude engine    |

`docs/` has no index file, so this PR adds no index link and does not edit `README.md`.
