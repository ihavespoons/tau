/**
 * An extension that prints to stdout, which is where the protocol lives. The
 * shim must redirect it; a single stray line would otherwise be read by tau as
 * a malformed frame and cost the extension a strike.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

console.log("this should not corrupt the stream");

const quiet = defineTool({
	name: "quiet",
	description: "Prints, then works",
	parameters: Type.Object({}),
	async execute() {
		console.log("this should not corrupt the stream either");
		return { content: [{ type: "text", text: "still working" }] };
	},
});

export default function (pi: ExtensionAPI) {
	process.stdout.write("nor should this\n");
	pi.registerTool(quiet);
}
