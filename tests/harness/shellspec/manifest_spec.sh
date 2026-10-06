# shellcheck shell=bash

Describe 'canonical harness manifest'
It 'crew-id-crew-id-resolves-from-worker-task-md-with-no-crew-id-in-the-environment-at-all'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "crew-id-crew-id-resolves-from-worker-task-md-with-no-crew-id-in-the-environment-at-all"
The status should be success
The output should be blank
The stderr should be blank
End

It 'crew-id-crew-id-the-task-document-wins-over-a-disagreeing-crew-id'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "crew-id-crew-id-the-task-document-wins-over-a-disagreeing-crew-id"
The status should be success
The output should be blank
The stderr should be blank
End

It 'crew-id-crew-id-resolves-from-a-subdirectory-of-the-worktree'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "crew-id-crew-id-resolves-from-a-subdirectory-of-the-worktree"
The status should be success
The output should be blank
The stderr should be blank
End

It 'crew-id-crew-id-crew-id-still-resolves-when-no-worker-task-md-exists'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "crew-id-crew-id-crew-id-still-resolves-when-no-worker-task-md-exists"
The status should be success
The output should be blank
The stderr should be blank
End

It 'crew-id-crew-id-falls-back-to-crew-id-when-worker-task-md-has-no-crew-id-line'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "crew-id-crew-id-falls-back-to-crew-id-when-worker-task-md-has-no-crew-id-line"
The status should be success
The output should be blank
The stderr should be blank
End

It 'refresh-models-a-realistic-list-models-fixture-parses-into-the-expected-cache-shape'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "refresh-models-a-realistic-list-models-fixture-parses-into-the-expected-cache-shape"
The status should be success
The output should be blank
The stderr should be blank
End

It 'refresh-models-the-write-is-atomic-and-lands-at-the-cursor-models-cache-path'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "refresh-models-the-write-is-atomic-and-lands-at-the-cursor-models-cache-path"
The status should be success
The output should be blank
The stderr should be blank
End

It 'refresh-models-a-stubbed-cursor-agent-failure-exits-nonzero-and-leaves-an-existing-cache-untouched'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "refresh-models-a-stubbed-cursor-agent-failure-exits-nonzero-and-leaves-an-existing-cache-untouched"
The status should be success
The output should be blank
The stderr should be blank
End

It 'refresh-models-cursor-agent-absent-from-path-exits-nonzero-and-leaves-an-existing-cache-untouched'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "refresh-models-cursor-agent-absent-from-path-exits-nonzero-and-leaves-an-existing-cache-untouched"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-the-first-park-seeds-the-cursor-and-reports-nothing'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-the-first-park-seeds-the-cursor-and-reports-nothing"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-fires-on-a-head-sha-move'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-fires-on-a-head-sha-move"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-fires-on-a-new-review-thread-reply'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-fires-on-a-new-review-thread-reply"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-fires-when-the-check-rollup-conclusion-flips'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-fires-when-the-check-rollup-conclusion-flips"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-one-in-flight-check-keeps-the-rollup-pending-not-successful'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-one-in-flight-check-keeps-the-rollup-pending-not-successful"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-fires-when-the-pr-merges'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-fires-when-the-pr-merges"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-an-unchanged-poll-reports-nothing-and-exits-0'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-an-unchanged-poll-reports-nothing-and-exits-0"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-the-cursor-prevents-re-delivery-across-restarts'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-the-cursor-prevents-re-delivery-across-restarts"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-runs-with-crew-id-unset-and-never-touches-the-bus'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-runs-with-crew-id-unset-and-never-touches-the-bus"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-works-with-no-git-repo-at-all-when-repo-is-given'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-works-with-no-git-repo-at-all-when-repo-is-given"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-derives-the-repo-from-the-origin-remote'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-derives-the-repo-from-the-origin-remote"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-aborts-without-a-pr-number'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-aborts-without-a-pr-number"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-rejects-an-unbounded-park'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-rejects-an-unbounded-park"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-rejects-an-unknown-flag'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-rejects-an-unknown-flag"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-a-first-poll-that-cannot-read-the-pr-fails-loudly'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-a-first-poll-that-cannot-read-the-pr-fails-loudly"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-crew-pr-watch-posts-the-event-to-the-crew-s-dispatcher'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-crew-pr-watch-posts-the-event-to-the-crew-s-dispatcher"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-crew-pr-watch-posts-nothing-when-the-park-times-out'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-crew-pr-watch-posts-nothing-when-the-park-times-out"
The status should be success
The output should be blank
The stderr should be blank
End

It 'pr-watch-default-clock-a-1s-park-really-waits'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "pr-watch-default-clock-a-1s-park-really-waits"
The status should be success
The output should be blank
The stderr should be blank
End

It 'role-watch-role-watch-a-permission-dialog-receives-no-keys-until-it-clears-then-the-assignment-lands-once'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "role-watch-role-watch-a-permission-dialog-receives-no-keys-until-it-clears-then-the-assignment-lands-once"
The status should be success
The output should be blank
The stderr should be blank
End

It 'role-watch-role-watch-option-select-quota-live-turn-and-unrecognised-claude-frames-defer'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "role-watch-role-watch-option-select-quota-live-turn-and-unrecognised-claude-frames-defer"
The status should be success
The output should be blank
The stderr should be blank
End

It 'role-watch-role-watch-an-idle-claude-input-box-receives-the-assignment'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "role-watch-role-watch-an-idle-claude-input-box-receives-the-assignment"
The status should be success
The output should be blank
The stderr should be blank
End

It 'role-watch-role-watch-queued-assignments-go-out-one-per-tick-in-order'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "role-watch-role-watch-queued-assignments-go-out-one-per-tick-in-order"
The status should be success
The output should be blank
The stderr should be blank
End

It 'role-watch-role-watch-a-dialog-raised-after-the-text-is-typed-is-never-confirmed'
When run script "$SHELLSPEC_PROJECT_ROOT/case-runner.sh" "role-watch-role-watch-a-dialog-raised-after-the-text-is-typed-is-never-confirmed"
The status should be success
The output should be blank
The stderr should be blank
End

End
