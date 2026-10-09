# `_hold_render`, verbatim apart from one patch: `jq -r` printed one @tsv row per
# line, and jqrun wants exactly one value, so the rows are collected and joined
# (the report.jq patch, same reason). The Go caller adds the one trailing
# newline, byte-identically, and prints nothing for the empty string — which is
# only ever zero rows, since a @tsv row always carries its six tabs.
[ .[] | [ .id, .wait.engine, .wait.window, (.wait.resets_at | tostring),
          .task.ref, .task.branch, .task.title ] | @tsv ] | join("\n")
