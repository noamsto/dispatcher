#!/usr/bin/env bash
# model-resolve — resolve one engine/tier row against a model catalog snapshot.
#
# Usage: model-resolve <engine> <tier> <catalog-file> <class-state-file>
#   stdin         dispatch-config's merged settings JSON (.modelMap seed and
#                 .classes patterns)
#   catalog-file  model-catalog.json: {"<engine>": {"fetched_epoch", "source",
#                 "models": [{"id", "created"?, "price"?}]}}
#   class-state   model-class-state.json: {"<engine>": {"<class>": {"default":
#                 "<id>"}}}, each class's incumbent default
#
# Prints one JSON object: admitted (the seed row's models, then catalog
# candidates the seed does not match, ordered by admitted class, pattern rank,
# then newest), regex (the seed row's), conditional ({id, requires} candidates
# whose only matching patterns carry a "requires" gate), default, trial (the
# newest non-dated catalog match of the row's class that sorts before the
# default, or null) and warnings (strings prefixed "model-resolve: ").
#
# A missing catalog or state file is not an error. A catalog or state file that
# is not a regular file, is over its size cap (4 MiB, 64 KiB) or is not one JSON
# object is ignored with a warning, as is a catalog entry of more than 5000
# models. With no catalog snapshot the seed row is the admitted set; the catalog
# and class-state are separate inputs, so a recorded default that passes the
# class-match check is still honoured.
#
# Pure: reads only stdin and the two named files. No clock, network, environment
# lookup or score. Exit 2 on a usage error, 1 on an unknown engine/tier row.
set -euo pipefail

[ "$#" -eq 4 ] || {
  echo "usage: model-resolve <engine> <tier> <catalog-file> <class-state-file>" >&2
  exit 2
}
engine=$1 tier=$2

# open_capped <file> <label> — sets mode (absent|read|bad) and fd. The file is
# opened once and every later read goes through that fd, so a swap after the
# checks cannot change what jq sees. A rejected file records a warning.
shell_warnings=()
open_capped() {
  mode=read fd=
  if [ ! -e "$1" ]; then
    mode=absent
  elif [ ! -f "$1" ] || ! { exec {fd}<"$1"; } 2>/dev/null || [ ! -f "/dev/fd/$fd" ]; then
    mode=bad fd=
    shell_warnings+=("model-resolve: $2 is not a readable regular file; ignoring it")
  fi
}
cat_cap=4194304 state_cap=65536
open_capped "$3" catalog
cat_mode=$mode cat_fd=$fd
open_capped "$4" class-state
state_mode=$mode state_fd=$fd

# capped_read <fd> <cap> — one byte past the cap is enough for jq to see the
# file is too large. No fd (an absent or rejected file) reads as empty.
capped_read() {
  [ -z "$1" ] || head -c $(($2 + 1)) <&"$1"
}

sw='[]'
if [ "${#shell_warnings[@]}" -gt 0 ]; then
  sw=$(printf '%s\n' "${shell_warnings[@]}" | jq -R . | jq -sc .)
fi

