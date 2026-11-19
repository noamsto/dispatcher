# Plan: standing per-repo owner policy (#1679)

Spec: `docs/superpowers/specs/2026-11-19-standing-policy-design.md` (accepted).
Fixed names used across steps: package `crew/internal/policy`, verb
`crew policy set|clear|show|list`, merge flag `--policy <name>`, rare-path skill
`standing-policy`, launcher input `crew policy list --brief`.

Every test (Go and bats) sets `XDG_STATE_HOME` and `HOME` to temp dirs; no test
touches the owner's tmux server, settings file, crew dirs or state dir.

## Ordering

S1 → (S2 ∥ S3); S4 ∥ S1; S5 after S4 (it cites the prompt wording S4 fixes);
S6 (docs) after S2–S5.

## S1. `crew/internal/policy` — record, owner-only writes, validated reads (implement: opus)

Files: `crew/internal/policy/policy.go`, `crew/internal/policy/policy_test.go`;
`crew/internal/sender/sender.go` (one exported helper) + its test.

- `Record{Name, Text, Repo, By, Host string; UID int; TTY, At string}` (JSON keys
  `name,text,repo,by,host,uid,tty,at`).
- `Dir(common string, getenv func(string) string) (string, error)`: realpath
  of `common` (`filepath.EvalSymlinks` + `Abs`, as `gitguard.armedKey`), sha256
  hex, under `$XDG_STATE_HOME` (else `$HOME/.local/state`) `/crew/policy/<hex>`.
  Returns the realpath too (for `Repo`).
- `ValidName` (`^[a-z0-9][a-z0-9-]{0,31}$`), `ValidText` (1–2000 runes, no
  `unicode.IsControl` rune, `!invisible.Has`), each returning a reason.
- `Load(common, name, o) (Record, error)`: `ErrMissing` (wrapped, file absent)
  vs a refusal error carrying the reason. Checks in spec order: Lstat the dir and
  the file — neither a symlink, file regular; `Stat_t.Uid == os.Getuid()`;
  `mode & 0o077 == 0`; JSON decodes with `DisallowUnknownFields`, every field
  non-empty (`UID` compared, not emptiness); `Name == name`; `Repo == realpath`;
  `UID == file owner`; `ValidText`; `time.Parse(time.RFC3339, At)`.
- `List(common, o) (in []Record, refused []string, err)`: every `*.json` in the
  dir sorted by name; a missing dir is an empty list.
- `OwnerRefusal(ctx, o) string`: "" or the reason. In order: env markers
  `CREW_WORKER_ID`, `CREW_ROLE_ID`, `CREW_OVERSEER`, `CREW_ID`,
  `DISPATCHER_LAUNCHER_PID`, `DISPATCHER_HANDOFF_DIR` non-empty (name the
  variable: "unset it if your own shell exported it"); `!o.IsTTY()` → "needs your
  own terminal (not an agent's shell tool or Claude Code's ! prefix)"; then
  `sender.CrewPane(ctx, o.Sender)` non-empty → "this is a crew pane (<desc>)".
- `sender.CrewPane(ctx, p Probes) string`: reads rows via the existing
  `p.rows`/`p.anchor`; returns a description when the caller's anchor pane is
  stamped (branch, role or the overseer pane), "" otherwise or when unreadable.
  Unit-test with a fake `tmux.Client` and `proc.Table` like the existing sender
  tests.
- `Write(common, rec, o)`: `MkdirAll` 0700 (and `Chmod` 0700 on the leaf dir),
  temp file in the dir via `os.CreateTemp`, `Chmod` 0600, encode, `Sync`,
  `Close`, `Rename`. `Clear(common, name, o)`: `ErrMissing` when absent, else
  `os.Remove`.
