bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# The fleet hold queue end to end, through the real crew binary: two registered
# repos, one live dispatcher and one fleet hold each, a stubbed dispatch-config
# for the budget and a fake tmux for the live lead panes. At most one hold is
# due machine-wide, whichever repo's dispatcher asks.

setup() {
  load helpers
  setup_repo
  unset TMUX TMUX_PANE
  REPO_A="$TEST_REPO"
  REPO_B="$(mktemp -d)"
  git init -q -b main "$REPO_B"
  CREW_A=crew-a
  CREW_B=crew-b
  COMMON_A="$(git -C "$REPO_A" rev-parse --path-format=absolute --git-common-dir)"
  COMMON_B="$(git -C "$REPO_B" rev-parse --path-format=absolute --git-common-dir)"
  export XDG_STATE_HOME="$BATS_TEST_TMPDIR/state"
  DUE_RECORD="$XDG_STATE_HOME/crew/fleet-due.json"

  # Both repos are registered, each with a dispatcher whose pid file names this
  # test's own, live, process.
  mkdir -p "$XDG_DATA_HOME/crew"
  printf '%s\n' "$COMMON_A" "$COMMON_B" >"$XDG_DATA_HOME/crew/repos"
  mkdir -p "$COMMON_A/crew/crews/$CREW_A" "$COMMON_B/crew/crews/$CREW_B"
  printf '%s\n' "$$" | tee "$COMMON_A/crew/crews/$CREW_A/pid" >"$COMMON_B/crew/crews/$CREW_B/pid"

  # fleetBudget 4: a deep lead (weight 3) leaves 1 free, which no standard hold
  # (weight 2) fits; with no lead both would, so only one-due-at-a-time can
  # keep the second out.
  printf '#!/bin/sh\necho %s\n' "'{\"fleetBudget\": 4}'" >"$BATS_TEST_TMPDIR/dispatch-config"
  chmod +x "$BATS_TEST_TMPDIR/dispatch-config"
  export DISPATCH_CONFIG_BIN="$BATS_TEST_TMPDIR/dispatch-config"

  LEAD_DIR="$BATS_TEST_TMPDIR/lead"
  mkdir -p "$LEAD_DIR"
  printf 'tier: deep\nkind: implement\n' >"$LEAD_DIR/WORKER_TASK.md"
  export FAKE_TMUX_PANES="$BATS_TEST_TMPDIR/panes"
  mkdir -p "$BATS_TEST_TMPDIR/fakebin"
  cat >"$BATS_TEST_TMPDIR/fakebin/tmux" <<'EOF'
#!/bin/sh
# Only the lead-pane scan is shown rows; the sender tripwire's read gets none.
if [ "$1 $2 $3" = "list-panes -a -F" ]; then
  case "$4" in
  '#{pane_current_command}'*) cat "$FAKE_TMUX_PANES" ;;
  esac
  exit 0
fi
exit 1
EOF
  chmod +x "$BATS_TEST_TMPDIR/fakebin/tmux"
  export PATH="$BATS_TEST_TMPDIR/fakebin:$PATH"
  lead_pane_live
}

teardown() {
  rm -rf "${REPO_B:-}"
  teardown_repo
}

# lead_pane_live / lead_pane_gone — the fake tmux lists one claude lead on a
# deep worktree, or nothing.
lead_pane_live() {
  printf 'claude\x1fcaptain\x1ffeat/lead\x1f\x1f%s\n' "$LEAD_DIR" >"$FAKE_TMUX_PANES"
}
lead_pane_gone() { : >"$FAKE_TMUX_PANES"; }

# add_hold <repo> <crew> [hold add flags] — one standard fleet hold for the
# crew's branch, added from inside its repo.
add_hold() {
  local repo="$1" crew="$2"
  shift 2
  (cd "$repo" && "$CREW" hold add --fleet --agent claude --ref 1 --branch "feat/$crew" \
    --tier standard --model sonnet --effort medium --crew "$crew" "$@" "hold for $crew")
}

# one_hold_each — A's hold is older than B's, so A is first in order.
one_hold_each() {
  HOLD_A="$(add_hold "$REPO_A" "$CREW_A")"
  sleep 0.05
  HOLD_B="$(add_hold "$REPO_B" "$CREW_B")"
}

# hold_due <repo> <crew> — `crew hold due --json` as the crew's dispatcher
# runs it, from inside its repo.
hold_due() {
  cd "$1" || return 1
  "$CREW" hold due --crew "$2" --json
}

