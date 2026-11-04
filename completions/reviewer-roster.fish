complete -c reviewer-roster -f

complete -c reviewer-roster -l base -r -f -d 'Revision the diff is taken against'
complete -c reviewer-roster -l repo -r -f -a '(__fish_complete_directories)' -d 'Repository to resolve for'
complete -c reviewer-roster -l harness -r -f -a '(__fish_complete_directories)' -d 'Harness reviewers directory'
complete -c reviewer-roster -l default -r -f -d 'Default reviewer ref'