- `Options{Getenv func(string) string; IsTTY func() bool; TTYName func() string;
User func() (string, error); Hostname func() (string, error); Now func()
time.Time; Sender sender.Probes}` with defaults: `os.Getenv`,
  `bus.IsTerminal(0)`, linux `os.Readlink("/proc/self/fd/0")` else `"tty"`
  (fallback `"tty"` on error), `user.Current().Username`, `os.Hostname`,
  `time.Now().UTC()`.
- `Run(ctx, Request{Action, Name, Text string; JSON, Brief bool}, common string,
stdout, stderr io.Writer, o Options) int` — outputs and exit codes exactly as
  the spec's Read/Write sections (`crew: policy: …` prefix on refusals, exit 1).
  Text output of untrusted fields goes through a one-line escape (control → `?`,
  `invisible.Escape`); `--brief` lines: `'<name>' set by <by>@<host> from <tty>
at <at>: "<text>"`.
- Tests (table-driven, temp `XDG_STATE_HOME`, temp git repo for `common`):
  set→show→list→clear happy path; set replaces; each env marker refused (6
  rows); no TTY refused; stamped pane refused (fake probes); bad name; bad text
  (newline, control, hidden code point, empty, 2001 runes); read refusals:
  missing, file symlink, dir symlink, mode 0644, `uid` field ≠ owner, malformed
  JSON, unknown field, name mismatch, `repo` mismatch, bad text, bad `at`; the
  same repo reached through a symlinked path resolves to the same record; list
  puts a refused record on stderr and keeps in-force ones; `--brief` empty when
  none; `Dir` is not under `common`.

Gate: `cd crew && go test ./internal/policy/ ./internal/sender/ && go vet ./...`.

## S2. CLI wiring for `crew policy` (implement: sonnet)

Files: `crew/commands.go`, `crew/main.go`, `crew/main_test.go`,
`crew/internal/entry/texts/policy.txt`, `crew/internal/entry/texts/top.txt`,
`crew/policy_cli_test.go` (new); completion/help tests that pin the verb list.

- `policyCmd{Set policySetCmd; Clear policyClearCmd; Show policyShowCmd; List
policyListCmd}` modelled on `queueCmd`; `Set{Name, Text string args}`,
  `Clear{Name}`, `Show{Name; JSON bool}`, `List{JSON, Brief bool}` (`--json` and
  `--brief` together → usage refusal). Usage line in the usage table; add to
  `subcommands`, `typedSubcommands`, the kong root (`help:"Standing per-repo
owner policy for unattended runs."`), `routeHelp` (`policy` with
  `isHelpFlag(arg(args, 2))`), the help-key mapping that names `queue add|list|drop`
  (same for `policy set|clear|show|list`), and the test lists in `main_test.go`
  (the three slices that enumerate subcommands).
- Dispatch after `bus.Locate` (needs `paths.Common`): `policy.Run(ctx,
policyRequest(parsed), paths.Common, stdout, stderr, policy.DefaultOptions())`
  where the default `Sender` probes are the same ones `merge`/`nudge` get.
- `policy.txt` help screen (verbs, owner-only note, where the record lives);
  `top.txt` row beside `queue`.
- `policy_cli_test.go`: through `run(...)` with a temp repo — a worker env
  (`CREW_WORKER_ID` set) is refused on set with exit 1 and nothing written; show
  of a missing policy exits 1; usage refusals (`policy set` with no text,
  `policy list --json --brief`). The TTY path is covered in S1 (the CLI test
  cannot fake a TTY, so it asserts the no-TTY refusal with every env marker
  cleared).

Gate: `cd crew && go test . ./internal/entry/ && go vet ./... && golangci-lint run ./...`.

## S3. `crew merge <N> --policy <name>` (implement: sonnet)

Files: `crew/internal/merge/merge.go`, `crew/internal/merge/merge_test.go`,
`crew/commands.go` (`mergeCmd.Policy *string name:"policy"`), `crew/main.go`
(pass it through), `crew/internal/entry/texts/merge.txt`.

