# `_local_pi_models_json` from adapters/core/local-models.sh, verbatim: pi's
# models.json document for every localModels entry. Input is the dispatcher
# settings object.
{providers: ((.localModels // {}) | to_entries
    | map({provider: (.key | split("/")[0]), id: (.key | sub("^[^/]*/"; "")), value})
    | group_by(.provider)
    | map({key: .[0].provider, value: {
        baseUrl: .[0].value.baseUrl,
        api: "openai-completions",
        apiKey: .[0].provider,
        models: (map({id, contextWindow: .value.contextWindow}
          + (.value | {reasoning, input, maxTokens, thinkingLevelMap, samplingParams, samplingParamsByThinkingLevel}
            + (if .thinkingFormat then {compat: {thinkingFormat}} else {} end)
            | with_entries(select(.value != null))))
          | sort_by(.id))}})
    | from_entries)}
