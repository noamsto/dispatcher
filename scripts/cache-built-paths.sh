#!/usr/bin/env bash
# Print the store paths CI should push to the binary cache for the given
# installables. `ultimate` marks outputs this machine built (not substituted);
# `deriver != null` drops eval-time source copies. Run after the build.
set -euo pipefail

[ "$#" -gt 0 ] || exit 0

# Captured first so a failing producer fails the script instead of reading as
# an empty result.
drv_list=$(nix path-info --derivation "$@")
drvs=()
while IFS= read -r drv; do
	[[ -n $drv ]] && drvs+=("$drv")
done <<<"$drv_list"
[ "${#drvs[@]}" -gt 0 ] || exit 0

path_list=$(nix-store --query --requisites --include-outputs "${drvs[@]}")
paths=()
while IFS= read -r p; do
	[[ -z $p || $p == *.drv ]] && continue
	paths+=("$p")
done <<<"$path_list"
[ "${#paths[@]}" -gt 0 ] || exit 0

chunk=500
for ((i = 0; i < ${#paths[@]}; i += chunk)); do
	invalid=$'\n'
	# Outputs of a failed or unbuilt derivation are absent from the store.
	while IFS= read -r p; do
		invalid+=$p$'\n'
	done < <(nix-store --check-validity --print-invalid "${paths[@]:i:chunk}")
	valid=()
	for p in "${paths[@]:i:chunk}"; do
		[[ $invalid == *$'\n'"$p"$'\n'* ]] || valid+=("$p")
	done
	[ "${#valid[@]}" -gt 0 ] || continue
	nix path-info --json --json-format 1 "${valid[@]}" |
		jq -r 'to_entries[] | select(.value.ultimate and .value.deriver != null) | .key'
done | sort -u
