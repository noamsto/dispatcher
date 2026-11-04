# overseer is `dispatcher --overseer`; it takes the launch flags of
# dispatcher that an overseer accepts (claude and pi only, no crew flags).

function __dispatch_models
    printf '%s\n' @cheap @standard @premium
    timeout 1 dispatch --models --json 2>/dev/null | jq -r '[.engines[].models[]] | unique[] | select(test("[*?[]") | not)' 2>/dev/null
end

complete -c overseer -f

complete -c overseer -l agent -r -f -a 'claude pi' -d 'Engine for the overseer'
complete -c overseer -l model -r -f -a '(__dispatch_models)' -d 'Model for that engine'
complete -c overseer -l effort -r -f -a 'low medium high xhigh max ultra' -d 'Reasoning effort'
complete -c overseer -l autocompact -r -f -a auto -d 'Autocompact threshold: auto or a token count'
