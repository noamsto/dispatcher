#!/usr/bin/env bash
# Pre-tool guard: ask before a `gh` post to a public repo carries private
# context — a private repo's `owner/name`, an agent session URL, a scratchpad
# path, a home-directory path, or a secret betterleaks recognizes. Once posted,
# GitHub's edit history keeps it public even after the body is fixed.
#
# `ask`, not `deny`: whether a name is a leak is a judgment call. Claude Code
# and cursor prompt; codex has no ask channel, so it gets a deny, and hookyard
# degrades pi's ask to deny itself.
#
#   engine        event                  command at             verdict shape
#   claude        PreToolUse Bash        tool_input.command     hookSpecificOutput ask
#   codex         PreToolUse Bash        tool_input.command     hookSpecificOutput deny
#                 (+ turn_id)
#   pi(hookyard)  canonical_event        tool_input.command     hookSpecificOutput ask
#                 pre_tool Bash
#   cursor        preToolUse Shell       tool_input.command     permission/user_message/
#   cursor        beforeShellExecution   command                agent_message ask
#
# Allow is no stdout, exit 0: cursor blocks the call on any non-JSON stdout.
#
# Private repos come from `gh api user/repos?visibility=private` (org repos
# included), cached for a day. Bare repo names are not matched: private repos
# named `api`, `docs` or `notes` would flag half of all prose. A target in that
# same list is private, so private-to-private posts pass. When the list can't
# be fetched and no cache exists, the guard abstains. betterleaks is optional;
# without it the secret scan is skipped, said once on stderr.
#
# Portable to macOS's bash 3.2 and BSD userland: no ${var,,}, and `timeout`
# only where it exists.

set -euo pipefail

command -v jq >/dev/null || {
  echo "public-leak-guard: jq not found; guard NOT enforcing" >&2
  exit 1
}

input=$(cat)

# shellcheck disable=SC2016
normalise='
def s: if type == "string" then . else "" end;
if type != "object" then empty else
(.tool_input | if type == "object" then . else {} end) as $ti
| (.hook_event_name | s) as $ev
| (.tool_name | s) as $tool
| (if (.canonical_event | type) == "string" then "hookyard"
   elif $ev == "PreToolUse" and has("turn_id") then "codex"
   elif $ev == "PreToolUse" then "claude"
   elif ($ev == "preToolUse" or $ev == "beforeShellExecution") then "cursor"
   else "" end) as $shape
| (if $shape == "cursor" and $ev == "beforeShellExecution" then .command | s
   elif $shape == "cursor" and $tool == "Shell" then $ti.command | s
   elif $shape != "" and $shape != "cursor" and $tool == "Bash" then $ti.command | s
   else "" end) as $command
