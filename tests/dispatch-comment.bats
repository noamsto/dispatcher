bats_require_minimum_version 1.5.0

# Coverage for `_post_dispatch_comment`: a best-effort `gh issue comment`
# posted on a GitHub-issue dispatch, carrying worker context, that must
# never abort the dispatch and must never fire for Linear or --pr dispatches.

setup() {
  load helpers
  DISPATCH="$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
  export CREW_REAL="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_dispatch() { bash -euo pipefail "$DISPATCH" "$@"; }
  setup_repo
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_IGNORE_RUNG DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE DISPATCH_DRAFT_PR
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
}

teardown() {
  teardown_repo
}

# Copied from tests/dispatch.bats: a launch far enough to reach tmux
# send-keys (worktree + identity + new-window).
stub_launch_bins() {
  git -C "$TEST_REPO" commit --allow-empty -q -m init

  git init -q --bare "$TEST_REPO/origin.git"
  git -C "$TEST_REPO" remote add origin "$TEST_REPO/origin.git"
  git -C "$TEST_REPO" push -q origin main

  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
repo\ view\ *) printf '%s\n' "${STUB_DEFAULT_BRANCH:-main}" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"

  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = switch ]; then
  br=""
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
    -c)
      br="$2"
      shift 2
      ;;
    *) shift ;;
    esac
  done
  [ -n "$br" ] || exit 1
  dest="$TEST_REPO/.dispatch-wt/${br//\//-}"
  mkdir -p "$(dirname "$dest")"
  git -C "$TEST_REPO" worktree add -b "$br" "$dest" HEAD >/dev/null
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"

  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = new-window ]; then
  printf '%s %s\n' '%1' '%1'
fi
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"

  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
identity) printf '%s\n' '{"name":"iris","color":"blue","tmux":"colour33"}' ;;
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

# Copied from tests/dispatch.bats.
stub_crew_gate() {
  printf '%s' "$1" >"$STUB_DIR/occ.json"
  printf '%s' "$2" >"$STUB_DIR/sess.json"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
identity) printf '%s\n' '{"name":"sage","color":"green","tmux":"colour28"}' ;;
occupants) cat "$STUB_DIR/occ.json" ;;
sessions) cat "$STUB_DIR/sess.json" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

# Copied from tests/dispatch.bats: an existing branch WITH its worktree
# already on disk, so a re-dispatch onto it resumes rather than creates.
setup_resume_branch() { # <branch>
  stub_launch_bins
  git -C "$TEST_REPO" branch "$1"
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.dispatch-wt/${1//\//-}" "$1"
  stub_crew_gate '[]' '[]'
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
}

# Copied from tests/dispatch.bats.
wt_path_for() {
  git -C "$TEST_REPO" worktree list --porcelain |
    awk -v b="refs/heads/$1" '/^worktree /{p=$2} $0=="branch "b{print p}'
}

# stub_gh_full <existing-issue-labels> <mint-issue-number> — extends
# dispatch.bats' stub_gh_claim with `gh issue comment` capture: each call
# appends the issue arg to $STUB_DIR/comment_issues.log (one line per call,
# so a test can assert call count) and overwrites $STUB_DIR/comment_body.txt
# with that call's --body value (only one call is ever expected, so
# "overwrite" is equivalent to "record"). This sidesteps parsing the
# multiline body back out of $STUB_LOG, where it sits inline with every other
# stubbed invocation. $STUB_COMMENT_EXIT (default 0) controls the stubbed
# exit status for the comment call only, so a test can prove a failing
# comment does not abort the dispatch.
stub_gh_full() {
  printf '%s' "$1" >"$STUB_DIR/gh_labels.txt"
  printf '%s' "$2" >"$STUB_DIR/gh_mint_num.txt"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
issue\ view\ *)
  cat "$STUB_DIR/gh_labels.txt"
  ;;
issue\ create\ *)
  num="$(cat "$STUB_DIR/gh_mint_num.txt")"
  printf 'https://github.com/o/r/issues/%s\n' "$num"
  ;;
repo\ view\ *) printf '%s\n' "${STUB_DEFAULT_BRANCH:-main}" ;;
esac
if [ "$1" = issue ] && [ "$2" = comment ]; then
  printf '%s\n' "$3" >>"$STUB_DIR/comment_issues.log"
  printf '%s' "$5" >"$STUB_DIR/comment_body.txt"
  exit "${STUB_COMMENT_EXIT:-0}"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
}

