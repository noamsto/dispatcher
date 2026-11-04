complete -c lead-launcher -f

complete -c lead-launcher -l fresh -r -f -a 'execute review' -d 'Inner mode: relaunch fresh at a seam'
complete -c lead-launcher -l engine -r -f -a 'claude pi' -d 'Engine to relaunch'
complete -c lead-launcher -l crew-dir -r -f -a '(__fish_complete_directories)' -d 'Crew bus directory'
complete -c lead-launcher -l branch -r -f -d 'Worker branch'
complete -c lead-launcher -l note -r -F -d 'Handoff note file'
complete -c lead-launcher -l sid -r -f -d 'Session id'
complete -c lead-launcher -l prompt -r -f -d 'Prompt for the relaunched lead'
complete -c lead-launcher -l execute-prompt -r -f -d 'Prompt for the execute seam'
complete -c lead-launcher -l review-prompt -r -f -d 'Prompt for the review seam'
