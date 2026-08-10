# Changelog

All notable changes to tau are recorded here. Versions follow
[semantic versioning](https://semver.org/); the `0.x` series is pre-1.0, so
minor bumps may still change behaviour.

## [0.22.0] - 2026-08-10

- Undo (`ctrl+-`). A run of typing, or a run of backspaces, is one step: undo
  takes back a word rather than a letter. The stack ends at a submission, so it
  cannot resurrect a prompt that has already been sent.
- A kill ring. `ctrl+w`, `alt+d`, `ctrl+u` and `ctrl+k` put what they remove on
  it, `ctrl+y` yanks the last kill back, and `alt+y` walks further back. Single
  characters stay off it — a backspace is a correction, not a cut. The ring
  outlives a submission, so text cut from one prompt can be yanked into the
  next.
- `ctrl+g` opens the prompt in `$VISUAL` or `$EDITOR` and adopts what comes
  back, as one undoable edit. If the editor fails, the prompt is left as it was.
- `pageUp`, `pageDown` and `copy` are documented as deliberately unclaimed: tau
  prints into the terminal's own scrollback, so scrolling and selection are the
  terminal's, and binding those keys would take them away from it.

## [0.21.1] - 2026-08-10

- Rendering a transcript allocates about a tenth less: theme colours resolve
  once per renderer instead of once per heading, bullet and fence, and
  highlighting no longer splits every syntax token on newlines it does not
  contain.
- Whitespace inside code blocks is no longer painted. A foreground colour on a
  space is invisible, so the escape sequences around it only added bytes — to
  the terminal's work and to what lands on the clipboard when code is copied.

## [0.21.0] - 2026-08-10

- Markdown is rendered by tau rather than by glamour. Your theme's ten `md*`
  colours and nine `syntax*` colours now reach the screen — until this release
  they were parsed, validated and ignored, and every transcript was painted
  with glamour's own palette.
- Code blocks keep their fences and language, and are highlighted with the
  theme's syntax colours. Tables, task lists, nested lists, blockquotes and
  strikethrough all render; long table cells wrap instead of being cut.
- Headings, links and struck-through text no longer emit one full escape
  sequence per character, which is what lipgloss does when asked for underline
  or strikethrough. A heading is roughly a tenth of the bytes it was.
- Prose still inherits the terminal's own foreground. A theme that declares no
  colour for something renders plainly rather than in a colour tau invented.

## [0.20.0] - 2026-08-10

- The last four slash commands: `/settings`, `/scoped-models`, `/import` and
  `/changelog`. Pi opens toggle menus for the first two; these are typed
  commands, so they work headless as well as in the interface.
- The `project_trust` extension hook is live. Extensions reachable without
  trusting the project — `-e` flags, `~/.tau/agent/extensions`, and the paths
  in global settings — start first and vote before the saved decision is read.
- Bash commands see `TAU_SESSION_ID`, `TAU_SESSION_FILE`, `TAU_PROVIDER`,
  `TAU_MODEL` and `TAU_REASONING_LEVEL`, read per command so `/model` is
  reflected in the next one. A tau running inside another tau's bash tool no
  longer inherits the outer session's identity.

## [0.19.0] - 2026-08-10

- Self-contained HTML export: `/export` writes a single file that opens in any
  browser with no network access, styled with the session's own theme.
- `/export <path>.jsonl` writes the raw session tree instead, reopenable with
  `tau -session <file>`.
- `tau export <file>.jsonl [out.html]` renders a saved session without starting
  an agent.
- `/share` uploads the exported page as a secret GitHub gist through `gh`. The
  gist is unlisted, not private — anyone with the link can read the transcript.

## [0.18.0] - 2026-08-10

- Package manager for resource bundles: `tau install npm:<pkg>`, `git:<url>`
  and local paths, installed under `~/.tau/agent/{npm,git}`.
- Installed packages contribute skills, prompt templates, themes and
  extensions, with per-resource enable and disable.

## [0.17.0] - 2026-08-10

- Agent Skills (`SKILL.md`) are discovered and registered as `/skill:<name>`
  commands, and advertised in the system prompt.
- Prompt templates become slash commands that expand to their body.

## [0.16.0] - 2026-08-10

- User-remappable keybindings, namespaced the way Pi names them, loaded from
  global and project settings.

## [0.15.0] - 2026-08-10

- Loadable JSON themes in Pi's format, from `~/.tau/agent/themes` and the
  project tree.

## [0.14.0] - 2026-08-10

- Extension shortcuts and message renderers reach the TUI, closing the P8 gap
  between what an extension may register and what the interface honours.

## [0.13.0] - 2026-08-08

- Subprocess extension protocol: LF-framed JSONL over stdio, declarative
  handshake, cancellation with a grace period, fail-safe defaults and health
  scoring.
- `@ihavespoons/tau-pi-host`, a TypeScript host shim that runs Pi extensions
  against tau largely unchanged.
- `--mode rpc`, with the extension UI surface proxied over the wire.

## [0.12.0] - 2026-08-05

- Context compaction, session trees, branch navigation and labels: `/compact`,
  `/tree`, `/fork`, `/clone`, `/label`.
- `tau import` reads a real `~/.pi` session tree and continues it in tau.

## [0.11.0] - 2026-08-05

- Constrained sampling, request retries and GitHub Copilot headers on the
  OpenAI-family wires.

## [0.10.0] - 2026-08-05

- The `pi-messages` wire, and the Radius gateway it unlocks. All ten wire APIs
  and all seven OAuth logins are now live.

## [0.9.0] - 2026-08-05

- The four remaining OAuth flows: OpenRouter, Kimi, xAI and Radius.

## [0.8.0] - 2026-08-05

- The Amazon Bedrock ConverseStream wire, over the real AWS SDK, and the
  ambient-credential path it needs.

## [0.7.0] - 2026-07-29

- The OpenAI Codex backend and ChatGPT login.
- `tau login` takes a provider argument.

## [0.6.1] - 2026-07-29

- The Mistral conversations wire.

## [0.6.0] - 2026-07-29

- The Azure, Google Gemini and Google Vertex wires.
- The OAuth device-code flow, and GitHub Copilot login.

## [0.5.0] - 2026-07-29

- The OpenAI Responses wire.
- The `grep`, `find` and `ls` tools, with automatic download of the `rg` and
  `fd` binaries they run on.

## [0.4.0] - 2026-07-29

- The model catalog is complete: 1153 models across 37 providers, compiled in,
  with per-model wire dispatch.

## [0.3.0] - 2026-07-29

- The model catalog is generated from models.dev rather than hand-maintained.

## [0.2.1] - 2026-07-29

- Prompt caching over chat-completions, and a stable session id to key it on.

## [0.2.0] - 2026-07-29

- The OpenAI chat-completions wire, and the providers that speak it.

## [0.1.0] - 2026-07-29

First usable release — the interactive agent end to end.

- The `ai` core: typed streams, compat flags, cost and usage accounting, the
  Anthropic wire, credential storage and Anthropic OAuth.
- The agent loop, tool contract, and the four core tools (read, bash, edit,
  write).
- Append-only session tree in JSONL, with resume and fork.
- Settings with the project-trust gate, the system-prompt builder, the
  slash-command engine, print mode and `--mode json`.
- The extension API: 33 hooks with Pi's composition policies.
- The interactive Bubble Tea interface, and the bundled MCP client.

## [0.0.1] - 2026-07-28

- Bootstrap: module, MIT license with attribution to Pi, CI, GoReleaser and the
  Homebrew tap.
