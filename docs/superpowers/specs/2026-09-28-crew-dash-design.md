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

## Decision: toolkit — Go + Bubble Tea/Lipgloss/Bubbles (owner direction)

The first slice chose bash (no new toolchain). The owner overrode that: the
dashboard is meant to become a serious TUI — collapsible trees, filtering,
gauges, sortable tables, drill-downs, a live bus tail, and later actions and
user-layer editing — and that is Bubble Tea's territory, with lipgloss/ansi
width math, `bubbles` widgets (help, textinput, progress, key) and golden
`View()` frames. The cost is accepted: a Go module in the flake
(`buildGoModule` + `vendorHash`), Go in the devshell, and `gofmt` in treefmt.

What carries over unchanged: the data layer (`dispatch-config --layers`,
`refresh-budget --report --json`, retro `rows`, rate `burn_median` — already
landed), the `crew dash` delegation in `crew.sh`, the model JSON shape of
`--json`, and the `--once` text format (its golden stays the contract). The
bash `crew-dash.sh` is deleted.

## Shape

- Go module `dash/` (`module github.com/noamsto/dispatcher/dash`, `go 1.24.2` per `go mod tidy`),
  on the **v1** Charm APIs: `github.com/charmbracelet/bubbletea` v1.3.x,
  `lipgloss` v1.1.x, `bubbles` v1.x, `x/ansi` (not the `charm.land/…/v2`
  modules), binary `crew-dash`:
  - `dash/main.go` — flags (`--once`, `--json`, none), mode selection
    (interactive only when stdin and stdout are TTYs; otherwise `--once`).
  - `dash/internal/data` — the read-only data layer: `Snapshot` (settings rows +
    layers + warnings, budget report, retro, ratings, roster crews, `Now`), a
    `Runner` interface (`Run(ctx, name, args...) (stdout, stderr []byte, err)`)
    with an exec implementation, `Collect(ctx, Runner, Config) Snapshot`
    (per-source degrade to `{error}`, stderr of a successful source → that
    pane's warnings), the bus reader (`EventsPath` via `git rev-parse
--path-format=absolute --git-common-dir`, `RecentEvents(branch, n)`), and
    JSON types matching the CLIs' `--json` contracts.
  - `dash/internal/once` — the `--once` renderer (pure: `Snapshot → string`,
    colour on/off as before: `CREW_DASH_COLOR`, `NO_COLOR`, TTY).
  - `dash/internal/ui` — the Bubble Tea program: root model (tabs, help bar,
    size, snapshot, refresh), one sub-model per view in its own file.
- Clock: `Snapshot.Now` is `$CREW_DASH_NOW` (epoch seconds) when set, else
  `time.Now()`; the TUI's age tick advances from it. Bats and Go goldens pin
  it, replacing the bash build's `date` shim.
- Resolution: `crew` from `$CREW_BIN` (executable → exec; else `bash
"$CREW_BIN"`), else `crew` on PATH; `dispatch-config` from
  `$DISPATCH_CONFIG_BIN`, else the path baked at build (`-ldflags -X
main.dispatchConfigBin=…`), else `dispatch-config` on PATH; `refresh-budget`
  and `git` from PATH.
- Flake: `crew-dash = pkgs.buildGoModule { src = ./dash; vendorHash = …;
ldflags bake the locked-layer-aware dispatch-config; postInstall
wrapProgram --prefix PATH [refresh-budget git] }`, so `nix build` runs `go
test ./...` (doCheck) and `checks.crew-dash` makes `nix flake check` build
  and test it. `crew` keeps `crew-dash` in runtimeInputs (no cycle: crew-dash
  does not list crew). Devshell gains `go`; treefmt gains `gofmt`.

## Data sources (read-only; no gh, no network)

(As accepted in the first slice; now implemented in Go.)

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

## `--once` text (format unchanged from the first slice)

The plain renderer keeps the accepted pane text exactly, so `tests/fixtures/crew-dash/once.golden` stays the contract; `--once` sections are headed `== Settings ==` etc. and never truncate.

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

## Interactive TUI

Follows the charm-tui skill: every inner size derived from
`GetHorizontalFrameSize`/`GetVerticalFrameSize` and measured chrome
(`lipgloss.Height` of tab bar and help bar), `max(0, …)` on every derived
dimension, `ansi.Truncate` on every string the dashboard does not control,
`MaxWidth`/`MaxHeight` on every sized box, no `len()` for column math, and
`View()` returns `""` until the first `WindowSizeMsg`.

**Chrome.** Row 1: tab bar `Settings │ Budget │ Runs │ Roster`, active tab
highlighted. Bottom: `bubbles/help` bound to one `KeyMap` (short help; `?`
toggles full help), plus a status segment `refreshed HH:MM:SS` and the active
filter. Body: the active view, given an explicit width and height.

**Global keys.** `1`–`4`, `tab`/`shift+tab` switch view; `r` re-collects the
snapshot (async `tea.Cmd`, spinner-free: status reads `refreshing…`); `?`
help; `q`/`ctrl+c` quit (in a text input only `esc`/`ctrl+c` escape it).

**Settings.** A collapsible tree built from the `--show-origin` rows: branch
nodes (`▸`/`▾`) collapse and expand with `enter`/`space`/`←`/`→`; `j`/`k`/`↑`/`↓`
move a cursor; `g`/`G` top/bottom. Each leaf: `key: value` (compact JSON,
truncated) and a right-aligned provenance badge with a distinct style per
origin — default (faint), user (cyan), env (magenta), locked (bold yellow with
`🔒`). `/` opens a `textinput` filter: a leaf matches when its dot-path or its
value contains the query (case-insensitive); matching leaves and their
ancestors are shown expanded; `esc` clears. Header: the three layer paths and
any warnings. Under `NO_COLOR` the lipgloss profile is ASCII; locked stays
distinct by `🔒` and bold.

**Budget.** `fetched <age> ago` (+ `stale` past 2h). Per engine a header
(`claude (oauth_usage) [plan]`); per window a row: name, a `bubbles/progress`
gauge (static `ViewAs`, width from the remaining space, min 10), `used%`,
`pace +N/-N/—`, `resets in`, and the verdict (truncated; `enter` on a window
shows the full verdict in a detail pane). Gauge colour by used %: <85 normal,
85–95 warning, ≥95 danger. Pi: spend/target line and the projection. Null
engine: `unknown`.

**Runs.** Two sections, `f` switches focus. (a) Ratings table by {tier,
engine, model}: `n  pr%  success  burn(med)` where success = merge% (merged
among settled), rendered with the rate markers (`—`, `(k)`, `!`); `s` cycles
the sort column, `o` flips the order; default sort tier/engine/model. (b) Runs
list from retro `rows`, newest `t0` first: run rows `branch  tier/engine/model
outcome  tags`, dispatcher rows `crew <id>  session_summary (truncated)  tags`.
`enter` opens a detail view of that row (every note `seam · tag` + full detail
wrapped to the width, scrollable); `esc` returns.

**Roster.** Live. On start the model watches the directory of the bus log
(`fsnotify`; if the watcher cannot be created, a 2s poll of the file's size and
mtime) and, on a change (debounced 300ms), re-runs only the roster sources
(`crew crews`, `crew roster`, `crew hold list`). A 1s tick re-renders ages (it
never re-collects). Table: codename in its recorded tmux colour (`colourNN` →
`lipgloss.Color("NN")`), state, `tier/engine/model`, detail (truncated), age,
PR. Holds listed below. `enter` on a worker shows its recent events: the last
20 bus events whose `from` is exactly `worker:<branch>` or starts with
`worker:<branch>#` (ts, kind, state /
detail, or msg `to` + body), control/bidi characters stripped. No live crew:
`no active crew`.

