setup() {
  load helpers
  ROOT="$BATS_TEST_DIRNAME/.."
}

@test "generator is idempotent" {
  # Compare checksums across two runs rather than `git diff --exit-code`: that
  # conflates generator drift with any unrelated uncommitted edit, and is
  # vacuous while the generated files are still untracked. CI's separate
  # "adapters are in sync" step is what catches committed output drifting from
  # its source, which is the right place for a git-based check (clean checkout).
  gen_paths=(
    adapters/claude-code/plugin/commands
    adapters/cursor/commands
    adapters/codex/plugin/skills
    adapters/claude-code/plugin/scripts
    adapters/codex/plugin/scripts
    adapters/claude-code/plugin/protocols
    adapters/codex/plugin/protocols
  )
  "$ROOT/scripts/gen-adapters.sh" >/dev/null
  before="$(cd "$ROOT" && find "${gen_paths[@]}" -type f -exec sha256sum {} + | sort)"
  "$ROOT/scripts/gen-adapters.sh" >/dev/null
  after="$(cd "$ROOT" && find "${gen_paths[@]}" -type f -exec sha256sum {} + | sort)"
  [ -n "$before" ]
  [ "$before" = "$after" ]
}

@test "all four commands reach claude-code and cursor" {
  for n in dispatcher autopilot finish-prs project-autopilot; do
    [ -f "$ROOT/adapters/claude-code/plugin/commands/$n.md" ]
    [ -f "$ROOT/adapters/cursor/commands/$n.md" ]
  done
}

@test "all four commands reach codex as skills" {
  for n in dispatcher autopilot finish-prs project-autopilot; do
    [ -f "$ROOT/adapters/codex/plugin/skills/$n/SKILL.md" ]
  done
}

@test "codex skills carry name and description frontmatter" {
  run head -4 "$ROOT/adapters/codex/plugin/skills/autopilot/SKILL.md"
  [[ "$output" == *"name: autopilot"* ]]
  [[ "$output" == *"description:"* ]]
}

