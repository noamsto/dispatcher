# Completions for dispatch. The __dispatch_* helpers ask the installed binary,
# bounded and silent: a slow or failing lookup completes nothing.

function __dispatch_engines
    timeout 1 dispatch --engines 2>/dev/null
end

function __dispatch_profiles
    timeout 1 dispatch --profiles --json 2>/dev/null | jq -r '.profiles | keys[]' 2>/dev/null
end

function __dispatch_models
    printf '%s\n' @cheap @standard @premium
    timeout 1 dispatch --models --json 2>/dev/null | jq -r '[.engines[].models[]] | unique[] | select(test("[*?[]") | not)' 2>/dev/null
end

function __dispatch_tier_slot
    set -l tokens (commandline -opc)
    test (count $tokens) -eq 1
end

function __dispatch_model_slot
    set -l tokens (commandline -opc)
    test (count $tokens) -eq 2; and contains -- $tokens[2] trivial standard deep
end

complete -c dispatch -f

complete -c dispatch -n __dispatch_tier_slot -a 'trivial standard deep' -d 'Review rigor'
complete -c dispatch -n __dispatch_tier_slot -a resume -d 'Relaunch the worker of this worktree'
complete -c dispatch -n __dispatch_model_slot -a '(__dispatch_models)' -d Model

complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l effort -r -f -a 'low medium high xhigh max ultra' -d 'Reasoning effort'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l profile -r -f -a '(__dispatch_profiles)' -d 'Task profile filling unset fields'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l plan -r -f -a 'provided required' -d 'Task doc is the plan of record'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l agent -r -f -a '(__dispatch_engines)' -d 'Engine for the lead'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l mcp -r -f -d 'MCP for the lead'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l chrome -d 'Claude in Chrome for the claude lead'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l grid -d 'Role grid derived from the tier'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l no-grid -d 'No role grid'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l roles -r -f -d 'Grid roles, name[=agent:model[@effort]]'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l lazy -d 'Stamp the grid, spawn roles on demand'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l status -d 'Add a crew-status pane beside the grid'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l fresh-seams -d 'Relaunch the lead fresh at the seams'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l no-fresh-seams -d 'Keep one lead session'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l parent -r -f -d 'Mint the issue as a sub-issue of N'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l also-closes -r -f -d 'Extra issue the PR also closes'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l crew-id -r -f -d 'Crew to post to'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l host -r -f -d 'Run the worker on a settings host'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l base -r -f -d 'Branch to base the worktree on'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l pr -r -f -d 'Reuse an open PR'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l review -d 'Review-only worker (needs --pr)'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l draft -d 'Open the PR as a draft'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l no-draft -d 'Ready for review'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l add-dir -r -F -d 'Grant read access to a directory'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l owner-auth -r -f -d "The owner's words authorizing a gated action"
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l ignore-budget -d 'Spend past the quota stop'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l fleet-hold -r -f -d 'The fleet hold this dispatch takes'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l ignore-map -d 'Skip the tier-map model refusals'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l engines -d 'List enabled and installed engines'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l in-budget -d 'With --engines: only affordable ones'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l models -d 'Show the tier map per engine'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l profiles -d 'List task profiles'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l json -d 'With --models or --profiles: machine-readable'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l recover -r -f -d 'With --host: record a remote worker session'
complete -c dispatch -n 'not __fish_seen_subcommand_from resume' -l help -d 'Show help'

# dispatch resume takes the flags of dispatch-resume.
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l agent -r -f -a '(__dispatch_engines)' -d 'Engine to relaunch on'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l model -r -f -a '(__dispatch_models)' -d 'Model for that engine'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l effort -r -f -a 'low medium high xhigh max ultra' -d 'Reasoning effort'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l mcp -r -f -d 'MCP for the lead'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l chrome -d 'Relaunch the claude lead with Claude in Chrome'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l fresh -d 'Start a new engine session'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l print -d 'Print the launch command instead of running it'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l rearm-roles -d 'Re-arm the role panes and launch nothing'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l ignore-budget -d 'Spend past the quota stop'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l ignore-map -d 'Skip the tier-map model refusals'
complete -c dispatch -n '__fish_seen_subcommand_from resume' -l help -d 'Show help'