## Seams for actions and editing (not built here)

- Reads are all behind `data.Runner`; writes will be a separate
  `data.Writer` (e.g. `SetUserKey(path, value)` writing
  `settings.json` then re-validating through `dispatch-config`, or `crew
reply`), so no view shells out.
- Each view exposes `Selection() data.Selection` — a tagged union of the
  focused entity (`SettingRow{Path, Origin, Editable}`, `Worker{Crew, Branch,
Session}`, `Run{…}`). An actions layer is a table `(key, Selection kind) →
tea.Cmd` consulted by the root model for keys no view consumed; the command
  performs the write and returns `refreshMsg`. Editing a user key adds one
  `textinput` modal to the settings view bound to `SettingRow.Editable`.
- Nothing of that is added now; the PR states it.

## Tests

- `data`: collector over a fake `Runner` (every source ok; each source failing
  → only its pane degrades; retro empty stdout → empty model; stderr warnings;
  older crew without `rows` → no crash), settings-tree leaf rule, bus reader.
- `once`: golden over snapshot fixtures (the existing `once.golden` content),
  colour on/off.
- `ui`: golden `View()` frames at 80×24 and 120×40 for each view (lipgloss
  forced to the ASCII profile for readable goldens), a resize test (80×24 →
  60×20 → 120×40: every frame has exactly `height` lines, each ≤ `width`
  cells by `ansi.StringWidth`), update tests per view (tab switching, tree
  collapse/expand, filter, cursor clamp, sort cycling, drill-in/out, roster
  refresh on a bus-change msg, age tick), and a distinct-style test (each
  origin's badge renders differently under TrueColor).
- `nix build .#crew-dash` runs all Go tests; `checks.crew-dash` puts them in
  `nix flake check`.
- `tests/crew-dash.bats` keeps the end-to-end contract through the real CLIs:
  it uses a prebuilt `$CREW_DASH_BIN` when set (e.g. `nix build
.#crew-dash`, works offline), else builds once with `go build` in
  `setup_file` and keeps the
  existing `--once` golden / `--json` / degraded / warnings / retro /
  backslash / large-value / older-crew / delegation / usage tests; the bash
  truncation and tmux frame tests move into Go.

## Acceptance mapping

- Golden `--once` over fixture settings/budget/retro/ratings (all three layers,
  a locked key, an ahead-of-pace engine, an empty retro store):
  `bats tests/crew-dash.bats` (real CLIs) and `go test ./internal/once`.
- Interactive 80×24 and resize without bleed: `go test ./internal/ui` golden
  frames + resize test; charm-tui-reviewer and go-reviewer at review.
- Packaged in the flake; `nix flake check` builds and tests it; shellcheck
  clean on shell.
- README "Dashboard" section updated for the new keys and views.

## Out of scope

Any write (settings edit, reply, reap, attach). Cross-repo retro. Any `gh`
call.
