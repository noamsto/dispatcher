# The arm's Actions ingest, verbatim from adapters/core/crew.sh. $t0 is the
# dispatch ts and $we the run's window end, both epoch ms ($we null for the
# last run on a branch). One patch, same reason as view.jq: gh's response
# arrives as $gh because jqrun hands its input as an array.
$gh | $p + {
  # Only the CI this run triggered: the dispatch-boundary window is
  # what stops run i+1 on a reused branch from inheriting the CI of
  # run i.
  first_ci_green: (
    [ (.workflow_runs // [])[]
      | {sha: .head_sha, c: (.created_at | fromdateiso8601 * 1000),
         status: .status, concl: .conclusion} ]
    | map(select(.c >= $t0 and ($we == null or .c < $we)))
    | if length == 0 then null
      else group_by(.sha) | map({first: (map(.c) | min), runs: .}) | min_by(.first)
           | .runs | map(select(.status == "completed"))
           | all(.concl == "success" or .concl == "skipped" or .concl == "neutral")
      end)
}
