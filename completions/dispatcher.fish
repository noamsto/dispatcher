# Completions for dispatcher. The __dispatch_models and __dispatcher_crew_ids
# helpers are bounded and silent; fish loads each completion file on its own,
# so dispatch.fish's model helper is repeated here.

function __dispatch_models
    printf '%s\n' @cheap @standard @premium
    timeout 1 dispatch --models --json 2>/dev/null | jq -r '[.engines[].models[]] | unique[] | select(test("[*?[]") | not)' 2>/dev/null
end

function __dispatcher_crew_ids
    timeout 1 crew crews 2>/dev/null | awk -F'\t' 'NR > 1 { print $1 }'
end

complete -c dispatcher -f

complete -c dispatcher -l agent -r -f -n 'not __fish_seen_argument -l overseer' -a 'claude codex cursor pi' -d 'Engine for the dispatcher'
complete -c dispatcher -l agent -r -f -n '__fish_seen_argument -l overseer' -a 'claude pi' -d 'Engine for the overseer'
complete -c dispatcher -l model -r -f -a '(__dispatch_models)' -d 'Model for that engine'
complete -c dispatcher -l effort -r -f -a 'low medium high xhigh max ultra' -d 'Reasoning effort'
complete -c dispatcher -l autocompact -r -f -a auto -d 'Autocompact threshold: auto or a token count'
complete -c dispatcher -l new -d "Mint a crew of this launcher's own"
complete -c dispatcher -l fresh -d 'Hand this crew off to a new session'
complete -c dispatcher -l note -r -F -d 'File holding the handoff note'
complete -c dispatcher -l no-note -d 'A --fresh after a crash, with no note'
complete -c dispatcher -l relaunch -r -f -a '(__dispatcher_crew_ids)' -d 'Restart a dead crew from its launch record'
complete -c dispatcher -l overseer -d 'Start the one-per-host fleet session'
complete -c dispatcher -l repo -r -f -a '(__fish_complete_directories)' -d "The crew's repo path, with --relaunch"
