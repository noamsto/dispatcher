# The roster final step (the `jq --argjson m "$idmap"` tail of the `roster)`
# arm in adapters/core/crew.sh, before the #829 Go port): merge the identity
# map, suffix colliding codenames, drop prev_state unless it explains an
# exit_suspect row. Run through gojq by internal/jqrun with `$m`.
      map(. + ($m[.branch] // {}))
      | (map(select(.name != null) | .name) | group_by(.) | map(select(length > 1) | .[0])) as $dupes
      | map(if (.name as $n | $dupes | index($n))
            then .name = (.name + "·" + ((.branch | capture("(?:[a-z]+/)?(?<id>[A-Za-z]+-[0-9]+|[0-9]+)") | .id) // .branch))
            else . end)
      | map(if .exit_suspect then . else del(.prev_state) end)
