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

# Render the Model map's generated doc regions (#560) before the protocols
# directory is copied into each plugin tree below.
bash "$root/scripts/gen-model-map-doc.sh"

# The claude and pi leads read renders of every worker protocol file; codex and
# cursor read the sources. Claude and pi dispatchers launch from their own renders.
# The tmp files sit inside the rev-hashed protocol dir, so every exit path removes them.
trap 'rm -f "$protocols"/.WORKER_*.tmp "$protocols"/.DISPATCHER_*.tmp' EXIT
worker_stems=()
for f in "$protocols"/WORKER_*.md; do
  case "$f" in *.claude.md | *.pi.md) continue ;; esac
  stem="$(basename "$f" .md)"
  worker_stems+=("$stem")
  for engine in claude pi; do
    tmp="$protocols/.$stem.$engine.md.tmp"
    bash "$root/scripts/render-engine.sh" "$engine" "$f" >"$tmp" || exit 1
    mv "$tmp" "$protocols/$stem.$engine.md"
  done
done
# A render whose source is gone (a renamed phase file) would otherwise ship stale.
for r in "$protocols"/WORKER_*.claude.md "$protocols"/WORKER_*.pi.md; do
  [ -e "$r" ] || continue
  base="${r%.claude.md}"
  [ "$base" != "$r" ] || base="${r%.pi.md}"
  [ -e "$base.md" ] || rm -f "$r"
done
dtmp_claude="$protocols/.DISPATCHER_PROTOCOL.claude.md.tmp"
dtmp_pi="$protocols/.DISPATCHER_PROTOCOL.pi.md.tmp"
bash "$root/scripts/render-engine.sh" claude "$protocols/DISPATCHER_PROTOCOL.md" >"$dtmp_claude" || exit 1
mv "$dtmp_claude" "$protocols/DISPATCHER_PROTOCOL.claude.md"
bash "$root/scripts/render-engine.sh" pi "$protocols/DISPATCHER_PROTOCOL.md" >"$dtmp_pi" || exit 1
mv "$dtmp_pi" "$protocols/DISPATCHER_PROTOCOL.pi.md"

# The protocol revision marker (#184, #193) is no longer a committed file:
# the content hash of adapters/core/protocols (sorted `name:sha256;` entries,
# sha256 of the concatenation, first 16 hex chars) is what flake.nix bakes
# into dispatch.sh / dispatch-resume.sh at build time, and their runtime guard
# recomputes the same hash from the files actually in $PROTOCOL_DIR. Nothing
# to write here — the rule only lives in flake.nix (Nix) and the two
# _check_protocol_rev copies (bash), pinned together by tests/module.bats.
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

# Commands that drive Claude Code agent teams (TaskCreate/SendMessage/
# subagent_type/teammateMode, plus /loop and /schedule) and have no codex/cursor
# equivalent yet. They ship to claude-code only; the body states claude-only at
# the top. Porting them onto the crew bus is tracked in README's roadmap.
claude_only=" project-autopilot finish-prs "
is_claude_only() { case "$claude_only" in *" $1 "*) return 0 ;; *) return 1 ;; esac }

