// The host: loads a Pi extension and runs it against tau's protocol.
//
// # Loading
//
// Node strips TypeScript types natively (22.18+), so a Pi extension's .ts file
// is imported directly — no jiti, no transpiler, no build step in this
// package. What that does not cover is TypeScript syntax that is not erasable:
// enums, namespaces, parameter properties, decorators. An extension using
// those fails to load with Node's own error, which names the construct.
//
// Pi's package names are aliased to the bridge with a module resolve hook, so
// `import { defineTool } from "@earendil-works/pi-coding-agent"` finds an
// implementation backed by the wire.
//
// # stdout is the protocol
//
// A stray console.log in an extension would be read by tau as a malformed
// frame. So stdout is hijacked before the extension loads: console output goes
// to stderr, and only frames reach the real stdout.

import { pathToFileURL } from "node:url";
import { register } from "node:module";
import { createAPI, declareTool, Registry } from "./bridge.mjs";
import { Conn, readLines } from "./wire.mjs";

export const PROTOCOL = 1;

/**
 * Event payload translation, tau -> Pi.
 *
 * tau's event structs were ported from Pi's, so most names match by
 * construction and are absent from this table. What is here is where they
 * genuinely differ, and each entry is a place a Pi extension would otherwise
 * read `undefined` and silently do nothing.
 */
const TO_PI = {
	tool_call(ev) {
		// Pi calls the validated arguments `input`; tau calls them `args`.
		return { ...ev, input: ev.args ?? {} };
	},
	tool_result(ev) {
		return { ...ev, output: ev.content };
	},
	session_before_switch(ev) {
		// Pi's event carries a reason its handlers branch on. tau does not
		// distinguish, and "resume" is the case that keeps confirm-style
		// extensions asking rather than silently taking the "new" branch.
		return { reason: "resume", ...ev };
	},
};

/** Result translation, Pi -> tau. */
const FROM_PI = {
	tool_call(result, ev) {
		// tau needs the block decision and the argument rewrite separately: a
		// Pi handler mutates event.input in place, which cannot cross a pipe,
		// so the mutated object travels back explicitly.
		return { result: result ?? null, args: ev.input };
	},
};

function toPi(name, payload) {
	const fn = TO_PI[name];
	return fn ? fn(payload ?? {}) : (payload ?? {});
}

function fromPi(name, result, piEvent) {
	const fn = FROM_PI[name];
	if (fn) return fn(result, piEvent);
	return result ?? null;
}

/** Hijack stdout so only frames reach tau. */
function protectStdout() {
	const realWrite = process.stdout.write.bind(process.stdout);
	process.stdout.write = (chunk, encoding, cb) => process.stderr.write(chunk, encoding, cb);
	return realWrite;
}

/**
 * Run one extension file against tau on the given streams.
 *
 * Exported so the tests can drive it over pipes without spawning a process.
 */
