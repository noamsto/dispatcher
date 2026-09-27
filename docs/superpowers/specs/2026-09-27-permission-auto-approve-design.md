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
session tree has **exactly one** pending tool call, the frame's request block
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

The frame gate recognises a plain dialog by an allowlisted **shape**, not by
denylisting the classifier's wording: between the header and
`Do you want to proceed?` only the request block may appear. Any other line —
the `│` reason box, a hook's reason text, anything unforeseen — goes to the
human.

## Honest expected yield

Every allowlisted root is already an `--add-dir` working dir, where Claude
reads without a dialog, and auto mode routes everything else through the
classifier. So for dispatched workers the positive path is expected to fire
rarely, perhaps never, until a plain-dialog capture shows otherwise. The value
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
| Everything the checker refuses                                     | human                | as today                                                                               |

The dispatcher never overrides a refusal, never approves on its own reading of
a frame, and never answers "don't ask again" (option 2) or "No".

## The checker

`adapters/core/permission-check.sh`, bash + jq, `set -euo pipefail`,
`LC_ALL=C`, fail-closed: any unexpected condition, parse failure or missing
tool exits non-zero.

```
permission-check.sh --pane <%id> --branch <branch> --worktree <dir> \
  [--crew-dir <dir>] [--answer]
permission-check.sh --capture <file> --branch <branch> --worktree <dir> \
  [--crew-dir <dir>]            # fixtures/tests; never answers
```

- `--crew-dir` defaults to the crew dir `crew` resolves; the lead record, the
  artifacts dir and the grants file come from it.
- Immutable read-only roots: `$DISPATCHER_PROTOCOL_DIR`-style dirs are not
  trusted from the environment; the checker takes the protocol, skills,
  reviewers and critics dirs from its own install location (the dirs beside
  `protocols/`), and treats a root as immutable only when its canonical path
  is under `/nix/store/`.
- Transcript base: `$HOME/.claude/projects`; `PERMISSION_CHECK_PROJECTS_DIR`
  overrides it for tests only.
- Output: exactly one line on stdout. `allow-once` with exit 0; otherwise
  `human: <reason>` with exit 1. Usage errors exit 2 (also human).
- `--answer` (with `--pane` only): after an allow verdict, capture again, require
  the frame byte-identical to the one decided on, then `tmux send-keys -t <pane> 1`.
  `1` selects option 1 (`Yes`) directly, whatever the cursor row.

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
2. The project dir is `<projects>/<slug>` with `<slug>` = the worktree's path
   with every `/` and `.` replaced by `-`.
3. Files: `<sid>.jsonl` and `<sid>/subagents/agent-*.jsonl`. A pending call is
   a `tool_use` block whose `id` has no `tool_result` in the same file. Exactly
   one pending call across all files, else human.
4. The pending call is in a subagent file whose `agent-<id>.meta.json`
   `agentType` equals the header `<name>`; its `name` is `Bash`; its `input`
   keys are exactly `command` plus optional `description`; if the entry carries
   `wireToolInputs[<id>]`, it equals `input`.
5. `command` is printable ASCII (0x20–0x7E) only, no newline, CR, tab, ESC or
   non-ASCII. `description` (if present) likewise.
6. The request block equals `[command]` (no description) or
   `[command, description]`, compared trimmed, byte for byte.

### Command grammar

Tokenised by the checker's own lexer; anything it cannot lex → human.

- **Characters.** Outside single quotes only `[A-Za-z0-9_./,:=@%+-]`, spaces,
  and the separators below. So no `$`, backtick, `(`, `)`, `{`, `}`, `<`, `>`,
  `*`, `?`, `[`, `]`, `~`, `!`, `#`, `"`, `\`, `&` (except in `&&`), `|` (except
  as a pipe), newline. Single-quoted strings (`'…'`, no `'` inside) are literal
  and are joined to adjacent word text, as bash does.
- **Separators.** `&&`, `;`, `|` between simple commands. `||`, `&`, `|&` →
  human. An empty simple command (leading, trailing or doubled separator) →
  human.
- **Command word.** The first word of every simple command is exactly one of
  `cd cat head tail wc grep rg ls`, unquoted. No `VAR=value` prefix (a first
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
  given). An operand that starts with `-` before `--` is a flag and must be
  allowlisted.
