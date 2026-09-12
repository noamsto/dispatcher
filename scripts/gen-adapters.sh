#!/usr/bin/env bash
# Project the shared command bodies in adapters/core/commands/ into each
# engine's native shape, and ship the protocols, the skills, and the reviewer
# and critic rosters inside each plugin tree.
#
# The reviewer roster ships verbatim to all three, rather than as per-engine
# agents: a reviewer runs by having its body read into a fresh context, which
# every engine can do, and no shipped agent name can then collide with a
# user's own. The critic roster ships that way to codex and cursor too, but
# claude gets it as plugin agents — only that registry can spawn `plan-critic`
# by name and pin its model.
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
reviewers="$root/adapters/core/reviewers"
critics="$root/adapters/core/critics"
skills="$root/adapters/core/skills"

cc="$root/adapters/claude-code/plugin/commands"
cx="$root/adapters/codex/plugin/skills"
cu="$root/adapters/cursor/commands"
cus="$root/adapters/cursor/skills"
cca="$root/adapters/claude-code/plugin/agents"
ccs="$root/adapters/claude-code/plugin/skills"

# Read a frontmatter description through a YAML parser and re-emit it through
# one, rather than hand-rolling quote/backslash escaping. Descriptions
# routinely contain ": " (invalid as a bare YAML scalar), and naive
# re-escaping of an already-escaped source value silently corrupts it — a `\"`
# in the source became a literal backslash in the output. jq owns the
# escaping, yq -P owns the YAML quoting, so both are correct by construction.
_desc() {
  awk 'NR==1 && /^---$/{inf=1; next} inf && /^---$/{exit} inf' "$1" |
    yq -r '.description // ""'
}

# Body with the source frontmatter stripped, if it has any.
_body() {
  if [ "$(head -1 "$1")" = "---" ]; then
    awk 'NR>1 && /^---$/ {found=1; next} found' "$1"
  else
    cat "$1"
  fi
}

# Clear ALL generated trees, codex skills included. Only clearing the command
# dirs would leave an orphaned codex skill behind whenever a command is renamed
# or removed: the idempotence test never exercises removal (it reruns with an
# unchanged source), and the CI drift gate sees no diff for a stale dir nobody
# rewrote — so the orphan would persist silently and forever.
rm -rf "$cc" "$cu" "$cx" "$cca" "$ccs" "$cus"
mkdir -p "$cc" "$cu" "$cx" "$cca" "$ccs" "$cus"

for f in "$src"/*.md; do
  name="$(basename "$f" .md)"

  cp "$f" "$cc/$name.md"
  cp "$f" "$cu/$name.md"

  mkdir -p "$cx/$name"

  {
    printf -- '---\n'
    jq -n --arg name "$name" --arg description "$(_desc "$f")" \
      '{name: $name, description: $description}' | yq -P -
    printf -- '---\n'
    _body "$f"
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
  rm -rf "$d/protocols" "$d/reviewers"
  cp -r "$protocols" "$d/protocols"
  cp -r "$reviewers" "$d/reviewers"
done

# codex reads a critic the way it reads a reviewer — body into a subagent
# prompt. claude is not in this loop: its copy IS the agents/ registry below.
rm -rf "$root/adapters/codex/plugin/critics"
cp -r "$critics" "$root/adapters/codex/plugin/critics"

# Cursor has no plugin tree to be self-contained inside, and ~/.cursor/hooks.json
# is a single shared file several flakes write — so the hook ships as a loose
# script referenced by its store path from a hand-managed stanza (see README),
# the same arrangement as codex's config.toml. It lives beside commands/, not
# inside it: that dir is cleared and regenerated above.
rm -rf "$root/adapters/cursor/scripts"
mkdir -p "$root/adapters/cursor/scripts"
cp "$root/adapters/core/dispatch-notify.sh" "$root/adapters/cursor/scripts/dispatch-notify.sh"
chmod +x "$root/adapters/cursor/scripts/dispatch-notify.sh"

# Both rosters and the protocols ship loose for cursor: a cursor worker
# resolves these references by path, and without these copies the only
# path that resolves is the exported one, which a non-Nix install does not
# have.
for r in "$reviewers" "$critics" "$protocols"; do
  rm -rf "$root/adapters/cursor/$(basename "$r")"
  cp -r "$r" "$root/adapters/cursor/$(basename "$r")"
done

# The two claude-only frontmatter keys live here rather than in the shared
# body: a body codex and cursor paste into a prompt must not name a model
# neither can spawn. `opus` is the escalate rung the spec-plan-critic critic
# table pins — bump both together.
for f in "$critics"/*.md; do
  name="$(basename "$f" .md)"
  {
    printf -- '---\n'
    jq -n --arg name "$name" --arg description "$(_desc "$f")" \
      '{name: $name, description: $description, tools: ["Read", "Grep", "Glob"], model: "opus"}' |
      yq -P -
    printf -- '---\n'
    _body "$f"
  } >"$cca/$name.md"
done

# All three engines load a skill directory, cursor's loose under ~/.cursor.
# Written after the clears above, so a renamed skill leaves no orphan.
for d in "$skills"/*/; do
  name="$(basename "$d")"
  mkdir -p "$ccs/$name" "$cx/$name" "$cus/$name"
  cp "$d/SKILL.md" "$ccs/$name/SKILL.md"
  cp "$d/SKILL.md" "$cx/$name/SKILL.md"
  cp "$d/SKILL.md" "$cus/$name/SKILL.md"
done

echo "adapters regenerated"
