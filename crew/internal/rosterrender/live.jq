# The live count of `_rr_pass` from adapters/core/crew.sh — the one number the
# renderer prints, and the only thing the daemon's quiet window reads. The same
# `.[0]` and `[ ... ]` patches as d2.jq: the arm piped the model into `jq`, and
# jqrun returns one value.
[ .[0] | (
([.rows[] | select(.state == "working" or .state == "blocked" or .state == "dispatched")] | length)
                                    + (.holds | length)
) ]
