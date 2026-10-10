# The waiver program of the `crew status` pr_open arm (adapters/core/crew.sh), verbatim.
                ($id | ascii_downcase | gsub("(?<ch>[^a-z0-9_ -])"; "\\\(.ch)")) as $ide
                | [inputs | (try fromjson catch null) | select(type == "object"
                  and .crew_id == $c and .kind == "msg" and .from == ("dispatcher:" + $c)
                  and .to == $f)
                  | (.body // "") | tostring | ascii_downcase
                  | split("(?:[.;!?](?=\\s|$)|\\n|\\bbut\\b)"; "g")[]
                  | select(test("\\bwaiv(?:e|es|ed|ing|er)\\b")
                    and (test("(?:\\b(?:not|never|no|cannot|without)\\b|n(?:\u0027|\u2019)t\\b)[^\\n]*\\bwaiv") | not)
                    and test("(?:^|[^a-z0-9_.-])" + $ide + "(?:$|[^a-z0-9_.-])"))] | length > 0
