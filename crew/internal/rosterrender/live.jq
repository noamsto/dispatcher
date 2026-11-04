# The live count of `_rr_pass` from adapters/core/crew.sh — the one number the
# renderer prints, and the only thing the daemon's quiet window reads. The arm
# piped the model into `jq`; `.[0]` and `[ ... ]` patch that for jqrun, which
# returns one value.
[ .[0] | (
([.rows[] | select(.state == "working" or .state == "blocked" or .state == "dispatched")] | length)
                                    + (.holds | length)
) ]