@test "every codex skill frontmatter is parseable YAML" {
  # Descriptions routinely contain ": " which is invalid as a bare YAML scalar.
  # An unquoted `description: Autonomous dev workflow: Linear ...` is a hard
  # parse error, so codex would reject the skill outright — assert the
  # generator's quoting rather than trusting it by eye.
  for f in "$ROOT"/adapters/codex/plugin/skills/*/SKILL.md; do
    awk 'NR==1 && /^---$/{inf=1; next} inf && /^---$/{exit} inf' "$f" >"$BATS_TEST_TMPDIR/fm.yaml"
    run yq -e '.name, .description' "$BATS_TEST_TMPDIR/fm.yaml"
    [ "$status" -eq 0 ]
  done
}

@test "codex skill bodies drop the source frontmatter" {
  # The source commands carry an argument-hint key codex skills don't use; if it
  # survives, the body was pasted in with its old frontmatter intact.
  run grep -c 'argument-hint' "$ROOT/adapters/codex/plugin/skills/autopilot/SKILL.md"
  [ "$output" = "0" ]
}

@test "every manifest is valid json" {
  jq -e . "$ROOT/adapters/claude-code/plugin/.claude-plugin/plugin.json"
  jq -e . "$ROOT/adapters/codex/plugin/.codex-plugin/plugin.json"
  jq -e . "$ROOT/adapters/codex/.agents/plugins/marketplace.json"
  jq -e . "$ROOT/adapters/claude-code/plugin/hooks/hooks.json"
  jq -e . "$ROOT/adapters/codex/plugin/hooks/hooks.json"
}

@test "each engine's hooks use that engine's plugin-root variable" {
  run grep -F 'CLAUDE_PLUGIN_ROOT' "$ROOT/adapters/claude-code/plugin/hooks/hooks.json"
  [ "$status" -eq 0 ]
  run grep -F '$PLUGIN_ROOT' "$ROOT/adapters/codex/plugin/hooks/hooks.json"
  [ "$status" -eq 0 ]
  run grep -c 'CLAUDE_PLUGIN_ROOT' "$ROOT/adapters/codex/plugin/hooks/hooks.json"
  [ "$output" = "0" ]
}

@test "the notify hook ships executable in both plugin trees" {
  [ -x "$ROOT/adapters/claude-code/plugin/scripts/dispatch-notify.sh" ]
  [ -x "$ROOT/adapters/codex/plugin/scripts/dispatch-notify.sh" ]
}

@test "the cursor rule sets alwaysApply, else cursor ignores it silently" {
  run head -3 "$ROOT/adapters/cursor/rules/dispatcher.mdc"
  [[ "$output" == *"alwaysApply: true"* ]]
}

@test "protocols ship inside both plugin trees" {
  # The command bodies tell the agent to fall back to a plugin-local protocol
  # when $DISPATCHER_PROTOCOL_DIR is unset. That fallback has to exist, or a
  # non-Nix install has no way to resolve the protocol at all.
  for tree in claude-code codex; do
    [ -f "$ROOT/adapters/$tree/plugin/protocols/DISPATCHER_PROTOCOL.md" ]
    [ -f "$ROOT/adapters/$tree/plugin/protocols/WORKER_PROTOCOL.md" ]
    [ -f "$ROOT/adapters/$tree/plugin/protocols/REVIEW_TASK.md" ]
  done
}

@test "every canonical protocol exactly matches both shipped protocol trees" {
  for source in "$ROOT"/adapters/core/protocols/*.md; do
    name="$(basename "$source")"
    cmp -s "$source" "$ROOT/adapters/claude-code/plugin/protocols/$name"
    cmp -s "$source" "$ROOT/adapters/codex/plugin/protocols/$name"
  done
}

@test "worker protocol defines bounded plan-shaped gate recovery" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    'Before the startup bus drain, initialize `replanned = false` for this run.' \
    'After initialization or any reset, the first qualifying amendment seeds the consecutive count at `1`.' \
    '`A(scope step 2) → B(interface step 4) → A(scope step 2)` reaches `1 → 2 → 3` and transfers control before the third fix.' \
    'The skipped-plan contradiction fallback and plan-shaped recovery share one execute-time budget.' \
    'If the execute ladder has no lower rung, implement at the current worker rung; this never consumes the planning budget.' \
    'A higher planner must be strictly above the authoritative tuple; a top or unavailable rung blocks without launching planning, and `replanned` remains false only when no earlier execute-time planning episode began.' \
    '**Claude:** Agent model override `haiku → sonnet → opus → fable`' \
    '**Codex:** on the exact model, increase `low → medium → high → xhigh → max`' \
    '**Cursor:** Task model override `cursor-grok-4.5-low-fast → cursor-grok-4.5-medium-fast → cursor-grok-4.5-high`.' \
    'Immediately before every stopping path, emit one complete latest-state metrics snapshot.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
  run grep -F 'Every pre-execute snapshot has `replanned: false`.' "$protocol"
  [ "$status" -eq 0 ]
}

@test "worker protocol pins the resume-a-killed-run contract" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    '`tier:`, `kind:`, `resume:`, authoritative `engine:`, `model:`, and `effort:`,' \
    'consult **Resuming a killed run** (below) first; unless resuming, run `spec-plan-critic` with `{ tier:' \
    '`resume: true` is read first and outranks `plan:` — see **Resuming a killed run** below;' \
    '**except under `resume: true`** (see **Resuming a killed run**): a recovered `SPEC.md` is the _output_ of a spec-critic gate in the interrupted run of this same task, not a task doc that never faced one.' \
    'Do **not** re-run the spec or plan phases. Continue from the first unfinished step.' \
    '**Before pushing, check whether this branch already has an open PR** (`gh pr view --json url,state`).' \
    'When a PR is already open, push to it, skip `gh pr create`, and report `crew status "$CREW_WORKER_ID" pr_open "" <existing url>` with that url' \
    'Consult **Resuming a killed run** (above) first; unless resuming, before the plan phase decide **once** whether to bring a top-tier consultant in to decompose the task' \
    '`Plan: recovered (resume)` when you resumed under `resume: true`'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "dispatcher protocol claim bullet pins the resume exemption and adopt release" {
  protocol="$ROOT/adapters/core/protocols/DISPATCHER_PROTOCOL.md"
  for statement in \
    'already there and the branch exists, it resumes that branch and re-adds the label; free, `dispatch` adds it before any scaffolding.' \
    '`crew adopt` on a dead-pid crew releases that crew'"'"'s own recorded claims the same way.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "the generator removes a codex skill whose command is gone" {
  # Only clearing the command dirs would orphan a codex skill on rename/removal
  # forever: the idempotence test reruns with an unchanged source so never
  # exercises removal, and the CI drift gate sees no diff for a stale dir
  # nobody rewrote.
  work="$BATS_TEST_TMPDIR/gen"
  mkdir -p "$work"
  cp -r "$ROOT/adapters" "$ROOT/scripts" "$work/"
  (cd "$work" && ./scripts/gen-adapters.sh >/dev/null)
  [ -d "$work/adapters/codex/plugin/skills/dispatcher" ]
  mv "$work/adapters/core/commands/dispatcher.md" "$work/adapters/core/commands/dispatcher-v2.md"
  (cd "$work" && ./scripts/gen-adapters.sh >/dev/null)
  [ ! -d "$work/adapters/codex/plugin/skills/dispatcher" ]
  [ -d "$work/adapters/codex/plugin/skills/dispatcher-v2" ]
  # spec-plan-critic is written after the clear, so it must survive.
  [ -d "$work/adapters/codex/plugin/skills/spec-plan-critic" ]
}

@test "a description with an embedded quote or backslash round-trips exactly" {
  # Hand-rolled re-escaping of an already-escaped YAML value silently corrupted
  # it (a source \" became a literal backslash). jq owns the escaping and yq -P
  # owns the quoting, so this must survive untouched.
  work="$BATS_TEST_TMPDIR/esc"
  mkdir -p "$work"
  cp -r "$ROOT/adapters" "$ROOT/scripts" "$work/"
  printf -- '---\ndescription: "Say \\"hi\\" to it: C:\\\\p\\\\q end"\n---\n\n# Body\n' \
    >"$work/adapters/core/commands/edgecase.md"
  (cd "$work" && ./scripts/gen-adapters.sh >/dev/null)
  awk 'NR==1 && /^---$/{inf=1; next} inf && /^---$/{exit} inf' \
    "$work/adapters/codex/plugin/skills/edgecase/SKILL.md" >"$work/fm.yaml"
  run yq -r '.description' "$work/fm.yaml"
  [ "$status" -eq 0 ]
  [ "$output" = 'Say "hi" to it: C:\p\q end' ]
}

@test "no command body references a plugin command without its namespace" {
  # All four commands ship namespaced under the dispatcher plugin, so a bare
  # /autopilot or /finish-prs cross-reference resolves to nothing. /loop and
  # /schedule are deliberately excluded — those skills stay outside this plugin.
  #
  # Scans the SHIPPED trees as well as the source, not source alone: the source
  # is what a fix edits, but the generated copies are what each engine actually
  # loads. The drift gate would catch a divergence eventually; asserting on what
  # ships makes this test mean what its name claims.
  run grep -rnoE '/(autopilot|finish-prs|project-autopilot)\b' \
    "$ROOT/adapters/core/commands/" \
    "$ROOT/adapters/claude-code/plugin/commands/" \
    "$ROOT/adapters/codex/plugin/skills/" \
    "$ROOT/adapters/cursor/commands/"
  filtered="$(printf '%s\n' "$output" | grep -v '/dispatcher:' || true)"
  [ -z "$filtered" ]
}

@test "no adapter file hardcodes a path into the old nix-config location" {
  # Narrower than a bare 'nix-config' grep: finish-prs.md legitimately shows
  # `noamsto/nix-config#42` as example report output. What must not survive is a
  # filesystem path pointing back at the pre-extraction home.
  run grep -rn '~/nix-config\|nix-config/home/ai' "$ROOT/adapters/"
  [ "$status" -ne 0 ]
}

@test "project-autopilot points teammates at the namespaced autopilot" {
  # The two load-bearing ones: the lead tells each teammate what to run, so a
  # bare /autopilot here resolves to nothing and the fan-out silently stalls.
  # Asserted on every shipped copy, not just the source.
  for f in \
    "$ROOT/adapters/core/commands/project-autopilot.md" \
    "$ROOT/adapters/claude-code/plugin/commands/project-autopilot.md" \
    "$ROOT/adapters/codex/plugin/skills/project-autopilot/SKILL.md" \
    "$ROOT/adapters/cursor/commands/project-autopilot.md"; do
    run grep -F '/dispatcher:autopilot' "$f"
    [ "$status" -eq 0 ]
    run grep -E '(^|[^:])/autopilot' "$f"
    [ "$status" -ne 0 ]
  done
}

@test "the dispatcher command resolves its protocol via the env var" {
  run grep -F '$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md' "$ROOT/adapters/core/commands/dispatcher.md"
  [ "$status" -eq 0 ]
}

@test "codex adapter ships no agents and no workflows" {
  [ ! -d "$ROOT/adapters/codex/plugin/agents" ]
  [ ! -d "$ROOT/adapters/codex/plugin/workflows" ]
}

@test "claude-code adapter ships the critic pipeline codex cannot express" {
  [ -f "$ROOT/adapters/claude-code/plugin/agents/spec-critic.md" ]
  [ -f "$ROOT/adapters/claude-code/plugin/agents/plan-critic.md" ]
  [ -f "$ROOT/adapters/claude-code/plugin/skills/spec-plan-critic/SKILL.md" ]
}

@test "worker protocol defines the retro-note vocabulary" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    '## Retro notes (all tiers)' \
    '**Write a note only when one of the branches below is taken.**' \
    '`command_not_found`' \
    '`gate_thrash`' \
    '`approach_abandoned`' \
    '`consult_failed`' \
    '`rung_blocked`' \
    '`review_unavailable`' \
    '{"seam":"<stage>","tag":"<tag>","detail":"<what>"}'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "worker protocol carries retro notes in the metrics snapshot" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    '"review_high":<int|null>,"review_mode":"<full|downgraded|none|unavailable>","notes":[]' \
    '`notes` = the retro notes you accumulated this run' \
    'An empty array is the healthy case.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "worker protocol emits mid-execute retro notes immediately" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    'crew msg "$CREW_WORKER_ID" "retro:$CREW_ID"' \
    '**Execute is the only stage that emits early**' \
    'a `tmux kill-window` or a stall-watch hang never reaches a stopping path' \
    'Like `metrics:`, `retro:` is a synthetic sink — it never wakes the dispatcher.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "worker protocol points each branch at its retro tag" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    'and write an `approach_abandoned` retro note.' \
    'that is a block, not an abandoned approach, so write no `approach_abandoned` note' \
    'Write a `consult_failed` retro note naming the consultant and the reason.' \
    'write a `command_not_found` retro note.' \
    'emit a `gate_thrash` retro note carrying the ledger rows via the mid-execute path' \
    'Write a `rung_blocked` retro note naming the rung and the reason.' \
    'write a `review_unavailable` retro note.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "worker protocol binds the review gate to every engine" {
  # The roles stay engine-neutral; only the spawn mechanism is per-engine. The
  # rungs must keep matching rule 1's execute ladder, which is why each row's
  # model is asserted alongside its mechanism.
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    '**This gate binds on every engine**: the roles below are engine-neutral, and only the spawn mechanism differs.' \
    '| **claude** | Agent tool, the named `*-reviewer` agent matching the diff' \
    'a general agent running the `find-bugs` skill when none fits | unchanged — each agent definition owns its model |' \
    '| **codex** | native subagent (`agents.enabled`, cap 3) with the role brief written into its prompt — codex has no named-agent registry, so the brief **is** the prompt. Rule 1'"'"'s `ultra` anti-double-orchestration clause covers **execute** subagents only — the review batch always spawns, at every session effort |' \
    'The exemption covers the **diverse** reviewer only: the same-engine language reviewer and test-runner still run, and having **no** reviewer at all is the terminal path below' \
    'rung (deep → terra, standard → luna); effort is whatever `dispatch` pinned, since codex has no per-spawn override |' \
    '| **cursor** | Task-tool subagent with an explicit model slug, same inline role brief |' \
    'slug (deep → `cursor-grok-4.5-medium-fast`, standard → `cursor-grok-4.5-low-fast`) |' \
    'Cap the review→fix loop at 2.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "worker protocol pins the fresh-context reviewer contract" {
  # Both named escape hatches get their own assertion: self-review (the spawn
  # contract) and the safe-default-on-timeout allowance, which would otherwise
  # let a standard codex worker default its way past the gate to `done`.
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    'A reviewer subagent receives **only** the task doc, the diff (or the command that computes it), and its role brief.' \
    'It carries **review authority only**: it does not fix, commit, push, open PRs, or act as the worker' \
    '**"review it yourself in this context" is not a permitted fallback on `standard`/`deep`**' \
    '**A missing review gate is never low-risk**, so a `review gate unavailable:` block is carved out of this allowance on every tier'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "worker protocol makes an unspawnable reviewer terminal and loud" {
  # The `crew status` shape matters, not just the path: dropping the worker id
  # makes `from=blocked`, crew exits 1, and the block never reaches the bus.
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    'Retry the spawn **once**. If it still fails, do not push, do not open a PR, and do not emit `none`:' \
    'crew status "$CREW_WORKER_ID" blocked "review gate unavailable: <what>"' \
    'crew msg "$CREW_WORKER_ID" dispatcher:<crew_id> "<engine and tier, mechanism attempted, how it failed including the retry, the two legal replies>"' \
    'the only two legal replies — **retry**, or **re-dispatch** (to an engine that can review, or as `tier: trivial`, where `none` is both legal and true)' \
    '**proceeding unreviewed at this tier is not a legal reply**' \
    '`unavailable` appears on a `blocked`/`failed` snapshot only and **never co-occurs with `done`**' \
    '**On `standard`/`deep` a `kind: implement` worker never validly reports `done` (or `pr_open`) with `review_mode: "none"`, on any engine**' \
    '`unavailable` is narrower than "no reviewer ran": it means the gate was **reached** and no reviewer could be spawned' \
    'Whether `none` is honest turns on one test — **did the run reach the review gate?**' \
    'emits `none` with `review_high: 0` per the "`0` if no reviewer ran" rule' \
    'A run that did reach it keeps whatever the gate produced — `full`/`downgraded` with its real `review_high`, or `unavailable` — even if it later fails, is stopped, or times out'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}

@test "worker protocol glosses review_mode none by the gate-reached condition" {
  # The old parenthetical read as permission for the exact degrade the
  # unavailability path exists to close, so its absence is the regression test.
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  run grep -F '`none` (**no reviewer was due** — the trivial tier, or a standard/deep run that stopped before reaching the gate), or `unavailable` (a reviewer was required and could not be spawned; see below)' "$protocol"
  [ "$status" -eq 0 ]
  run grep -F '`none` (trivial / no reviewer)' "$protocol"
  [ "$status" -ne 0 ]
}

@test "worker protocol scopes the metrics carve-out to the critics only" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    '`review_mode` = which review depth actually ran (`full`|`downgraded`|`none`|`unavailable`, per the Code review gate' \
    '**The code review gate is not part of that carve-out**: on `standard`/`deep` they run it like any other engine' \
    'on `trivial` they emit `review_high: 0` with `review_mode: "none"`, the same as a trivial claude worker.' \
    'On an `unavailable` snapshot `review_high` is `null`'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
  # Phrase unique to the deleted carve-out — `Codex and cursor` alone would also
  # match the tier-scoped sentence that replaced it.
  run grep -F 'nor the claude code-review gate' "$protocol"
  [ "$status" -ne 0 ]
}

@test "the cursor rule runs the review gate, keeping only the critic carve-out" {
  # Hand-maintained — gen-adapters.sh never touches adapters/cursor/rules/, so
  # no drift gate sees this file. With alwaysApply: true it is in every cursor
  # session's context, and this test is its only protection.
  rule="$ROOT/adapters/cursor/rules/dispatcher.mdc"
  run grep -F 'code-review gate like any other engine' "$rule"
  [ "$status" -eq 0 ]
  run grep -F '`plan_critic_first_pass: null` — the critic half of the carve-out only.' "$rule"
  [ "$status" -eq 0 ]
  run grep -F 'review_mode: "none"' "$rule"
  [ "$status" -ne 0 ]
  run grep -F 'review_high: null' "$rule"
  [ "$status" -ne 0 ]
}
