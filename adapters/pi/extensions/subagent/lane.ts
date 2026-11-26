// dispatcher patch: roster agents pin an OpenRouter model, which bills the wrong
// account when the lead runs on another lane. dispatch exports the lead's lane
// model (DISPATCHER_PI_SUBAGENT_MODEL) and whether it takes the agent's thinking
// suffix (DISPATCHER_PI_SUBAGENT_LEVELS=0 drops it, for local models). Kept free
// of pi imports so the offline tests can load it.

const THINKING_SUFFIX = /:(off|minimal|low|medium|high|xhigh|max)$/;

/** A pinned openrouter/* model is swapped for the lead's lane; anything else is left alone. */
export function laneModel(pinned: string | undefined, lane: string | undefined, levels: boolean): string | undefined {
	if (!pinned || !lane || !pinned.startsWith("openrouter/")) return pinned;
	if (!levels) return lane;
	const level = THINKING_SUFFIX.exec(pinned)?.[1];
	const base = lane.replace(THINKING_SUFFIX, "");
	return level ? `${base}:${level}` : base;
}
