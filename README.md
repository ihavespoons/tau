# tau

A coding agent for your terminal. tau is a Go reimplementation of
[Pi](https://github.com/earendil-works/pi) — same architecture, same feature surface,
same minimalist philosophy — shipped as a single static binary with no runtime
dependencies.

> **Status: early development.** tau is being built in phases toward parity with
> Pi v0.82.1. The interactive agent works on Anthropic models; other providers,
> session trees, and the extension subprocess protocol are still landing.
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
tau login                  # Claude Pro/Max OAuth, or an API key
tau                        # interactive session in the current directory
tau -c                     # …continuing the most recent one
tau -p "fix the failing test"    # one-shot, non-interactive
tau -p --mode json "…"     # JSONL events, for scripts and CI
```

Inside a session: `Enter` sends, `Alt+Enter` adds a newline, `Esc` stops the
agent, `Ctrl+P` cycles models, `Ctrl+T` cycles thinking level, and `/help` lists
the commands. Typing while the agent is working *steers* it — the message is
delivered at the next turn boundary instead of starting a competing run.

The transcript is printed into your terminal's scrollback rather than an
alternate screen, so scrolling, selection, and search keep working, and session
length costs nothing to render.

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
