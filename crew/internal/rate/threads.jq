# The arm's review-thread ingest, verbatim from adapters/core/crew.sh: the one
# field the GraphQL call answers. One patch, same reason as view.jq: gh's
# response arrives as $gh because jqrun hands its input as an array.
$gh | $p + {unresolved_notes: ([ (.data.repository.pullRequest.reviewThreads.nodes // [])[]
                           | select(.isResolved == false) ] | length)}