@test "a full budget leaves every fleet hold waiting" {
  one_hold_each

  run --separate-stderr hold_due "$REPO_A" "$CREW_A"
  [ "$status" -eq 1 ]
  [ "$(jq -c '.due' <<<"$output")" = "[]" ]

  run --separate-stderr hold_due "$REPO_B" "$CREW_B"
  [ "$status" -eq 1 ]
  [ "$(jq -c '.due' <<<"$output")" = "[]" ]

  run --separate-stderr "$CREW" fleet queue --json
  [ "$status" -eq 0 ]
  [ "$(jq -c '.due' <<<"$output")" = "null" ]
  [ "$(jq -r '.holds | map(.state) | unique | join(",")' <<<"$output")" = "waiting" ]
}

@test "free capacity makes exactly one hold due, the first in order" {
  one_hold_each
  lead_pane_gone

  run --separate-stderr hold_due "$REPO_A" "$CREW_A"
  [ "$status" -eq 0 ]
  [ "$(jq -r '.due | map(.id) | join(",")' <<<"$output")" = "$HOLD_A" ]

  run --separate-stderr hold_due "$REPO_B" "$CREW_B"
  [ "$status" -eq 1 ]
  [ "$(jq -c '.due' <<<"$output")" = "[]" ]

  run --separate-stderr "$CREW" fleet queue --json
  [ "$status" -eq 0 ]
  [ "$(jq -r '.due.id' <<<"$output")" = "$HOLD_A" ]
  [ "$(jq -r '.holds | map(.id + ":" + .state) | join(",")' <<<"$output")" = "$HOLD_A:due,$HOLD_B:waiting" ]
}

@test "a repo priority puts the other repo's hold first" {
  one_hold_each
  lead_pane_gone

  run --separate-stderr "$CREW" fleet prio "$REPO_B" 5
  [ "$status" -eq 0 ]

  run --separate-stderr hold_due "$REPO_A" "$CREW_A"
  [ "$status" -eq 1 ]
  [ "$(jq -c '.due' <<<"$output")" = "[]" ]

  run --separate-stderr hold_due "$REPO_B" "$CREW_B"
  [ "$status" -eq 0 ]
  [ "$(jq -r '.due | map(.id) | join(",")' <<<"$output")" = "$HOLD_B" ]

  run --separate-stderr "$CREW" fleet queue --json
  [ "$(jq -r '.due.id' <<<"$output")" = "$HOLD_B" ]
}

@test "a high hold priority outranks an older normal one" {
  HOLD_A="$(add_hold "$REPO_A" "$CREW_A")"
  sleep 0.05
  HOLD_B="$(add_hold "$REPO_B" "$CREW_B" --prio high)"
  lead_pane_gone

  run --separate-stderr hold_due "$REPO_A" "$CREW_A"
  [ "$status" -eq 1 ]

  run --separate-stderr hold_due "$REPO_B" "$CREW_B"
  [ "$status" -eq 0 ]
  [ "$(jq -r '.due | map(.id) | join(",")' <<<"$output")" = "$HOLD_B" ]
}

@test "nothing is due while the host load is over its cores" {
  one_hold_each
  lead_pane_gone
  printf '%s 0.00 0.00 1/1 1\n' "$(($(nproc) + 8))" >"$DISPATCH_LOADAVG_FILE"

  run --separate-stderr hold_due "$REPO_A" "$CREW_A"
  [ "$status" -eq 1 ]
  [ "$(jq -c '.due' <<<"$output")" = "[]" ]

  run --separate-stderr "$CREW" fleet queue --json
  [ "$(jq -r '.load_over' <<<"$output")" = "true" ]
  [ "$(jq -c '.due' <<<"$output")" = "null" ]
}

@test "only a dispatcher's own hold due run records the demotion clock" {
  one_hold_each
  lead_pane_gone

  run --separate-stderr "$CREW" fleet queue --json
  [ "$status" -eq 0 ]
  [ "$(jq -r '.due.id' <<<"$output")" = "$HOLD_A" ]
  [ ! -e "$DUE_RECORD" ]

  CREW_WORKER_ID=worker:feat/x run --separate-stderr hold_due "$REPO_A" "$CREW_A"
  [ "$status" -eq 0 ]
  [ ! -e "$DUE_RECORD" ]

  run --separate-stderr hold_due "$REPO_A" "$CREW_A"
  [ "$status" -eq 0 ]
  [ -f "$DUE_RECORD" ]
  [ "$(jq -r '.holds | keys | join(",")' "$DUE_RECORD")" = "$HOLD_A" ]
}
