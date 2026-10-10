# The items program of the `crew status` pr_open arm (adapters/core/crew.sh), verbatim.
            "(?:(?<id>[^\\s;,()]+)\\s+)?(?:(?<k>pass)(?=\\(\\s*[^\\s)])(?<b>\\((?:[^()]|\\g<b>)*\\))|(?<w>waived)\\(dispatcher(?:[:;,\\s](?:[^()]|\\g<b>)*)?\\))" as $item
            | [$d | match($item; "gi") | [.captures[] | select(.name != null) | {(.name): .string}] | add]
            | .[] | [(.id // ""), (if .k != null then "pass" else "waived" end), (.b // "" | gsub("[\\n\\r\\x1f]"; " "))] | join("\u001f")
