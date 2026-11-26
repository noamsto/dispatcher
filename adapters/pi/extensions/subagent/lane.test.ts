import { test } from "node:test";
import assert from "node:assert/strict";
import { laneModel } from "./lane.ts";

const PINNED = "openrouter/deepseek/deepseek-v4.1-flash:max";

test("no lane keeps the pinned OpenRouter model", () => {
	assert.equal(laneModel(PINNED, undefined, true), PINNED);
	assert.equal(laneModel(PINNED, "", true), PINNED);
});

test("a direct lane replaces the model and keeps the agent's thinking suffix", () => {
	assert.equal(laneModel(PINNED, "anthropic/claude-sonnet-5-5", true), "anthropic/claude-sonnet-5-5:max");
	assert.equal(
		laneModel("openrouter/deepseek/deepseek-v4.1-flash:low", "google/gemini-4-argon", true),
		"google/gemini-4-argon:low",
	);
});

test("the lead's own thinking suffix never doubles up", () => {
	assert.equal(laneModel(PINNED, "anthropic/claude-sonnet-5-5:high", true), "anthropic/claude-sonnet-5-5:max");
});

test("a local lane takes no suffix", () => {
	assert.equal(laneModel(PINNED, "lemonade/qwen3-coder:30b", false), "lemonade/qwen3-coder:30b");
});

test("an unpinned or non-OpenRouter model is left alone", () => {
	assert.equal(laneModel(undefined, "anthropic/claude-sonnet-5-5", true), undefined);
	assert.equal(laneModel("anthropic/claude-opus-5-5", "google/gemini-4-argon", true), "anthropic/claude-opus-5-5");
});
