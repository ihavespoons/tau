// The bridge: what `import { ... } from "@earendil-works/pi-coding-agent"`
// resolves to when a Pi extension runs under tau.
//
// A Pi extension is a function that receives an ExtensionAPI and registers
// against it. Everything it can register or call has to exist here, backed by
// a frame on the wire rather than by tau's own memory.
//
// # What is not here
//
// Pi's API includes surfaces that need a component tree: mounting a custom
// editor, drawing an overlay, replacing the transcript renderer. Those cannot
// cross a pipe, and pretending otherwise would mean an extension that appears
// to work and silently does nothing. They throw a named error instead, and the
// shim reports them once at load so the user learns at startup rather than at
// the moment they press a key.

import { Type } from "./typebox.mjs";

export { Type };

/** Registration state, filled by the extension's factory and read at handshake. */
export class Registry {
	constructor() {
		this.handlers = new Map();
		this.tools = new Map();
		this.commands = new Map();
		this.shortcuts = new Map();
		this.flags = new Map();
		this.renderers = [];
		this.warnings = [];
	}

	warn(message) {
		if (!this.warnings.includes(message)) this.warnings.push(message);
	}
}

/**
 * defineTool is identity at runtime.
 *
 * In Pi it exists for its types: it constrains the parameters schema so the
 * execute callback's argument is inferred. None of that survives to runtime,
 * and a tool object is already exactly what the protocol wants.
 */
export function defineTool(tool) {
	return tool;
}

/** Pi's helper for a text tool result. */
export function textResult(text, details) {
	return { content: [{ type: "text", text }], details };
}

/** Raised by the API surfaces that cannot cross a pipe. */
export class UnsupportedInRPCError extends Error {
	constructor(what) {
		super(
			`${what} is not available to an extension running out of process: ` +
				"it needs a component tree, and there is a pipe between this " +
				"extension and the renderer. Port the extension to tau's " +
				"in-process Go API if it must draw.",
		);
		this.name = "UnsupportedInRPCError";
	}
}

/**
 * Build the ExtensionAPI object the extension's default export receives.
 *
 * `send` is how a call reaches tau: it takes a frame and returns a promise for
 * the reply's payload.
 */
export function createAPI(registry, ctx, send) {
	const unsupported = (what) => () => {
		registry.warn(`${what} is not supported over the extension protocol`);
		throw new UnsupportedInRPCError(what);
	};

	const api = {
		// --- events ---
		on(event, handler) {
			const list = registry.handlers.get(event) ?? [];
			list.push(handler);
			registry.handlers.set(event, list);
			return api;
		},

		// --- registration ---
		registerTool(tool) {
			registry.tools.set(tool.name, tool);
			// Registering after the handshake has to reach tau as an action,
			// or a tool an extension adds mid-session exists only here. An MCP
			// bridge announcing tools/list_changed depends on this.
			if (ctx.handshakeComplete) {
				void send({
					type: "action",
					method: "registerTool",
					params: { tool: declareTool(tool) },
				});
			}
			return api;
		},
		// Pi's signature is registerCommand(name, spec). An older shape passed
		// a single object with the name inside it, and extensions in the wild
		// use both, so both are accepted rather than one failing obscurely on
		// an undefined name.
		registerCommand(nameOrSpec, maybeSpec) {
			const spec = maybeSpec ?? nameOrSpec;
			const name = typeof nameOrSpec === "string" ? nameOrSpec : nameOrSpec?.name;
			registry.commands.set(name, { ...spec, name });
			return api;
		},
		registerShortcut(keyOrSpec, maybeSpec) {
			const spec = maybeSpec ?? keyOrSpec;
			const key = typeof keyOrSpec === "string" ? keyOrSpec : keyOrSpec?.key;
			registry.shortcuts.set(key, { ...spec, key });
			return api;
		},
		registerFlag(nameOrSpec, maybeSpec) {
			const spec = maybeSpec ?? nameOrSpec;
			const name = typeof nameOrSpec === "string" ? nameOrSpec : nameOrSpec?.name;
			registry.flags.set(name, { ...spec, name });
			return api;
		},
		registerMessageRenderer(selector, render) {
			registry.renderers.push({ kind: "message", selector: selector ?? "", render });
			return api;
		},
		registerEntryRenderer(selector, render) {
			registry.renderers.push({ kind: "entry", selector: selector ?? "", render });
			return api;
		},

		// A provider registered from out of process would have to stream
		// through this pipe for every token. That is a different feature with
		// a different cost, and shipping a slow one silently would be worse
		// than not shipping it.
		registerProvider: unsupported("registerProvider"),
		setEditorComponent: unsupported("setEditorComponent"),

		// --- actions ---
		async sendMessage(message, deliverAs) {
			await send({ type: "action", method: "sendMessage", params: { message, deliverAs } });
		},
		async sendUserMessage(text, deliverAs) {
			await send({
				type: "action",
				method: "sendMessage",
				params: { message: { role: "user", content: text }, deliverAs },
			});
		},
		// Pi's setters and getters for session state are synchronous, and
		// extensions use their return values inline. Over a pipe nothing can
		// be synchronous, so the shim keeps a mirror: seeded at handshake,
		// updated by the events that change it, and written through
		// optimistically when the extension sets something. The wire call
		// still happens; it is the waiting that is skipped.
		setSessionName(name) {
			ctx.state.sessionName = name;
			void send({ type: "action", method: "setSessionName", params: { name } });
		},
		getSessionName() {
			return ctx.state.sessionName ?? "";
		},
		async exec(command) {
			return await send({ type: "action", method: "exec", params: { command } });
		},
		getActiveTools() {
			return ctx.state.activeTools ?? [];
		},
		setActiveTools(names) {
			ctx.state.activeTools = [...names];
			void send({ type: "action", method: "setActiveTools", params: { names } });
		},
		getModel() {
			return ctx.state.model ?? undefined;
		},
		setModel(provider, modelId) {
			void send({ type: "action", method: "setModel", params: { provider, modelId } });
		},
		getThinkingLevel() {
			return ctx.state.thinkingLevel ?? "off";
		},
		setThinkingLevel(level) {
			ctx.state.thinkingLevel = level;
			void send({ type: "action", method: "setThinkingLevel", params: { level } });
		},
		// Pi exposes the whole command list so an extension can offer them.
		// It is mirrored for the same reason the rest is.
		getCommands() {
			return ctx.state.commands ?? [];
		},

		// --- identity, available from the moment the factory runs ---
		get name() {
			return ctx.name;
		},
		get path() {
			return ctx.path;
		},
		get cwd() {
			return ctx.cwd;
		},
		get mode() {
			return ctx.mode;
		},
		get isProjectTrusted() {
			return ctx.trusted;
		},
		getFlag(name) {
			return ctx.flags?.[name];
		},
	};

	return api;
}

/** Reduce a registered tool to what the handshake declares. */
export function declareTool(tool) {
	return {
		name: tool.name,
		description: tool.description ?? tool.label ?? "",
		// A TypeBox schema is a plain JSON Schema object at runtime, so it
		// crosses the wire as-is and reaches the provider unchanged.
		parameters: tool.parameters ?? { type: "object" },
		streaming: typeof tool.execute === "function",
	};
}
