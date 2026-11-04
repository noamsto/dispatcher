# Completions for dispatch-resume. __dispatch_models is shared in text with
# dispatch.fish: fish loads each completion file on its own.

function __dispatch_models
    printf '%s\n' @cheap @standard @premium
    timeout 1 dispatch --models --json 2>/dev/null | jq -r '[.engines[].models[]] | unique[] | select(test("[*?[]") | not)' 2>/dev/null
end

complete -c dispatch-resume -f

complete -c dispatch-resume -l agent -r -f -a 'claude codex cursor pi' -d 'Engine to relaunch on'
complete -c dispatch-resume -l model -r -f -a '(__dispatch_models)' -d 'Model for that engine'
complete -c dispatch-resume -l effort -r -f -a 'low medium high xhigh max ultra' -d 'Reasoning effort'
complete -c dispatch-resume -l mcp -r -f -d 'MCP for the lead'
complete -c dispatch-resume -l chrome -d 'Relaunch the claude lead with Claude in Chrome'
complete -c dispatch-resume -l fresh -d 'Start a new engine session'
complete -c dispatch-resume -l print -d 'Print the launch command instead of running it'
complete -c dispatch-resume -l rearm-roles -d 'Re-arm the role panes and launch nothing'
complete -c dispatch-resume -l ignore-budget -d 'Spend past the quota stop'
complete -c dispatch-resume -l ignore-map -d 'Skip the tier-map model refusals'
complete -c dispatch-resume -l help -d 'Show help'