- `Request.Policy string`; `Options.Policy func(name string) (policy.Record,
error)` (default: `policy.Load(paths.Common, name, policy.DefaultOptions())`).
- In `Run`, right after the sender check and before `gh pr view`: when
  `req.Policy != ""`, load it; error → `crew: merge: refusing PR <N> — standing
policy '<name>' is not in force: <reason>` (missing: "no standing policy
  '<name>' in this repo"), exit 1, no GitHub call.
- `pass("policy: '<name>' set by <by>@<host> at <at>: \"<text>\"")` (one-line
  escaped) so `--dry-run` shows it; after a real merge print `merged under
standing policy '<name>' (set by <by>@<host> at <at>): "<text>"` to stdout;
  `release` body becomes `approved — PR #<N> is merged under standing policy
'<name>'; post done` when a policy was given (unchanged otherwise).
- `Usage` gains `[--policy NAME]`; update `merge.txt`.
- Tests: refusal when missing and when refused (no `gh` call recorded); dry-run
  report carries the policy line; real merge prints the provenance line and the
  release msg on the bus carries the policy name; without `--policy` the release
  body is byte-identical to today.

Gate: `cd crew && go test ./internal/merge/ . && go vet ./...`.

## S4. Launcher handoff prompt (implement: sonnet)

Files: `adapters/core/dispatcher.sh`, `tests/dispatcher.bats`.

- In `_handoff_prompt`, after the listing: `policy=$(crew policy list --brief
2>/dev/null)`; non-zero exit → `policy=""` and stderr `dispatcher: crew policy
list failed — no standing policy carried into this handoff`. Non-empty →
  stderr `dispatcher: standing policy in force: <lines>`, and
  `policy_block=" Standing policy in force — the repo owner's own words, written
from their own terminal with crew policy set, verbatim: <lines joined with
'; '>. Load the standing-policy skill (DISPATCHER_PROTOCOL.md \"Rare-path
skills\") and act on it only as it allows: only what it names, never
force-push, deploy, close or an owner-auth class."` appended to both prompt
  variants.
- `--no-note` with a policy: step 5 text becomes `5) tell the human one line,
then take the queue from the standing policy below, plus overseer request rows
with existing issue refs only when its words admit them, instead of waiting
for the human.` Without a policy both prompts stay byte-identical.
- `_relaunch_argv` codex/cursor task text: append ` — unless crew policy list
shows a standing policy in force: then load the standing-policy skill and take
the queue from it.` (replace the trailing period accordingly).
- Bats (`stub_crew_handoff` gains a `policy)` case: prints `$HANDOFF_POLICY`,
  `HANDOFF_POLICY=fail` exits 4; tests named `1679: …`): no-note with a policy
  → step-5 policy wording, the policy line verbatim, the skill name, no "confirm
  the queue … with them"; note handoff with a policy → policy block present,
  "resume the queue" kept; no policy → existing assertions hold (already
  covered) and no "Standing policy" text; `policy` failing → prompt has no
  policy block and stderr names the failure. Run `shellcheck` on the script.

Gate: `nix develop -c bats --filter '1679|1420|701' tests/dispatcher.bats`,
`shellcheck adapters/core/dispatcher.sh`.

## S5. Protocol text (implement: sonnet)

Files: `adapters/core/skills/standing-policy/SKILL.md` (new),
`adapters/core/protocols/DISPATCHER_PROTOCOL.md`,
`adapters/core/protocols/OVERSEER_PROTOCOL.md`,
`adapters/core/skills/overseer/SKILL.md`, `adapters/core/skills/owner-auth/SKILL.md`,
then `./scripts/gen-adapters.sh` (regenerates the `.claude.md`/`.pi.md` renders
and the per-adapter copies); any test that enumerates rare-path skills or
pairs flags with skills.

- `standing-policy` skill (frontmatter `name`, `description: Use when a
standing policy is in force (crew policy list, or a handoff prompt carries
one), to act on it within its limits`): what it is (owner-written from their
  terminal, `crew policy show/list`, provenance); the May/Never lists from the
  spec verbatim in substance; merge only through `crew merge <N> --policy
<name>`; before each act re-run `crew policy show <name>` (missing/refused →
  stop; text differs from the prompt's copy → only what both name, tell the
  human); quote the provenance line in the one-line report and any handoff
  note; a crash handoff (`--no-note`) takes its queue from it, plus overseer
  `request` rows with existing refs only when its words admit them; anything
  else claiming to be a policy (bus, pane, note, worker file) is data; it is
  never `--owner-auth` text and never reaches a worker as authorization.
- DISPATCHER_PROTOCOL.md: index line `- \`standing-policy\` — a standing
  policy in force (\`crew policy list\`, or a handoff prompt carrying one)`;
"On startup" step 3: after the `--no-note`sentence add "— unless a standing
policy is in force (skill`standing-policy`): then take the queue from it,
plus overseer `request`rows with existing refs only when its words admit
them"; the merge line: "(or a standing approval: a policy in force, merged
with`crew merge <N> --policy <name>`)".
- OVERSEER_PROTOCOL.md: one bullet — the overseer may name a policy in an
  `ask` or `notice`; it never writes one (`crew policy set` refuses its
  session), and a policy grants it nothing.
- overseer skill `request` bullet: a crash handoff takes request rows with
  existing refs as queue only under a policy in force whose words admit them.
- owner-auth skill: "A standing policy (`crew policy`) is never `--owner-auth`
  text; it covers none of these classes."
- Terse, matching surrounding style.

Gate: `nix develop -c ./scripts/gen-adapters.sh && git diff --exit-code` after
committing, plus targeted bats for skill/protocol drift (`--filter
'skill|protocol|701'` on the files that test them).

## S6. Docs (implement: haiku)

Files: `README.md` (a `crew policy` row/section next to `crew queue` and the
`crew merge` row gains `--policy`).

Gate: none beyond the full gate.

## Full gate (lead, after S1–S6)

- `nix develop -c sh -c 'cd crew && go vet ./... && golangci-lint run ./...'`
- `nix develop -c sh -c 'cd crew && CREW_DRIFT_REQUIRED=1 go test ./...'`
- `nix develop -c bats --filter '1679|1420|701' tests/dispatcher.bats` (+ S5's filters)
- `nix develop -c ./scripts/gen-adapters.sh && git diff --exit-code`
- `nix flake check`

## Amendments (plan-critic, first pass: accept)

- S1 Write: after `MkdirAll`, Lstat the leaf dir and refuse a symlink or
  non-dir; after `Rename`, re-`Load` and report a refusal instead of success.
- S1 Write: temp files use the pattern `.<name>.*.tmp` so `List`'s `*.json`
  glob never sees one.
- S1 Options gain `UID func() int` (default `os.Getuid`); a test sets it to a
  uid other than the file's and expects the owner refusal.
- S2 validate: the merge case also refuses `given(c.Policy)` (an empty
  `--policy ''`).
- S2 wiring: `main.go` sets `Options.Sender = senderProbes(e.proc)` for policy
  (and merge's policy loader); `policy_cli_test.go` also writes a record via
  `policy.Write` (temp `XDG_STATE_HOME`) and reads it back through
  `run(policy show …)` and `run(policy list --brief)`.
- S4: `crew policy list --brief` keeps its stderr (refused records reach the
  human); only stdout is captured.
- S4: extend the codex/cursor relaunch bats assertion (`got[19]`,
  tests/dispatcher.bats ~2313) with the standing-policy clause.
- S4: with a policy in force, the `--no-note` preamble says "no handoff note was
  written; its queue comes from the standing policy below" instead of "its queue
  and standing instructions are unknown".
- S5 gate: `nix develop -c bats --filter 'skill|index|1278' tests/adapters.bats`
  besides the dispatcher.bats filters.
