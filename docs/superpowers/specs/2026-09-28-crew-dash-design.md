# `crew dash` — read-only settings, budget and last-run dashboard (#584)

Part of #559. Makes the layered settings (#571/#583) visible, next to the
per-engine budget and the conclusions of recent runs, without reading several
JSON files by hand.

## Problem

Answering "what will dispatch actually use, and why" today takes four files and
five commands: `dispatch-config --show-origin`, `engine-budget.json` plus the
lever lines `refresh-budget` prints only right after a live probe, `crew retro
--report`, `crew rate --report`, `crew roster <crew>`. Nothing shows them side by
side, and the lever verdicts cannot be re-read at all without re-probing (a
network call against a rate-limited endpoint).

## Decision: toolkit — bash, no new toolchain

|                                  | Go + Bubble Tea/Lipgloss                                                                                                                                         | bash (chosen)                                                                                                                                                                                                                                              |
| -------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Packaging                        | new toolchain: `go.mod`/`go.sum`, `buildGoModule` + a `vendorHash` to refresh on every dep bump, Go in the devshell and CI                                       | one more `writeShellApplication`, identical to the 11 CLIs already in the flake                                                                                                                                                                            |
| Test story                       | golden `View()` frames in `go test`; but the data sources are bash CLIs, so "fixture files → `--once` output" needs a second harness (bats) or a two-hop fixture | bats end to end: fixture files → the real `dispatch-config`/`refresh-budget`/`crew` → `crew dash --once` golden; interactive frames captured from a private tmux server at 80×24 and after `resize-window` (the suite already drives private tmux servers) |
| Width correctness                | `lipgloss.Width` handles wide glyphs                                                                                                                             | autowrap disabled while the TUI owns the screen (`\e[?7l`) so an over-long line is clipped by the terminal, never wrapped; plus a width-aware truncate (see below). No vertical borders, so there is no border to bleed                                    |
| Next step: edit a user-layer key | `textinput` + a confirm view                                                                                                                                     | a cursor already exists on settings rows (below); edit = `e` on a row → a one-line prompt in the status row (`read -e -i`) → `jq setpath` into `settings.json`, validated by re-running `dispatch-config`                                                  |

The flake is described as a "shell-based agent-orchestration harness"; every
data source is a bash CLI emitting JSON, so the dashboard is a presentation
layer over JSON — jq does the shaping, bash only paints. Go would buy width math
and golden-frame ergonomics at the price of a second language and a hash to
maintain; the clip-don't-wrap terminal mode removes the failure the width math
exists to prevent. Revisit if the edit slice grows forms beyond one-line values.

## Shape

- New script `adapters/core/crew-dash.sh`, packaged as `crew-dash`
  (`writeShellApplication`, `withConfig` so it gets the baked `dispatch-config`).
- `crew dash [args]` in `crew.sh` execs `crew-dash`, passing its own path as
  `CREW_BIN` so crew-dash calls back into the same `crew` build. `crew`'s
  `runtimeInputs` gains `crew-dash`; `crew-dash` does **not** list `crew` (that
  would be an eval cycle — the `dispatch-resume` precedent) and resolves
  `${CREW_BIN:-crew}` instead. `crew-dash` lists `refresh-budget` and `jq`,
  `coreutils`, `ncurses` (for `tput`).
- Added to the `default` symlinkJoin, so a standalone `crew-dash` works too.

```
crew dash            # interactive when stdin and stdout are TTYs; otherwise as --once
crew dash --once     # all four panes as plain text, then exit
crew dash --json     # the collected model (the same document both renderers read)
```

`NO_COLOR` (any non-empty value) disables all SGR in both modes; `--once` to a
non-TTY emits no SGR either.

## Data sources (read-only; no gh, no network)

One collector builds a single JSON model; both renderers read only that model.
Each source failure degrades its pane to a one-line `unavailable: <stderr first
line>` — it never aborts the dashboard.

| Pane          | Source                                                                                         | New surface needed                         |
| ------------- | ---------------------------------------------------------------------------------------------- | ------------------------------------------ |
| Settings      | `dispatch-config --show-origin`, `dispatch-config --layers`                                    | **yes** — `--layers`                       |
| Budget        | `refresh-budget --report --json`                                                               | **yes** — see below                        |
| Runs: notes   | `crew retro --report --json`                                                                   | **yes** — additive `rows` field            |
| Runs: ratings | `crew rate --report --json` (repo-scoped, as today)                                            | **yes** — additive `burn_median` aggregate |
| Roster        | `crew crews` (live = `alive == true`), `crew roster <id>`, `crew hold list --crew <id> --json` | none                                       |

