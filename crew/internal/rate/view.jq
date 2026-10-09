# The arm's `gh pr view` ingest, verbatim from adapters/core/crew.sh including
# the ts2ms def: gh marshals an unset timestamp as Go's zero time rather than
# null, and a gh timestamp is an ISO-8601 string while t0_ms/window_end_ms are
# epoch ms — comparing the two silently succeeds in jq (every string sorts
# above every number), so the conversion happens at ingest, once.
def ts2ms: if . == null or . == "" or startswith("0001-01-01") then null else (fromdateiso8601 * 1000) end;
$gh
| ([ (.reviews // [])[]
   | select(.state == "CHANGES_REQUESTED" or .state == "COMMENTED")
   | (.submittedAt | ts2ms) ] | map(select(. != null))) as $rv
| ([ (.commits // [])[] | (.committedDate | ts2ms) ] | map(select(. != null))) as $cm
| $p + {
    pr_state: .state,
    closed_at_ms: (.closedAt | ts2ms),
    merged_at_ms: (.mergedAt | ts2ms),
    merge_commit: (.mergeCommit.oid // null),
    # A round is a maximal group of rework reviews followed by >=1
    # later commit, so three reviews before one fix commit is ONE
    # round: bucket each review by the first commit that answers it
    # and count the distinct buckets. Reviews never answered by a
    # commit bucket to null and contribute 0.
    review_rounds: ([ $rv[] as $r | ($cm | map(select(. > $r)) | min) ]
                    | map(select(. != null)) | unique | length)
  }
