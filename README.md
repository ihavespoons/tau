# tau

A coding agent for your terminal. tau is a Go reimplementation of
[Pi](https://github.com/earendil-works/pi) — same architecture, same feature surface,
same minimalist philosophy — shipped as a single static binary with no runtime
dependencies.

> **Status: early development.** tau is being built in phases toward parity with
> Pi v0.82.1. The interactive agent, all ten wire APIs, session trees, Pi
> import, the extension system, themes, keybindings, skills, prompt templates,
> the package manager, HTML export/share and the full slash-command set are in;
> the TUI polish pass is still landing. `/settings` is a typed command here
> rather than Pi's toggle menu — that picker is still to come.
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

`Tab` completes three things: a `/command` name, that command's argument — so
`/model ant` offers the models it matches — and a file path after `@`, anywhere
in a line. Path completion goes through fd when it is there, so it skips what
`.gitignore` skips, and falls back to reading the one directory you named when
it is not. Accepting a directory offers what is inside it.

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

In the interface `/tree` opens a picker over the whole tree rather than a list
of your own messages, because a tool result is where a session went wrong often
enough that it has to be reachable. Five views decide what is on screen —
`ctrl+o` cycles them, or go straight to one:

```
ctrl+d    default: everything except bookkeeping entries
ctrl+t    hide tool results as well
ctrl+u    your messages only
ctrl+l    labelled entries only
ctrl+a    everything, including model switches and leaf moves
```

Each of those keys toggles, so pressing it again returns to the default view.
`ctrl+left` folds the branch under the cursor and `ctrl+right` unfolds it; with
nothing to fold they move a row instead. `shift+l` labels the highlighted entry
— an emptied prompt clears the label — and `shift+t` shows when each label was
set. Typing searches the summaries and the labels, and the first `escape` takes
the search back rather than closing the picker.

Narrowing a view usually hides the row under the cursor. It lands on that row's
nearest visible ancestor instead of jumping to the top, so you keep your place.

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

## Skills and prompt templates

A **skill** is a folder with a `SKILL.md` in it — instructions the agent reads
when a task matches:

```markdown
---
name: deploy
description: How to release and roll back the service
---

Run `make release`. If the smoke test fails, `make rollback` and stop.
```

tau reads `~/.tau/agent/skills/` and `.tau/skills/` in the project. Only the
name and description go into the system prompt; the body is read on demand, so
a long skill costs nothing until it is used. Relative paths inside a skill
resolve against its own directory, which makes a skill folder with scripts
beside the markdown work the way you would expect.

Each skill is also a command — `/skill:deploy` — for when you want it applied
now rather than when the model decides. Set `"enableSkillCommands": false` in
settings to keep skills prompt-only, or put `disable-model-invocation: true` in
a skill's frontmatter to invert it: reachable by command, invisible to the
model.

A **prompt template** is a markdown file in `~/.tau/agent/prompts/` or
`.tau/prompts/` that becomes a slash command expanding to its own body:

```markdown
---
description: Review a file for bugs
argument-hint: <path>
---

Review $1 for correctness bugs. Ignore style.
```

That is `/review api.go`. Arguments substitute as `$1`, `$2`, `$@` or
`$ARGUMENTS` for the lot, `${1:-default}` when one is missing, and `${@:2}` to
drop the first.

Scanning honours `.gitignore`, `.ignore`, and `.fdignore`, so a skills folder
living next to build output does not drag it in, and a skill reached twice
through a symlink is one skill rather than a name collision. `/reload` picks up
edits to both without restarting the session.

Project-local skills and prompts are behind the trust gate: a directory you
have not approved contributes neither. Both put text in front of the model, and
cloning a repository should not be enough to steer the agent.

## Themes

Set one in `~/.tau/settings.json`:

```json
{ "theme": "dark" }
```

`dark` and `light` are built in. A custom theme is a JSON file in
`~/.tau/agent/themes/`, named by its `name` field rather than its filename, and
`themes` in settings adds further files or directories to search.

Give the setting two names separated by a slash and tau picks between them by
what your terminal is:

```json
{ "theme": "solarized-light/solarized-dark" }
```

The background is read from `COLORFGBG` when the terminal sets it, and asked for
over OSC 11 when it does not.

The file format is Pi's, unchanged — a `colors` table of 52 tokens, each a
`#rrggbb` string, a 256-colour palette index, the name of an entry in `vars`, or
`""` for no colour:

```json
{
  "name": "solarized-dark",
  "vars": { "base0": "#839496", "cyan": "#2aa198" },
  "colors": { "text": "base0", "accent": "cyan", "error": "#dc322f" }
}
```

A theme written for Pi loads in tau and vice versa; copying one out of
`~/.pi/agent/themes` is all the migration there is. A theme that fails to
load — a missing token, a variable that refers to nothing — is named at startup
with the reason, and the rest still load.

## Keybindings

The keys are in `~/.tau/agent/keybindings.json`, mapping a binding id to a key
or a list of keys:

```json
{
  "tui.app.model.cycle": ["ctrl+p", "f2"],
  "tui.input.submit": "ctrl+s",
  "tui.app.suspend": []
}
```

An empty list unbinds an action entirely. Identifiers are written
modifier-first — `ctrl+p`, `shift+ctrl+p`, `alt+left` — and modifier order
carries no meaning, so `shift+ctrl+p` and `ctrl+shift+p` are the same key.

This file is global only. There is no project-level version and no setting that
introduces one: cloning a repository should not change what Ctrl+C does in your
terminal.

The ids are Pi's, so a `keybindings.json` copied out of `~/.pi/agent` works
unchanged. A file that still uses Pi's older flat names is migrated in place the
first time tau reads it, and anything tau does not recognise is preserved
verbatim rather than dropped on the rewrite. A binding tau cannot parse — an
unknown id, a key that names nothing — falls back to its default and is reported
at startup, along with any two actions that now answer to the same key.

The defaults worth knowing:

| Key | Action |
|---|---|
| `enter` | send |
| `shift+enter`, `ctrl+j` | newline |
| `alt+enter` | queue a follow-up — sends immediately when nothing is running |
| `esc` | stop the agent |
| `ctrl+c` | clear the prompt; twice on an empty one quits |
| `ctrl+d` | quit, when the prompt is empty and nothing is running |
| `ctrl+p`, `shift+ctrl+p` | next / previous model |
| `shift+tab` | cycle the thinking level |
| `ctrl+t` | show or hide thinking blocks |
| `ctrl+z` | suspend |

The prompt is a readline: `ctrl+a`/`ctrl+e`, `ctrl+u`/`ctrl+k`, `ctrl+w`,
`alt+b`/`alt+f`, `alt+d`, and the arrows, all rebindable under `tui.editor.*`.
Typing always wins — binding an action to a bare letter shadows nothing, because
a config that made `a` untypeable would leave you unable to type the command
that undoes it.

`ctrl+-` undoes, a word or a run of typing at a time rather than a character.
`ctrl+w`, `alt+d`, `ctrl+u` and `ctrl+k` put what they remove on a kill ring;
`ctrl+y` yanks the last kill back and `alt+y` walks further back through the
ring. The ring outlives a submission, so text cut from one prompt can be yanked
into the next. `ctrl+g` opens the prompt in `$VISUAL` or `$EDITOR` and adopts
whatever comes back — as one undoable edit, so `ctrl+-` takes it back.

The session picker (`/resume`) acts on the highlighted row: `ctrl+s` flips the
order, `ctrl+p` shows full paths, `ctrl+r` renames, and `ctrl+d` deletes after
a confirmation. Every one of them closes the picker, does the thing, and opens
it again on a fresh listing. `ctrl+backspace` also deletes, but only while the
filter is empty — with nothing typed there is no text for backspace to remove,
which is what makes the key safe to reuse there.

The session-tree picker (`/tree`) has its own set — views, folding and labels —
described under [Session trees](#session-trees). `shift+l` and `shift+t` work
there because a capital letter and `shift+<letter>` are now understood as one
key; a terminal cannot spell the second, it just sends the character shift
produced. `shift+ctrl+o`, the backward cycle, is the exception: terminals send
the same byte for `ctrl+o` with and without shift, so it only fires on terminals
that speak a protocol tau does not yet read. Cycling forward wraps, so nothing
is unreachable.

Three caveats. `shift+enter` needs a terminal that distinguishes it from
`enter`, which most do not report to tau yet; `ctrl+j` is the one that always
works. `ctrl+backspace` and `ctrl+h` are the same key: a terminal sends one
byte for both, so binding them to different actions binds one key twice. Image paste is still landing, so binding `pasteImage` does nothing today.
And `pageUp`, `pageDown` and `copy` are deliberately left alone: tau prints into
the terminal's own scrollback rather than taking over the screen, so scrolling
and selection belong to the terminal, and claiming those keys would take them
away from it.

## Running shell commands

A prompt starting with `!` runs in your shell instead of going to the model:

```
!git status
!go test ./...
!!npm install          # runs, but the model never sees it
```

The prompt gutter changes colour as soon as the `!` is typed, so there is never
a question about where Enter will send the line. What the command printed is
recorded in the session and shown to the model on the next turn — which is the
point: you run the test, and the model can see why it failed without you pasting
anything.

Doubling the prefix keeps the run out of the model's context. Use it for the
command you are about to run twenty times while iterating, where each run would
cost tokens and tell the model nothing new. It is still recorded in the
transcript and in an export; it is just never sent.

This is your shell, not the model's tool: there is no approval step, no timeout,
and `$EDITOR`-style interactive programs will not work, because the command runs
without a terminal attached. Output beyond 64 KiB is captured to a file the
transcript points at.

## Settings

`/settings` reads and writes the merged configuration without leaving the
session:

```
/settings                          # what is set, and which file it came from
/settings theme                    # one key
/settings theme gruvbox-dark       # write it to ~/.tau/settings.json
/settings compaction.enabled false # one level of nesting
/settings unset theme              # remove it
```

A value is read as JSON when it parses as JSON and as a string when it does
not, so `true` is a boolean, `2` is a number, `["pnpm","add"]` is a list, and
`gruvbox-dark` is a string. Writes go to the global file; project settings are
listed with their scope but edited in `.tau/settings.json` directly, since a
directory tau has not trusted must not be written to on a one-line command.
Keys tau does not model are kept in the file and reported as unread rather than
rejected — an extension may own them.

`/scoped-models` narrows the set `ctrl+p` cycles through. On its own it opens a
checklist: space ticks a model, `ctrl+p` ticks a whole provider, `ctrl+a` and
`ctrl+x` tick everything and nothing, `alt+↑`/`alt+↓` reorder — the order is the
order cycling walks — and Enter saves. Ticking everything and ticking nothing
both mean "no scope", so both put the whole catalog back.

Patterns still work, and are the only form that works headless:

```
/scoped-models anthropic/* openai/gpt-5.2      # patterns, saved to settings
/scoped-models all                             # back to the whole catalog
```

Patterns are matched against both `provider/id` and the bare id, so
`anthropic/*` also picks up the same models offered through a reseller. A
pattern matching nothing is refused rather than saved, because an empty set
silently falls back to every model.

`/changelog` prints what changed in each release, oldest first, from the copy
compiled into the binary. `/import <path>.jsonl` is the other half of
`/export`: it copies a session file into this directory's history and continues
it, leaving the file you pointed at untouched.

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

## Packages

A package is a bundle of skills, prompt templates, themes and extensions that
someone else wrote. Install one and its resources join yours:

```sh
tau install npm:some-pack               # from npm
tau install git:github.com/you/pack     # from a git host
tau install ./pack                      # a directory already on disk
tau packages                            # what is installed, and from where
tau update                              # refresh everything unpinned
tau remove npm:some-pack
```

npm sources take a version (`npm:some-pack@1.2.3`), git sources a ref
(`git:github.com/you/pack#v2`). Either one, spelled exactly, is *pinned* and
`tau update` leaves it alone: a version you wrote down is a version you meant.
npm and git packages land under `~/.tau/agent/npm` and `~/.tau/agent/git`. A
local package is used where it lies, so `tau remove` only stops reading it —
tau does not delete your directory.

Inside a package, resources are found by convention — `skills/`, `prompts/`,
`themes/`, `extensions/` — or declared in `package.json`:

```json
{ "name": "some-pack", "tau": { "skills": ["skills/*"], "themes": ["midnight.json"] } }
```

The key is `tau`, and `pi` is accepted too, so a package written for Pi works
unchanged.

Installing records the source in `~/.tau/settings.json`. Expand that entry to
switch individual resources off without uninstalling:

```json
{
  "packages": [
    "npm:some-pack",
    { "source": "git:github.com/you/pack", "skills": ["!deploy"], "themes": [] }
  ]
}
```

Patterns filter what loads: a bare glob includes, `!` excludes, and `+`/`-`
force a single file in or out regardless of the rest. `"autoload": false` flips
the default, so nothing from the package loads except what a pattern names.

`tau install -l` writes `.tau/settings.json` instead, so the package travels
with the checkout. That path is behind the trust gate — a package is code, and
cloning a repository should not be enough to make tau fetch and run it — so an
unapproved project refuses the install and tells you to pass `-approve`.

When two packages ship a resource under the same name, the first one registered
wins: paths you wrote by hand in settings beat package paths, and a project
package beats a user one. `/reload` picks up a package installed mid-session.

## Exporting and sharing

`/export` writes the conversation to a single HTML file that opens in a browser
with no server and no network — the transcript, the tool calls, the diffs and
the system prompt are all inside it:

```
/export                      # tau-session-<name>.html, in the working directory
/export ~/notes/bug.html     # somewhere else
/export ~/notes/bug.jsonl    # a session file instead of a page
```

The suffix picks the format. A `.jsonl` path writes a session file tau can open
again with `tau --session <file>`, flattened to the current branch; anything
else renders the page. Both keep the whole history, including what a compaction
summarized away — an export is the transcript, not the model's context.

The same renderer runs on a session file you already have:

```sh
tau export <file>.jsonl                              # tau-session-<name>.html, here
tau export <file>.jsonl out.html -theme gruvbox-dark
```

`/share` exports the page and uploads it as a secret GitHub gist through the
`gh` CLI, which must be installed and logged in. **A secret gist is unlisted,
not private:** anyone holding the link can read the whole transcript, including
file contents and command output that ended up in it. Read what you are sending
before you send it.

tau prints the gist URL and stops there — there is no tau-operated viewer, and
tau will not hand your transcript to a third-party site by default. `gh gist
view --web` opens it, and downloading the file opens the page as exported. If
you run a viewer that takes a gist id in its fragment, point `TAU_SHARE_VIEWER_URL`
at it and tau will print `<url>#<gistId>` alongside the gist link.

Tool output that tau renders in the terminal but the viewer does not know about
yet — `grep`, `find`, and extension tools — falls back to a generic view in the
page. `bash`, `read`, `write`, `edit` and `ls` render fully.

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
