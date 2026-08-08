# @ihavespoons/tau-pi-host

Runs [Pi](https://github.com/earendil-works/pi) coding-agent extensions against
[tau](https://github.com/ihavespoons/tau).

tau is a Go reimplementation of Pi. Its extension system has two surfaces: an
in-process Go API, and a subprocess protocol over stdio that any program can
speak. This package is the second surface, speaking Pi's dialect — it loads a Pi
extension written in TypeScript and translates between what that extension
expects and what tau sends.

## Install

```sh
npm i -g @ihavespoons/tau-pi-host
```

tau never installs this for you. Running `npx` on an extension's behalf would
fetch and execute code from the network as a side effect of opening a directory,
which is exactly what tau's project-trust gate exists to prevent.

## Use

You do not run it directly — tau does:

```sh
tau -e ./my-extension.ts
```

tau finds the shim on your PATH, spawns it with the extension's entry file, and
speaks its protocol over the pipe.

## What works

Most of Pi's extension API, because most of it is data:

- `pi.on(event, handler)` for every event tau dispatches
- `registerTool` with TypeBox schemas, including tools registered mid-session
- `registerCommand` with argument completions, `registerShortcut`, `registerFlag`
- `ctx.ui.confirm/select/input/notify/setStatus/setTitle/setWidget`
- `ctx.signal` — tau's cancel frame aborts it, so an abortable extension aborts
- The synchronous getters (`getSessionName`, `getModel`, `getActiveTools`,
  `getCommands`, `getThinkingLevel`), answered from a mirror seeded at handshake
  and kept current from events, because nothing can be synchronous across a pipe
- Message and entry renderers, run headlessly — a pi-tui component's
  `render(width) => string[]` is pure string production and needs no terminal

## What does not

Anything that needs a component tree. These throw `UnsupportedInRPCError` and
are reported once when the extension loads, rather than appearing to work:

- `setEditorComponent` — replacing the editor
- `registerProvider` — every token would cross the pipe; that is a different
  feature with a different cost, and shipping a slow one silently would be worse
  than not shipping it
- Overlays and anything else that owns the screen

An extension that needs one of these has to be ported to tau's in-process Go API.

## Requirements

Node 22.18 or newer, which strips TypeScript types natively — so this package
carries no transpiler and has no build step. TypeScript syntax that is not
erasable (enums, namespaces, parameter properties, decorators) will not load;
Node's own error names the construct.

`@sinclair/typebox` is used when it is installed, and a small compatible
fallback covers the common constructors when it is not.

## Notes

`stdout` is the protocol. The shim redirects the extension's console output to
stderr before loading it, because a single stray `console.log` would otherwise
reach tau as a malformed frame.

`ctx.mode` is `"rpc"`, whatever tau's own mode is. Pi extensions already branch
on `mode !== "tui"` to decide whether a component tree exists, and across a pipe
one does not.

## License

MIT. Derived from Pi, MIT © 2025 Mario Zechner.
