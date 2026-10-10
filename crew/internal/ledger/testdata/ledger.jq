# The ledger program of the `crew status` pr_open arm (adapters/core/crew.sh), verbatim.
            "(?:[^\\s;,()]+\\s+)?(?:pass(?=\\(\\s*[^\\s)])(?<b>\\((?:[^()]|\\g<b>)*\\))|waived\\(dispatcher(?:[:;,\\s](?:[^()]|\\g<b>)*)?\\))" as $item
            | $d | test("^\\s*(?:\($item)(?:\\s*[;,]\\s*|\\s+|\\s*[;.]?\\s*$))*$"; "i") | not
