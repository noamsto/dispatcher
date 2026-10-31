#!/usr/bin/env bash
# dispatch-config — resolve the dispatcher's settings and print them as JSON.
#
# Four layers, merged with jq `*` (objects merge recursively, a later layer
# wins per key; arrays and scalars are replaced whole):
#   base   — defaults.json, baked in at build; a raw-source run reads the copy
#            beside this script.
#   user   — ${XDG_CONFIG_HOME:-~/.config}/dispatcher/settings.json, optional.
#   locked — when built by the home-manager module, the file baked in at
#            @lockedSettings@; otherwise the file named by
#            $DISPATCH_LOCKED_SETTINGS, optional. Once set it must be a
#            readable JSON object (fail closed: it is the security layer). A
#            baked build ignores $DISPATCH_LOCKED_SETTINGS, warning on stderr
#            if it names a different path.
#   env    — DISPATCH_ENGINES (whitespace-split), DISPATCH_GRANT_ROOTS
#            (colon-split), DISPATCH_OPENROUTER_MONTHLY_USD,
#            DISPATCH_OPENROUTER_KEY_FILE, DISPATCH_PROFILE,
#            DISPATCH_FLEET_BUDGET (integer),
#            DISPATCH_REPO_TRACKERS/DISPATCH_ORG_TRACKERS (whitespace-separated
#            key=value pairs, split at the first "="; a repeated key is
#            last-wins), DISPATCH_DISPATCHER_MCP (the same pairs, each value
#            a comma-separated opt-in list: key=claudeai,analytics; key= is an
#            empty list), DISPATCH_ROSTER_AUTO_OPEN (0/false/no/off, any case,
#            is false; any other value is true); an empty var contributes
#            nothing.
#
# Security: grantRoots and openrouter.keyFile widen what a worker can read, so
# they are honoured only from the locked layer or the environment — a copy in
# the base or user layer is dropped with a warning on stderr.
#
# localModels maps "<provider>/<model>" dispatch ids to local OpenAI-compatible
# endpoints for pi; it is validated on the merged tree and has no env var.
# workerDeniedMcpServers names the MCP servers claude workers must not load
# (interactive browsers); it is validated on the merged tree and has no env var.
# dispatcherChrome (boolean) launches the claude dispatcher session with
# --chrome; it is honoured from every layer and has no env var.
# freshSeams (boolean, default false) arms dispatch's fresh lead relaunch at
# the execute and review seams for eligible claude/pi standard/deep implement
# workers; dispatch --fresh-seams/--no-fresh-seams override it per dispatch.
# dispatcherMcp maps an owner/repo or org key to the MCP opt-ins (claudeai:
# every claude.ai connector; analytics: the generated --mcp profile; or any
# mcpServers name) that the dispatcher launch restores for that repo; the
# default launch has none. It is honoured from every layer: it widens only the
# dispatcher's own tools.
# mcpServers is the named catalog of remote MCP servers ({url, description?,
# headers?}) that dispatcherMcp and `dispatch --mcp` pick from; a header value
# is only an env reference or a !command, never a literal. It is validated on
# the merged tree and has no env var; its names are case-sensitive.
#
# classes maps each engine to ordered cheap|standard|premium glob lists (a pattern
# is a glob string or {glob, requires}, where requires names a condition the
# resolver reports: only pi_month_target) plus tiers.<tier>.{class, admits}. An
# engine row may also set autoPromote, a boolean: false makes trial promotion
# propose-only for that engine. It is validated on the merged tree and has no
# env var. Every modelMap engine and tier row needs a classes row with a class.
# A missing admits is filled with [class] (fail closed) with a warning, and a
# pattern overlapping another class's pattern warns: an id matching both takes
# the more expensive class.
#
# laneProfiles maps an engine or "<engine>/<model>" glob to a lane profile (its
# name, and optionally inline notes text) that `dispatch` applies to the task
# doc; like localModels it is validated on the merged tree and has no env var.
#
# hosts maps a host name to an ssh destination and the remote clone path of each
# owner/repo, for `crew remote-run`, plus an optional chrome flag marking the
# host that runs the owner's browser, and an optional peer flag marking a peer
# dispatcher host for `crew stream`'s peer sync (its name must equal that
# host's short hostname); like localModels it is validated on the merged tree
# and has no env var. Unlike grantRoots it is allowed from the user layer: an
# entry only chooses where work is sent and grants the worker nothing.
#
# Tracker map keys (repoTrackers/orgTrackers) and dispatcherMcp keys are
# lowercased within each layer before the merge, so a later layer's entry for
# the same key (any case) collapses and wins.
#
# --show-origin prints the same tree with every leaf replaced by
# {"value": …, "origin": "base|user|locked|env"}.
#
# --layers prints the file layers' paths and presence, reading no contents:
# {"base": …, "user": {"path": …, "present": …}, "locked": … or null}.
set -euo pipefail

