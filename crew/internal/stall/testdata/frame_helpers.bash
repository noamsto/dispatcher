# Verbatim from the pre-port adapters/core/crew.sh stall-watch arm: _runaway_prose, _out_tokens, _permission_detail.
  _runaway_prose() {
    printf '%s\n' "$1" | tail -60 | awk '
      function flush(  i) { for (i = 1; i <= n; i++) print buf[i]; n = 0 }
      /^[[:space:]]*⏺/ { skip = 0; n = 0 }
      /^[[:space:]]*⏺[[:space:]]+[A-Za-z_]+\(/ { next }
      /^[[:space:]]*(>|❯)/ { next }
      /^[[:space:]]*⎿/ { skip = 1; next }
      /^[[:space:]]*Tool output/ { skip = 2; next }
      skip == 1 && (/^[[:space:]]*$/ || /^     /) { next }
      skip == 2 && (/^[[:space:]]*$/ || /^  /) { next }
      { skip = 0; buf[++n] = $0 }
      END { flush() }'
  }

  # _out_tokens <text> — output-token count as an integer: the last `↓ N[kM]`
  # in the bottom lines (claude's live meter, pi's footer). Empty when absent.
  _out_tokens() {
    printf '%s\n' "$1" | tail -12 | grep -oE '↓ ?[0-9.]+[kM]?' | tail -1 |
      awk '{ sub(/^↓ ?/, ""); n=$0; m=1; if (n ~ /k$/) m=1000; else if (n ~ /M$/) m=1000000; sub(/[kM]$/, "", n); printf "%d", n * m }' || true
  }
  _permission_detail() {
    local pane="$2" clean suffix body max
    clean=$(printf '%s\n' "$1" |
      sed -E $'s/\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?//g; s/\x1b\\[[0-9;?]*[ -\\/]*[@-~]//g; s/\x1b[@-Z\\\\-_]//g' |
      tr -d '\000-\010\013-\037\177')
    body=$(printf '%s\n' "$clean" | awk '
      /^[[:space:]]*[A-Za-z][A-Za-z ]+ · from the / {
        t=$0; sub(/^[[:space:]]+/,"",t); sub(/[[:space:]]+· from the .*/,"",t); r=""; have=1; next
      }
      have && r == "" && $0 !~ /^[[:space:]]*$/ {
        r=$0; gsub(/^[[:space:]]+|[[:space:]]+$/,"",r)
      }
      END {
        if (have && r != "") printf "%s: %s", "prompt: permission — " t, r
        else printf "%s", "prompt: permission — (unparsed)"
      }')
    suffix=" — pane $pane"
    body=$(printf '%s' "$body" | tr '\n\t' '  ' | sed -E 's/[[:space:]]+/ /g')
    max=$((160 - ${#suffix}))
    [ "$max" -lt 0 ] && max=0
    printf '%s%s' "${body:0:$max}" "$suffix"
  }