| select($command != "")
| [$shape, $command, (first(.cwd, $ti.cwd, .workspace_roots[0]? | select(type == "string" and . != "")) // "")]
end'

parsed=$(jq -c "$normalise" <<<"$input" 2>/dev/null) || {
  echo "public-leak-guard: could not parse hook payload; guard NOT enforcing" >&2
  exit 1
}
[[ -n $parsed ]] || exit 0
shape=$(jq -r '.[0]' <<<"$parsed")
command=$(jq -r '.[1]' <<<"$parsed")
cwd=$(jq -r '.[2]' <<<"$parsed")
cwd=${cwd:-$PWD}

post_re='(^|[;&|(][[:space:]]*|[[:space:]])gh[[:space:]]+((issue|pr)[[:space:]]+(create|comment|edit|review)|release[[:space:]]+(create|edit)|api)([[:space:]]|$)'
[[ $command =~ $post_re ]] || exit 0
# Scan from the `gh` call on, so a leading `cd ~/git/private-repo &&` is not
# mistaken for body text.
text=${BASH_REMATCH[0]}${command#*"${BASH_REMATCH[0]}"}

# `gh api` without a field or input sends no body.
if [[ ${BASH_REMATCH[2]} == api ]]; then
  [[ $text =~ [[:space:]](-f|-F|--field|--raw-field|--input)([[:space:]=]) ]] || exit 0
fi

# The Bash tool resets its cwd each call, so a post from another checkout
# arrives as `cd <dir> && gh …`.
if [[ $command =~ (^|[;&|][[:space:]]*|[[:space:]])cd[[:space:]]+(\"[^\"]+\"|\'[^\']+\'|[^[:space:];&|]+) ]]; then
  dir=${BASH_REMATCH[2]}
  dir=${dir#[\"\']}
  dir=${dir%[\"\']}
  dir=${dir/#\~/$HOME}
  [[ $dir == /* ]] || dir=$cwd/$dir
  cwd=$dir
fi

slug='([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)'
target=
if [[ $text =~ (-R|--repo)[[:space:]=]+[\"\']?([A-Za-z0-9_.-]+/)?$slug ]]; then
  target=${BASH_REMATCH[3]}
elif [[ $text =~ github\.com/$slug/(issues|pull|releases) ]]; then
  target=${BASH_REMATCH[1]}
elif [[ $text =~ repos/$slug(/|[[:space:]\"\']|$) ]]; then
  target=${BASH_REMATCH[1]}
else
  remote=$(git -C "$cwd" remote get-url origin 2>/dev/null || true)
  [[ $remote =~ github\.com[:/]$slug ]] && target=${BASH_REMATCH[1]}
fi
target=$(tr '[:upper:]' '[:lower:]' <<<"${target%.git}")
[[ -n $target ]] || exit 0

cache=${XDG_CACHE_HOME:-$HOME/.cache}/dispatcher/private-repos
if [[ -z $(find "$cache" -mmin -1440 2>/dev/null) ]]; then
  mkdir -p "$(dirname "$cache")"
  limit=()
  command -v timeout >/dev/null && limit=(timeout 3)
  if fetched=$(${limit[@]+"${limit[@]}"} gh api 'user/repos?visibility=private&per_page=100' --paginate --jq '.[].full_name' 2>/dev/null); then
    tr '[:upper:]' '[:lower:]' <<<"$fetched" >"$cache.tmp" && mv "$cache.tmp" "$cache"
  fi
fi
[[ -s $cache ]] || exit 0

grep -qxF "$target" "$cache" && exit 0

# Bodies also arrive by file: `--body-file`/`-F <path>`, `--input <path>`,
# and `gh api`'s `-F body=@<path>`. The path itself is dropped from the scanned
# text: a body file in the scratchpad is not a leak, its contents may be.
# `-F` takes a path only without `=`; `gh api -F key=value` is a field.
file_arg_re='(--body-file|--input)[[:space:]=]+[^[:space:]]+|-F[[:space:]]+[^[:space:]=]+([[:space:]]|$)|=@[^[:space:]]+'
body=$(sed -E "s#$file_arg_re##g" <<<"$text")
while read -r path; do
  [[ -n $path && $path != - ]] || continue
  [[ $path == /* ]] || path=$cwd/$path
  if [[ -f $path ]]; then
    body+=$'\n'$(<"$path")
  fi
done < <(grep -oE "$file_arg_re" <<<"$text" |
  sed -E 's/^(--body-file|-F|--input)[[:space:]=]+//; s/^.*=@//; s/[[:space:]]+$//; s/^["'\'']//; s/["'\'']$//' || true)

hits=$(grep -oiwF -f "$cache" <<<"$body" | tr '[:upper:]' '[:lower:]' | sort -u || true)
hits+=$'\n'$(grep -oE 'claude\.ai/code/session_[A-Za-z0-9]+|/tmp/claude-[^[:space:]]*|'"$HOME"'/[^[:space:]]*' <<<"$body" | sort -u || true)
if command -v betterleaks >/dev/null; then
  hits+=$'\n'$(betterleaks stdin --no-banner -l error --redact -f json -r - <<<"$body" 2>/dev/null |
    jq -r '.[] | "secret: \(.RuleID)"' | sort -u || true)
else
  echo "public-leak-guard: betterleaks not found; secret scan skipped" >&2
fi
hits=$(sed '/^$/d' <<<"$hits")
[[ -n $hits ]] || exit 0

reason="This posts to $target, which is not in your private repos, and the text carries private context:
$hits
Rewrite it for an outside reader: drop private repo names and links, session URLs and local paths, and describe the finding so someone without that access can act on it and reproduce it."

case $shape in
cursor) jq -cn --arg r "$reason" '{permission: "ask", user_message: $r, agent_message: $r}' ;;
codex) jq -cn --arg r "$reason" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}' ;;
*) jq -cn --arg r "$reason" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "ask", permissionDecisionReason: $r}}' ;;
esac
