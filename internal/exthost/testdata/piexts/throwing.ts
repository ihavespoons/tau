/**
 * A tool that throws. Written in Pi's own idiom, to check that Pi's contract
 * survives the wire: a tool that throws produces an isError result the model
 * can read, not a transport failure the user has to interpret.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

const boom = defineTool({
	name: "boom",
	description: "Always fails",
	parameters: Type.Object({}),
	async execute() {
		throw new Error("deliberate failure");
	},
});

export default function (pi: ExtensionAPI) {
	pi.registerTool(boom);
}
