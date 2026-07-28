# tau

A coding agent for your terminal. tau is a Go reimplementation of
[Pi](https://github.com/earendil-works/pi) — same architecture, same feature surface,
same minimalist philosophy — shipped as a single static binary with no runtime
dependencies.

> **Status: early development.** tau is being built in phases toward parity with
> Pi v0.82.1. Nothing here is usable yet.

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

## License

MIT. tau is derived from Pi, MIT © 2025 Mario Zechner — see
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
