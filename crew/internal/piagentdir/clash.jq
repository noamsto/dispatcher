# The `clash=$(jq -nr …)` check in crew.sh's `_pi_agent_dir`: the first
# localModels provider whose name matches (case-insensitively) a credential
# stored in the worker's auth.json, or "" when none does.
#
# Vars: $s the dispatcher settings, $a the auth.json values (the arm's
# --slurpfile).
#
# Patch: the arm's trailing `// empty` is `// ""`. jqrun returns exactly one
# value and counts no output as an error, and the arm read an empty `$(…)` as
# "no clash" — which is what "" means to the caller.
($a[0] | keys | map(ascii_downcase)) as $stored
| [($s.localModels // {}) | keys[] | split("/")[0] | select(ascii_downcase | IN($stored[]))][0] // ""
