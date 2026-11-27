/**
 * Registers provider "stub" pointing at the local error-injection server in
 * $HSUP_STUB_URL. The key is a dummy; the stub ignores it.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.registerProvider("stub", {
    name: "Stub",
    baseUrl: process.env.HSUP_STUB_URL,
    apiKey: "stub",
    api: "anthropic-messages",
    models: [
      {
        type: "chat",
        id: "stub-model",
        name: "Stub Model",
        reasoning: false,
        input: ["text"],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: 200000,
        maxTokens: 4096,
      },
    ],
  });
}