show_origin=false
layers=false
if [[ $# -eq 1 && $1 == --show-origin ]]; then
  show_origin=true
elif [[ $# -eq 1 && $1 == --layers ]]; then
  layers=true
elif [[ $# -ne 0 ]]; then
  echo "usage: dispatch-config [--show-origin | --layers]" >&2
  exit 2
fi

die() {
  echo "dispatch-config: $*" >&2
  exit 1
}

# holds(p) — the value at path p exists, even when it is null.
# shellcheck disable=SC2016 # jq program text, not shell
jq_defs='def holds($p): (try getpath($p[:-1]) catch null) | type == "object" and has($p[-1]);'

# layer <path> — the file's single top-level JSON object, compacted.
layer() {
  jq -ces 'select(length == 1 and (.[0] | type) == "object") | .[0]' "$1" 2>/dev/null ||
    die "$1 is not a JSON object"
}

# check_no_blank_engines <json> <path> — die, naming <path>, when the layer's
# engines array holds an empty or whitespace-only string.
check_no_blank_engines() {
  if jq -e '(if (.engines | type) == "array" then .engines else [] end) | any(type == "string" and (gsub("^\\s+|\\s+$"; "") == ""))' <<<"$1" >/dev/null; then
    die "$2 sets a blank engine name in engines"
  fi
}

# check_model_shapes <json> <file> <layer> — die, naming <file> and <layer>
# and the JSON path, when the layer's modelMap / escalation / paceDowngrades /
# burnClasses / orchestratorDefaults / rosterDiagram holds a wrong-shaped leaf. Only shapes
# present in the layer are checked, so a layer that refines a single row stays
# valid and the merge of well-shaped layers is well-shaped.
check_model_shapes() {
  local violations path what
  violations=$(jq -r '
    def string_array: type == "array" and all(.[]; type == "string");
    def err($p; $w): "\($p)\t\($w)";
    # A ladder list item that names a class: @cheap|@standard|@premium, optionally
    # :<glob>. Anything else after @ would silently match nothing, so it fails.
    def ladder_items($l; $p):
      if ($l|string_array) then
        ($l | to_entries[] | select((.value|startswith("@")) and (.value|test("\\A@(cheap|standard|premium)(:.+)?\\z")|not))
          | err("\($p)[\(.key)]"; "a glob, @cheap, @standard, @premium or @<class>:<glob>"))
      else empty end;
    def modelmap($m):
      if ($m|type) != "object" then err("modelMap"; "an object")
      else ($m | to_entries[] |
        if (.value|type) != "object" then err("modelMap.\(.key)"; "an object")
        else (.key as $a | .value | to_entries[] |
          if (.value|type) != "object" then err("modelMap.\($a).\(.key)"; "an object")
          else (.key as $t | .value as $row |
            (if ($row|has("default")) and (($row.default|type) != "string") then err("modelMap.\($a).\($t).default"; "a string") else empty end),
            (if ($row|has("expected")) and (($row.expected|type) != "string") then err("modelMap.\($a).\($t).expected"; "a string") else empty end),
            (if ($row|has("models")) and (($row.models|string_array)|not) then err("modelMap.\($a).\($t).models"; "an array of strings") else empty end),
            (if ($row|has("regex")) and (($row.regex|string_array)|not) then err("modelMap.\($a).\($t).regex"; "an array of strings") else empty end)
          ) end
        ) end
      ) end;
    def escalation($e):
      if ($e|type) != "object" then err("escalation"; "an object")
      else ($e | to_entries[] |
        if (.value|type) != "object" then err("escalation.\(.key)"; "an object")
        else (.key as $a | .value | to_entries[] |
          if (.value|type) != "array" then err("escalation.\($a).\(.key)"; "an array")
          else (.key as $t | .value | to_entries[] |
            .key as $i | .value as $row |
            (if ($row|type) != "object" then err("escalation.\($a).\($t)[\($i)]"; "an object")
             else
              (if (($row|has("failed"))|not) or (($row.failed|string_array)|not) then err("escalation.\($a).\($t)[\($i)].failed"; "an array of strings") else empty end),
              (if (($row|has("baseline"))|not) or (($row.baseline|type) != "string") then err("escalation.\($a).\($t)[\($i)].baseline"; "a string") else empty end),
              (if (($row|has("inRow")) and ($row|has("outOfRow"))) or ((($row|has("inRow"))|not) and (($row|has("outOfRow"))|not)) then err("escalation.\($a).\($t)[\($i)]"; "exactly one of inRow or outOfRow") else empty end),
              (if ($row|has("inRow")) and (($row.inRow|string_array)|not) then err("escalation.\($a).\($t)[\($i)].inRow"; "an array of strings") else empty end),
              (if ($row|has("outOfRow")) and (($row.outOfRow|string_array)|not) then err("escalation.\($a).\($t)[\($i)].outOfRow"; "an array of strings") else empty end),
              (["failed", "inRow"][] as $k | select($row|has($k)) | ladder_items($row[$k]; "escalation.\($a).\($t)[\($i)].\($k)")),
              (if ($row.outOfRow|string_array) then ($row.outOfRow | to_entries[] | select(.value|startswith("@"))
                | err("escalation.\($a).\($t)[\($i)].outOfRow[\(.key)]"; "an exact model id (outOfRow widens what the tier gate admits, so it takes no class)")) else empty end)
             end)
          ) end
        ) end
      ) end;
    def pace($d):
      if ($d|type) != "object" then err("paceDowngrades"; "an object")
      else ($d | to_entries[] |
        if (.value|type) != "array" then err("paceDowngrades.\(.key)"; "an array")
        else (.key as $a | .value | to_entries[] |
          .key as $i | .value as $row |
          (if ($row|type) != "object" then err("paceDowngrades.\($a)[\($i)]"; "an object")
           else
            (if (($row|has("models"))|not) or (($row.models|string_array)|not) then err("paceDowngrades.\($a)[\($i)].models"; "an array of strings") else empty end),
            (if (($row|has("to"))|not) or (($row.to|type) != "string") then err("paceDowngrades.\($a)[\($i)].to"; "a string") else empty end),
            (if ($row|has("models")) then ladder_items($row.models; "paceDowngrades.\($a)[\($i)].models") else empty end),
            (if ($row.to|type) == "string" and ($row.to|startswith("@")) and ($row.to|test("\\A@(cheap|standard|premium)\\z")|not) then err("paceDowngrades.\($a)[\($i)].to"; "a model id, @cheap, @standard or @premium") else empty end)
           end)
        ) end
      ) end;
    def burnclasses($b):
      if ($b|type) != "array" then err("burnClasses"; "an array")
      else ($b | to_entries[] |
        .key as $i | .value as $row |
        (if ($row|type) != "object" then err("burnClasses[\($i)]"; "an object")
         else
          (if (($row|has("match"))|not) or (($row.match|type) != "string") then err("burnClasses[\($i)].match"; "a string") else empty end),
          (if (($row|has("class")) and ($row|has("byEffort"))) or ((($row|has("class"))|not) and (($row|has("byEffort"))|not)) then err("burnClasses[\($i)]"; "exactly one of class or byEffort") else empty end),
          (if ($row|has("class")) then
             (if (($row.class|type) != "string") then err("burnClasses[\($i)].class"; "a string") else empty end),
             (if (($row|has("weight"))|not) or (($row.weight|type) != "number") then err("burnClasses[\($i)].weight"; "a number") else empty end)
           else empty end),
          (if ($row|has("byEffort")) then
             (if ($row.byEffort|type) != "object" then err("burnClasses[\($i)].byEffort"; "an object")
              else
               (if (($row.byEffort|has("default"))|not) then err("burnClasses[\($i)].byEffort"; "a default entry") else empty end),
               ($row.byEffort | to_entries[] |
                 .key as $e | .value as $v |
                 (if ($v|type) != "object" then err("burnClasses[\($i)].byEffort.\($e)"; "an object")
                  else
                   (if (($v|has("class"))|not) or (($v.class|type) != "string") then err("burnClasses[\($i)].byEffort.\($e).class"; "a string") else empty end),
                   (if (($v|has("weight"))|not) or (($v.weight|type) != "number") then err("burnClasses[\($i)].byEffort.\($e).weight"; "a number") else empty end)
                  end)
               )
              end)
           else empty end)
         end)
      ) end;
    def orchestratordefaults($o):
      if ($o|type) != "object" then err("orchestratorDefaults"; "an object")
      else ($o | to_entries[] |
        if (.value|type) != "object" then err("orchestratorDefaults.\(.key)"; "an object")
        else
         (if ((.value|has("model"))|not) or ((.value.model|type) != "string") then err("orchestratorDefaults.\(.key).model"; "a string") else empty end),
         (if (.value|has("effort")) and ((.value.effort|type) != "string") then err("orchestratorDefaults.\(.key).effort"; "a string") else empty end),
         (if (.value|has("autoCompact")) and ((.value.autoCompact | (type == "number" and . > 0 and . == floor))|not) then err("orchestratorDefaults.\(.key).autoCompact"; "a positive integer") else empty end)
        end
      ) end;
    def rosterdiagram($d):
      if ($d|type) != "object" then err("rosterDiagram"; "an object")
      else
       (if ($d|has("autoOpen")) and (($d.autoOpen|type) != "boolean") then err("rosterDiagram.autoOpen"; "a boolean") else empty end)
      end;
    . as $r
    | (if ($r|has("modelMap")) then modelmap($r.modelMap) else empty end),
      (if ($r|has("escalation")) then escalation($r.escalation) else empty end),
      (if ($r|has("paceDowngrades")) then pace($r.paceDowngrades) else empty end),
      (if ($r|has("burnClasses")) then burnclasses($r.burnClasses) else empty end),
      (if ($r|has("orchestratorDefaults")) then orchestratordefaults($r.orchestratorDefaults) else empty end),
      (if ($r|has("rosterDiagram")) then rosterdiagram($r.rosterDiagram) else empty end)
  ' <<<"$1") ||
    die "$2 ($3 layer): could not validate the modelMap / escalation / paceDowngrades / burnClasses / orchestratorDefaults / rosterDiagram shapes"
  while IFS=$'\t' read -r path what; do
    [ -n "$path" ] || continue
    die "$2 ($3 layer): $path must be $what"
  done <<<"$violations"
}

# strip <json> <path> — drop the keys only the locked and env layers may set.
strip() {
  local key
  for key in grantRoots openrouter.keyFile; do
    if jq -e --arg k "$key" "$jq_defs"' holds($k | split("."))' <<<"$1" >/dev/null; then
      echo "dispatch-config: ignoring $key from $2 — it is honoured only from the locked settings or the environment" >&2
    fi
  done
  jq -c 'del(.grantRoots) | if (.openrouter | type) == "object" then del(.openrouter.keyFile) else . end' <<<"$1"
}

base_file="@defaultsJson@"
if [[ $base_file == @* ]]; then
  base_file="$(dirname "${BASH_SOURCE[0]}")/defaults.json"
fi

user_file="${XDG_CONFIG_HOME:-$HOME/.config}/dispatcher/settings.json"

locked_file="@lockedSettings@"
if [[ $locked_file == @* ]]; then
  locked_file="${DISPATCH_LOCKED_SETTINGS:-}"
elif [[ -n ${DISPATCH_LOCKED_SETTINGS:-} && $DISPATCH_LOCKED_SETTINGS != "$locked_file" ]]; then
  echo "dispatch-config: ignoring DISPATCH_LOCKED_SETTINGS — this build bakes $locked_file" >&2
fi

if [[ $layers == true ]]; then
  jq -n --arg b "$base_file" --arg u "$user_file" --argjson p "$([[ -e $user_file ]] && echo true || echo false)" --arg l "$locked_file" \
    '{base: $b, user: {path: $u, present: $p}, locked: (if $l == "" then null else $l end)}'
  exit 0
fi

base=$(layer "$base_file")
check_model_shapes "$base" "$base_file" base
base=$(strip "$base" "$base_file")

user='{}'
if [[ -e $user_file ]]; then
  user=$(layer "$user_file")
  check_no_blank_engines "$user" "$user_file"
  check_model_shapes "$user" "$user_file" user
  user=$(strip "$user" "$user_file")
fi

locked='{}'
if [[ -n $locked_file ]]; then
  [[ -r $locked_file ]] || die "$locked_file is not readable"
  locked=$(layer "$locked_file")
  check_no_blank_engines "$locked" "$locked_file"
  check_model_shapes "$locked" "$locked_file" locked
fi

env_layer=$(jq -cn '
  def from_env($var; $p; f): ($ENV[$var] // "") as $v | if $v == "" then . else setpath($p; $v | f) end;
  def from_env_nonempty($var; $p; f):
    ($ENV[$var] // "") as $v
    | if $v == "" then .
      else ($v | f) as $r | if ($r | length) > 0 then setpath($p; $r) else . end
      end;
  def trackers:
    [splits("\\s+")] | map(select(. != ""))
    | reduce .[] as $e ({}; ($e | capture("^(?<k>[^=]*)=(?<v>.*)$") // {k: $e, v: $e}) as $kv | . + {($kv.k | ascii_downcase): $kv.v});
  # A bare key keeps its string value, so the array check below refuses it.
  def mcp_optins:
    [splits("\\s+")] | map(select(. != ""))
    | reduce .[] as $e ({}; ($e | capture("^(?<k>[^=]*)=(?<v>.*)$") // null) as $kv
        | if $kv then . + {($kv.k | ascii_downcase): ($kv.v | split(",") | map(select(. != "")))}
          else . + {($e | ascii_downcase): $e} end);
  {}
  | from_env_nonempty("DISPATCH_ENGINES"; ["engines"]; [splits("\\s+")] | map(select(. != "")))
  | from_env("DISPATCH_GRANT_ROOTS"; ["grantRoots"]; split(":") | map(select(. != "")))
  | from_env("DISPATCH_OPENROUTER_MONTHLY_USD"; ["openrouter", "monthlyUsd"]; .)
  | from_env("DISPATCH_OPENROUTER_KEY_FILE"; ["openrouter", "keyFile"]; .)
  | from_env("DISPATCH_PROFILE"; ["profile"]; .)
  | from_env("DISPATCH_FLEET_BUDGET"; ["fleetBudget"]; tonumber? // .)
  | from_env_nonempty("DISPATCH_REPO_TRACKERS"; ["repoTrackers"]; trackers)
  | from_env_nonempty("DISPATCH_ORG_TRACKERS"; ["orgTrackers"]; trackers)
  | from_env_nonempty("DISPATCH_DISPATCHER_MCP"; ["dispatcherMcp"]; mcp_optins)
  | from_env("DISPATCH_ROSTER_AUTO_OPEN"; ["rosterDiagram", "autoOpen"]; test("^(0|false|no|off)$"; "i") | not)')

printf '%s\n' "$base" "$user" "$locked" "$env_layer" | jq -n --argjson show_origin "$show_origin" "$jq_defs"'
  def string_array: type == "array" and all(.[]; type == "string");
  def die($p; $what): "dispatch-config: \($p | join(".")) must be \($what) (merged settings)\n" | halt_error(1);
  def need($p; $what; ok):
    if holds($p) and (getpath($p) | ok | not) then die($p; $what) else . end;
  def pos_int: type == "number" and . == floor and . > 0;
  def pat: if type == "object" then .glob else . end;
  # One [path, what] per violation in .classes, then one per modelMap engine and
  # tier row that has no valid classes row. Lookups are guarded so a malformed
  # sibling reports itself instead of erroring jq.
  def class_violations:
    .classes as $c
    | .modelMap as $mm
    | if ($c | type) != "object" then [["classes"], "an object"]
      else
        (($c | to_entries[]) as {key: $e, value: $v}
          | if ($v | type) != "object" then [["classes", $e], "an object"]
            else
              ($v | keys[] | select(IN("cheap", "standard", "premium", "tiers", "autoPromote") | not) | [["classes", $e, .], "one of cheap, standard, premium, tiers, autoPromote"]),
              (select($v | has("autoPromote")) | select(($v.autoPromote | type) != "boolean") | [["classes", $e, "autoPromote"], "a boolean"]),
              (("cheap", "standard", "premium") as $cl
                | select($v | has($cl))
                | ["classes", $e, $cl] as $p
                | $v[$cl] as $list
                | if ($list | type) != "array" then [$p, "an array of patterns"]
                  else
                    (($list | to_entries[]) as {key: $i, value: $x}
                      | $p + [$i] as $pp
                      | if ($x | type) == "string" or (($x | type) == "object" and ($x | keys) == ["glob", "requires"] and ($x.glob | type) == "string" and $x.requires == "pi_month_target")
                        then ($x | pat) as $g
                          | if ($g | test("\\A[A-Za-z0-9._/*?-]+\\z") | not) then [$pp, "a glob of letters, digits, \".\", \"_\", \"/\", \"-\", \"*\" and \"?\""]
                            elif $e == "pi" and ($g | test("\\A[a-z0-9][a-z0-9._-]*/") | not) then [$pp, "a pi pattern with a literal provider (e.g. openrouter/...)"]
                            else empty
                            end
                        else [$pp, "a glob string or {glob, requires: \"pi_month_target\"}"]
                        end)
                  end),
              (select($v | has("tiers"))
                | $v.tiers as $t
                | if ($t | type) != "object" then [["classes", $e, "tiers"], "an object"]
                  else
                    (($t | to_entries[]) as {key: $k, value: $r}
                      | ["classes", $e, "tiers", $k] as $tp
                      | if ($k | IN("trivial", "standard", "deep") | not) then [$tp, "one of trivial, standard, deep"]
                        elif ($r | type) != "object" then [$tp, "an object"]
                        else
                          ($r | select((.class | IN("cheap", "standard", "premium")) | not) | [$tp + ["class"], "one of cheap, standard or premium"]),
                          ($r | select(has("admits") and ((.admits | type == "array" and length > 0 and ((unique | length) == length) and all(.[]; IN("cheap", "standard", "premium")) and index($r.class) != null) | not)) | [$tp + ["admits"], "a non-empty array of distinct cheap, standard or premium values including its class"])
                        end)
                  end)
            end),
        (($mm | if type == "object" then to_entries[] else empty end) as {key: $e, value: $ev}
          | ($ev | if type == "object" then keys[] else empty end) as $t
          | (try $c[$e].tiers[$t] catch null) as $row
          | select(($row | type) != "object" or (($row.class | IN("cheap", "standard", "premium")) | not))
          | [["classes", $e, "tiers", $t], "an object with class and admits (every modelMap row needs one)"])
      end;
  # Newline-terminated stderr lines for a valid .classes tree.
  # Whether two globs (arrays of single-char strings) match a common string.
  def globs_meet($a; $b):
    ($a | length) as $m | ($b | length) as $n
    | reduce range($m; -1; -1) as $i (null;
        . as $next
        | reduce range($n; -1; -1) as $j ([];
            . as $row
            | .[$j] = (
                if $i == $m and $j == $n then true
                elif $i == $m then $b[$j] == "*" and $row[$j + 1]
                elif $j == $n then $a[$i] == "*" and $next[$j]
                elif $a[$i] == "*" or $b[$j] == "*" then $next[$j] or $row[$j + 1]
                else ($a[$i] == $b[$j] or $a[$i] == "?" or $b[$j] == "?") and $next[$j + 1] end)))
    | .[0];
  def class_warnings:
    ["cheap", "standard", "premium"] as $order
    | ((.classes | to_entries[]) as {key: $e, value: $v}
        | [$order | to_entries[] | .key as $r | .value as $cl | ($v[$cl] | arrays | .[] | pat | strings | {cl: $cl, r: $r, g: .})] as $pats
        | ($pats[] as $a | $pats[] as $b
            | select($a.r < $b.r and globs_meet($a.g | gsub("\\*+"; "*") | split(""); $b.g | gsub("\\*+"; "*") | split("")))
            | "dispatch-config: classes.\($e): pattern \"\($a.g)\" (\($a.cl)) overlaps \"\($b.g)\" (\($b.cl)); an id matching both takes \($b.cl)\n")),
      ((.classes | to_entries[]) as {key: $e, value: $v}
        | ($v.tiers | objects | to_entries[])
        | select(.value | has("admits") | not)
        | "dispatch-config: classes.\($e).tiers.\(.key).admits is missing; admitting only its class \(.value.class)\n");
  # One [path, what] per violation in .localModels, entries in key order.
  def local_model_violations:
    (["openrouter"] + [.modelMap.pi[]?.models[]? | split("/")[0]] + [.classes | objects | .pi | objects | (.cheap, .standard, .premium) | arrays | .[] | pat | strings | split("/")[0]] | map(ascii_downcase)) as $reserved
    | .localModels as $all
    | if ($all | type) != "object" then [["localModels"], "an object"]
      else
        ($all | to_entries[]) as {key: $k, value: $v}
        | ($k | split("/")[0] | ascii_downcase) as $prov
        | ["localModels", $k] as $p
        | if ($k | test("^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._/-]*$") | not)
          then [$p, "keyed <provider>/<model> with letters, digits, \".\", \"_\", \"-\" and \"/\" (no \":\")"]
          elif ($v | type) != "object" then [$p, "an object"]
          else
            ($v | keys[] | select(IN("baseUrl", "contextWindow", "maxConcurrent", "maxTokens", "tiers", "workerNotes", "reasoning", "thinkingFormat", "thinkingLevelMap", "samplingParams", "samplingParamsByThinkingLevel", "effortThinking", "input") | not) | [$p + [.], "one of baseUrl, contextWindow, maxConcurrent, maxTokens, tiers, workerNotes, reasoning, thinkingFormat, thinkingLevelMap, samplingParams, samplingParamsByThinkingLevel, effortThinking, input"]),
            ($v.baseUrl | select(type != "string" or (test("^https?://[^[:space:]]*[^/[:space:]]$") | not)) | [$p + ["baseUrl"], "an http(s) URL without whitespace or a trailing \"/\""]),
            ($v.contextWindow | select(pos_int | not) | [$p + ["contextWindow"], "a positive integer"]),
            ($v | select(has("maxConcurrent") and (.maxConcurrent | pos_int | not)) | [$p + ["maxConcurrent"], "a positive integer"]),
            ($v | select(has("maxTokens") and (.maxTokens | pos_int | not)) | [$p + ["maxTokens"], "a positive integer"]),
            ($v | select(has("tiers") and ((.tiers | type == "array" and length > 0 and all(.[]; IN("trivial", "standard", "deep"))) | not)) | [$p + ["tiers"], "a non-empty array of trivial, standard or deep"]),
            ($v | select(has("workerNotes") and (($v.workerNotes | type) != "string")) | [$p + ["workerNotes"], "a string"]),
            ($v | select(has("reasoning") and (.reasoning | type != "boolean")) | [$p + ["reasoning"], "a boolean"]),
            # pi treats a model with no `input` as text-only, so a vision
            # endpoint declares ["text","image"]; `text` is required, and a
            # repeat would reach pi unchanged.
            ($v | select(has("input") and ((.input | string_array and length > 0 and ((unique | length) == length) and all(.[]; IN("text", "image")) and index("text") != null) | not)) | [$p + ["input"], "a non-empty array of distinct \"text\" or \"image\" values including \"text\""]),
            ($v | select(has("thinkingFormat") and (.thinkingFormat | type != "string" or length == 0)) | [$p + ["thinkingFormat"], "a non-empty string"]),
            ($v | to_entries[] | select(.key | IN("thinkingLevelMap", "samplingParams", "samplingParamsByThinkingLevel")) | select((.value | type != "object") or (.key == "thinkingLevelMap" and (.value | all(.[]; . == null or type == "string")) == false) or (.key == "samplingParamsByThinkingLevel" and (.value | all(.[]; type == "object")) == false)) | [$p + [.key], (if .key == "thinkingLevelMap" then "an object of strings or null" elif .key == "samplingParamsByThinkingLevel" then "an object of objects" else "an object" end)]),
            ($v | select(has("effortThinking") and ((.effortThinking | type == "object" and all(.[]; IN("off", "minimal", "low", "medium", "high", "xhigh", "max"))) | not)) | [$p + ["effortThinking"], "an object of pi thinking levels (off, minimal, low, medium, high, xhigh or max)"]),
            ($v | select(has("effortThinking") and (.effortThinking | type == "object") and ((.effortThinking | keys - ["low", "medium", "high", "xhigh", "max"] | length) > 0)) | [$p + ["effortThinking"], "keyed by low, medium, high, xhigh or max"]),
            ($prov | select(IN($reserved[])) | [$p, "a provider outside the hosted pi ladder (\($prov) is reserved for it)"]),
            ([$all | to_entries[] | select(.key | split("/")[0] | ascii_downcase == $prov)][0].value.baseUrl as $first
              | select($first != $v.baseUrl) | [$p + ["baseUrl"], "the same URL as the other \($prov) entries"])
          end
      end;
  # One [path, what] per violation in .laneProfiles, entries in key order. The
  # name is a profile id: it reaches a task header line and a notes filename, so
  # it never holds a path separator, whitespace, or a newline.
  def lane_profile_violations:
    .laneProfiles as $all
    | if ($all | type) != "object" then [["laneProfiles"], "an object"]
      else
        ($all | to_entries[]) as {key: $k, value: $v}
        | ["laneProfiles", $k] as $p
        | if ($v | type) != "object" then [$p, "an object"]
          else
            ($v | keys[] | select(IN("name", "notes") | not) | [$p + [.], "one of name, notes"]),
            ($v | select((has("name") | not) or (($v.name | type) != "string") or (($v.name | test("^[A-Za-z0-9][A-Za-z0-9._-]*$")) | not)) | [$p + ["name"], "a string of letters, digits, \".\", \"_\" or \"-\""]),
            ($v | select(has("notes") and (($v.notes | type) != "string")) | [$p + ["notes"], "a string"])
          end
      end;
  # One [path, what] per violation in .hosts, entries in key order. Names and
  # the ssh value reach an ssh argv, so they are anchored with \z (a "$" would
  # let a trailing newline through) and an ssh value never starts with "-".
  def host_violations:
    .hosts as $all
    | if ($all | type) != "object" then [["hosts"], "an object"]
      else
        ($all | to_entries[]) as {key: $k, value: $v}
        | ["hosts", $k] as $p
        | if ($k | test("\\A[A-Za-z0-9][A-Za-z0-9._-]*\\z") | not)
          then [$p, "a name of letters, digits, \".\", \"_\" or \"-\" starting with a letter or digit"]
          elif ($v | type) != "object" then [$p, "an object"]
          else
            ($v | keys[] | select(IN("ssh", "repos", "chrome", "peer") | not) | [$p + [.], "one of ssh, repos, chrome, peer"]),
            ($v | select((.ssh | type != "string") or ((.ssh | test("\\A[A-Za-z0-9_][A-Za-z0-9._@%+-]*\\z")) | not)) | [$p + ["ssh"], "an ssh destination of letters, digits and \"._@%+-\" not starting with \"-\" (use an ~/.ssh/config alias for options)"]),
            ($v | select(has("chrome") and ((.chrome | type) != "boolean")) | [$p + ["chrome"], "a boolean"]),
            ($v | select(has("peer") and ((.peer | type) != "boolean")) | [$p + ["peer"], "a boolean"]),
            ($v | select(has("repos") and ((.repos | type) != "object")) | [$p + ["repos"], "an object"]),
            (($v.repos | if type == "object" then to_entries[] else empty end) as {key: $r, value: $d}
              | [$p + ["repos", $r], "an owner/repo key mapped to an absolute path without control characters"]
              | select(($r | test("\\A[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\\z") | not) or ($d | type != "string" or (startswith("/") | not) or test("[\\x00-\\x1f\\x7f]"))))
          end
      end;
  # One [path, what] per violation in .mcpServers, entries in key order. Every
  # field reaches a launch argv or a generated config, so the patterns are
  # anchored with \A/\z, and a header value is never a literal: only an env
  # reference (optionally after one scheme word) or a !command, so no secret
  # can sit in settings.
  def mcp_server_violations:
    .mcpServers as $all
    | if ($all | type) != "object" then [["mcpServers"], "an object"]
      else
        (($all | to_entries[]) as {key: $k, value: $v}
          | ["mcpServers", $k] as $p
          | if ($k | test("\\A[A-Za-z0-9_-]+\\z") | not) then [$p, "a name of letters, digits, \"_\" or \"-\""]
            elif ($k | IN("claudeai", "analytics")) then [$p, "a name other than claudeai or analytics (reserved dispatcherMcp opt-ins)"]
            elif ($v | type) != "object" then [$p, "an object"]
            else
              ($v | keys[] | select(IN("url", "description", "headers") | not) | [$p + [.], "one of url, description, headers"]),
              ($v.url | select(type != "string" or (test("\\Ahttps://[^\\s\"\\x27\\\\\\x00-\\x1f\\x7f]+\\z") | not)) | [$p + ["url"], "an https URL without whitespace, quotes or backslashes"]),
              ($v | select(has("description") and ((.description | type != "string") or (.description | test("[\\x00-\\x1f\\x7f]")) or (.description | length > 200))) | [$p + ["description"], "a string of at most 200 characters without control characters"]),
              ($v | select(has("headers") and (.headers | type != "object")) | [$p + ["headers"], "an object"]),
              (($v.headers | if type == "object" then to_entries[] else empty end) as {key: $h, value: $hv}
                | if ($h | test("\\A[A-Za-z0-9-]+\\z") | not) then [$p + ["headers", $h], "a header name of letters, digits and \"-\""]
                  elif ($hv | type == "string" and (test("\\A([A-Za-z]+ )?\\$\\{[A-Za-z_][A-Za-z0-9_]*\\}\\z") or (test("\\A!\\S") and (test("[\\x00-\\x1f\\x7f]") | not)))) then empty
                  else [$p + ["headers", $h], "an env reference (\"${NAME}\", optionally after one scheme word such as \"Bearer \") or a \"!command\", never a literal value"]
                  end)
            end),
        # pi counts names that differ only in "-" and "_" as one server.
        ($all | keys | group_by(gsub("-"; "_"))[] | select(length > 1)
          | [["mcpServers", .[1]], "a name distinct from \"\(.[0])\" once \"-\" and \"_\" count as the same"])
      end;
  def tag($layers; $p):
    if type == "object" and length > 0
    then with_entries(.key as $k | .value |= tag($layers; $p + [$k]))
    else {value: ., origin: ($layers | to_entries | map(select(.value | holds($p)) | .key) | last)}
    end;
  def lower_trackers:
    reduce ("repoTrackers", "orgTrackers", "dispatcherMcp") as $k
      (.; if (.[$k] | type) == "object" then .[$k] |= with_entries(.key |= ascii_downcase) else . end);
  [inputs | lower_trackers] as [$base, $user, $locked, $env]
  | $base * $user * $locked * $env
  | need(["engines"]; "a non-empty array of strings"; string_array and length > 0)
  | need(["grantRoots"]; "an array of strings without \":\""; string_array and all(.[]; contains(":") | not))
  | need(["openrouter"]; "an object"; type == "object")
  | need(["openrouter", "keyFile"]; "a string"; type == "string")
  | need(["openrouter", "monthlyUsd"]; "a number or string"; type == "number" or type == "string")
  | need(["profile"]; "a string"; type == "string")
  | need(["fleetBudget"]; "a non-negative integer"; type == "number" and . == floor and . >= 0)
  | need(["repoTrackers"]; "an object of strings"; type == "object" and all(.[]; type == "string"))
  | need(["orgTrackers"]; "an object of strings"; type == "object" and all(.[]; type == "string"))
  | need(["workerDeniedMcpServers"]; "an array of MCP server names (letters, digits, \"_\", \"-\")"; string_array and all(.[]; test("^[A-Za-z0-9_-]+$")))
  | need(["dispatcherChrome"]; "a boolean"; type == "boolean")
  | need(["freshSeams"]; "a boolean"; type == "boolean")
  | need(["dispatcherMcp"]; "an object keyed owner/repo or org, each an array of claudeai, analytics or an mcpServers name";
      type == "object"
      and all(keys[]; test("^[a-z0-9][a-z0-9._-]*(/[a-z0-9._-]+)?$"))
      and all(.[]; type == "array" and all(.[]; type == "string")))
  | if has("mcpServers") then (first(mcp_server_violations) // null) as $bad | if $bad then die($bad[0]; $bad[1]) else . end else . end
  | if has("dispatcherMcp") then
      (["claudeai", "analytics"] + (.mcpServers // {} | keys)) as $valid
      | (first(.dispatcherMcp | to_entries[] | .key as $k | .value[] | select(IN($valid[]) | not) | [$k, .]) // null) as $bad
      | if $bad then "dispatch-config: dispatcherMcp must be an object keyed owner/repo or org, each an array of claudeai, analytics or an mcpServers name: \"\($bad[0])\" names unknown \"\($bad[1])\" (valid: \($valid | join(", "))) (merged settings)\n" | halt_error(1) else . end
    else . end
  | (first(class_violations) // null) as $bad | if $bad then die($bad[0]; $bad[1]) else . end
  | ([class_warnings] | map(stderr)) as $w
  | .classes |= map_values(if (.tiers | type) == "object" then .tiers |= map_values(.admits //= [.class]) else . end)
  | if has("localModels") then (first(local_model_violations) // null) as $bad | if $bad then die($bad[0]; $bad[1]) else . end else . end
  | if has("laneProfiles") then (first(lane_profile_violations) // null) as $bad | if $bad then die($bad[0]; $bad[1]) else . end else . end
  | if has("hosts") then (first(host_violations) // null) as $bad | if $bad then die($bad[0]; $bad[1]) else . end else . end
  | if $show_origin then tag({base: $base, user: $user, locked: $locked, env: $env}; []) else . end'
