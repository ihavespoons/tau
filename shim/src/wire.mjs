// The wire: LF-framed JSONL over stdio, and the request bookkeeping on top.
//
// This is the mirror image of extension/wire/serve.go. Where the two could
// differ, the Go side is the definition — it is what the conformance suite
// drives, and what tau's own host was written against.

import { StringDecoder } from "node:string_decoder";

/**
 * Serialize one record.
 *
 * JSON.stringify escapes every control character inside strings, so the only
 * newline in the output is the terminator this adds.
 */
export function serialize(value) {
	return `${JSON.stringify(value)}\n`;
}

/**
 * Read LF-framed records from a stream.
 *
 * Deliberately not node:readline. Readline splits on \r, \n, and — the reason
 * this exists — U+2028 and U+2029, which are legal inside a JSON string. A
 * payload containing one would be torn in half and reported as two parse
 * errors instead of one message.
 */
export function readLines(stream, onLine) {
	const decoder = new StringDecoder("utf8");
	let buffer = "";

	stream.on("data", (chunk) => {
		buffer += typeof chunk === "string" ? chunk : decoder.write(chunk);
		for (;;) {
			const i = buffer.indexOf("\n");
			if (i === -1) return;
			const line = buffer.slice(0, i);
			buffer = buffer.slice(i + 1);
			// A trailing CR is framing from a peer whose platform translated
			// the newline, not payload.
			const trimmed = line.endsWith("\r") ? line.slice(0, -1) : line;
			if (trimmed.trim() !== "") onLine(trimmed);
		}
	});

	stream.on("end", () => {
		buffer += decoder.end();
		if (buffer.trim() !== "") onLine(buffer);
	});
}

/**
 * Conn owns the connection to tau: it writes frames, tracks the requests this
 * process originated, and hands inbound frames to a router.
 */
export class Conn {
	constructor(output) {
		this.output = output;
		this.pending = new Map();
		this.inflight = new Map();
		this.nextId = 0;
	}

	write(frame) {
		this.output.write(serialize(frame));
	}

	log(level, message) {
		this.write({ type: "log", level, message: String(message) });
	}

	newId() {
		this.nextId += 1;
		return `x${this.nextId}`;
	}

	/** Send a request to tau and wait for its reply. */
	ask(frame) {
		const id = this.newId();
		frame.id = id;
		return new Promise((resolve, reject) => {
			this.pending.set(id, { resolve, reject });
			this.write(frame);
		});
	}

	/** Route a reply to whoever is waiting for it. */
	deliverReply(reply) {
		const waiter = this.pending.get(reply.id);
		if (!waiter) return;
		this.pending.delete(reply.id);
		if (reply.error) {
			waiter.reject(new Error(reply.error));
			return;
		}
		waiter.resolve(reply);
	}

	/**
	 * Register an AbortController for an inbound request, so tau's cancel
	 * frame can reach the handler running it. Pi extensions are written
	 * against `ctx.signal`, so this is what makes an abortable Pi extension
	 * abortable here.
	 */
	track(id) {
		const controller = new AbortController();
		this.inflight.set(id, controller);
		return controller;
	}

	untrack(id) {
		this.inflight.delete(id);
	}

	cancel(id) {
		const controller = this.inflight.get(id);
		if (controller) controller.abort();
	}

	/** Answer a request from tau. */
	reply(id, payload, error) {
		const frame = { type: "result", id };
		if (error) {
			frame.error = error instanceof Error ? error.message : String(error);
		} else if (payload !== undefined && payload !== null) {
			frame.payload = payload;
		}
		this.write(frame);
	}
}
