complete -c permission-check -f

complete -c permission-check -l answer -d 'Answer the dialog instead of only judging it'
complete -c permission-check -l pane -r -f -d 'tmux pane id, %N'
complete -c permission-check -l capture -r -F -d 'File holding the captured pane'
complete -c permission-check -l branch -r -f -d 'Worker branch'
complete -c permission-check -l worktree -r -f -a '(__fish_complete_directories)' -d "Worker's worktree"
complete -c permission-check -l crew-dir -r -f -a '(__fish_complete_directories)' -d 'Crew bus directory'
complete -c permission-check -l projects-dir -r -f -a '(__fish_complete_directories)' -d 'Engine projects directory'
complete -c permission-check -l ro-root -r -f -a '(__fish_complete_directories)' -d 'Extra read-only root (repeatable)'