### `dispatch-config --layers` (new)

The base and locked paths are baked into the `dispatch-config` build
(`@defaultsJson@`, `@lockedSettings@`), so no other program can know them — on
the home-manager install the locked file is only reachable through
`dispatch-config`. Add `--layers`, printing `{base: <path>, user: {path,
present}, locked: <path>|null}` using the same resolution the merge uses
(including the baked-path-wins rule). It reads no layer contents and fails on
nothing but a bad argument. `--layers` and `--show-origin` are mutually
exclusive.

### Stderr of sources

Every source's stderr is captured by the collector and never reaches the
terminal (it would paint over the alternate screen). Non-empty stderr from a
source that _succeeded_ is carried into the model as that pane's `warnings`
(e.g. `dispatch-config: ignoring grantRoots from …`) and shown under the pane
header; stderr of a _failed_ source becomes its `unavailable:` line.

### `refresh-budget --report [--json]` (new, no probing)

Today the per-window lever verdicts exist only as stderr text printed right
after a live probe. Add a render-only mode that reads the cached
`engine-budget.json` and never probes, never calls `dispatch-config`:

- `--report`: prints exactly what the post-probe path prints today (summary
  lines on stdout, `budget lever:` lines on stderr) — minus the leading file path.
- `--report --json`: one object `{fetched_epoch, engines: {<e>: null | {source,
plan_type, credits_cover, spend_usd, target_usd, elapsed_pct,
projected_month_end_usd, windows: [{key, used_pct, resets_at, resets_in_s,
ahead_pts, verdict}], projection}}}` (the openrouter-only fields null
  elsewhere); `ahead_pts` is set for 7d-family and `month`
  windows with a future reset (null otherwise), `verdict` is the lever advice
  string for windows ≥85% and null below; `projection` is the pi month-end
  over-target line or null.
- The window→verdict and pace math moves into shared jq defs that both the
  text lever renderer and `--json` use, so they cannot disagree. The existing
  `refresh-budget.bats` lever assertions are the regression net: the text path's
  bytes do not change.
- Argument parsing happens before the top-level `dispatch-config` call, and
  the report path skips that call: it needs no settings, and a broken user
  settings file must not hide the budget.
- A missing cache file: `--report` exits 1 with `refresh-budget: no cached
budget at <path> — run refresh-budget`.

### `crew retro --report --json` — additive `rows`

Adds `rows: [{kind: "run"|"dispatcher", crew, branch, engine, model, tier,
outcome, t0, notes: [{seam, tag, detail}]}]` — the same rows the bare-mode TSV
already derives (`crew` from the dispatch event's `crew_id`, or the `retro:<crew>`
sink for dispatcher rows). `tags`/`unknown` are unchanged. With no
`events.jsonl` retro prints nothing at all (it exits before jq); the collector
maps empty stdout to the empty model `{tags: [], unknown: [], rows: []}`, so the
empty-store case reads `no retro notes yet`, not `unavailable`. Details stay raw JSON
strings; the dashboard applies the same control/bidi `clean` retro already uses
before painting.

### `crew rate --report --json` — additive `burn_median`

The task asks for median burn; `rate` reports the mean `cost_proxy` (`cost_hours`).
Add `burn_median: agg(k; n; median of cost_proxy / 3600000)` beside it, JSON
only — the table columns do not change.

## Panes

**1 Settings.** The `--show-origin` tree, one row per leaf (a node is a leaf
only when its keys are exactly `origin` and `value` and `.origin` is a string —
a user object that itself holds `value`/`origin` keys is tagged recursively by
`dispatch-config`, so its `.origin` is an object and it stays a branch), indented by depth,
`path-segment: value` with the value as compact JSON, and an origin badge:
`default` (origin `base`), `user`, `env`, `🔒 locked`. Locked rows are bold
(and the badge is the only emoji on the screen), so they are distinct even
under `NO_COLOR`. Header shows the layer files from `dispatch-config --layers`: default path,
user path (`present`/`absent`), locked path or `none`; then any warnings. Rows are a model array `{path, value,
origin, editable}` where `editable = origin ∈ {base, user}` and the key is not
locked-only (`grantRoots`, `openrouter.keyFile`) — unused in this slice, it is
the seam the edit slice binds to.

**2 Budget.** `fetched <age> ago` (stale >2h flagged). Per engine a heading; per
window a row: `window  used%  pace  resets-in  verdict`. `pace` = `+N` points
ahead / `-N` behind / `—`. `unknown` for a null engine. The pi projection line
under pi.

