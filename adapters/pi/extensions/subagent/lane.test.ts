import { test } from "node:test";
import assert from "node:assert/strict";
import { laneModel } from "./lane.ts";

const PINNED = "openrouter/deepseek/deepseek-v4.1-flash:max";
const lane = (model: string) => ({ DISPATCHER_PI_SUBAGENT_MODEL: model });

test("no lane keeps the pinned OpenRouter model", () => {
	assert.equal(laneModel(PINNED, {}), PINNED);
	assert.equal(laneModel(PINNED, lane("")), PINNED);
});

test("a lane replaces the pinned model and its thinking suffix", () => {
	assert.equal(laneModel(PINNED, lane("anthropic/claude-sonnet-5-5")), "anthropic/claude-sonnet-5-5");
	assert.equal(laneModel("openrouter/deepseek/deepseek-v4.1-flash:low", lane("lemonade/qwen3-coder:30b")), "lemonade/qwen3-coder:30b");
});

test("an unpinned or non-OpenRouter model is left alone", () => {
	assert.equal(laneModel(undefined, lane("anthropic/claude-sonnet-5-5")), undefined);
	assert.equal(laneModel("anthropic/claude-opus-5-5", lane("google/gemini-4-argon")), "anthropic/claude-opus-5-5");
});
