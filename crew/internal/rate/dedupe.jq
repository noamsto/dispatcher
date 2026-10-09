# The `rate` arm's store dedupe, verbatim from adapters/core/crew.sh: the
# `crew roster` last-row-wins idiom over the append-only ratings store. Run
# through gojq by internal/jqrun as its own fold (the arm ran it as its own
# `jq -s -c`), so the jq sort/tie semantics stay one shared source.
group_by(.run_id) | map(max_by(.swept_at))
