// dispatcher patch: roster agents pin an OpenRouter model, which bills the wrong
// account when the lead runs on another lane. dispatch exports the lead's lane
// model as DISPATCHER_PI_SUBAGENT_MODEL (unset for an OpenRouter lead). Kept free
// of pi imports so the offline tests can load it.

/**
 * A pinned openrouter/* model is swapped for the lead's lane model, and the
 * pinned thinking suffix goes with it: the child runs at the lead's own level.
 * Anything else is returned unchanged.
 */
export function laneModel(pinned: string | undefined, env: Record<string, string | undefined>): string | undefined {
	const lane = env.DISPATCHER_PI_SUBAGENT_MODEL;
	if (!pinned || !lane || !pinned.startsWith("openrouter/")) return pinned;
	return lane;
}
