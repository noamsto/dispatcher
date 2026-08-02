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
  [ -f "$ROOT/adapters/claude-code/plugin/workflows/spec-plan-critic.js" ]
  [ -f "$ROOT/adapters/claude-code/plugin/skills/spec-plan-critic/SKILL.md" ]
}
