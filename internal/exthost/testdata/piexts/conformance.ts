/**
 * The conformance extension, written the way a Pi author would write it.
 *
 * It must behave identically to the in-process Go version in
 * conformance_test.go and to the standalone Go one in testdata/conformext.
 * Where the three differ, the in-process one is right: its composition
 * policies were ported from Pi line by line.
 *
 * Nothing here is tau-specific. It imports Pi's packages, uses Pi's event
 * names, and reads `event.input` — which is the point.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

const echo = defineTool({
	name: "conform_echo",
	description: "Echo the text back",
	parameters: Type.Object({
		text: Type.String({ description: "Text to echo" }),
	}),
	async execute(toolCallId, params) {
		if (params.text === "fail") {
			throw new Error("tool failure");
		}
		return {
			content: [{ type: "text", text: `echo:${params.text}` }],
			details: { callId: toolCallId },
		};
	},
});

export default function (pi: ExtensionAPI) {
	pi.registerTool(echo);

	pi.on("tool_call", async (event) => {
		if (event.input.text === "blocked") {
			return { block: true, reason: "conformance says no" };
		}
		if (event.input.text === "rewrite") {
			// Pi's idiom: mutate the arguments in place. The shim sends the
			// mutated object back, because there is no shared map across a pipe.
			event.input.text = "rewritten";
			return undefined;
		}
		return undefined;
	});

	pi.on("input", async (event) => {
		if (event.text === "transform-me") {
			return { action: "transform", text: "transformed" };
		}
		// No opinion — not an empty transform, which would replace the input
		// with nothing.
		return undefined;
	});

	pi.on("context", async () => {
		// Likewise, and here confusing the two would delete the conversation.
		return undefined;
	});

	pi.registerCommand("conform", {
		description: "conformance command",
		handler: async (args) => {
			if (args === "fail") {
				throw new Error("conformance failure");
			}
		},
	});
}
