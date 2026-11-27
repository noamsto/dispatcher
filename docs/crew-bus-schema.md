# Crew Bus Schema & Directory Layout

Frozen reference for `events.jsonl` rows and crew directory layouts, derived from `crew/internal/bus/` and `crew/internal/stream/`. Intended for downstream consumers (`houston`, `overseer`).

## Schema Versioning Status

- **`events.jsonl` rows**: Currently **unversioned** (no `schema_version` field on event rows). This document freezes the current v1 baseline.
- **`meta.json` (layout 2)**: Versioned via `"layout": 2`.

---

## `events.jsonl` Row Schema

Each line in `events.jsonl` is a single compact JSON value (typically a JSON object).

### Envelope Fields

| Field     | Type            | Required | Description                                                                                                           |
| --------- | --------------- | -------- | --------------------------------------------------------------------------------------------------------------------- |
| `kind`    | `string`        | yes      | Event type (see [Event Kinds](#event-kinds)).                                                                         |
| `from`    | `string`        | yes      | Sender identifier (e.g. `worker:<branch>#s<pid>`, `dispatcher:<crew_id>`).                                            |
| `to`      | `string`        | yes      | Recipient identifier or broadcast target (`*`, `dispatcher:<crew_id>`, `metrics:<crew_id>`, `retro:<crew_id>`, etc.). |
| `crew_id` | `string`        | yes      | Crew identifier (`<ts>-<pid>` or named crew ID).                                                                      |
| `ts`      | `number`        | yes      | Unix timestamp in milliseconds.                                                                                       |
| `branch`  | `string`        | no       | Git branch associated with the event.                                                                                 |
| `session` | `string`        | no       | Session identifier.                                                                                                   |
| `body`    | `object \| any` | no       | Kind-specific payload.                                                                                                |

### Event Kinds

Defined in `crew/internal/bus/bus.go`:

- `status` — worker/session lifecycle state update
- `msg` — direct or broadcast message (including synthetic sinks like `metrics:` and `retro:`)
- `dispatch` — worker dispatch event
- `resume` — worker resume event
- `window` — tmux window lifecycle event
- `nudge` — dispatcher nudge to a worker pane
- `nudge_wait` — nudge wait/ack event
- `reap` — crew/worker reap event
- `release` — crew/worker release event
- `claim` — crew/worker claim event
- `reclaim` — crew/worker reclaim event

Unrecognised `kind` values are preserved as-is by readers.

### `status` Event `body.state` Values

For `kind: "status"`, `body.state` takes one of:

| State      | Terminal? | Notes                              |
| ---------- | --------- | ---------------------------------- |
| `working`  | no        | Active work / heartbeat            |
| `blocked`  | no        | Awaiting reply or watchdog-flagged |
| `pr_open`  | no        | PR opened, awaiting completion     |
| `done`     | yes       | Completed                          |
| `failed`   | yes       | Terminal failure                   |
| `exited`   | yes       | Process/window exited              |
| `reaped`   | yes       | Posted by `crew reap`              |
| `migrated` | yes       | Migrated to another host           |

### Decoding Semantics

- One JSON value per line; leading UTF-8 BOM (`\xef\xbb\xbf`) is stripped.
- Tolerant readers (`EventsFromLines`, `ReadEventsTolerant`) skip blank, torn, or multi-value lines so a corrupt tail does not hide earlier or later valid rows.
- Non-object JSON values (e.g. bare `null` or numbers) are accepted by the raw decoder with empty typed convenience fields.

---

## Crew Directory Layout

### Layout 1 (Repo-Scoped)

Located under the repository's git common directory:

```text
<git_common_dir>/crew/
├── events.jsonl          # Shared event log for layout-1 crews in this repo
├── cursor                # Stream read cursor (byte/line offset)
├── pane                  # Associated tmux pane id
├── pid                   # Stream/daemon PID
└── stream.lock.d/        # Atomic directory lock for stream exclusivity
```

### Layout 2 (State-Root-Scoped)

Located under `${XDG_STATE_HOME:-$HOME/.local/state}/crew/`:

```text
<state_root>/
├── crews/<id>/
│   ├── meta.json         # Pins crew as layout 2 (created atomically via hard link)
│   ├── events.jsonl      # Crew-specific event log
│   ├── cursor            # Crew-scoped stream cursor
│   ├── pane              # Crew-scoped pane file
│   ├── pid               # Crew-scoped PID file
│   └── stream.lock.d/    # Crew-scoped stream lock
├── archive/<id>/
│   ├── meta.json         # Archived crew metadata
│   └── events.jsonl.gz   # Gzipped event log after archive
└── machine/
    └── <YYYY-MM>.jsonl   # Monthly UTC machine log for events outside any crew
```

### `meta.json` Schema (Layout 2)

Defined in `crew/internal/bus/meta.go`:

| Field                | Type             | Required | Description                                                     |
| -------------------- | ---------------- | -------- | --------------------------------------------------------------- |
| `layout`             | `integer`        | yes      | Always `2`.                                                     |
| `id`                 | `string`         | yes      | Crew ID (must match directory name).                            |
| `created`            | `integer`        | yes      | Creation timestamp in milliseconds.                             |
| `home_host`          | `string`         | yes      | Short hostname (origin host when visiting).                     |
| `home_repo`          | `string \| null` | yes      | `RepoTag` of home repo, or `null` if registered outside a repo. |
| `home_slug`          | `string`         | no       | Optional repo slug (`omitempty`).                               |
| `visiting`           | `boolean`        | yes      | Whether this crew is visiting from another host.                |
| `created_by`         | `string`         | yes      | `"register"`, `"init"` or `"remote-dispatch"`.                  |
| `dispatcher_session` | `string`         | no       | Optional dispatcher session id (`omitempty`).                   |

### Crew Resolution Order

`bus.Crew` classifies a crew ID in order:

1. Validate ID (`ValidCrewID`).
2. `<state_root>/crews/<id>/meta.json` exists → **Layout 2** (`<state_root>/crews/<id>/events.jsonl`).
3. `<state_root>/archive/<id>/meta.json` exists → **`ArchivedError`** (refused).
4. Repo index lists `<id>` without a live layout-2 dir → **`IndexOnlyError`** (refused; never falls back to layout 1).
5. Otherwise → **Layout 1** (`<git_common_dir>/crew/events.jsonl`, requires a git repo).

### Creating a crew directory

`crew init [<id>]` creates `<state_root>/crews/<id>/` with `meta.json` (`created_by: "init"`) and an empty `events.jsonl`, and claims nothing: no `pid`, `pane` or `engine` file (`crew register` writes those). It is idempotent (an existing `meta.json` is kept, an existing log is never truncated) and prints one JSON line `{"id","dir","log","created"}`. Consumers use it instead of `mkdir` under the state dir.

### Reading the bus: `crew dash --json --view events`

Cursor-resumable tail of one crew's `events.jsonl`, schema version 1:

```
{"schema_version":1,"view":"events","generated_at":<s>,"host":"<short host>",
 "cursor":"<opaque>","reset":false,"more":false,"errors":[],
 "events":[{"id","host","ts","kind","from","to","crew","branch","text","row":{…raw row…}}]}
```

- `id` is `<start byte offset>-<first 16 hex digits of the SHA-256 of the raw line>`: stable across readers and runs, and unique within a log even for byte-identical lines.
- `host` is the short hostname of the machine whose log was read.
- No `--since`: the last `--limit` rows (default 200). `--since <cursor>`: rows after that cursor, at most `--limit` (`more: true` when rows remain; pass the returned `cursor` again).
- The cursor is opaque (`<inode>:<byte offset>`); a torn last line is never consumed, so no row is missed or replayed. A cursor naming another inode or an offset past end of file (rotation, truncation) restarts from the top with `reset: true`. Logs are append-only: in-place truncation that regrows past the old offset is not detected. Complete lines that are not JSON are skipped and counted in `errors`.
- `--watch` emits the first page, then one line per batch of new rows (nothing when none); resume a dropped stream with `--since` of the last line's `cursor`.

`--view overview` workers also carry `session`, `live` and `exit_suspect`, and `dispatcher` carries `pid`, `pane` and `alive` (pid/pane files, signal-0 probe; `null` when unknown).
