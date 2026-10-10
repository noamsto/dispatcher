# The final fold of `_rr_model` from adapters/core/crew.sh: the roster rows
# without a pending branch (each carrying the base the dispatch fold recorded),
# plus one `dispatched` row per pending branch, sorted by branch, with the
# outstanding holds and the live role panes beside them.
#
# One patch: `[inputs]` becomes `.`, because the arm fed the five values in on
# stdin under `jq -n` and jqrun hands them over as the input array.
. as [$r, $p, $ids, $holds, $roles]
      | ($p | map({key: .branch, value: .}) | from_entries) as $pm
      | {rows: ([$r[] | select($pm[.branch].pending | not) | . + {base: $pm[.branch].base}]
                + [$p[] | select(.pending)
                   | {branch, ts, base, title, tier, engine, model, state: "dispatched",
                      detail: null, source: null, sessions: [], pr_url: null} + $ids[.branch]]
                | sort_by(.branch)),
         holds: $holds, roles: $roles}
