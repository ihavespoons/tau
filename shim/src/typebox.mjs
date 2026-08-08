// TypeBox, or enough of it.
//
// Pi extensions declare tool parameters with `Type.Object({...})` from
// @sinclair/typebox. A TypeBox schema is a plain JSON Schema object at runtime
// — the library's value is in its types, which do not survive to runtime — so
// what crosses the wire is the same either way.
//
// The real package is preferred when it is installed: an extension using a
// constructor this file does not implement should get the real behaviour
// rather than a near-miss. The fallback exists so that `npm i -g` of this shim
// pulls in nothing, and an extension that only uses the common constructors
// works on a machine with no network.

let real;
try {
	// Both names, because Pi re-exports it under the short one.
	real = (await import("@sinclair/typebox")).Type;
} catch {
	try {
		real = (await import("typebox")).Type;
	} catch {
		real = undefined;
	}
}

/** Whether the real library is in use, for diagnostics. */
export const usingRealTypeBox = real !== undefined;

const clean = (opts) => {
	const out = { ...(opts ?? {}) };
	delete out.$id;
	return out;
};

const fallback = {
	Object(properties, opts) {
		// TypeBox makes every property required unless wrapped in Optional,
		// which is the opposite of most JSON Schema builders and exactly the
		// behaviour a Pi extension author is relying on.
		const required = Object.entries(properties ?? {})
			.filter(([, v]) => !v?.[optionalMark])
			.map(([k]) => k);
		const props = {};
		for (const [k, v] of Object.entries(properties ?? {})) {
			const copy = { ...v };
			delete copy[optionalMark];
			props[k] = copy;
		}
		const schema = { type: "object", properties: props, ...clean(opts) };
		if (required.length > 0) schema.required = required;
		return schema;
	},
	String: (opts) => ({ type: "string", ...clean(opts) }),
	Number: (opts) => ({ type: "number", ...clean(opts) }),
	Integer: (opts) => ({ type: "integer", ...clean(opts) }),
	Boolean: (opts) => ({ type: "boolean", ...clean(opts) }),
	Null: () => ({ type: "null" }),
	Any: (opts) => ({ ...clean(opts) }),
	Unknown: (opts) => ({ ...clean(opts) }),
	Array: (items, opts) => ({ type: "array", items, ...clean(opts) }),
	Literal: (value, opts) => ({ const: value, ...clean(opts) }),
	Union: (variants, opts) => ({ anyOf: variants, ...clean(opts) }),
	Record: (_key, value, opts) => ({ type: "object", additionalProperties: value, ...clean(opts) }),
	Optional: (schema) => ({ ...schema, [optionalMark]: true }),
};

const optionalMark = Symbol.for("tau/typebox/optional");

export const Type = real ?? fallback;
