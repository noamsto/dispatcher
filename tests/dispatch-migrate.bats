bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# dispatch migrate against the fake-ssh-host harness: tests/remote-host-helpers.bash.

load remote-host-helpers

setup() { remote_host_setup; }
teardown() { remote_host_teardown; }

BRANCH=feat/7-x

# seed_local_worker — feat/7-x exists here, dispatched on host laptop as w-first
# and resumed as w-local, the session crew migrate mark posts under.
seed_local_worker() {
  git -C "$TEST_REPO" branch "$BRANCH"
  mkdir -p "${LBUS%/*}"
  local now
  now="$(($(date +%s) * 1000))"
  jq -nc --arg b "$BRANCH" --argjson ts "$now" \
    '{ts: $ts, crew_id: "c1", kind: "dispatch", branch: $b, worker_id: "w-first", host: "laptop"}' >>"$LBUS"
  jq -nc --arg b "$BRANCH" --argjson ts "$((now + 1))" \
    '{ts: $ts, crew_id: "c1", kind: "resume", branch: $b, worker_id: "w-local", prev_worker_id: "w-first"}' >>"$LBUS"
}

mark_migrated_to() { # <host>
  (cd "$TEST_REPO" && crew migrate mark --crew c1 "$BRANCH" --to-host "$1" --mode coop \
    --tip "$(git rev-parse HEAD)" --tree "$(git rev-parse 'HEAD^{tree}')")
}

worker_status() { # <body json>
  jq -nc --argjson body "$1" '{ts: (now * 1000 | floor), crew_id: "c1", kind: "status", from: "w-local", to: "dispatcher:c1", body: $body}' >>"$LBUS"
}

refused_as_held_by() { # <holder text>
  [ "$status" -eq 1 ]
  [[ $stderr == *"issue #7 is already held by $1 — "* ]]
}

@test "a migrate mark releases the branch to its target host: --host mini passes the dup guard" {
  seed_local_worker
  mark_migrated_to mini
  [ "$(jq -c 'select(.kind == "status" and .body.source == "migrate") | [.from, .body.state, .body.to_host]' "$LBUS")" = '["w-local","migrated","mini"]' ]

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 7 x
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  jq -e --arg b "$BRANCH" 'select(.kind == "dispatch" and .remote == true) | .host == "mini" and .branch == $b' "$LBUS"
}

@test "a branch released to mini still blocks every other host" {
  seed_local_worker
  mark_migrated_to mini

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host chromebox 7 x
  refused_as_held_by "$BRANCH on host laptop (migrating to mini)"
}

@test "an exited backstop and a watchdog row after the mark keep the release" {
  seed_local_worker
  mark_migrated_to mini
  worker_status '{"state": "exited", "detail": "SessionEnd"}'
  worker_status '{"state": "blocked", "source": "watchdog", "detail": "quiet: no events for 10m"}'

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host chromebox 7 x
  refused_as_held_by "$BRANCH on host laptop (migrating to mini)"
}

@test "a working row from the worker after the mark ends the release" {
  seed_local_worker
  mark_migrated_to mini
  worker_status '{"state": "working", "detail": "resumed here"}'

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 7 x
  refused_as_held_by "$BRANCH on host laptop"
  [[ $stderr != *migrating* ]]
}

@test "a worker-posted migrated row without source migrate does not release" {
  seed_local_worker
  worker_status '{"state": "migrated", "to_host": "mini", "detail": "moving to mini"}'

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 7 x
  refused_as_held_by "$BRANCH on host laptop"
  [[ $stderr != *migrating* ]]
}

@test "a plain local dispatch of a branch released to mini is refused before any side effect" {
  seed_local_worker
  mark_migrated_to mini
  before="$(wc -l <"$LBUS")"

  TMUX_PANE=%0 run --separate-stderr dispatch standard sonnet --effort medium --crew-id c1 7 x
  [ "$status" -eq 1 ]
  [[ $stderr == *"dispatch: $BRANCH was migrated to host mini — dispatch --host mini … to resume it there, or dispatch resume $BRANCH here to abandon an unfinished migration"* ]]
  [ "$(wc -l <"$LBUS")" -eq "$before" ]
  [ ! -s "$CLAIMS" ]
}

@test "a plain local dispatch of a branch whose newest dispatch row is remote on mini is refused" {
  seed_local_worker
  jq -nc --arg b "$BRANCH" '{ts: (now * 1000 | floor), crew_id: "c1", kind: "dispatch", branch: $b, worker_id: "w-remote", host: "mini", remote: true}' >>"$LBUS"

  TMUX_PANE=%0 run --separate-stderr dispatch standard sonnet --effort medium --crew-id c1 7 x
  [ "$status" -eq 1 ]
  [[ $stderr == *"dispatch: $BRANCH was migrated to host mini — dispatch --host mini … to resume it there"* ]]
}
