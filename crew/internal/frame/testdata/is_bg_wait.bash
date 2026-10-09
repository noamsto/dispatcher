# Verbatim from the pre-port adapters/core/crew.sh (_frame_classifier). Needs _meter_line.
  _is_bg_wait() {
    local tail_n
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -8 || true)
    printf '%s\n' "$tail_n" | grep -qE '·[[:space:]]done[[:space:]]+[0-9]{1,2}:[0-9]{2}' &&
      printf '%s\n' "$tail_n" | grep -qE '(^|[^0-9])[1-9][0-9]*[[:space:]]shells?([[:space:]]still running|[[:space:]]·|$)' &&
      ! printf '%s\n' "$tail_n" | grep -qE '^[^[:alnum:]]*[A-Za-z]+…' &&
      [ -z "$(_meter_line "$1")" ]
  }
