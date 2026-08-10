# tau

A coding agent for your terminal. tau is a Go reimplementation of
[Pi](https://github.com/earendil-works/pi) — same architecture, same feature surface,
same minimalist philosophy — shipped as a single static binary with no runtime
dependencies.

> **Status: early development.** tau is being built in phases toward parity with
> Pi v0.82.1. The interactive agent, all ten wire APIs, session trees, Pi
> import and the extension system are in; skills, themes and the HTML export
> are still landing.
>
> **tau does not ask before it acts.** It edits files and runs shell commands
> without a confirmation prompt. Run it on a clean git tree or in a scratch
> directory until approval gating lands.

## Goals

- **Single binary.** No Node, no npm. `brew install` or download one file.
- **Pi parity.** The agent loop, session trees, providers, extensions, themes,
  skills, and modes you know from Pi, in idiomatic Go.
- **Easy migration.** tau reads your existing `~/.pi` state (sessions, settings,
  auth, models) and imports it. Existing Pi TypeScript extensions run against tau
  via a host shim.
- **Libraries first.** `ai`, `agent`, `session`, `tools`, and `extension` are
  importable Go packages, not just internals of the CLI.

## Install

```sh
brew install ihavespoons/tap/tau
# or
go install github.com/ihavespoons/tau/cmd/tau@latest
```

## Use

```sh
tau login                  # Claude Pro/Max OAuth, or an API key with -k
tau                        # interactive session in the current directory
tau -c                     # …continuing the most recent one
tau -p "fix the failing test"    # one-shot, non-interactive
tau -p --mode json "…"     # JSONL events, for scripts and CI
```

Tools: `read`, `write`, `edit`, `bash`, `grep`, `find`, `ls`. Search runs on
ripgrep and fd, found on your PATH or downloaded into `~/.tau/agent/bin` on
first use — set `TAU_OFFLINE=1` to refuse the download instead.

Inside a session: `Enter` sends, `Alt+Enter` adds a newline, `Esc` stops the
agent, `Ctrl+P` cycles models, `Ctrl+T` cycles thinking level, and `/help` lists
the commands. Typing while the agent is working *steers* it — the message is
delivered at the next turn boundary instead of starting a competing run.

The transcript is printed into your terminal's scrollback rather than an
alternate screen, so scrolling, selection, and search keep working, and session
length costs nothing to render.

## Other providers

**37 providers and 1,153 models ship compiled in** — Anthropic, OpenAI,
OpenRouter, Google, Groq, Cerebras, xAI, DeepSeek, Mistral, Together, Fireworks,
Moonshot, Z.ai, MiniMax, NVIDIA, Bedrock, Vertex, Azure, Copilot, the Vercel and
Cloudflare gateways, and the rest. Set the provider's API key environment
variable and its models appear in `tau models`, ready to select.

The catalog is generated from models.dev and diffed field-by-field against
Pi's own, so the per-model quirks come along with it: which thinking levels a
model accepts, which token field it wants, where its pricing tier changes,
whether it needs cache markers, which wire it actually speaks.

All ten wire APIs are live: Anthropic messages, OpenAI chat-completions, OpenAI
responses, Azure OpenAI, Gemini, Vertex AI, Mistral, Bedrock ConverseStream, the
ChatGPT Codex backend, and pi-messages — Pi's own protocol. Every provider in
the catalog is reachable.

Seven providers have a login rather than a key, so a subscription you already
pay for works without one:

```sh
tau login                    # Anthropic — Claude Pro/Max
tau login github-copilot     # device flow
tau login openai-codex       # ChatGPT subscription
tau login openrouter         # browser; yields a permanent API key
tau login xai                # device flow — SuperGrok or X Premium
tau login kimi-coding        # device flow
tau login radius             # browser or device code
```

Radius is a gateway rather than a vendor, and it publishes no fixed model list:
which models you can reach is a property of your account. Logging in fetches the
catalog and caches it in `~/.tau/agent/models-store.json`, so it is there on the
next start without a network round trip; logging in again refreshes it. Point
tau at your own deployment with `TAU_RADIUS_GATEWAY`.

Two authenticate from the ambient cloud environment instead. Vertex uses
Application Default Credentials, so `gcloud auth application-default login` is
enough; set `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION`. Bedrock uses the
standard AWS chain — `AWS_PROFILE`, environment keys, SSO, or an instance role —
and honours `AWS_REGION`, inference-profile ARNs, and `AWS_BEARER_TOKEN_BEDROCK`
for Bedrock API keys. Everything else takes an API key from the environment.

Anything not listed that speaks `/v1/chat/completions` — vLLM, llama.cpp,
LiteLLM, Ollama, a private gateway — can be declared in
`~/.tau/agent/models.json` and used immediately:

```jsonc
{
  "providers": {
    "openrouter": {
      "name": "OpenRouter",
      "baseUrl": "https://openrouter.ai/api/v1",
      // A "$VAR" reference is read from the environment, so the key
      // never sits in a file.
      "apiKey": "$OPENROUTER_API_KEY",
      "models": [
        {
          "id": "anthropic/claude-sonnet-4.5",
          "reasoning": true,
          "input": ["text", "image"],
          "contextWindow": 1000000,
          "maxTokens": 64000,
          "cost": { "input": 3.0, "output": 15.0, "cacheRead": 0.3, "cacheWrite": 3.75 }
        }
      ]
    }
  }
}
```

Then `tau --model openrouter/anthropic/claude-sonnet-4.5`, or set
`defaultModel` in settings. Omitting `api` assumes `openai-completions`, which
is what nearly every endpoint speaks; the per-provider quirks are detected from
the provider id and base URL. Any of the ten wires can be named instead —
`"api": "pi-messages"` reaches a backend speaking Pi's own protocol.

Qualify a model with its provider — `anthropic/claude-sonnet-5`, not
`claude-sonnet-5`. A bare id is matched loosely across the whole catalog, and a
dozen providers resell the same models.

To refresh the compiled catalogs after an upstream change:
`go run ./cmd/tau-genmodels -refresh`.

## Long sessions

A conversation that outgrows the model's context window is compacted: the older
part is replaced by a structured summary, and recent work is kept verbatim. It
happens on its own before a turn that would not fit, and again if the provider
rejects one for length anyway — which is the case no estimate can predict, and
the one where the alternative is a failed turn. `/compact` forces it, and takes
an optional focus (`/compact keep the migration details`).

Files are tracked separately from the prose. A summary may paraphrase what was
decided; it must not paraphrase which files were read and which were changed, so
those come out as an exact list.

## Session trees

A session is an append-only tree, not a list. Nothing is ever deleted:

```
/tree            go back to an earlier point; later branches stay in the file
/fork            copy the session up to a chosen message and continue there
/clone           copy the whole session
/label <text>    bookmark where you are, so it is findable later
```

Going back offers to summarize the branch being left, so the next turn knows the
exploration happened instead of silently losing it.

## Coming from Pi

`tau import` reads `~/.pi/agent` and reports what is there. Nothing is copied
until you say what to bring:

```sh
tau import                      # what does this Pi installation contain?
tau import --sessions           # history only
tau import --all --dry-run      # everything — but show me first
tau import --all                # everything, including stored credentials
```

Sessions land where tau looks for them, because the working-directory encoding
is byte-identical to Pi's: an imported conversation is found by `tau -c` in the
directory it came from. Older session formats are migrated on the way in.

Your Pi installation is never modified — not the sessions, not the credentials.
The two can coexist while you decide, and an import that damages what it
imported is not one you would run twice. Files that already exist under `~/.tau`
are left alone unless you pass `--overwrite`.

## Extensions

An extension observes and shapes what the agent does: gate a tool call, rewrite
the context, register tools and commands, ask the user something. There are two
ways to write one, and they are the same feature.

**In-process, in Go** — `import "github.com/ihavespoons/tau/extension"`, write a
factory, compile it in. This is what the bundled MCP client is.

**Out of process, in anything** — a program that reads and writes JSON lines on
stdin and stdout. tau spawns it, hands it the events it subscribed to, and
treats it exactly like a compiled-in one: the same composition rules, the same
error handling, the same tool registry.

```sh
tau -e ./my-extension.ts        # repeatable
```

Discovery, nearest first: `-e` flags, then `extensions` in your settings, then
`~/.tau/agent/extensions/`, then `.tau/extensions/` — the last only in a
directory you have trusted, because cloning a repository must not be enough to
make tau launch what is in it.

A `.ts` or `.js` file runs under the host shim; an executable runs directly.
tau never installs anything on an extension's behalf.

### Keys and renderers

An extension can bind a key and draw a message, and both work the same whether
it is compiled in or spawned:

- **Shortcuts** — `Ctrl+C` and `Esc` always reach tau, because interrupt and
  abort are the two ways out of a wedged turn. Every other key is claimable,
  including tau's own bindings, so an extension can take over `Ctrl+P`; a
  shortcut bound to a bare letter will shadow typing. When two extensions bind
  the same key the first loaded wins.
- **Message renderers** — a renderer draws a transcript message in place of
  tau's own rendering. Returning no lines means *no opinion* and the built-in
  rendering runs, so hiding a message takes a blank line rather than an empty
  list. A renderer is called on the draw path with a 100 ms deadline; one that
  fails or overruns it falls back to the built-in rendering instead of stalling
  the transcript.

Entry renderers can be registered and reach a subprocess extension, but nothing
calls them yet: tau's transcript is built from messages, not session entries.

### Pi extensions

Pi's TypeScript extensions run on tau unmodified, through a shim you install
once:

```sh
npm i -g @ihavespoons/tau-pi-host
tau -e ~/.pi/agent/extensions/my-extension.ts
```

The shim aliases Pi's packages to an implementation backed by the protocol, so
`import { defineTool } from "@earendil-works/pi-coding-agent"` resolves and
works. Node 22.18+ strips TypeScript types natively, so there is no build step.

What does not cross a pipe is anything that needs to draw: custom editors,
overlays, and provider registration throw a named error and are reported once at
load, rather than appearing to work and silently doing nothing. Those need a Go
port.

## Driving tau from another program

`--mode rpc` turns tau into a JSONL server on stdin and stdout — the mode for an
editor or a supervisor rather than a person:

```sh
tau --mode rpc
```

Commands go in, responses and events come out, one JSON value per line. Unlike
`-p`, the connection stays usable while a turn runs: you can steer it, abort it,
navigate the session tree, or answer a dialog an extension opened. The command
and event shapes are Pi's, so a client written against Pi drives tau unchanged.

## MCP

tau speaks the Model Context Protocol through a bundled extension. Configure
servers in `~/.tau/agent/mcp.json`, using the same `mcpServers` shape other MCP
hosts read:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    },
    "example": {
      "url": "https://example.com/mcp",
      "headers": { "Authorization": "Bearer …" }
    }
  }
}
```

A project may add servers in `.tau/mcp.json`, but only in a directory you have
trusted — otherwise cloning a repository would be enough to make tau launch a
process. Run `/mcp` to see what connected.

Everything MCP does goes through the public `extension` package, which is the
point: it is the acceptance test for that API, not a privileged core feature.

## License

MIT. tau is derived from Pi, MIT © 2025 Mario Zechner — see
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
