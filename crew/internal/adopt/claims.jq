# Ported verbatim from crew.sh's `adopt` arm (#920), with one patch: the arm's
# final `.[] | @tsv` is dropped, because jqrun yields one value and Go reads the
# rows by position instead of re-splitting TSV text (the same patch
# crew/internal/stall documents for its two programs). `group_by(.issue|tostring)`
# is what makes a numeric 73 and a string "73" one issue — `group_by` compares
# raw JSON, and `gh issue edit` treats them as one issue either way.

map(select(.kind=="claim-issue" and .issue != null and (.branch // "") != ""))
| group_by(.issue|tostring)
| map(
    max_by(.ts) as $mine
    | select($mine.crew_id == $id)
    | [($mine.issue|tostring), $mine.branch] + (map(.branch) | unique - [$mine.branch])
  )
