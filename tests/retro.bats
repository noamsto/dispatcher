bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
  unset CREW_ID
}

teardown() {
  teardown_repo
}

# seed_dispatch <branch> <ts_ms> [engine] [model] [tier] — a dispatch event with
# an explicit ts, so the dispatch-boundary windowing fixtures control run
# ordering exactly (crew.sh has no `dispatch` subcommand — dispatch events are
# written by dispatch.sh, so tests seed them directly, as rate.bats does).
seed_dispatch() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg b "$1" --argjson ts "$2" --arg e "${3:-claude}" --arg m "${4:-sonnet}" --arg t "${5:-standard}" \
    '{ts:$ts, crew_id:"c1", kind:"dispatch", branch:$b, engine:$e, model:$m, tier:$t, effort:"medium", title:"t"}' >>"$logf"
}

# seed_status <from> <ts_ms> <state> — a status event with an explicit ts
# (`crew status` always stamps `now`, too coarse for the windowing fixtures).
seed_status() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg f "$1" --argjson ts "$2" --arg s "$3" \
    '{ts:$ts, crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status", body:{state:$s}}' >>"$logf"
}

# seed_msg <from> <to> <ts_ms> <body-json-string> — a msg event whose `body` is
# the JSON *string* form, which is what `crew msg` itself writes (the bus wart
# the reader handles with `fromjson`).
seed_msg() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg f "$1" --arg t "$2" --argjson ts "$3" --arg b "$4" \
    '{ts:$ts, crew_id:"c1", from:$f, to:$t, kind:"msg", body:$b}' >>"$logf"
}

HDR=$'branch\tengine\tmodel\ttier\toutcome\ttags'

# ---------------------------------------------------------------------------
# Fold and run attribution
# ---------------------------------------------------------------------------

