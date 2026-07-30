#!/usr/bin/env bash
# Project the shared command bodies in adapters/core/commands/ into each
# engine's native shape, and ship the protocols inside each plugin tree.
# Idempotent — CI regenerates and asserts no diff.
#
#   claude-code : commands/<name>.md   (native slash commands)
#   cursor      : commands/<name>.md   (native slash commands)
#   codex       : skills/<name>/SKILL.md
#                 codex has NO custom slash commands (custom prompts are
#                 deprecated in favour of skills), so each command becomes a
#                 skill invoked as $<name> or via /skills.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$root/adapters/core/commands"
protocols="$root/adapters/core/protocols"

cc="$root/adapters/claude-code/plugin/commands"
cx="$root/adapters/codex/plugin/skills"
cu="$root/adapters/cursor/commands"

# Clear ALL generated trees, codex skills included. Only clearing the command
# dirs would leave an orphaned codex skill behind whenever a command is renamed
# or removed: the idempotence test never exercises removal (it reruns with an
# unchanged source), and the CI drift gate sees no diff for a stale dir nobody
# rewrote — so the orphan would persist silently and forever.
rm -rf "$cc" "$cu" "$cx"
mkdir -p "$cc" "$cu" "$cx"

for f in "$src"/*.md; do
  name="$(basename "$f" .md)"

  cp "$f" "$cc/$name.md"
  cp "$f" "$cu/$name.md"

  mkdir -p "$cx/$name"

  # Read the description through a YAML parser and re-emit through one, rather
  # than hand-rolling quote/backslash escaping. Descriptions routinely contain
  # ": " (invalid as a bare YAML scalar), and naive re-escaping of an
  # already-escaped source value silently corrupts it — a `\"` in the source
  # became a literal backslash in the output. jq owns the escaping, yq -P owns
  # the YAML quoting, so both are correct by construction.
  desc="$(
    awk 'NR==1 && /^---$/{inf=1; next} inf && /^---$/{exit} inf' "$f" |
      yq -r '.description // ""'
  )"

  {
    printf -- '---\n'
    jq -n --arg name "$name" --arg description "$desc" \
      '{name: $name, description: $description}' | yq -P -
    printf -- '---\n\n'
    # Body with the source frontmatter stripped, if it has any.
    if [ "$(head -1 "$f")" = "---" ]; then
      awk 'NR>1 && /^---$/ {found=1; next} found' "$f"
    else
      cat "$f"
    fi
  } >"$cx/$name/SKILL.md"
done

# Ship the notify hook and the protocols inside both plugin trees, so a plugin
# is self-contained: the command bodies tell the agent to fall back to the
# plugin-local protocols when $DISPATCHER_PROTOCOL_DIR is unset (a non-Nix
# install, where nothing exports it).
for d in "$root/adapters/claude-code/plugin" "$root/adapters/codex/plugin"; do
  mkdir -p "$d/scripts"
  cp "$root/adapters/core/dispatch-notify.sh" "$d/scripts/dispatch-notify.sh"
  chmod +x "$d/scripts/dispatch-notify.sh"
  rm -rf "$d/protocols"
  cp -r "$protocols" "$d/protocols"
done

# codex gets spec-plan-critic as a skill; it can express neither agents nor
# workflows, so its workers stay process-light per WORKER_PROTOCOL.md.
mkdir -p "$cx/spec-plan-critic"
cp "$root/adapters/claude-code/plugin/skills/spec-plan-critic/SKILL.md" \
  "$cx/spec-plan-critic/SKILL.md"

echo "adapters regenerated"
