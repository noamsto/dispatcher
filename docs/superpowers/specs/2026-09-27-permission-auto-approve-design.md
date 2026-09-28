# Dispatcher auto-approve policy for Claude permission dialogs (#441)

## Problem

#413 ships with every Claude tool-permission dialog going to the human. #485
(#435) made `crew stall-watch` recognise the dialog and post
`prompt: permission — <tool>: <request> — pane <%id>`. #441 asks for a policy
under which the dispatcher answers **allow once** by itself, only for requests
that are provably read-only and confined to protocol-mandated or granted dirs,
and sends everything else to the human as today. It must close the round-2
findings of #413 (chained `cd`, rg `-r`/`-E`, `VAR=` prefixes, globs in
worker-writable dirs, bare `cd`/`cd -`, the symlink-swap race) and compose with
#454/#457 (permission denials await in-band; `--owner-auth`).

## Threat model

The worker is the adversary: a prompt-injected model, or one that has already
got a process of its own running. It controls:

- every byte of the tool call it emits (command, description, extra keys);
- every file under its worktree, `<crew_dir>/artifacts/<branch>`, and every
  grant — all `--add-dir` working dirs, read-write in effect, symlinks included;
- the transcript lines above the dialog in its pane.

Pane-scraped text is attacker-influenceable. The dispatcher is an LLM; any
rule it has to apply by reading that text is a rule the worker can argue with.

## What a real dialog looks like

One capture exists (`435-capture/permission-dialog.txt`, Claude Code 2.1.283,
subagent-raised):

```
 Bash command · from the shell-reviewer agent

   bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30
   Run bats tests matching grant filter in dispatch-resume.bats

 │ Auto mode classifier requires confirmation for this command.
 │ …
 │ Latest blocked action: [Irreversible Local Destruction]

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No

 Esc to cancel · Tab to amend
```