@test "posts one gh issue comment with full worker context on an existing GitHub issue dispatch" {
  stub_launch_bins
  stub_gh_full "" ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "context comment test"
  [ "$status" -eq 0 ]

  [ -f "$STUB_DIR/comment_issues.log" ]
  [ "$(wc -l <"$STUB_DIR/comment_issues.log")" -eq 1 ]
  [ "$(cat "$STUB_DIR/comment_issues.log")" = 42 ]

  branch="feat/42-context-comment-test"
  wt_path="$(wt_path_for "$branch")"
  worker_id="$(printf '%s' "$output" | grep '^worker_id: ' | cut -d' ' -f2)"
  [ -n "$worker_id" ]
  [ -f "$wt_path/WORKER_TASK.md" ]
  crew_id_stamp="$(grep '^crew_id: ' "$wt_path/WORKER_TASK.md" | cut -d' ' -f2)"
  agent_name="$(grep '^agent_name: ' "$wt_path/WORKER_TASK.md" | cut -d' ' -f2)"
  [ "$crew_id_stamp" = c1 ]
  [ "$agent_name" = iris ]

  body="$(cat "$STUB_DIR/comment_body.txt")"
  [[ "$body" == *"$agent_name"* ]]
  [[ "$body" == *"claude · sonnet · standard"* ]]
  [[ "$body" == *"effort: medium"* ]]
  [[ "$body" == *"$branch"* ]]
  [[ "$body" == *"$wt_path"* ]]
  [[ "$body" == *"$worker_id"* ]]
  [[ "$body" == *"$crew_id_stamp"* ]]
  [ "$(tail -1 "$STUB_DIR/comment_body.txt")" = "<!-- dispatched -->" ]
}

@test "posts one gh issue comment for a newly minted GitHub issue" {
  stub_launch_bins
  stub_gh_full "" 77
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 "mint comment test"
  [ "$status" -eq 0 ]

  [ -f "$STUB_DIR/comment_issues.log" ]
  [ "$(wc -l <"$STUB_DIR/comment_issues.log")" -eq 1 ]
  [ "$(cat "$STUB_DIR/comment_issues.log")" = 77 ]
  [ "$(tail -1 "$STUB_DIR/comment_body.txt")" = "<!-- dispatched -->" ]
}

@test "never posts a gh issue comment for a Linear dispatch" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 ENG-1234 "linear comment test"
  [ "$status" -eq 0 ]
  [ ! -f "$STUB_DIR/comment_issues.log" ]
  run ! grep -q '^issue comment' "$STUB_LOG"
}

@test "never posts a gh issue comment for a --pr review dispatch" {
  git commit --allow-empty -qm init
  git branch eng-7691-foo
  export PR_HEAD=eng-7691-foo
  export PR_HEAD_OID
  PR_HEAD_OID="$(git rev-parse eng-7691-foo)"
  export PR_BASE=extract

  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
pr\ view\ *)
  printf '{"headRefName":"%s","headRefOid":"%s","baseRefName":"%s","isCrossRepository":false}\n' "$PR_HEAD" "$PR_HEAD_OID" "$PR_BASE"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"

  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
branch="$2"
mkdir -p "$TEST_REPO/.worktrees"
git worktree add -q "$TEST_REPO/.worktrees/$branch" "$branch"
exit 0
EOF
  chmod +x "$STUB_DIR/wt"

  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
identity) printf '%s\n' '{"name":"coral-fox","tmux":"colour1"}' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"

  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
new-window) printf '%s\n' '%1 %2' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "pr comment test"
  [ "$status" -eq 0 ]
  [ ! -f "$STUB_DIR/comment_issues.log" ]
  run ! grep -q '^issue comment' "$STUB_LOG"
}

@test "a resume dispatch's comment prefixes (resumed) before dispatched on its first line" {
  setup_resume_branch feat/42-do-a-thing
  stub_gh_full "" ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"resuming branch feat/42-do-a-thing"* ]]

  [ -f "$STUB_DIR/comment_issues.log" ]
  [ "$(wc -l <"$STUB_DIR/comment_issues.log")" -eq 1 ]

  first_line="$(head -1 "$STUB_DIR/comment_body.txt")"
  [[ "$first_line" == *"(resumed)"* ]]
  [[ "$first_line" == *"dispatched"* ]]
  resumed_idx="$(printf '%s' "$first_line" | grep -bo '(resumed)' | head -1 | cut -d: -f1)"
  dispatched_idx="$(printf '%s' "$first_line" | grep -bo 'dispatched' | head -1 | cut -d: -f1)"
  [ "$resumed_idx" -lt "$dispatched_idx" ]
}

@test "a failing gh issue comment warns but does not abort the dispatch" {
  stub_launch_bins
  stub_gh_full "" ""
  export STUB_COMMENT_EXIT=1
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "comment failure test"
  unset STUB_COMMENT_EXIT
  [ "$status" -eq 0 ]
  [[ "$output" == *"could not post dispatch-context comment on issue #42"* ]]
  grep -q 'issue edit 42 --add-label dispatched' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}