rc=0
jq -n --arg e "$engine" --arg t "$tier" \
  --arg catmode "$cat_mode" --rawfile catraw <(capped_read "$cat_fd" "$cat_cap") --argjson catcap "$cat_cap" \
  --arg statemode "$state_mode" --rawfile stateraw <(capped_read "$state_fd" "$state_cap") --argjson statecap "$state_cap" \
  --argjson sw "$sw" '
  def clean: gsub("[[:cntrl:]\\p{Cf}]"; "");
  def glob_re:
    "\\A" + (explode | map(
      if . == 42 then ".*" elif . == 63 then "."
      else ([.] | implode) | if test("\\A[A-Za-z0-9_]\\z") then . else "\\" + . end end
    ) | join("")) + "\\z";
  def effort_tail: "-(none|low|medium|high|xhigh|max)(-fast)?\\z";
  def shape_ok($e):
    length <= 200 and (
      if $e == "claude" then
        (test("\\A(opus|sonnet|haiku|fable)\\z") or test("\\Aclaude-[a-z0-9]+(-[a-z0-9]+)*\\z"))
        and (test(effort_tail) | not)
      elif $e == "codex" then test("\\Agpt-[0-9]+\\.[0-9]+-[a-z0-9]+\\z") or test("\\Agpt-5\\.[45]\\z")
      elif $e == "cursor" then
        test("\\A[a-z0-9][a-z0-9.-]*\\z") and (test("\\A(opus|sonnet|haiku|fable)\\z") | not)
        and (if test("\\A(claude|gpt)-") then test(effort_tail) else true end)
      elif $e == "pi" then test("\\A[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._/-]*\\z")
      else false end);
  def ver: split("/") | last | [scan("[0-9]+") | tonumber] | . as $v | [range(0; 8) | -($v[.] // -1)];
  def pglob: if type == "string" then . else .glob end;
  def rank_of: ["cheap", "standard", "premium"] | index(.);
  def undated: (test("-([0-9]{4}|[0-9]{8})\\z") or test("(\\A|[-./_])(preview|exp|beta)([-./_]|\\z)")) | not;

  [inputs]
  | if length != 1 or (.[0] | type) != "object" then "model-resolve: stdin must be one settings JSON object\n" | halt_error(2) else .[0] end
  | . as $s
  | ($s.modelMap[$e][$t]?) as $row
  | ($s.classes[$e].tiers[$t]?) as $tier
  | if ($row | type) != "object" or ($tier | type) != "object"
    then "model-resolve: no model row for engine \($e | clean) tier \($t | clean)\n" | halt_error(1)
    else . end
  | $tier.class as $class
  | ($tier.admits // [$class]) as $admits
  | $s.classes[$e] as $cl
  | $row.default as $seed_default
  | ($row.regex // []) as $regex
  | def pats($c): ($cl[$c] // []) | map(select(type == "string" or (type == "object" and (.glob | type) == "string")));
    ([["cheap", "standard", "premium"][] | {key: ., value: (pats(.) | to_entries | map(. + {re: (.value | pglob | glob_re)}))}] | from_entries) as $cp
  | ($row.models | map(glob_re)) as $seedre
  | def cmatches($id; $c): ($cp[$c] // []) | map(select(.re as $r | $id | test($r)));
    def class_of($id):
      ["premium", "standard", "cheap"] | map(select(. as $c | cmatches($id; $c) | length > 0)) | first // null;
    def seedm($id):
      ($seedre | any(.[]; . as $r | $id | test($r))) or ($regex | any(.[]; . as $r | try ($id | test($r)) catch false));

    (($catraw | utf8bytelength) > $catcap) as $cat_big
  | (($stateraw | utf8bytelength) > $statecap) as $state_big
  | (if $cat_big then ["model-resolve: catalog is larger than \($catcap) bytes; ignoring it"] else [] end) as $w_catbig
  | (if $state_big then ["model-resolve: class-state is larger than \($statecap) bytes; ignoring it"] else [] end) as $w_statebig
  | ($catmode == "read" and ($cat_big | not)) as $cat_read
  | ($statemode == "read" and ($state_big | not)) as $state_read
  | ($catraw | if $cat_read then (try fromjson catch null) else null end) as $cobj
  | ($stateraw | if $state_read then (try fromjson catch null) else null end) as $sobj
  | (if $cat_read and ($cobj | type) != "object" then ["model-resolve: catalog is not one JSON object; ignoring it"] else [] end) as $w_cat
  | (if $state_read and ($sobj | type) != "object" then ["model-resolve: class-state is not one JSON object; ignoring it"] else [] end) as $w_state
  | (if ($cobj | type) == "object" and ($cobj | has($e)) then $cobj[$e] else null end) as $snap
  | (if ($cobj | type) == "object" and ($cobj | has($e)) and (($snap | type) != "object" or ($snap.models | type) != "array")
     then ["model-resolve: catalog entry for engine \($e | clean) has no models array; ignoring it"] else [] end) as $w_snap
  | (if ($snap | type) == "object" and ($snap.models | type) == "array" then $snap.models else null end) as $models0
  | (if ($models0 | length) > 5000
     then ["model-resolve: catalog entry for engine \($e | clean) has more than 5000 models; ignoring it"] else [] end) as $w_many
  | (if ($w_many | length) > 0 then null else $models0 end) as $models
  | (($models // []) | map(select(type == "object" and (.id | type) == "string"))) as $entries
  | ($entries | map(.id)) as $catalog_ids

  # Candidates: shape-clean catalog ids of an admitted class, first duplicate wins.
  | ($entries
     | map(select(.id | shape_ok($e)))
     | reduce .[] as $m ({seen: {}, out: []}; if .seen[$m.id] then . else .seen[$m.id] = true | .out += [$m] end)
     | .out
     | map(.id as $id | class_of($id) as $k
         | select($k != null and ($admits | index($k)) != null)
         | cmatches($id; $k) as $m
         | (if (.created | type) == "number" then [0, -.created] else [1, 0] end) as $ck
         | ($id | ver) as $v
         | {id: $id, cls: $k, rank: $m[0].key,
            cond: ($m | all(.value | type == "object")),
            requires: (first($m[] | select(.value | type == "object") | .value.requires) // null),
            key: [($admits | index($k)), $m[0].key] + (if $e == "pi" then [$ck, $v, $id] else [$v, $ck, $id] end)})
    ) as $cands
  | ($cands | map(select(.cond | not))) as $uncond

  # Default: honour the recorded incumbent only inside the row class.
  | (($sobj | type) == "object" and ($sobj | has($e)) and ($sobj[$e] | type) == "object" and ($sobj[$e] | has($class))) as $has_rec
  | (if $has_rec then $sobj[$e][$class] | (if type == "object" then .default else null end) else null end) as $rec
  | (if $has_rec and ($rec | type) != "string"
     then ["model-resolve: \($e | clean) \($t | clean): recorded \($class | clean) default is not a string; using seed default \($seed_default | clean)"] else [] end) as $w_rec_type
  | (($rec | type) == "string"
     and ($rec | shape_ok($e))
     and ($rec == $seed_default or (cmatches($rec; $class) | length > 0))
     and (class_of($rec) as $k | $k == null or ($k | rank_of) <= ($class | rank_of))
     and (seedm($rec) or ($uncond | any(.id == $rec)))) as $rec_ok
  | (if ($rec | type) == "string" and ($rec_ok | not)
     then ["model-resolve: \($e | clean) \($t | clean): recorded \($class | clean) default \($rec | clean | .[0:120]) is not valid for the class; using seed default \($seed_default | clean)"] else [] end) as $w_rec
  | (if $rec_ok then $rec else $seed_default end) as $d0
  | (if $models != null and ($catalog_ids | index($d0)) == null
     then if seedm($d0)
          then {d: $d0, w: ["model-resolve: \($e | clean) \($t | clean): default \($d0 | clean) is not in the catalog; keeping it (seed match)"]}
          else {d: $seed_default, w: ["model-resolve: \($e | clean) \($t | clean): default \($d0 | clean) is not in the catalog; using seed default \($seed_default | clean)"]} end
     else {d: $d0, w: []} end) as $dd
  | $dd.d as $default

  # Trial: the newest non-dated match of the row class, only if it sorts before
  # the default. A default outside the candidates is keyed as a candidate would be.
  | ($uncond | map(select(.cls == $class))) as $rowc
  | ($rowc | map(select(.id | undated)) | sort_by(.key) | first) as $head
  | (if ($rowc | any(.id == $default))
     then ($rowc | map(select(.id == $default)) | first | .key)
     else cmatches($default; $class) as $dm
       | if ($dm | length) > 0
         then [($admits | index($class)), $dm[0].key] + (if $e == "pi" then [[1, 0], ($default | ver), $default] else [($default | ver), [1, 0], $default] end)
         else null end
     end) as $dkey
  | {
      admitted: ($row.models + ($uncond | map(select(seedm(.id) | not)) | sort_by(.key) | map(.id))),
      regex: $regex,
      conditional: ($cands | map(select(.cond and (seedm(.id) | not))) | sort_by(.key) | map({id, requires})),
      default: $default,
      trial: (if $head == null or ($dkey != null and ($head.key < $dkey | not)) then null else $head.id end),
      warnings: ($sw + $w_catbig + $w_statebig + $w_cat + $w_state + $w_snap + $w_many + $w_rec_type + $w_rec + $dd.w | map(clean))
    }' || rc=$?

[ -z "$cat_fd" ] || exec {cat_fd}<&-
[ -z "$state_fd" ] || exec {state_fd}<&-
exit "$rc"
