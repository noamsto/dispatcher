#!/usr/bin/env bash
# Print the store paths CI should push to the binary cache for the given
# installables. `ultimate` marks outputs this machine built (not substituted);
# `deriver != null` drops eval-time source copies. Run after the build.
set -euo pipefail

[ "$#" -gt 0 ] || exit 0

drvs=()
while IFS= read -r drv; do
	[[ -n $drv ]] && drvs+=("$drv")
done < <(nix path-info --derivation "$@")
[ "${#drvs[@]}" -gt 0 ] || exit 0

paths=()
while IFS= read -r p; do
	[[ -z $p || $p == *.drv ]] && continue
	paths+=("$p")
done < <(nix-store --query --requisites --include-outputs "${drvs[@]}")
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
