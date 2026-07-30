#!/usr/bin/env bash
# Project the shared command bodies in adapters/core/commands/ into each
# engine's native shape. Idempotent — CI regenerates and asserts no diff.
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

cc="$root/adapters/claude-code/plugin/commands"
cx="$root/adapters/codex/plugin/skills"
cu="$root/adapters/cursor/commands"

rm -rf "$cc" "$cu"
mkdir -p "$cc" "$cu" "$cx"

for f in "$src"/*.md; do
  name="$(basename "$f" .md)"

  cp "$f" "$cc/$name.md"
  cp "$f" "$cu/$name.md"

  # codex: same body, re-fronted as a skill.
  mkdir -p "$cx/$name"

  # Descriptions routinely contain ": " (e.g. "Autonomous dev workflow: Linear
  # ticket -> ...") which is invalid as a bare YAML scalar, so always emit a
  # double-quoted scalar with backslashes and quotes escaped. Take everything
  # after `description:` rather than splitting on quotes, so an embedded quote
  # cannot silently truncate the value.
  desc="$(sed -n 's/^description:[[:space:]]*//p' "$f" | head -1)"
  desc="${desc#\"}"
  desc="${desc%\"}"
  desc="${desc//\\/\\\\}"
  desc="${desc//\"/\\\"}"

  {
    printf -- '---\n'
    printf 'name: %s\n' "$name"
    printf 'description: "%s"\n' "$desc"
    printf -- '---\n\n'
    # Body with the source frontmatter stripped, if it has any.
    if [ "$(head -1 "$f")" = "---" ]; then
      awk 'NR>1 && /^---$/ {found=1; next} found' "$f"
    else
      cat "$f"
    fi
  } >"$cx/$name/SKILL.md"
done

# Copy the notify hook into both plugin trees.
for d in "$root/adapters/claude-code/plugin" "$root/adapters/codex/plugin"; do
  mkdir -p "$d/scripts"
  cp "$root/adapters/core/dispatch-notify.sh" "$d/scripts/dispatch-notify.sh"
  chmod +x "$d/scripts/dispatch-notify.sh"
done

# codex gets spec-plan-critic as a skill; it can express neither agents nor
# workflows, so its workers stay process-light per WORKER_PROTOCOL.md.
mkdir -p "$cx/spec-plan-critic"
cp "$root/adapters/claude-code/plugin/skills/spec-plan-critic/SKILL.md" \
  "$cx/spec-plan-critic/SKILL.md"

echo "adapters regenerated"
