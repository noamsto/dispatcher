complete -c pr-watch -f

complete -c pr-watch -n 'test (count (commandline -opc)) -ge 2' -l repo -r -f -d 'Repository, owner/name'
complete -c pr-watch -n 'test (count (commandline -opc)) -ge 2' -l timeout -r -f -d 'Seconds to watch before giving up'
complete -c pr-watch -n 'test (count (commandline -opc)) -ge 2' -l interval -r -f -d 'Seconds between polls'