@test "windowing: two dispatches on one branch keep their own notes" {
  seed_dispatch feat/reuse 1000
  seed_msg 'worker:feat/reuse#s1' retro:c1 1200 '{"seam":"execute","tag":"gate_thrash","detail":"first run"}'
  seed_status 'worker:feat/reuse#s1' 1500 failed
  seed_dispatch feat/reuse 2000
  seed_msg 'worker:feat/reuse#s2' retro:c1 2200 '{"seam":"execute","tag":"command_not_found","detail":"second run"}'
  seed_status 'worker:feat/reuse#s2' 2500 done

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$HDR" ]
  [ "${lines[1]}" = $'feat/reuse\tclaude\tsonnet\tstandard\tfailed\tgate_thrash' ]
  [ "${lines[2]}" = $'feat/reuse\tclaude\tsonnet\tstandard\tdone\tcommand_not_found' ]
  [ "${#lines[@]}" -eq 3 ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '
    (.tags | map({key: .tag, value: .}) | from_entries) as $t
    | $t.gate_thrash.hits == 1 and $t.gate_thrash.runs == 1
      and $t.gate_thrash.nondone_pct == 100
      and $t.command_not_found.hits == 1 and $t.command_not_found.runs == 1
      and $t.command_not_found.nondone_pct == 0' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "killed worker: seam notes survive a run with no terminal status" {
  seed_dispatch feat/killed 1000
  seed_status 'worker:feat/killed#s1' 1100 working
  seed_msg 'worker:feat/killed#s1' retro:c1 1200 '{"seam":"execute","tag":"gate_thrash","detail":"circled build"}'
  seed_dispatch feat/hardkill 3000
  seed_msg 'worker:feat/hardkill#s1' retro:c1 3200 '{"seam":"execute","tag":"rung_blocked","detail":"top rung"}'

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = $'feat/hardkill\tclaude\tsonnet\tstandard\t—\trung_blocked' ]
  [ "${lines[2]}" = $'feat/killed\tclaude\tsonnet\tstandard\tworking\tgate_thrash' ]
  [ "${#lines[@]}" -eq 3 ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '[.tags[] | select(.tag == "gate_thrash" or .tag == "rung_blocked")]
             | length == 2 and all(.hits == 1 and .runs == 1 and .nondone_pct == 100)' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "snapshot supersede: the later metrics snapshot wins and does not duplicate" {
  seed_dispatch feat/resumed 1000
  seed_msg 'worker:feat/resumed#s1' metrics:c1 1200 '{"rework_count":1,"notes":[{"seam":"execute","tag":"other","detail":"early"}]}'
  seed_msg 'worker:feat/resumed#s1' metrics:c1 1300 '{"rework_count":2,"notes":[{"seam":"execute","tag":"gate_thrash","detail":"late"}]}'
  seed_status 'worker:feat/resumed#s1' 1400 done

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = $'feat/resumed\tclaude\tsonnet\tstandard\tdone\tgate_thrash' ]
  [ "${#lines[@]}" -eq 2 ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '(.tags | length) == 1 and .tags[0].tag == "gate_thrash"
             and .tags[0].hits == 1 and .tags[0].details == ["late"]' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "cross-path dedupe: a note in both the seam msg and the snapshot counts once" {
  seed_dispatch feat/dedupe 1000
  seed_msg 'worker:feat/dedupe#s1' retro:c1 1200 '{"seam":"execute","tag":"gate_thrash","detail":"gate=build target=pkg/x"}'
  seed_msg 'worker:feat/dedupe#s1' metrics:c1 1300 '{"notes":[{"seam":"execute","tag":"gate_thrash","detail":"gate=build target=pkg/x"}]}'
  seed_status 'worker:feat/dedupe#s1' 1400 done

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = $'feat/dedupe\tclaude\tsonnet\tstandard\tdone\tgate_thrash' ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '(.tags | length) == 1 and .tags[0].hits == 1' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "a repeated tag inside one run is counted, not deduped" {
  seed_dispatch feat/twice 1000
  seed_msg 'worker:feat/twice#s1' retro:c1 1200 '{"seam":"execute","tag":"command_not_found","detail":"no just"}'
  seed_msg 'worker:feat/twice#s1' retro:c1 1250 '{"seam":"execute","tag":"command_not_found","detail":"no golangci-lint"}'
  seed_msg 'worker:feat/twice#s1' retro:c1 1260 '{"seam":"execute","tag":"gate_thrash","detail":"circled"}'
  seed_status 'worker:feat/twice#s1' 1400 failed

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = $'feat/twice\tclaude\tsonnet\tstandard\tfailed\tcommand_not_found x2,gate_thrash' ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '(.tags | map({key: .tag, value: .}) | from_entries) as $t
             | $t.command_not_found.hits == 2 and $t.command_not_found.runs == 1' <<<"$output"
  [ "$status" -eq 0 ]
}

# ---------------------------------------------------------------------------
# Vocabulary drift
# ---------------------------------------------------------------------------

@test "unknown tag: reported with its value, listed in --json's unknown, never dropped" {
  seed_dispatch feat/unknown 1000
  seed_msg 'worker:feat/unknown#s1' retro:c1 1200 '{"seam":"execute","tag":"gate_thrashh","detail":"typo tag"}'
  seed_msg 'worker:feat/unknown#s1' retro:c1 1250 '{"seam":"execute","tag":"oops","detail":"bare guess"}'
  seed_msg 'worker:feat/unknown#s1' retro:c1 1260 '{"seam":"execute","tag":"gate_thrash","detail":"real one"}'
  seed_status 'worker:feat/unknown#s1' 1400 failed

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = $'feat/unknown\tclaude\tsonnet\tstandard\tfailed\tgate_thrashh,oops,gate_thrash' ]

  run run_crew retro --report
  [ "$status" -eq 0 ]
  # Known tags first in vocabulary order, then the unknown ones alphabetically.
  [[ "${lines[1]}" == gate_thrash\ * ]]
  [[ "${lines[2]}" == gate_thrashh\ * ]]
  [[ "${lines[3]}" == oops\ * ]]
  [ "${lines[4]}" = "2 unrecognized tag(s): gate_thrashh, oops" ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '.unknown == ["gate_thrashh", "oops"]
             and ([.tags[] | select(.known | not) | .hits] == [1, 1])' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "no unknown tags leaves the --report footer off and --json's unknown empty" {
  seed_dispatch feat/clean-vocab 1000
  seed_msg 'worker:feat/clean-vocab#s1' retro:c1 1200 '{"seam":"execute","tag":"other","detail":"d"}'
  seed_status 'worker:feat/clean-vocab#s1' 1400 done

  run run_crew retro --report
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 2 ]
  [[ "$output" != *unrecognized* ]]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '.unknown == []' <<<"$output"
  [ "$status" -eq 0 ]
}

# ---------------------------------------------------------------------------
# Silence, dispatcher notes, robustness
# ---------------------------------------------------------------------------

@test "a clean run is silent in all three modes" {
  seed_dispatch feat/clean 1000
  seed_status 'worker:feat/clean#s1' 1100 working
  seed_status 'worker:feat/clean#s1' 1400 done
  seed_msg 'worker:feat/clean#s1' metrics:c1 1450 '{"rework_count":0,"notes":[]}'

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "$output" = "$HDR" ]

  run run_crew retro --report
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 1 ]
  [ "$output" = "tag  hits  runs  engines  nondone  sample" ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '.tags == [] and .unknown == []' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "dispatcher notes: their own row, em-dash columns, and no run attribution" {
  seed_dispatch feat/w 1000
  seed_msg 'worker:feat/w#s1' retro:c1 1200 '{"seam":"execute","tag":"gate_thrash","detail":"d"}'
  seed_status 'worker:feat/w#s1' 1400 done
  seed_msg dispatcher:c1 retro:c1 5000 '{"seam":"drained","tag":"misrouted","detail":"trivial, but the diff touched auth"}'
  seed_msg dispatcher:c1 retro:c1 5100 '{"seam":"drained","tag":"session_summary","detail":"one worker, one thrash"}'

  run run_crew retro
  [ "$status" -eq 0 ]
  # Dispatcher rows sort last.
  [ "${lines[1]}" = $'feat/w\tclaude\tsonnet\tstandard\tdone\tgate_thrash' ]
  [ "${lines[2]}" = $'dispatcher:c1\t—\t—\t—\t—\tmisrouted,session_summary' ]
  [ "${#lines[@]}" -eq 3 ]

  run run_crew retro --report
  [ "$status" -eq 0 ]
  # misrouted is a dispatcher tag: runs 0, no engines, no non-done share.
  [[ "$output" == *"misrouted           1     0  —              —  trivial, but the diff touched auth"* ]]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '(.tags | map({key: .tag, value: .}) | from_entries) as $t
             | $t.misrouted.runs == 0 and $t.misrouted.engines == []
               and $t.misrouted.nondone_pct == null
               and $t.session_summary.runs == 0' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "an unparseable note body does not abort the fold" {
  seed_dispatch feat/broken 1000
  seed_msg 'worker:feat/broken#s1' retro:c1 1200 'not json at all'
  seed_status 'worker:feat/broken#s1' 1400 failed
  seed_dispatch feat/good 2000
  seed_msg 'worker:feat/good#s1' retro:c1 2200 '{"seam":"execute","tag":"consult_failed","detail":"codex refused"}'
  seed_status 'worker:feat/good#s1' 2400 done

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = $'feat/good\tclaude\tsonnet\tstandard\tdone\tconsult_failed' ]
  [ "${#lines[@]}" -eq 2 ]
}

