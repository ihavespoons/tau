// Module resolve hooks: Pi's package names point at the bridge.
//
// A Pi extension imports from @earendil-works/pi-coding-agent and
// @earendil-works/pi-ai. Neither is installed here and neither ever will be —
// resolving them to the bridge is precisely what makes an unmodified Pi
// extension runnable against tau.
//
// The legacy names are accepted too, because extensions published before Pi
// renamed its packages are exactly the ones most likely to be sitting in
// someone's ~/.pi and never updated.

const BRIDGE = new URL("./bridge.mjs", import.meta.url).href;
const PITUI = new URL("./pitui.mjs", import.meta.url).href;

const TO_BRIDGE = new Set([
	"@earendil-works/pi-coding-agent",
	"@earendil-works/pi-ai",
	"@earendil-works/pi-agent-core",
	// Legacy names.
	"@mariozechner/pi-coding-agent",
	"@mariozechner/pi-ai",
	"@mariozechner/pi-agent-core",
	"pi-coding-agent",
	"pi-ai",
]);

const TO_PITUI = new Set(["@earendil-works/pi-tui", "@mariozechner/pi-tui", "pi-tui"]);

export async function resolve(specifier, context, next) {
	if (TO_BRIDGE.has(specifier)) return { url: BRIDGE, shortCircuit: true };
	if (TO_PITUI.has(specifier)) return { url: PITUI, shortCircuit: true };
	// TypeBox is aliased so an extension importing it directly gets the same
	// object the bridge hands out, real or fallback.
	if (specifier === "typebox" || specifier === "@sinclair/typebox") {
		try {
			return await next(specifier, context);
		} catch {
			return { url: new URL("./typebox.mjs", import.meta.url).href, shortCircuit: true };
		}
	}
	return next(specifier, context);
}
