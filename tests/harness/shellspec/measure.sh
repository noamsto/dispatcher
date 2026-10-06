#!/usr/bin/env bash
set -euo pipefail

readonly repetitions=${1:-5}
readonly examples=26
readonly calls=100
time_bin=$(type -P time)
readonly time_bin
work=$(mktemp -d)
readonly work
trap 'gtrash put "$work"' EXIT
: >"$work/.shellspec"

[[ $repetitions =~ ^[1-9][0-9]*$ ]] || {
  echo 'usage: measure.sh [positive-repetitions]' >&2
  exit 2
}

{
  echo "Describe 'noop'"
  for ((i = 1; i <= examples; i++)); do
    printf "  It 'case-%s'\n    When call :\n    The status should be success\n  End\n" "$i"
  done
  echo 'End'
} >"$work/noop_spec.sh"

for ((i = 1; i <= examples; i++)); do
  printf '@test "case-%s" { :; }\n' "$i"
done >"$work/noop.bats"

cat >"$work/command-mock" <<'EOF'
#!/usr/bin/env bash
:
EOF
chmod +x "$work/command-mock"

cat >"$work/mock_spec.sh" <<EOF
call_mock() {
  i=0
  while [ "\$i" -lt $calls ]; do
    fake_command one two
    i=\$((i + 1))
  done
}
Describe 'function mock calls'
  Mock fake_command
    :
  End
  It 'runs without an executable per call'
    When call call_mock
    The status should be success
  End
End
EOF

cat >"$work/command-loop.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
for ((i = 0; i < $calls; i++)); do
  "$work/command-mock" one two
done
EOF
chmod +x "$work/command-loop.sh"

printf 'measurement\trepetition\twall_s\tuser_s\tsys_s\n'
for ((rep = 1; rep <= repetitions; rep++)); do
  "$time_bin" -q -f "shellspec_noop\t$rep\t%e\t%U\t%S" \
    shellspec --directory "$work" --shell bash --default-path "$work/noop_spec.sh" >/dev/null
  "$time_bin" -q -f "bats_exec_test_noop\t$rep\t%e\t%U\t%S" \
    bats "$work/noop.bats" >/dev/null
  "$time_bin" -q -f "shellspec_function_mock_${calls}_calls\t$rep\t%e\t%U\t%S" \
    shellspec --directory "$work" --shell bash --default-path "$work/mock_spec.sh" >/dev/null
  "$time_bin" -q -f "path_command_mock_${calls}_calls\t$rep\t%e\t%U\t%S" \
    "$work/command-loop.sh" >/dev/null
done 2>&1
