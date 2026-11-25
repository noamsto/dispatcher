# dash-anim

Throwaway spike (issue #1820): can an animated Rust dashboard built on
`rataflow` (node graph), `tachyonfx` (effects) and `ratatui` render the crew
map and attention list, stay quiet when nothing moves, and ship as a small
binary? It is **not shipped** and not wired into the root build or the Go
`dash`; it exists to answer that question and be deleted or ported.

## Usage

Run from `spikes/dash-anim`. The golden fixtures are the Go dash test data.

```fish
set G ../../dash/internal/view/testdata/golden

# Replay one fixture (a JSON document stream), one document per --replay-ms
cargo run --release -- --fixture $G/map.json

# Several fixtures are merged: map feeds the Graph tab, overview the List tab
cargo run --release -- --fixture $G/map.json --fixture $G/overview.json

# Generated workers and roles, animated
cargo run --release -- --synthetic 10

# One headless frame as text (WxH), then exit
cargo run --release -- --fixture $G/map.json --dump 160x50
cargo run --release -- --fixture $G/map.json --fixture $G/overview.json --dump 120x40 --tab list

# Live mode: spawns `crew dash --json --view map|overview --watch`
cargo run --release
```

Live mode is **not exercised against a real crew**. It is covered only by the
JSON-document parsing unit tests and the fixture replay that feeds the same
code path.

| Flag               | Effect                                                            |
| ------------------ | ----------------------------------------------------------------- |
| `--fixture <path>` | Replay a document stream; repeatable                              |
| `--replay-ms <n>`  | Delay between replayed documents (default 1500)                   |
| `--synthetic <n>`  | Generate `n` workers (with plan-critic/reviewer roles)            |
| `--horizontal`     | Lay the graph out left to right                                   |
| `--reduced-motion` | No effects, no edge animation; removed list rows drop immediately |
| `--dump WxH`       | Render one frame as text and exit (forces reduced motion)         |
| `--tab graph/list` | Tab shown first, or dumped                                        |

Keys: `Tab` switches tabs, `1` Graph, `2` List, `q` or `Ctrl-C` quits.

## Nix

The acceptance checks, from the repo root:

```fish
nix shell nixpkgs#cargo nixpkgs#rustc nixpkgs#clippy -c cargo clippy -- -D warnings
nix shell nixpkgs#cargo nixpkgs#rustc -c cargo test
```

Run those inside `spikes/dash-anim`. The spike also has its own flake whose
devShell carries cargo, rustc, clippy and rustfmt:

```fish
nix develop ./spikes/dash-anim
nix develop path:. -c cargo --version   # from the spike dir; prints cargo 1.99.0
```

## Findings

### Layout quality at 3, 10 and 20 workers

All captures are `--dump 160x50` of the release binary. Node borders are
drawn by rataflow; the state colour is not visible in text.

**Golden map (11 workers, no roles), 160x50.** Readable: every node is
bordered and titled with its state, the label survives, edges fan out from the
dispatcher. Nodes are only about 14 columns wide, so long branch names would
clip. The full capture is `captures/map-160x50.txt`; the list tab is
`captures/list-120x40.txt`.

**3 workers** is good. Three tiers, wide boxes, edges and arrow heads clear.
Nodes are taller than needed (5 rows for one line of text) but nothing clips.

```text
                                                                                 ┌──────────────────────────┐
                                                                                 │dispatcher                │
                                                                                 └──────────────────────────┘
                                                                                               │
                                                  ╭────────────────────────────────────────────┼── ── ── ── ── ── ── ── ── ──╮
                                                  ▼                                            ▼                             ▼
                                    ┌pr_open───────────────────┐                 ┌blocked───────────────────┐  ┌working───────────────────┐
                                    │feat/s2                   │                 │feat/s1                   │  │feat/s0                   │
                                    └──────────────────────────┘                 └──────────────────────────┘  └──────────────────────────┘
                                                  │                                            │
                                   ╭── ── ── ── ──┴── ── ── ── ──╮                             │
                                   ▼                             ▼                             ▼
                     ┌working───────────────────┐  ┌working───────────────────┐  ┌blocked───────────────────┐
                     │reviewer                  │  │plan-critic               │  │plan-critic               │
                     └──────────────────────────┘  └──────────────────────────┘  └──────────────────────────┘
```

(The capture has three blank interior rows per node; two of them are removed
here.)

**10 workers, vertical** is marginal. The fit-view zoom shrinks the worker nodes to
height 1, so their borders and state titles disappear. Labels survive only because
the node renderer falls back to label-only reverse video, and the fixed
zoom makes sibling nodes touch with no gap. Still legible, but the state is
carried by colour alone.

```text
                                                              ┌────────────┐
                                                              │dispatcher  │
                                                              └────────────┘
                                                                     │
      ▼─────────────▼────────────────────▼─────────────▼─────────────▼────────────────────▼─────────────▼─────────────▼────────────────────▼ ── ── ── ── ▼
feat/s9      feat/s8              feat/s7       feat/s6       feat/s5              feat/s4       feat/s3       feat/s2              feat/s1       feat/s0
             ▼ ── ──┴── ── ▼             ▼                    ▼──────┴──────▼             ▼                    ▼─ ── ─┴ ── ──▼             ▼
      ┌working─────┐┌working─────┐┌blocked─────┐       ┌blocked─────┐┌blocked─────┐┌working─────┐       ┌working─────┐┌working─────┐┌blocked─────┐
      │reviewer    ││plan-critic ││plan-critic │       │reviewer    ││plan-critic ││plan-critic │       │reviewer    ││plan-critic ││plan-critic │
      └────────────┘└────────────┘└────────────┘       └────────────┘└────────────┘└────────────┘       └────────────┘└────────────┘└────────────┘
```

**20 workers, vertical** fails. Nodes are about 7 columns wide, so labels are
cut to `feat/s1`, `plan-cr`, `reviewe`; the dispatcher is clipped to
`dispatc`; columns overlap and lose their borders; the numeric suffix of
`feat/s10`..`feat/s19` is unreadable. The graph is unusable at this size
in 160 columns.

```text
                                                                 dispatc
     ▼      ▼          ▼       ▼      ▼           ▼      ▼       ▼  │       ▼      ▼       ▼          ▼       ▼      ▼          ▼       ▼      ▼           ▼
1feat/s1─feat/s1────feat/s1feat/s1─feat/s1────feat/s1─feat/s1feat/s1┴───feat/s1─feat/s9feat/s8─────feat/s7feat/s6─feat/s5────feat/s4feat/s3─feat/s2────feat/s1 f
        ▼───────▼                  ▼── ── ▼                  ▼──────▼                  ▼─ ── ──▼                 ▼───────▼                  ▼── ── ▼
r    revieweplan-cr plan-cr    reviewe plan-crplan-cr    reviewe plan-crplan-cr     revieweplan-cr plan-cr    revieweplan-cr plan-cr    reviewe plan-crplan-cr
```

**10 workers, `--horizontal`** is the best large layout: the tree grows
downward in lanes, every node keeps its border, state title and label, and
vertical room scales better than horizontal room on a wide-but-short pane.
Defects: the trunk line runs along the left borders of the first-level nodes
(see API gaps), and the 20-worker horizontal layout was not captured. A
real port would scroll or collapse finished workers rather than rely on
fit-view.

```text
                                                                       ┌working──────┐
                                                                       ▶│feat/s6      │
                                                                       │└─────────────┘  ┌blocked───────┐
                                                                       │                 │reviewer      │
                                                      ┌──────────────┐ │┌reaped───────┐ ▶└──────────────┘
                                                      │dispatcher    │ ││feat/s5      │ │
                                                      └──────────────┘─▶└─────────────┘─┤┌blocked───────┐
                                                                       │                ││plan-critic   │
```

### Effect timing

| Effect                         | Duration | Interpolation | tachyonfx call             |
| ------------------------------ | -------- | ------------- | -------------------------- |
| Node glow on state change/add  | 600 ms   | SineOut       | `fade_from(color, color)`  |
| List row appear                | 350 ms   | QuadOut       | `slide_in` (left to right) |
| List row vanish (removed item) | 350 ms   | QuadIn        | `fade_to(Black, Black)`    |

The glow is long enough to catch the eye on a quiet map and uses SineOut so it
decays smoothly instead of snapping. Appear and vanish stay near 350 ms so a
burst of changes finishes before the next poll. Vanish uses QuadIn so the row
lingers briefly and then drops. The list is diffed by item id, so only changed
rows animate; a rebuild does not replay everything.

Node glow fires only while the Graph tab is shown and not under
`--reduced-motion`; changes that arrive on the List tab are not replayed
when switching back. An Overview document alone does not re-glow unchanged
workers. When the feed ends, the header shows `[feed ended]`.

Honest caveat: feel was judged from the timing parameters and frame
captures, not from long interactive use. The release binary was run in a pty
(see Idle CPU) but nobody watched the effects for an extended session.

### Idle CPU

Release binary in a real pty inside a private tmux server (`pidstat -p <pid> 1 30`, average over 30 s, 160x50 pane, 4 s warm-up).

| Case                                 | Average %CPU | Threads | State  |
| ------------------------------------ | ------------ | ------- | ------ |
| (a) idle: no `working`, replay ended | 0.00         | 2       | `Ssl+` |
| (b) animated: golden map, 2 working  | 0.60         | 2       | `Ssl+` |
| (c) same as (b), `--reduced-motion`  | 0.00         | 2       | `Ssl+` |
| (d) `--synthetic 20`, animated       | 0.53         | 2       | `Ssl+` |

Idle truly blocks: the main thread parks in `recv()` with no timeout when no
effect is running and no edge is animating (`S` state, 0.00 %CPU over 30 s;
the second thread is the blocked input reader). Animation costs about 0.5 to
0.6 %CPU at the 120 ms edge tick, with a 33 ms tick only while an effect runs.
The 20-worker case is not meaningfully dearer than the 2-worker one.

The numbers above were measured before edge ticks were limited to the Graph
tab; on the List tab the loop now parks in `recv()` regardless of working
nodes (not re-measured).

### Binary size and build time

| Item                                                        | Value                       |
| ----------------------------------------------------------- | --------------------------- |
| `target/release/dash-anim` (lto, opt-level z, strip, cgu 1) | 1,340,512 B (about 1.3 MB)  |
| `cargo build --release`, deps already in the cargo cache    | 33.8 s wall (1 m 20 s user) |

The build time excludes crate download (sources were already fetched);
a cold fetch adds network time.

### Build approach and Nix packaging

The spike builds with plain cargo from a Nix shell, and
`spikes/dash-anim/flake.nix` exposes a devShell with cargo, rustc, clippy
and rustfmt (`nix develop ./spikes/dash-anim`, verified). No package output is
defined. The real-port route is a `rustPlatform.buildRustPackage` with
`cargoLock.lockFile` (or a vendored `cargoHash`) over the committed
`Cargo.lock`; it was not built here.

### API gaps and bugs hit

1. **Fit-view zoom scales node heights.** A bordered 3-row node can render at
   height 2 (or 1), so the label row vanishes. Workaround: the node renderer
   draws the label alone in reverse video when the border has no room.
2. **Default handle dots overdraw border-title letters.** Worked around with
   `set_handles_hidden`.
3. **`NodeRenderContext` is `pub(crate)`**, so a node's screen rect is not
   available to effects. Rects are recomputed from `flow.viewport.world_to_canvas`.
4. **Horizontal layout trunk overlaps left borders**, as in the 10-worker
   capture above.
5. **tachyonfx effects show their hidden first state in a single t=0
   frame**, so `--dump` forces reduced motion.
6. **No topology-update API.** A change in the set of nodes or edges rebuilds
   the `Flow`, and fit-view resets pan and zoom, so the map jumps whenever a
   worker appears or leaves. State-only changes use `refresh` and keep the view.

### Recommendation

Conditional go for a port, no-go for rataflow as the primary view at scale.
tachyonfx and ratatui are solid: effects are cheap, idle is genuinely 0 %CPU
with a blocking event loop, and the binary is about 1.3 MB. rataflow is the
risk: it is a 0.1 crate, fit-view fails above about 10 workers in 160
columns, there is no incremental topology update, and node internals are not
public. A real port should keep the list view and glow effects, render the
graph for a bounded set (active workers only, or scrolled), and be prepared to
vendor or replace rataflow's layout. The Go dash remains the shipped surface
until live mode has been run against a real crew.