- **Pipelines.** The first stage must name at least one file operand (rg/grep
  with none would search the cwd). A later stage (`head tail wc grep rg`)
  takes no file operand — it reads the pipe. `cat`, `ls` and `cd` cannot be a
  later stage.

### Path rules

- A relative operand is allowed only after a leading `cd <abs> &&`, resolved
  against that directory. With no leading `cd` the Bash tool's persistent cwd
  is unknown, so relative operands → human.
- Resolution is lexical first: any `.` or `..` component, or a `//`, → human.
- The lexical path must lie under an allowed root as spelled or as
  canonicalised. Roots: the worktree, the branch's artifacts dir, each grant in
  `<crew_dir>/grants/<branch>` re-validated as `dispatch`'s `_add_dir_ok` does,
  and the immutable protocol/skills/reviewers/critics dirs. A grant or root
  that is missing, a symlink, or fails validation is dropped. `WORKER_TASK.md`
  `add_dir:` lines are never read.
- Every component below the root is checked with `lstat`: none may be a
  symlink. `realpath -e` of the path must equal the canonical root + the
  remainder.
- File operands (cat/head/tail/wc/grep/rg) must be regular files. `cd`'s operand
  and `ls`'s operands must be regular files or directories.
- **Directory walks** — `grep -r`, or a directory operand to `grep`/`rg` — are
  allowed only under an immutable root. Under a worker-writable root → human.
- **No secrets.** Any path whose basename matches `.env*`, `*.pem`, `*.key`,
  `id_*`, `*credentials*`, `*.netrc`, `*secret*`, or which lies under a
  dir named `.ssh`, `.gnupg`, `.aws`, `.kube`, `.docker`, `.password-store`,
  `keyrings` → human, even under a grant.

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
frame; a top-level frame; an unknown tool.

Positive cases: `cat /wt/README.md`; `rg -n foo /wt/README.md`;
`cd /wt && cat docs/x.md`; `cat <grant>/notes.md`; `head -n 5 /wt/a | wc -l`;
`grep -r -n foo <nix-store protocol dir>` — each with a plain frame and a single
matching pending call → `allow-once`.

## Residual risks (stated, not closed)

- **Rename race.** Between the check and the command running, a process the
  worker already runs can rename a symlink over a checked file. The pre-check
  cannot close that; it only refuses a symlink present at decision time.
  Exploiting it needs a concurrently running worker-controlled process as the
  same user, under the same sandbox as the approved command — which can read the
  target itself. The approval adds no capability that process lacks.
- **Environment.** The checker cannot see the worker's Bash environment. A
  `PATH` entry, `BASH_ENV` or `RIPGREP_CONFIG_PATH` planted through a
  worker-edited, worker-`direnv allow`ed `.envrc` on a resume already turns
  every classifier-allowed `cat`/`rg` into worker code with no dialog at all.
  Same argument: no capability gain. The human's own shell aliases/functions
  are trusted configuration.
- **Transcript forgery.** Needs a worker process writing under
  `~/.claude/projects` _and_ a rendering trick from A — prior code execution
  again.
- **Keystroke race.** `--answer` re-captures and compares immediately before
  `send-keys`; a different dialog can only replace this one if something else
  answers it in that window.

## Protocol edits

- `DISPATCHER_PROTOCOL.md`, the `prompt:` bullet: replace "Tool-permission
  dialogs go to the human … a follow-up issue tracks a dispatcher approval
  policy" with the policy: run
  `permission-check.sh --pane <%id> --branch <b> --worktree <wt> --answer`;
  on `allow-once` it has answered; on `human: …` relay the captured request and
  the refusal reason to the human exactly as today. Never answer otherwise,
  never option 2 or 3, never from the detail text. Classifier escalations
  always go to the human. Keep the verify-then-act step 2 and the manual
  pane-injection bullet consistent with it.
- A short "why" pointer to this spec for the counterexamples and residual risks.
- `bash scripts/gen-adapters.sh` to regenerate adapter copies; the checker ships
  beside the other core scripts wherever they are packaged.

## Out of scope

- Top-level dialogs (#486), Read/Grep/Glob tool dialogs (no captures): human.
- `find`, `diff`, `ls -R`: dropped from the draft's allowlist, human.
- Role panes: no lead record for them → human.
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
