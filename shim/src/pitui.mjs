// pi-tui, headless.
//
// Pi's renderers are built from pi-tui components, and a component's contract
// is render(width) => string[]. That is pure string production: no terminal,
// no cursor, no escape-sequence state machine. So a renderer can run out of
// process and hand its lines back, which is what the render frame carries.
//
// What cannot work is anything that reads the terminal or owns the screen —
// input handling, the differ, focus. Those are absent rather than stubbed: an
// extension that reaches for one should fail where it does so, not draw
// nothing and leave the author looking for the bug elsewhere.

/** A container that stacks children vertically. */
export class Container {
	constructor(...children) {
		this.children = children.flat();
	}
	addChild(child) {
		this.children.push(child);
		return this;
	}
	render(width) {
		return this.children.flatMap((c) => toLines(c, width));
	}
}

/** A block of text, wrapped at the given width. */
export class TextComponent {
	constructor(text = "") {
		this.text = text;
	}
	setText(text) {
		this.text = text;
		return this;
	}
	render(width) {
		return String(this.text)
			.split("\n")
			.flatMap((line) => wrap(line, width));
	}
}

/** Pi's whitespace component. */
export class WhitespaceComponent {
	constructor(lines = 1) {
		this.lines = lines;
	}
	render() {
		return Array.from({ length: this.lines }, () => "");
	}
}

export function toLines(component, width) {
	if (component == null) return [];
	if (typeof component === "string") return component.split("\n");
	if (Array.isArray(component)) return component.flatMap((c) => toLines(c, width));
	if (typeof component.render === "function") {
		const out = component.render(width);
		return Array.isArray(out) ? out.map(String) : String(out).split("\n");
	}
	return [String(component)];
}

/**
 * Wrap on width, counting visible characters.
 *
 * ANSI sequences are zero-width, and a wrapper that counted their bytes would
 * break a styled line early and leave the terminal mid-escape.
 */
export function wrap(line, width) {
	if (!width || width <= 0) return [line];
	const out = [];
	let current = "";
	let visible = 0;
	for (const token of tokenize(line)) {
		if (token.ansi) {
			current += token.text;
			continue;
		}
		for (const ch of token.text) {
			if (visible >= width) {
				out.push(current);
				current = "";
				visible = 0;
			}
			current += ch;
			visible += 1;
		}
	}
	out.push(current);
	return out;
}

function* tokenize(line) {
	const re = /\x1b\[[0-9;]*[A-Za-z]/g;
	let last = 0;
	let m;
	while ((m = re.exec(line)) !== null) {
		if (m.index > last) yield { text: line.slice(last, m.index), ansi: false };
		yield { text: m[0], ansi: true };
		last = m.index + m[0].length;
	}
	if (last < line.length) yield { text: line.slice(last), ansi: false };
}

// Styling helpers are identity: colour survives as escape sequences in the
// strings a renderer produces, and tau draws them unchanged.
const identity = (s) => String(s);
export const chalk = new Proxy(identity, { get: () => identity });

export default { Container, TextComponent, WhitespaceComponent, toLines, wrap };