export async function run({ entry, input = process.stdin, output, onExit } = {}) {
	const frameWrite = output ?? protectStdout();
	const out = { write: (s) => frameWrite(s) };
	const conn = new Conn(out);

	const registry = new Registry();
	const ctx = { handshakeComplete: false, flags: {}, state: {} };

	// The resolve hook has to be installed before the extension is imported,
	// and it needs the bridge's URL, which is fixed.
	installAliases();

	const send = async (frame) => {
		const reply = await conn.ask(frame);
		return reply.payload;
	};

	const api = createAPI(registry, ctx, send);
	let factory;

	readLines(input, (line) => {
		let frame;
		try {
			frame = JSON.parse(line);
		} catch (err) {
			conn.log("error", `undecodable frame: ${err.message}`);
			return;
		}
		void route(frame);
	});

	input.on("end", () => {
		if (onExit) onExit(0);
		else process.exit(0);
	});

	async function route(frame) {
		switch (frame.type) {
			case "init":
				await handleInit(frame);
				return;
			case "shutdown":
				if (onExit) onExit(0);
				else process.exit(0);
				return;
			case "reply":
				conn.deliverReply(frame);
				return;
			case "cancel":
				conn.cancel(frame.id);
				return;
			case "event":
				await handleEvent(frame);
				return;
			case "tool_execute":
				await handleTool(frame);
				return;
			case "command":
				await handleCommand(frame);
				return;
			case "completions":
				await handleCompletions(frame);
				return;
			case "shortcut":
				await handleShortcut(frame);
				return;
			case "render":
				await handleRender(frame);
				return;
			default:
				conn.log("warn", `unexpected frame ${frame.type}`);
		}
	}

	async function handleInit(frame) {
		Object.assign(ctx, {
			name: frame.name,
			path: frame.path,
			cwd: frame.cwd,
			// tau reports "rpc" to every out-of-process extension. Pi
			// extensions already branch on mode !== "tui" to decide whether a
			// component tree exists, and here one does not.
			mode: frame.mode ?? "rpc",
			trusted: !!frame.trusted,
			flags: frame.flags ?? {},
			generation: frame.generation ?? 0,
			state: {
				sessionName: frame.state?.sessionName ?? "",
				model: frame.state?.model,
				thinkingLevel: frame.state?.thinkingLevel ?? "off",
				activeTools: frame.state?.activeTools ?? [],
				commands: frame.state?.commands ?? [],
			},
		});

		if (frame.protocol !== PROTOCOL) {
			conn.write({
				type: "init_result",
				protocol: PROTOCOL,
				error: `this shim speaks protocol ${PROTOCOL}, tau speaks ${frame.protocol}`,
			});
			return;
		}

		try {
			const mod = await import(pathToFileURL(entry).href);
			factory = mod.default ?? mod.activate ?? mod.extension;
			if (typeof factory !== "function") {
				throw new Error("the extension has no default export function");
			}
			await factory(api);
		} catch (err) {
			conn.write({
				type: "init_result",
				protocol: PROTOCOL,
				error: `loading ${entry}: ${err?.stack ?? err}`,
			});
			return;
		}

		ctx.handshakeComplete = true;
		conn.write({
			type: "init_result",
			protocol: PROTOCOL,
			name: frame.name,
			subscriptions: [...registry.handlers.keys()],
			tools: [...registry.tools.values()].map(declareTool),
			commands: [...registry.commands.values()].map((c) => ({
				name: c.name,
				description: c.description ?? "",
				completions: typeof c.getArgumentCompletions === "function",
			})),
			shortcuts: [...registry.shortcuts.values()].map((s) => ({
				key: s.key,
				description: s.description ?? "",
			})),
			flags: [...registry.flags.values()].map((f) => ({
				name: f.name,
				description: f.description ?? "",
				type: f.type ?? "string",
				default: f.default,
			})),
			renderers: registry.renderers.map((r) => ({ kind: r.kind, selector: r.selector })),
			warnings: registry.warnings,
		});
	}

	/** The context object a Pi handler receives as its second argument. */
	function extensionContext(signal) {
		return {
			signal,
			cwd: ctx.cwd,
			mode: ctx.mode,
			// Pi extensions gate every dialog on this. tau's own UI may be a
			// terminal or an rpc client, and either can answer, so it is true
			// whenever the connection is live.
			hasUI: true,
			isProjectTrusted: ctx.trusted,
			ui: makeUI(conn),
			...actions(),
		};
	}

	function actions() {
		return {
			sendMessage: api.sendMessage,
			sendUserMessage: api.sendUserMessage,
			exec: api.exec,
			getActiveTools: api.getActiveTools,
			setActiveTools: api.setActiveTools,
			getModel: api.getModel,
			setModel: api.setModel,
			getThinkingLevel: api.getThinkingLevel,
			setThinkingLevel: api.setThinkingLevel,
			setSessionName: api.setSessionName,
			getSessionName: api.getSessionName,
		};
	}

	/**
	 * Keep the mirror current.
	 *
	 * Pi's getters are synchronous, so the shim answers them from here rather
	 * than from a round trip. That only stays true if the events that change
	 * the underlying state are applied — otherwise an extension asking for the
	 * model after the user switched it gets the one from startup.
	 */
	function updateMirror(name, payload) {
		switch (name) {
			case "session_info_changed":
				if (payload?.name !== undefined) ctx.state.sessionName = payload.name;
				break;
			case "model_select":
				if (payload?.model) ctx.state.model = payload.model;
				break;
			case "thinking_level_select":
				if (payload?.level !== undefined) ctx.state.thinkingLevel = payload.level;
				break;
		}
	}

	async function handleEvent(frame) {
		updateMirror(frame.event, frame.payload);
		const handlers = registry.handlers.get(frame.event) ?? [];
		const piEvent = toPi(frame.event, frame.payload);

		const controller = conn.track(frame.id);
		let result = null;
		try {
			for (const h of handlers) {
				// Pi's own composition is per-event, and tau applies it on its
				// side across extensions. Within one extension the last
				// non-undefined result wins, which is Pi's behaviour for
				// multiple handlers on the same event.
				const r = await h(piEvent, extensionContext(controller.signal));
				if (r !== undefined && r !== null) result = r;
			}
		} catch (err) {
			conn.untrack(frame.id);
			if (!frame.noReply) conn.reply(frame.id, undefined, err);
			return;
		}
		conn.untrack(frame.id);

		// A hot event carries no reply. Answering would put a frame on the
		// wire that no request is waiting for.
		if (frame.noReply) return;
		conn.reply(frame.id, fromPi(frame.event, result, piEvent));
	}

	async function handleTool(frame) {
		const tool = registry.tools.get(frame.tool);
		if (!tool || typeof tool.execute !== "function") {
			conn.reply(frame.id, undefined, new Error(`no tool named ${frame.tool}`));
			return;
		}
		const controller = conn.track(frame.id);
		const onUpdate = (partial) => {
			conn.write({
				type: "tool_update",
				id: frame.id,
				partial: { output: textOf(partial), details: partial?.details },
			});
		};
		try {
			const res = await tool.execute(
				frame.callId,
				frame.args ?? {},
				controller.signal,
				onUpdate,
				extensionContext(controller.signal),
			);
			conn.reply(frame.id, {
				output: textOf(res),
				details: res?.details,
				isError: !!res?.isError,
			});
		} catch (err) {
			// Pi's contract: a tool that throws produces an error result the
			// model can read, not a transport failure.
			conn.reply(frame.id, { output: String(err?.message ?? err), isError: true });
		} finally {
			conn.untrack(frame.id);
		}
	}

	async function handleCommand(frame) {
		const command = registry.commands.get(frame.name);
		if (!command) {
			conn.reply(frame.id, undefined, new Error(`no command named ${frame.name}`));
			return;
		}
		const controller = conn.track(frame.id);
		try {
			await command.handler(frame.args ?? "", extensionContext(controller.signal));
			conn.reply(frame.id, null);
		} catch (err) {
			conn.reply(frame.id, undefined, err);
		} finally {
			conn.untrack(frame.id);
		}
	}

	async function handleCompletions(frame) {
		const command = registry.commands.get(frame.name);
		if (!command || typeof command.getArgumentCompletions !== "function") {
			conn.reply(frame.id, { items: [] });
			return;
		}
		try {
			const items = (await command.getArgumentCompletions(frame.prefix ?? "")) ?? [];
			conn.reply(frame.id, {
				items: items.map((i) =>
					typeof i === "string"
						? { value: i, label: i }
						: { value: i.value, label: i.label, description: i.description },
				),
			});
		} catch (err) {
			conn.reply(frame.id, undefined, err);
		}
	}

	async function handleShortcut(frame) {
		const shortcut = registry.shortcuts.get(frame.key);
		if (!shortcut) {
			conn.reply(frame.id, null);
			return;
		}
		const controller = conn.track(frame.id);
		try {
			await shortcut.handler(extensionContext(controller.signal));
			conn.reply(frame.id, null);
		} catch (err) {
			conn.reply(frame.id, undefined, err);
		} finally {
			conn.untrack(frame.id);
		}
	}

	async function handleRender(frame) {
		// An extension may register several renderers of one kind, each
		// claiming a different role or entry type. Matching on kind alone
		// would hand every message to whichever was registered first.
		const sel = frame.selector ?? "";
		const claims = (r) => r.kind === frame.kind && (!r.selector || r.selector === sel);
		const renderer = registry.renderers.find(claims);
		if (!renderer) {
			conn.reply(frame.id, { lines: [] });
			return;
		}
		try {
			// Pi's Component.render(width) returns string[] and is pure string
			// production — no terminal needed, which is what makes running a
			// pi-tui renderer headlessly possible at all.
			const out = await renderer.render(frame.payload, frame.width ?? 80);
			conn.reply(frame.id, { lines: normalizeLines(out) });
		} catch (err) {
			conn.reply(frame.id, undefined, err);
		}
	}
}