@test "a malformed status body costs one run its outcome, not the whole fold" {
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  # An object body whose state is itself an object — sorts before every other
  # branch here, so a fold that died on it would take the good row down too.
  seed_dispatch feat/aobjstate 500
  seed_msg 'worker:feat/aobjstate#s1' retro:c1 700 '{"seam":"execute","tag":"gate_thrash","detail":"must survive"}'
  jq -nc '{ts:800, crew_id:"c1", from:"worker:feat/aobjstate#s1",
           to:"dispatcher:c1", kind:"status", body:{state:{weird:true}}}' >>"$logf"
  seed_dispatch feat/badstatus 1000
  seed_msg 'worker:feat/badstatus#s1' retro:c1 1200 '{"seam":"execute","tag":"gate_thrash","detail":"still visible"}'
  # `crew status` cannot write this, but a hand-edited or truncated bus can.
  jq -nc '{ts:1300, crew_id:"c1", from:"worker:feat/badstatus#s1",
           to:"dispatcher:c1", kind:"status", body:"oops-not-an-object"}' >>"$logf"
  seed_dispatch feat/other 2000
  seed_msg 'worker:feat/other#s1' retro:c1 2200 '{"seam":"execute","tag":"consult_failed","detail":"codex refused"}'
  seed_status 'worker:feat/other#s1' 2400 done

  run run_crew retro
  [ "$status" -eq 0 ]
  # Both corrupt bodies degrade to the same em dash a status-less run gets.
  [ "${lines[1]}" = $'feat/aobjstate\tclaude\tsonnet\tstandard\t—\tgate_thrash' ]
  [ "${lines[2]}" = $'feat/badstatus\tclaude\tsonnet\tstandard\t—\tgate_thrash' ]
  [ "${lines[3]}" = $'feat/other\tclaude\tsonnet\tstandard\tdone\tconsult_failed' ]
  [ "${#lines[@]}" -eq 4 ]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '[.tags[] | .tag] == ["gate_thrash", "consult_failed"]' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "control characters in a tag or detail never reach a rendered surface" {
  esc="$(printf '\033')"
  nel="$(printf '\302\205')"     # U+0085 NEL
  del="$(printf '\177')"         # U+007F DEL
  rlo="$(printf '\342\200\256')" # U+202E RLO
  lri="$(printf '\342\201\246')" # U+2066 LRI
  seed_dispatch feat/inject 1000
  # Cursor-up + erase-line: raw, this repaints the row above it in the table.
  # NEL/DEL/RLO/LRI tacked on the end cover the multibyte C1 range and the
  # other stripped classes, alongside the ESC sequence, in one detail.
  seed_msg 'worker:feat/inject#s1' retro:c1 1200 \
    "$(jq -nc --arg d "benign${esc}[1A${esc}[2KFAKE ROW${esc}[0m${nel}N${del}D${rlo}R${lri}I" \
       '{seam:"execute", tag:"gate_thrash", detail:$d}')"
  seed_msg 'worker:feat/inject#s1' retro:c1 1250 \
    "$(jq -nc --arg t 'drift
tag	x' '{seam:"execute", tag:$t, detail:"tag carries a newline and a tab"}')"
  seed_status 'worker:feat/inject#s1' 1400 done

  run run_crew retro --report
  [ "$status" -eq 0 ]
  [[ "$output" != *"$esc"* ]]
  # Header + two rows + footer: the tag no longer splits its own row.
  [ "${#lines[@]}" -eq 4 ]
  # Sample is the last column, so its exact suffix proves NEL/DEL/RLO/LRI are
  # all gone, not merely absent from a truncated prefix.
  [[ "${lines[1]}" == *"benign[1A[2KFAKE ROW[0mNDRI" ]]
  [[ "${lines[2]}" == "drift tag x"* ]]
  [ "${lines[3]}" = "1 unrecognized tag(s): drift tag x" ]

  run run_crew retro
  [ "$status" -eq 0 ]
  [[ "$output" != *"$esc"* ]]
  [ "${lines[1]}" = $'feat/inject\tclaude\tsonnet\tstandard\tdone\tgate_thrash,drift tag x' ]

  # --json keeps the detail verbatim; its encoder is what renders the byte inert.
  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  [[ "$output" != *"$esc"* ]]
  run jq -e '.unknown == ["drift tag x"]' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "--report rows carry no trailing whitespace" {
  seed_dispatch feat/pad 1000
  seed_msg 'worker:feat/pad#s1' retro:c1 1200 '{"seam":"execute","tag":"gate_thrash","detail":"short"}'
  seed_msg 'worker:feat/pad#s1' retro:c1 1250 '{"seam":"execute","tag":"command_not_found","detail":"a sample long enough to widen the last column"}'
  seed_status 'worker:feat/pad#s1' 1400 done

  run run_crew retro --report
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 3 ]
  for line in "${lines[@]}"; do
    [[ "$line" != *" " ]]
  done
}

@test "oversized gate_thrash detail: one parseable line through the real crew msg" {
  seed_dispatch feat/big 1000
  big="$(printf 'ledger-row-%.0s' $(seq 1 600))"
  note="$(jq -nc --arg d "$big" '{seam:"execute", tag:"gate_thrash", detail:$d}')"
  [ "${#note}" -gt 4096 ]
  CREW_ID=c1 run_crew msg 'worker:feat/big#s1' retro:c1 "$note"

  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  # The #20/#55 guard: the note lands as ONE record inside _LINE_MAX, and
  # _shrink cuts the string leaf rather than the object, so the body still
  # parses and the tag survives.
  run wc -l <"$logf"
  [ "$output" -eq 2 ]
  run jq -e -s 'length == 2 and (.[1].body | fromjson | .tag) == "gate_thrash"' "$logf"
  [ "$status" -eq 0 ]
  run awk 'END { print longest } { if (length($0) > longest) longest = length($0) }' "$logf"
  [ "$output" -le 4096 ]

  run run_crew retro
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = $'feat/big\tclaude\tsonnet\tstandard\t—\tgate_thrash' ]

  # The report truncates the sample; --json keeps the shrunken detail whole.
  run run_crew retro --report
  [ "$status" -eq 0 ]
  [[ "${lines[1]}" == *"…" ]]

  run run_crew retro --report --json
  [ "$status" -eq 0 ]
  run jq -e '.tags[0].details[0] | length > 60' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "the retro sink does not wake the dispatcher" {
  CREW_ID=c1 run_crew msg 'worker:feat/sink#s1' retro:c1 '{"seam":"execute","tag":"other","detail":"d"}'
  CREW_ID=c1 run_crew msg dispatcher:c1 retro:c1 '{"seam":"drained","tag":"session_summary","detail":"d"}'

  CREW_ID=c1 run run_crew inbox dispatcher:c1
  [ "$status" -eq 0 ]
  [ -z "$output" ]

  CREW_ID=c1 run --separate-stderr run_crew watch --since 0 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

# ---------------------------------------------------------------------------
# Flags
# ---------------------------------------------------------------------------

@test "--json without --report is an error" {
  run --separate-stderr run_crew retro --json
  [ "$status" -eq 1 ]
  [ "$stderr" = "crew: retro takes --report and --json" ]
}

@test "an unknown flag or a positional argument is the same error" {
  run --separate-stderr run_crew retro --bogus
  [ "$status" -eq 1 ]
  [ "$stderr" = "crew: retro takes --report and --json" ]

  run --separate-stderr run_crew retro c1
  [ "$status" -eq 1 ]
  [ "$stderr" = "crew: retro takes --report and --json" ]
}

@test "a missing bus exits 0 with no output" {
  run --separate-stderr run_crew retro
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [ -z "$stderr" ]

  run --separate-stderr run_crew retro --report
  [ "$status" -eq 0 ]
  [ -z "$output" ]

  run --separate-stderr run_crew retro --report --json
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}
