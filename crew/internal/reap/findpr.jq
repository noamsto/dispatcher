# `_reap_find_pr`'s program from crew.sh's `reap)` arm (`jq -er` over gh's
# `pr list` JSON), verbatim but for two patches:
#
#   the program  →  [ .[] | … ]
#     jqrun hands over one value: the array of JSON values gh printed, which jq
#     read one by one from its input. The outputs are collected; Go prints
#     them one per line as `jq -r` did.
#   test("…/pull/[0-9]+$")  →  "\\n?\\z" anchor
#     Oniguruma's `$` also matches before one trailing newline, Go regexp's is
#     absolute end-of-text.
#
# A fork's PR on the same head name is not this branch's.
[ .[] | map(select(.isCrossRepository == false)) | first
    | if . == null then "-"
      elif (.url | type == "string" and test("^https://[^\\s]+/pull/[0-9]+\\n?\\z")) then .url
      else error("not a PR url") end ]
