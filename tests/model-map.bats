#!/usr/bin/env bats
# The model map as data (#560): the defaults.json lookups must reproduce, for
# every fixture row, the decision the pre-#560 gate code made (the fixtures were
# generated from 4c5ed21's own dispatch.sh/dispatch-resume.sh text).

setup() {
  load helpers
  DISPATCH="$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
  RESUME="$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  FIXTURES="$BATS_TEST_DIRNAME/fixtures/model-map"
  settings="$("$DISPATCH_CONFIG_BIN")"
  for fn in _glob_match _model_in_row _row_expected _escalation_hop _pace_downgrade; do
    eval "$(sed -n "/^${fn}() {/,/^}/p" "$DISPATCH")"
  done
}

teardown() {
  [ -z "${TEST_REPO:-}" ] || teardown_repo
}

# fixture_rows <file> — each data row as one line with its `-` cells emptied,
# fields separated by \x1f (tab is IFS-whitespace, so `read` would collapse the
# empty cells a tab separator leaves).
fixture_rows() {
  local line f i IFS=$'\x1f'
  while IFS= read -r line; do
    [[ $line == '#'* ]] && continue
    mapfile -d $'\t' -t f < <(printf '%s' "$line")
    for i in "${!f[@]}"; do
      [ "${f[i]}" != - ] || f[i]=""
    done
    printf '%s\n' "${f[*]}"
  done <"$1"
}

# report_mismatches — print every collected mismatch, fail when there is any.
report_mismatches() {
  [ "${#mismatches[@]}" -eq 0 ] && return 0
  printf '%s\n' "${#mismatches[@]} mismatches:" "${mismatches[@]}" >&2
  return 1
}

# dispatch_setup — tests/dispatch.bats's setup() body: stub bins and a
# protocols dir, enough for a dispatch to reach the tier gate.
dispatch_setup() {
  export CREW_REAL="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_dispatch() { bash -euo pipefail "$DISPATCH" "$@"; }
  setup_repo
  export HOME="$TEST_REPO"
  unset CREW_WORKER_ID DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_IGNORE_RUNG DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE DISPATCH_DRAFT_PR DISPATCH_REPO_TRACKERS DISPATCH_ORG_TRACKERS
  export STUB_PANE_PID=$$
  stub_bin tmux
  stub_bin crew
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  stub_bin gh
  stub_bin wt
  stub_bin direnv
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR"/{WORKER_PROTOCOL.md,EVIDENCE_REVIEW.md,GRID_PROTOCOL.md,REVIEW_TASK.md}
  export DISPATCHER_SKILLS_DIR="$TEST_REPO/harness-skills"
  mkdir -p "$DISPATCHER_SKILLS_DIR/spec-plan-critic"
  printf -- '---\nname: spec-plan-critic\ndescription: seeded\n---\n' \
    >"$DISPATCHER_SKILLS_DIR/spec-plan-critic/SKILL.md"
  export CROSS_REPO_HINT_LIB="$BATS_TEST_DIRNAME/../adapters/core/cross-repo-hint.sh"
}

@test "model map: the tier gate admits and describes each row as the old case arms did" {
  mismatches=()
  while IFS=$'\x1f' read -r -u 4 a t m ok expected; do
    got_ok=0
    _model_in_row "$a" "$t" "$m" && got_ok=1
    got_expected="$(_row_expected "$a" "$t")"
    [ "$got_ok" = "$ok" ] && [ "$got_expected" = "$expected" ] ||
      mismatches+=("$a $t $m: want $ok '$expected', got $got_ok '$got_expected'")
  done 4< <(fixture_rows "$FIXTURES/gate.tsv")
  report_mismatches
}

@test "model map: dispatch escalation admits, refuses and records as the old code did" {
  mismatches=()
  while IFS=$'\x1f' read -r -u 4 a t failed m kind label _; do
    if _model_in_row "$a" "$t" "$m"; then
      hop="$(_escalation_hop "$a" "$t" "$failed" "$m" inRow)"
      got="record|${hop:+$hop (record only)}"
    else
      hop="$(_escalation_hop "$a" "$t" "$failed" "$m" outOfRow)"
      if [ -n "$hop" ]; then got="admit|$hop"; else got="refuse|"; fi
    fi
    [ "$got" = "$kind|$label" ] || mismatches+=("$a $t $failed $m: want $kind|$label, got $got")
  done 4< <(fixture_rows "$FIXTURES/escalation.tsv")
  report_mismatches
}

@test "model map: resume escalation admits as the old dispatch-resume.sh code did" {
  unset -f _glob_match _escalation_hop
  for fn in _glob_match _escalation_hop; do
    def="$(sed -n "/^${fn}() {/,/^}/p" "$RESUME")"
    [ -n "$def" ]
    eval "$def"
  done
  mismatches=()
  while IFS=$'\x1f' read -r -u 4 a t failed m _ _ kind label; do
    hop="$(_escalation_hop "$a" "$t" "$failed" "$m" outOfRow)"
    if [ -n "$hop" ]; then got="admit|$hop"; else got="none|"; fi
    [ "$got" = "$kind|$label" ] || mismatches+=("$a $t $failed $m: want $kind|$label, got $got")
  done 4< <(fixture_rows "$FIXTURES/escalation.tsv")
  report_mismatches
}

@test "model map: pace downgrades each premium model as the old case did" {
  mismatches=()
  while IFS=$'\x1f' read -r -u 4 a m want; do
    got="$(_pace_downgrade "$a" "$m")"
    [ "$got" = "$want" ] || mismatches+=("$a $m: want '$want', got '$got'")
  done 4< <(fixture_rows "$FIXTURES/pace.tsv")
  report_mismatches
}

@test "model map: a dispatch refused by the tier gate prints the old refusal byte for byte" {
  dispatch_setup
  mismatches=()
  while IFS=$'\x1f' read -r -u 4 a t m want; do
    run run_dispatch "$t" "$m" --agent "$a" --effort high --crew-id c1 42 "t"
    got="$(grep -F "is not $t's row" <<<"$output" || true)"
    [ "$status" -eq 1 ] && [ "$got" = "$want" ] ||
      mismatches+=("$a $t $m (status $status): want '$want', got '$got'")
  done 4< <(fixture_rows "$FIXTURES/refusals.tsv")
  report_mismatches
}

# dispatch-resume.sh is a standalone build, so it carries its own copies.
@test "model map: the settings helpers are byte-identical between dispatch.sh and dispatch-resume.sh" {
  for fn in _settings_load _glob_match _escalation_hop; do
    a="$(sed -n "/^${fn}() {/,/^}/p" "$DISPATCH")"
    b="$(sed -n "/^${fn}() {/,/^}/p" "$RESUME")"
    [ -n "$a" ]
    [ "$a" = "$b" ]
  done
}