**3 Runs.** (a) Per crew with notes (newest first, cap 5): crew id, the latest
`session_summary` detail (wrapped in `--once`, clipped in the TUI), then one
line of tag counts over every note of that crew — its dispatcher row _and_ its
run rows — except `session_summary` itself (`misrouted, gate_thrash x2`), so
the dispatcher's own conclusions (`misrouted`, `fanout_binder`,
`spec_too_thin`) are counted; then the last 3 of those non-summary notes, ordered by row `t0` (dispatcher row: its first note ts) then note order, printed last-first as
`tag: detail` lines. An
empty retro store prints `no retro notes yet`. (b) Ratings table by
`{tier, engine, model}`: `n  pr%  merge%  burn(med)`, the `rate` markers (`—`,
`(k)`, `!`) reused. Empty store: `no runs swept for this repo yet`.

**4 Roster.** For each live crew: `crew <id>` then per worker `name  state
tier/engine/model  age  pr`, and any outstanding holds. No live crew: `no active
crew`.

## Rendering

- `--once`: sections `== Settings ==` etc., no truncation (piping), deterministic
  order. A test clock: every "age"/"in" is computed by the collector from the
  source's absolute timestamps against `date +%s` — including roster ages, which
  `crew roster` computes with jq's `now`; the collector ignores its `age_s` and
  recomputes from `ts` — so the existing bats `date` shim pins every figure.
- Locale: `crew-dash` exports `LC_ALL=C.UTF-8` (built into glibc ≥2.35 without a
  locale archive, and present on macOS), so `${#s}`, `wc -L` and the
  char-by-char trim are UTF-8 aware and never split a code point.
- Interactive: alternate screen, cursor hidden, autowrap off; restored on every
  exit path (`trap … EXIT INT TERM`). Row 1: tab bar (`1 Settings  2 Budget  3
Runs  4 Roster`, active tab reversed). Row `LINES`: status (`r refresh · tab/1-4
pane · j/k scroll · q quit · refreshed HH:MM:SS`). Rows 2..LINES-1: the active
  pane's lines from its scroll offset. Every painted line is truncated to
  `COLUMNS` display cells (ASCII fast path; a line holding non-ASCII is measured
  with `wc -L`, and trimmed char by char until it fits, `…` last), then padded
  with an erase-to-EOL. Minimum 40×10; below it, a single `terminal too small`
  line.
- Keys: `q`/Ctrl-C quit; `r` re-collect all sources; `1`–`4`, Tab, ←/→ switch
  pane; `j`/`k`/↓/↑ move (the cursor on Settings, scroll elsewhere); PgDn/PgUp,
  `g`/`G`. Resize: `trap WINCH` sets a flag; the key loop reads with a 1s
  timeout and repaints when the flag is set. No timer-driven re-collection.

## Acceptance mapping

- Golden `--once` over fixtures: base `defaults.json` fixture + user
  `settings.json` + a locked file (`DISPATCH_LOCKED_SETTINGS`), one key set in all
  three (locked wins, 🔒); a budget cache with claude 7d at 90% with ~50%
  elapsed (ahead of pace, verdict shown); an empty retro store; a ratings store
  with two groups. Plus: `NO_COLOR` absent/present, `--json` shape, a failing
  source degrades one pane.
- Interactive: tmux private server, `crew dash` in an 80×24 window,
  `capture-pane`. Because autowrap is off, "every line ≤80 cells" alone cannot
  fail, so the fixture includes a deliberately over-long locked row (🔒 plus a
  long value with a CJK/wide character) and the test asserts _our_ truncation:
  the row ends in `…` inside the 80 cells, and the row below it is the next
  settings row (not a wrapped tail). Tab bar on row 1, status on row 24.
  `resize-window -x 60 -y 20` → the same assertions at 60 cells, status on row 20. `q` restores the main screen. Unit-level: the truncate helper over ASCII,
  🔒 and CJK strings at an exact width.
- Unit tests for the new `refresh-budget --report [--json]`, retro `rows`, rate
  `burn_median`.
- Flake: `crew-dash` package, in `default`, plus `checks.crew-dash` pointing at
  it — `nix flake check` only evaluates `packages`, so without the check nothing
  builds it (and `writeShellApplication`'s build-time shellcheck never runs).
  CI's `shellcheck adapters/core/*.sh` covers the source.
- README "Dashboard" section.

## Out of scope

Editing any settings file (the `editable` field and the cursor are the only
preparation). Background refresh. Cross-repo retro. Any `gh` call.