It is an **auto-mode classifier escalation**, not a path-permission prompt. No
top-level (non-subagent) capture exists (#486), and no capture of a plain
(non-classifier) dialog exists.

## Design it twice (three times)

### A. Dispatcher parses the pane text (the draft's shape) — rejected as a sole source

The pane cannot establish the exact bytes Claude will run. Concrete
counterexamples, each reasoned from how a terminal and Ink render, none
verified live (the re-dispatch forbids building such payloads):

1. **Two lines, two readings.** A command `cat /wt/README.md` + newline +
   `rm -rf /wt` sent with no `description` renders as two lines — exactly the
   shape of a one-line command followed by its description line.
2. **Word wrap.** Ink wraps a long command at a word boundary, so
   `cat /wt/README.md ;rm -rf /wt/<long word>` can render as a first line
   `cat /wt/README.md` and a second line that reads as the description.
3. **Overwrite.** If a carriage return or cursor-motion sequence in the command
   reaches the terminal, `rm -rf /wt<CR>cat /wt/README.md` shows only the text
   written last. A capture holds final cell contents, so the stripping
   `_permission_detail` does cannot see what was overwritten. Whether Claude
   Code strips these bytes before rendering is unverified.
4. **Extra tool-input keys are invisible.** `dangerouslyDisableSandbox`,
   `run_in_background` or `timeout` never appear in the frame.
5. **Truncation.** `_permission_detail` keeps the first request line, capped at
   160 characters; it is a relay hint, never the request.

So pane text alone can never be grounds for approval. This part of the answer is
the negative result the task allows for, and it stays in the policy as a rule:
the relayed `prompt:` detail and the pane text are never parsed for approval.

### B. Bind the dialog to the session transcript — chosen

Claude Code writes every tool call, byte-exact, to the session transcript:
`~/.claude/projects/<slug>/<session>.jsonl` for the lead,
`<session>/subagents/agent-<id>.jsonl` (+ `agent-<id>.meta.json`, carrying
`agentType`) for subagents. A pending call is a `tool_use` with no matching
`tool_result`. Observed on this machine: an in-flight call reaches the file
within ~5 s (an async write queue), and a parked dialog is older than that
by the time the dispatcher reads it.

The checker approves only when the pane frame is a **plain** Bash dialog, the
session tree has **exactly one** pending tool call (beside that call's live
parent `Agent` call), the pane runs that session, the frame's request block
equals that call's `command` (and `description`, when one is given) byte for
byte, and the call's exact `command` passes the grammar and path rules below.
The decision is taken from transcript bytes; the pane is only a witness that
the dialog on screen is that call's dialog. Every rendering uncertainty in A
can only make the witness fail to match, which sends the request to the human.

The session id comes from the dispatcher's own record
`<crew_dir>/leads/<branch>` (`claude <sid>`, written by `dispatch`), never from
the pane or the worktree.

### C. A `PermissionRequest` hook inside the worker — rejected

Claude Code's `PermissionRequest` hook receives the exact `tool_input` and can
return `allow` once (no `updatedPermissions`), with no keystroke and no race.
But its input carries no field distinguishing a classifier escalation from a
plain prompt (documented fields: `tool_name`, `tool_input`, `cwd`,
`permission_mode`, `permission_suggestions`, `agent_id`, `agent_type`). Every
dispatched claude worker and role pane runs `--permission-mode auto`, where the
only dialog captured so far is a classifier escalation. A hook that must not
pre-empt the classifier (below) would therefore have to decline every request
in auto mode — dead code for every dispatched session. It also would not be the
dispatcher's decision, and the task asks for the allow-once keystroke.

## Classifier escalations

**Never auto-approve a classifier escalation.** The frame says "Please review
the transcript before continuing": the classifier is asking a human to look.
#457 states the harness "never evades or pre-empts the classifier"; answering
its escalation for it is exactly a pre-emption. Classifier **denials** remain
permission blocks under #457 and are untouched by this policy.

Two independent guards, so neither alone has to be right:

1. **Frame shape.** The gate recognises a plain dialog by an allowlisted
   shape, not by denylisting the classifier's wording: between the header and
   `Do you want to proceed?` only the request block may appear. Any other line
   — the `│` reason box, a hook's reason text, anything unforeseen — goes to
   the human.
2. **Transcript history.** The one captured escalation followed "3 consecutive
   actions were blocked". Each classifier denial is a `tool_result` whose text
   contains `denied by the Claude Code auto mode classifier` (copied from a
   real denial in this session's own transcript, and pinned in a fixture). If
   any of the last five `tool_result`s in
   the pending call's own transcript file is such a denial, the request goes to
   the human even when the frame looks plain.

The plain frame shape is **inferred**: the escalation capture minus its reason
box. A live capture of a plain dialog was attempted with a benign command in a
scratch session on a private tmux server; the auto-mode classifier denied
driving that session (Self-Modification), and the attempt was dropped rather
than worked around. A wrong inference fails closed — no plain frame matches, so
nothing is approved — because the approval is decided from transcript bytes
and the frame is only a witness. The dangerous inference error would be an
escalation that renders with no extra line; guard 2 covers the known trigger.
Validating the frame against a real plain capture is left to #486.

## Honest expected yield

Every allowlisted root is already an `--add-dir` working dir, where Claude
reads without a dialog, and auto mode routes everything else through the
classifier. On the owner's machine `~/.claude/settings.json` also allows
`Bash(cat *)`, `Bash(rg *)`, `Bash(head *)`, `Bash(ls *)` and `Bash(wc *)`
outright, so those never draw a dialog there. So for dispatched workers the
positive path is expected to fire rarely, perhaps never, until a plain-dialog
capture shows otherwise. The value
shipped is a mechanical, tested boundary: the dispatcher no longer has to
judge a relayed request at all, and the one case it may answer is defined by
code, not prose.

## Mechanical vs judgement

| Decision                                                           | Who                  | Why                                                                                    |
| ------------------------------------------------------------------ | -------------------- | -------------------------------------------------------------------------------------- |
| Frame shape (plain Bash dialog, exact option rows, footer last)    | checker              | pattern match on a capture; no judgement needed                                        |
| Which call the dialog is for; its exact bytes                      | checker              | transcript bytes, exactly-one-pending rule                                             |
| Grammar, flags, separators, quoting                                | checker              | a closed allowlist, table-tested                                                       |
| Path resolution, roots, symlinks, file types, secrets              | checker              | filesystem checks at decision time                                                     |
| Keystroke and when to send it                                      | checker (`--answer`) | re-captures and compares, then sends `1` itself, shrinking the race to one `tmux` call |
| Whether to run the checker at all; relaying a refusal to the human | dispatcher           | routing only, no content judgement                                                     |
| `--pane`, `--branch`, and running from the worker's repo           | dispatcher           | copied from its own `dispatch` event; the checker derives the worktree and crew dir    |
| Everything the checker refuses                                     | human                | as today                                                                               |

The dispatcher never overrides a refusal, never approves on its own reading of
a frame, and never answers "don't ask again" (option 2) or "No".

## The checker

`adapters/core/permission-check.sh`, bash + jq, `set -euo pipefail`,
`LC_ALL=C`, fail-closed: any unexpected condition, parse failure or missing
tool exits non-zero.

```
permission-check.sh --pane <%id> --branch <branch> [--crew-dir <dir>] [--answer]
                                # run from the worker's repo
permission-check.sh --capture <file> --branch <branch> --worktree <dir> \
  [--crew-dir <dir>] [--projects-dir <dir>] [--ro-root <dir>]...
                                # fixtures/tests; never answers
```

- `--pane` must be a literal `%<n>` pane id — a relative tmux target (`{last}`,
  `!`, `sess:`) is re-resolved by each tmux call, so the session check, the
  frame and the keystroke could hit different panes.
- `--crew-dir` defaults to `<git common dir>/crew` of the repo the checker
  runs in; the lead record, the artifacts dir and the grants file come from it.
- The worktree: in `--pane` mode the checker takes it from
  `git worktree list --porcelain` in that repo — the entry whose
  `branch refs/heads/<branch>` matches, else human — and refuses `--worktree`.
  `--worktree` exists only in `--capture` mode, where there is no live repo.
- Immutable read-only roots: `$DISPATCHER_PROTOCOL_DIR`-style dirs are not
  trusted from the environment; the checker takes the protocol, skills,
  reviewers and critics dirs from its own install location (the dirs beside
  `protocols/`), and treats a root as immutable only when its canonical path
  is under `/nix/store/`.
- Transcript base: `$HOME/.claude/projects`. `--projects-dir <dir>` overrides
  it, accepted in `--capture` mode only.
- Output: exactly one line on stdout. `allow-once` with exit 0; otherwise
  `human: <reason>` with exit 1. Usage errors exit 2 (also human).
- **Settle.** The decision takes two observations: capture the frame and scan
  the transcripts, wait 10 seconds in `--pane` mode (twice the ~5 s flush
  observed; `PERMISSION_CHECK_SETTLE` can raise it, never lower it), then
  capture and scan again. `--capture` mode re-reads its file on every
  observation and settles `PERMISSION_CHECK_SETTLE` seconds (default 0), so
  a test can change the frame or the transcript during the wait. Both
  observations must see a byte-identical frame and the same single pending
  call id; any difference → human. A live call not yet flushed at the first
  scan shows up at the second as a second pending call.
- `--answer` (with `--pane` only): after an allow verdict, capture a third time,
  require it byte-identical to the decided frame, then
  `tmux send-keys -t <pane> 1`. `1` selects option 1 (`Yes`) directly, whatever
  the cursor row. It never sends anything else.
- `--branch` comes from the dispatcher's own `dispatch` event on the bus — not
  a `crew roster` row, whose `branch` comes from the worker's self-asserted
  status, and never from the pane, the `prompt:` detail or the worker's
  `WORKER_TASK.md`. The dispatcher runs the checker from the repo it
  dispatched the worker into.

### Frame gate (on the plain capture, stripped as `_permission_detail` strips)

1. The last non-empty line is exactly `Esc to cancel · Tab to amend` (trimmed).
2. Walking up over non-empty lines: `3. No`; a line starting
   `2. Yes, and don’t ask again for:`; exactly `❯ 1. Yes`;
   exactly `Do you want to proceed?`.
3. Above that, up to the header: one or two non-empty lines (the request
   block), and nothing else.
4. The header is exactly `Bash command · from the <name> agent` with `<name>`
   matching `[A-Za-z0-9:_-]+`. A top-level header (#486) or any other tool →
   human.

### Transcript binding

1. `<crew_dir>/leads/<branch>` is a regular, non-symlink file reading
   `claude <uuid>`; else human.
2. The project dir is `<projects>/<slug>` with `<slug>` = the canonical (`realpath`) worktree path
   with every character outside `[A-Za-z0-9]` replaced by `-`.
3. Files: `<sid>.jsonl` (the lead file) and `<sid>/subagents/agent-*.jsonl`.
   A pending call is a `tool_use` block whose `id` has no `tool_result` in the
   same file. Any line that is not a JSON object → human. Across the subagent
   files exactly one call is pending, and it is the last `tool_use` in its
   file. The lead file's pending set must be exactly that subagent's live
   parent `Agent` call — the call whose `id` equals the subagent's `meta.json`
   `toolUseId`, pending for as long as a foreground subagent runs — and that
   call must be the last `tool_use` in the lead file. Only `spawnDepth: 1`
   subagents are eligible; a deeper one leaves its ancestor's call pending in a
   subagent file, so it refuses. A lead parked in its own foreground call (e.g.
   `crew await`) has a second pending call in the lead file, and refuses. An
   orphan — a call a killed subagent or an interrupted run left without a
   `tool_result` — is likewise a second pending call (in its subagent file, or
   in the lead file beside the live parent), so every later request in that
   session refuses for as long as it stays unmatched: the safe direction.
4. The pending call is in a subagent file whose `agent-<id>.meta.json`
   `agentType` equals the header `<name>`; its `name` is `Bash`; its `input`
   keys are exactly `command` plus optional `description`; if the entry carries
   `wireToolInputs[<id>]`, it equals `input`.
5. `command` is printable ASCII (0x20–0x7E) only, no newline, CR, tab, ESC or
   non-ASCII. `description` (if present) likewise.
6. The request block equals `[command]` (no description) or
   `[command, description]`, compared trimmed, byte for byte.
7. No classifier denial among the last five `tool_result`s of that file
   (see Classifier escalations).

### Pane binding

The transcript says which call is pending; only the pane says whose dialog is
on screen. In `--pane` mode the pane's process tree must run `claude` with
`--session-id <sid>` or `--resume <sid>`, `<sid>` being the lead record's
uuid — every `claude` descendant of the pane, matched token-exact on its argv,
else human. It is checked in each observation. Without it, a dialog in another
pane (a role pane, a second session) whose request block equals the lead's
pending command would be answered. A role pane runs its own session, so it
refuses here.

### Command grammar

Tokenised by the checker's own lexer; anything it cannot lex → human.

- **Characters.** Outside single quotes only `[A-Za-z0-9_./,:=@%+-]`, spaces,
  and the separators below. So no `$`, backtick, `(`, `)`, `{`, `}`, `<`, `>`,
  `*`, `?`, `[`, `]`, `~`, `!`, `#`, `"`, `\`, `&` (except in `&&`), `|` (except
  as a pipe), newline. Single-quoted strings (`'…'`, no `'` inside) are literal
  and are joined to adjacent word text, as bash does. Every rule below runs on
  the **joined, de-quoted** word, so `rg '-r' x f` and `rg -'-pre=x' p f` are
  flags `-r` and `--pre=x`, and refused.
- **Separators.** `&&`, `;`, `|` between simple commands. `||`, `&`, `|&` →
  human. An empty simple command (leading, trailing or doubled separator) →
  human.
- **Command word.** The first word of every simple command is exactly one of
  `cd cat head tail wc grep rg ls`, written with no quote character at all
  (`c'at' f` → human). No `VAR=value` prefix (a first
  word containing `=` is not a command name), no path (`/bin/cat`), no builtin
  or wrapper (`command`, `env`, `xargs`, `eval`, `exec`, `sudo`).
- **`cd`.** Only as the first simple command, followed by `&&`, with exactly
  one absolute operand that passes the path rules as a directory. A later `cd`,
  a bare `cd`, `cd -`, `cd ~`, `cd` followed by `;` or `|` → human.
- **Flags** (each its own word; no bundling like `-ni`, no `-A3`, no
  `--flag=value`; a numeric argument is `[0-9]+`):

  | command        | allowed                                                                                                |
  | -------------- | ------------------------------------------------------------------------------------------------------ |
  | `cat`          | none                                                                                                   |
  | `head`, `tail` | `-n N`                                                                                                 |
  | `wc`           | `-l -c -w`                                                                                             |
  | `grep`         | `-n -i -l -L -c -w -F -E -H -h -s`, `-e PAT`, `-A N -B N -C N -m N`, `-r`, `--`                        |
  | `rg`           | `-n -i -l -c -w -F -H -s`, `--no-heading`, `-e PAT`, `-A N -B N -C N -m N`, `-g GLOB`, `-t TYPE`, `--` |
  | `ls`           | `-l -a -1 -d -h`                                                                                       |

  rg's `-L` (follow), `-r` (replace), `-E` (encoding), `-z`, `--pre`,
  `--hostname-bin` and every unlisted flag are refused by omission.

- **Operands.** After `--`, or once a non-flag word has been seen, every word
  is an operand (for grep/rg the first operand is the pattern unless `-e` was
  given, and a pattern is required). Before `--`, a word starting with `-` is a
  flag and must be allowlisted; after an operand but before `--`, such a word
  is refused outright, because GNU getopt and rg still read it as a flag. `-`
  (stdin) is never an operand. `-g`/`-t` values may not start with `-`.
- **Pipelines.** The first stage must name at least one file operand (rg/grep
  with none would search the cwd). A later stage (`head tail wc grep rg`)
  takes no file operand — it reads the pipe. `cat`, `ls` and `cd` cannot be a
  later stage, and `grep -r` cannot be one (with no file operand it walks the
  cwd). A leading `cd` bars `;` anywhere in the command: if the `cd` failed,
  a command after `;` would resolve relative paths against an unknown cwd.

### Path rules

- A relative operand is allowed only after a leading `cd <abs> &&`, resolved
  against that directory. Claude's Bash tool runs each command as
  `eval '<command>'` after sourcing a shell snapshot, then records `pwd -P` as
  the next call's cwd (seen in this machine's process list), so without a
  leading `cd` the cwd is whatever an earlier call left: unknown to the
  checker, and relative operands → human.
- Resolution is lexical first: any `.` or `..` component, a `//`, a trailing
  `/`, or `/` itself → human.
- The lexical path must lie under an allowed root as spelled or as
  canonicalised. Roots: the worktree, the branch's artifacts dir, each grant in
  `<crew_dir>/grants/<branch>` re-validated as `dispatch`'s `_add_dir_ok` does,
  and the immutable protocol/skills/reviewers/critics dirs. A grant or root
  that is missing, a symlink, or fails validation is dropped. The validator is
  that same `_add_dir_ok`, from the shared lib `adapters/core/grant-check.sh`,
  run only for grants the command's operands touch. `WORKER_TASK.md`
  `add_dir:` lines are never read. The worktree root itself is dropped if it
  is a symlink, `/`, or `$HOME` or an ancestor of it.
- Every component below the root is checked with `lstat`: none may be a
  symlink. `realpath -e` of the path must equal the canonical root + the
  remainder.
- File operands (cat/head/tail/wc/grep/rg) must be regular files. `cd`'s operand
  and `ls`'s operands must be regular files or directories.
- **Hard links.** Under a worker-writable root, a file with a link count above
  one → human: a hard link from the worktree to a file outside the roots would
  otherwise pass every path rule. Immutable roots are exempt, since the Nix
  store hard-links identical files.
- **Directory walks** — `grep -r` (even on a single file), or a directory
  operand to `grep`/`rg` — are
  allowed only under an immutable root. Under a worker-writable root → human.
- **No secrets.** Any path whose basename matches `.env*`, `*.pem`, `*.key`,
  `id_*`, `*credentials*`, `*.netrc`, `*secret*`, or which lies under a
  dir named `.ssh`, `.gnupg`, `.aws`, `.kube`, `.docker`, `.password-store`,
  `keyrings` → human, even under a grant. Names match case-blind (macOS
  filesystems are case-insensitive) and are checked on both the spelled and
  the resolved path.

## Finding → rule → test

| #441 finding                                                                 | Closing rule                                                             | Refusing case                                               |
| ---------------------------------------------------------------------------- | ------------------------------------------------------------------------ | ----------------------------------------------------------- |
| HIGH chained `cd`                                                            | `cd` only as the first command, absolute, `&&`; any `..` refused         | `cd /wt/a && cd /wt/b && cat ../../x`; `cd /wt && cat ../x` |
| rg `-r`/`-E` take arguments                                                  | rg allowlist omits both                                                  | `rg -E utf8 notes.md`; `rg -r x pat f`                      |
| `VAR=` prefix                                                                | command word must be an allowlisted name                                 | `RIPGREP_CONFIG_PATH=./r rg x f`                            |
| glob expands into a flag                                                     | no unquoted glob characters at all                                       | `cat /wt/*.md`; `rg x /wt/?`                                |
| bare `cd`, `cd -`                                                            | `cd` needs one absolute operand                                          | `cd && cat x`; `cd - && cat x`; `cd ~ && cat x`             |
| symlink swap                                                                 | no symlink component below a root; regular files only; see residual risk | `/wt/link → README` refused even while benign               |
| kept from round 1: `rg --hostname-bin`, `-z`, `find -follow`, `diff` on dirs | allowlist omission (`find`, `diff` dropped)                              | `rg -z x f`; `find /wt`; `diff a b`                         |
| kept: secrets under a grant                                                  | secrets rule                                                             | `cat <grant>/.env`                                          |

Pane-fidelity refusals: a two-line block whose second line is not the
transcript's description; a block that is not byte-equal to the pending
command; any command with CR/newline/ESC/non-ASCII; two pending calls; extra
input keys (`dangerouslyDisableSandbox`, `run_in_background`); a classifier
frame; a top-level frame; an unknown tool; a plain frame whose file shows a
recent classifier denial; a frame or pending id that changed across the settle
interval; an orphan pending call beside the live one; a lead-file pending call
other than the live parent `Agent` call; a pane whose `claude` is not running
the lead session.

Quoting refusals: `rg '-r' x f`; `rg -'-pre=x' p f`; `c'at' f`.

Positive cases: `cat /wt/README.md`; `rg -n foo /wt/README.md`;
`cd /wt && cat docs/x.md`; `cat <grant>/notes.md`; `head -n 5 /wt/a | wc -l`;
`grep -r -n foo <nix-store protocol dir>` — each with a plain frame and a single
matching pending call → `allow-once`.

## Residual risks (stated, not closed)

The owner's machine runs Claude with `sandbox.enabled: false`, so the risks
below are stated for the unsandboxed case.

- **Rename race.** Between the check and the command running, a symlink can
  be renamed over a checked file. The pre-check cannot close that; it only
  refuses a symlink present at decision time. The swap needs no long-lived
  process: a parallel subagent's classifier-allowed call — a `git checkout`
  of a committed symlink, an `mv` — can make it. The approval still adds no
  capability: a classifier-allowed `cat /wt/<link>` already reads through a
  symlink the classifier cannot see.
- **Environment.** The checker cannot see the worker's Bash environment. A
  `PATH` entry, `BASH_ENV` or `RIPGREP_CONFIG_PATH` planted through a
  worker-edited, worker-`direnv allow`ed `.envrc` on a resume already turns
  every allowed `cat`/`rg` into worker code with no dialog at all. Same
  argument: no capability gain. The human's own shell snapshot (aliases,
  functions) is trusted configuration.
- **Transcripts are a trust root that only the classifier protects.** A
  worker needs no running process to forge one: a single classifier-allowed
  append under `~/.claude/projects` from a parallel subagent can plant a fake
  pending call and a fake `tool_result` for the real one. Combined with a
  rendering trick from A, the checker would then approve a command it never
  saw. The settle rule, last-`tool_use` rule and `agentType` match raise the
  bar but do not close it. The policy's guarantee is conditional: **if the
  session's transcript files are authentic, the approved bytes are the bytes
  that run.**
- **Pane binding trusts the process table.** The pane↔session check reads
  argv from the process table; a same-user process named `claude` can carry
  any argv, including the lead's `--session-id`. Planting one presupposes
  worker code execution, which already has every capability an approval
  grants.
- **Keystroke race.** `--answer` re-captures and compares immediately before
  `send-keys`; a different dialog can only replace this one if something else
  answers it in that window.
- **The `--session-id`/`--resume` token check shows how the process was
  launched, not which session is live.** A human-run `--fork-session` or an
  in-TUI `/clear`/`/resume` would leave argv naming the old session while a
  new one runs; no dispatcher launch path does this, and recovering from it
  needs human action.
- **Git worktree metadata is worker-writable.** `<common>/worktrees/<id>/gitdir`
  can be edited from inside the worktree, so the derived worktree path is not
  itself trusted. What pins it is the transcript lookup — `<projects>/<slug>`
  with `<slug>` derived from the _canonical_ worktree path must hold the
  `<sid>.jsonl` for the lead's own session — plus the symlink and hard-link
  path rules. Anyone reusing the derived path elsewhere must keep that pin.

## Protocol edits

- `DISPATCHER_PROTOCOL.md`, the `prompt:` bullet: replace "Tool-permission
  dialogs go to the human … a follow-up issue tracks a dispatcher approval
  policy" with the policy: run
  `permission-check.sh --pane <%id> --branch <b> --answer` from the worker's repo;
  on `allow-once` it has answered; on `human: …` relay the captured request and
  the refusal reason to the human exactly as today. Never answer otherwise,
  never option 2 or 3, never from the detail text. Classifier escalations
  always go to the human. Keep the verify-then-act step 2 and the manual
  pane-injection bullet consistent with it.
- _Permission blocks_' "Never deliver an authorization by pane injection":
  carve out the checker's `--answer` keystroke. It answers a dialog the checker
  verified; it never carries an authorization for a denied action, and a
  permission block is still never answered in the pane.
- Name `--branch`'s source: the dispatcher's own `dispatch` event — not a
  `crew roster` row (self-asserted by the worker), never the pane, the detail
  or the worktree. The checker derives the worktree and crew dir itself.
- A short "why" pointer to this spec for the counterexamples and residual risks.
- `bash scripts/gen-adapters.sh` to regenerate adapter copies; the checker ships
  beside the other core scripts wherever they are packaged.

## Out of scope

- Top-level dialogs (#486), Read/Grep/Glob tool dialogs (no captures): human.
- `find`, `diff`, `ls -R`: dropped from the draft's allowlist, human.
- Role panes: the dispatcher sends their dialogs straight to the human; the
  checker would refuse one anyway, since the pane is not running the lead
  session (Pane binding).
- codex/cursor/pi: no permission dialog recognised.

## Acceptance

- Every #441 finding has a refusing bats case; the draft's round-1 fixes keep
  theirs.
- Positive: plain `cat`/`rg` of a file in the worktree or a granted dir →
  `allow-once`.
- Classifier escalations, writes, a pipe to a non-allowlisted command, an
  unresolvable path → `human:`.
- `nix flake check` green.
- Security-reviewer pass (escalated model) on the final policy text.