function textOf(result) {
	if (result == null) return "";
	if (typeof result === "string") return result;
	if (Array.isArray(result?.content)) {
		return result.content
			.filter((c) => c?.type === "text")
			.map((c) => c.text)
			.join("");
	}
	if (typeof result.output === "string") return result.output;
	return "";
}

function normalizeLines(out) {
	if (out == null) return [];
	if (Array.isArray(out)) return out.map(String);
	if (typeof out === "string") return out.split("\n");
	if (typeof out.render === "function") return normalizeLines(out.render(80));
	return [];
}

/** The ui object on a handler's context, backed by ui_request frames. */
function makeUI(conn) {
	const ask = async (req) => {
		const reply = await conn.ask({ type: "ui_request", ...req });
		return reply;
	};
	return {
		async confirm(title, message) {
			const reply = await ask({ method: "confirm", title, message });
			if (reply.cancelled) return false;
			return !!reply.payload?.confirmed;
		},
		async select(title, options) {
			const reply = await ask({ method: "select", title, options });
			if (reply.cancelled) return undefined;
			return reply.payload?.value;
		},
		async input(title, placeholder) {
			const reply = await ask({ method: "input", title, placeholder });
			if (reply.cancelled) return undefined;
			return reply.payload?.value;
		},
		async editor(title, prefill) {
			const reply = await ask({ method: "editor", title, prefill });
			if (reply.cancelled) return undefined;
			return reply.payload?.value;
		},
		notify(message, type) {
			void ask({ method: "notify", message, notifyType: type ?? "info" });
		},
		setStatus(key, text) {
			void ask({ method: "setStatus", statusKey: key, statusText: text ?? null });
		},
		setTitle(title) {
			void ask({ method: "setTitle", title });
		},
		setWidget(key, component, placement) {
			void ask({
				method: "setWidget",
				widgetKey: key,
				widgetLines: component == null ? null : normalizeLines(component),
				widgetPlacement: placement ?? "aboveEditor",
			});
		},
	};
}

let aliasesInstalled = false;

/**
 * Alias Pi's package names to the bridge.
 *
 * A Pi extension imports from packages that are not installed here and never
 * will be — resolving them to the bridge is what makes an unmodified extension
 * runnable at all.
 */
export function installAliases() {
	if (aliasesInstalled) return;
	aliasesInstalled = true;
	register("./hooks.mjs", import.meta.url);
}