for f in "$src"/*.md; do
  name="$(basename "$f" .md)"

  cp "$f" "$cc/$name.md"

  # Claude-only commands are not projected to codex or cursor.
  if is_claude_only "$name"; then
    continue
  fi

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

# Ship the notify hook, the guards, and the protocols inside both
# plugin trees, so a plugin is self-contained: the command bodies tell the
# agent to fall back to the plugin-local protocols when
# $DISPATCHER_PROTOCOL_DIR is unset (a non-Nix install, where nothing exports
# it).
for d in "$root/adapters/claude-code/plugin" "$root/adapters/codex/plugin"; do
  mkdir -p "$d/scripts"
  cp "$root/adapters/core/dispatch-notify.sh" "$d/scripts/dispatch-notify.sh"
  chmod +x "$d/scripts/dispatch-notify.sh"
  cp "$root/adapters/core/secret-read-guard.sh" "$d/scripts/secret-read-guard.sh"
  chmod +x "$d/scripts/secret-read-guard.sh"
  cp "$root/adapters/core/public-leak-guard.sh" "$d/scripts/public-leak-guard.sh"
  chmod +x "$d/scripts/public-leak-guard.sh"
  cp "$root/adapters/core/test-scope-guard.sh" "$d/scripts/test-scope-guard.sh"
  chmod +x "$d/scripts/test-scope-guard.sh"
  cp "$root/adapters/core/phase-status.sh" "$d/scripts/phase-status.sh"
  chmod +x "$d/scripts/phase-status.sh"
  cp "$root/adapters/core/directive-delivery.sh" "$d/scripts/directive-delivery.sh"
  chmod +x "$d/scripts/directive-delivery.sh"
  rm -rf "$d/protocols" "$d/reviewers"
  cp -r "$protocols" "$d/protocols"
  cp -r "$reviewers" "$d/reviewers"
done

# The claude plugin's local fallback is claude-only; codex keeps the core copy.
for stem in "${worker_stems[@]}"; do
  cp "$protocols/$stem.claude.md" "$root/adapters/claude-code/plugin/protocols/$stem.md"
done
cp "$protocols/DISPATCHER_PROTOCOL.claude.md" "$root/adapters/claude-code/plugin/protocols/DISPATCHER_PROTOCOL.md"
# Only a claude lead reads the claude render and only a pi lead the pi render;
# codex and cursor never launch one.
rm -f "$root/adapters/claude-code/plugin/protocols/"*.pi.md
rm -f "$root/adapters/codex/plugin/protocols/"*.claude.md "$root/adapters/codex/plugin/protocols/"*.pi.md

# codex reads a critic the way it reads a reviewer — body into a subagent
# prompt. claude is not in this loop: its copy IS the agents/ registry below.
rm -rf "$root/adapters/codex/plugin/critics"
cp -r "$critics" "$root/adapters/codex/plugin/critics"

# Cursor has no plugin tree to be self-contained inside, and ~/.cursor/hooks.json
# is a single shared file several flakes write — so the hooks ship as loose
# scripts referenced by their store paths from hand-managed stanzas (see
# README), the same arrangement as codex's config.toml. They live beside
# commands/, not inside it: that dir is cleared and regenerated above.
rm -rf "$root/adapters/cursor/scripts"
mkdir -p "$root/adapters/cursor/scripts"
cp "$root/adapters/core/dispatch-notify.sh" "$root/adapters/cursor/scripts/dispatch-notify.sh"
chmod +x "$root/adapters/cursor/scripts/dispatch-notify.sh"
cp "$root/adapters/core/secret-read-guard.sh" "$root/adapters/cursor/scripts/secret-read-guard.sh"
chmod +x "$root/adapters/cursor/scripts/secret-read-guard.sh"
cp "$root/adapters/core/public-leak-guard.sh" "$root/adapters/cursor/scripts/public-leak-guard.sh"
chmod +x "$root/adapters/cursor/scripts/public-leak-guard.sh"
cp "$root/adapters/core/test-scope-guard.sh" "$root/adapters/cursor/scripts/test-scope-guard.sh"
chmod +x "$root/adapters/cursor/scripts/test-scope-guard.sh"
cp "$root/adapters/core/phase-status.sh" "$root/adapters/cursor/scripts/phase-status.sh"
chmod +x "$root/adapters/cursor/scripts/phase-status.sh"
cp "$root/adapters/core/directive-delivery.sh" "$root/adapters/cursor/scripts/directive-delivery.sh"
chmod +x "$root/adapters/cursor/scripts/directive-delivery.sh"

# Both rosters and the protocols ship loose for cursor: a cursor worker
# resolves these references by path, and without these copies the only
# path that resolves is the exported one, which a non-Nix install does not
# have.
for r in "$reviewers" "$critics" "$protocols"; do
  rm -rf "$root/adapters/cursor/$(basename "$r")"
  cp -r "$r" "$root/adapters/cursor/$(basename "$r")"
done
rm -f "$root/adapters/cursor/protocols/"*.claude.md "$root/adapters/cursor/protocols/"*.pi.md

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

# pi has no plugin registry: its lead spawns children through the pinned
# `subagent` extension, which discovers adapters/pi/agents/*.md. A child gets only
# its task text plus the agent body, so each body carries the full contract,
# including the secret rule lifted from the worker protocol.
pi_src="$root/adapters/pi/src"
pi_agents="$root/adapters/pi/agents"
pi_model="$(jq -r '.modelMap.pi.deep.default' "$root/adapters/core/defaults.json")"
pi_light="$pi_model:low"
pi_exec="$pi_model:high"
pi_esc="$pi_model:max"
pi_tools="read, grep, find, ls, bash"

rule7_head="$(awk 'index($0, "7. **Never print a secret") == 1 { sub(/^7\. /, ""); print; exit }' "$protocols/WORKER_PROTOCOL.md")"
if [ -z "$rule7_head" ]; then
  echo "gen-adapters: rule 7 (Never print a secret) not found in WORKER_PROTOCOL.md" >&2
  exit 1
fi
rule7="$rule7_head
If a secret value surfaces, stop and tell the lead without repeating it."

# Print a stamp with its {{RULE7}} line replaced by the rule text.
_stamp() {
  while IFS= read -r line; do
    if [ "$line" = "{{RULE7}}" ]; then
      printf '%s\n' "$rule7"
    else
      printf '%s\n' "$line"
    fi
  done <"$1"
}

# _pi_agent <name> <description> <tools|""> <model> <stamp> [body-file]
# An empty tools value omits the key, which gives the child every built-in.
_pi_agent() {
  local name="$1" description="$2" tools="$3" model="$4" stamp="$5" body="${6:-}"
  local tmp="$pi_agents/.$name.md.tmp"
  {
    printf -- '---\n'
    jq -n --arg name "$name" --arg description "$description" --arg tools "$tools" --arg model "$model" \
      '{name: $name, description: $description}
       + (if $tools == "" then {} else {tools: $tools} end)
       + {model: $model}' | yq -P -
    printf -- '---\n'
    _stamp "$pi_src/$stamp-stamp.md"
    if [ -n "$body" ]; then
      printf '\n'
      _body "$body"
    fi
  } >"$tmp"
  mv "$tmp" "$pi_agents/$name.md"
  pi_written+=("$name.md")
}

mkdir -p "$pi_agents"
pi_written=()
for f in "$reviewers"/*.md "$critics"/*.md; do
  [ "$(basename "$f")" != README.md ] || continue
  _pi_agent "$(basename "$f" .md)" "$(_desc "$f")" "$pi_tools" "$pi_esc" review "$f"
done
_pi_agent repo-reviewer "Run a repo-local reviewer brief passed in the task text." "$pi_tools" "$pi_esc" review "$pi_src/repo-reviewer.md"
_pi_agent refuter "Adversarially verify one review finding; default refuted." "$pi_tools" "$pi_esc" review "$pi_src/refuter.md"
_pi_agent test-runner "Run the named acceptance and behavior tests and report pass/fail." "$pi_tools" "$pi_exec" review "$pi_src/test-runner.md"
_pi_agent execute-light "Execute one plan step whose files and edit are named (cheap rung)." "" "$pi_light" execute
_pi_agent execute "Execute one judgment plan step (default rung)." "" "$pi_exec" execute
_pi_agent execute-escalated "Execute one plan step tagged high-risk (escalated rung)." "" "$pi_esc" execute

# An agent whose source is gone (a renamed reviewer) would otherwise ship stale.
for a in "$pi_agents"/*.md; do
  [ -e "$a" ] || continue
  case " ${pi_written[*]} " in *" $(basename "$a") "*) ;; *) rm -f "$a" ;; esac
done

echo "adapters regenerated"
