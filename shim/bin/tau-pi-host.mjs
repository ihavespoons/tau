#!/usr/bin/env node
// tau-pi-host — run a Pi coding-agent extension against tau.
//
// tau spawns this with the extension's entry file as its only argument, and
// speaks the subprocess extension protocol over stdio.

import { run } from "../src/host.mjs";

const entry = process.argv[2];
if (!entry) {
	process.stderr.write("usage: tau-pi-host <extension.ts>\n");
	process.exit(2);
}

run({ entry }).catch((err) => {
	process.stderr.write(`tau-pi-host: ${err?.stack ?? err}\n`);
	process.exit(1);
});
