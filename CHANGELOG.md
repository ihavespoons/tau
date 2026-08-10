# Changelog

All notable changes to tau are recorded here. Versions follow
[semantic versioning](https://semver.org/); the `0.x` series is pre-1.0, so
minor bumps may still change behaviour.

## [0.34.0] - 2026-08-10

- A cross-provider conformance suite, `ai/conformance`. The per-wire tests each
  assert their own module; none of them could assert the claim the whole `ai`
  package rests on — that swapping providers does not change what a caller sees.
- It compares the wires against each other rather than against a written-down
  expectation, because "the wires agree" is the claim and so agreement is the
  test. Four scenarios — text, a tool call, a length stop, a 4xx — across
  Anthropic's named SSE events, OpenAI's chunk deltas and Google's candidate
  parts.
- They agree exactly. Google's fixture sends a tool call whole, with no streamed
  arguments, and still normalizes to the same `toolcall_start`/`delta`/`end` as
  the wires that stream them.
- Three dialects rather than all ten on purpose: what this proves is that
  normalization holds across genuinely different shapes. The remaining wires
  keep their own package tests but do not yet take part in the comparison.

## [0.33.0] - 2026-08-10

- An evals harness, `evals`. A task is a prompt, a seeded working directory and
  a check; the runner scores what the agent did and reports the failures first.
  It answers the question no unit test does — whether a change made tau better
  or worse at actual work.
- Checks read what the agent *did*: the files it wrote, the tools it called.
  What it changed on disk is the work; what it said about it is commentary.
- A failed check and a failed run are separate results. "The agent did the wrong
  thing" and "the agent broke" need different responses.
- A turn that failed at the provider is an error, not a pass. A stream never
  returns one — the failure arrives as a terminal event on the assistant
  message — and a harness that trusted the error return alone would score a turn
  that never happened as a success.
- Each task runs in its own directory, so one task's mess cannot score another,
  and a failed one is left on disk to be looked at.

## [0.32.0] - 2026-08-10

- `tau --mode acp` speaks the Agent Client Protocol, so an editor that drives
  ACP agents can drive tau. JSON-RPC 2.0 over stdio, one message per line,
  against schema v1.
- `initialize`, `session/new`, `session/prompt` and `session/cancel`. A turn
  streams back as `session/update` notifications — `agent_message_chunk`,
  `agent_thought_chunk`, `tool_call`, `tool_call_update` — and ends with a stop
  reason. Images in a prompt are accepted and advertised.
- No session up front, unlike `--mode rpc`: the editor names a working directory
  per `session/new` and may open several in one process.
- `loadSession` is advertised as false and tau never sends
  `session/request_permission`. An ACP session id is not a tau session file, and
  tau does not gate tools at all — claiming either would be a promise it does
  not keep.
- Cancelling answers the in-flight prompt with the `cancelled` stop reason
  rather than an error, which is what the protocol requires even when the cancel
  tore something underneath.
- The protocol constants are taken from the published schema rather than
  written from memory: method names, the update discriminators, stop reasons and
  tool statuses all come from `schema-v1.20.0`.
- Verified end to end against the built binary for `initialize` and
  `session/new`. **A full prompt turn has not yet been driven by a real ACP
  client** — that is the outstanding check.

## [0.31.0] - 2026-08-10

- `tau server`: a supervisor that owns several agents at once and hands them out
  over HTTP. Each is a `tau --mode rpc` subprocess, addressable by id and
  outliving any single connection — which that mode alone cannot be, being one
  agent on one pair of pipes.
- List, start, inspect, stop, one-shot RPC, and a server-sent-events stream of
  everything an agent emits. Responses are matched to their command by id, so
  commands in flight together cannot receive each other's replies.
- A slow event subscriber is dropped rather than allowed to block: a stalled
  HTTP client must not be able to freeze a running turn.
- A process that dies without being asked is recorded as failed with the reason,
  and its record stays listed — a client asking what happened to an agent needs
  it still in the answer. Agents go down with the server, because leaving them
  writing to session files with nothing left to address them is worse.
- **The socket is full control of every agent it owns.** tau does not ask before
  editing a file or running a shell command, so anything that can reach it runs
  commands as you. It defaults to a Unix socket at mode 0600, and refuses to
  start if a live server already holds it.
- `rpc.Writer.EmitCommand` writes the client direction. tau only ever writes the
  other three, but a supervisor driving tau needs the same framing and the same
  lock, and a second encoder would be somewhere for the escaping to drift.

## [0.30.0] - 2026-08-10

- A SQLite session backend, `storage/sqlite`. It exists for what one-file-per-
  session is bad at: thousands of sessions, a listing that would otherwise mean
  opening every file to read its first line, and querying from outside tau. WAL
  mode, so a reader can run while a session is being appended to.
- It is a satellite. The binary does not import it, so nothing is linked into
  `tau` unless a program asks for it, and the driver is pure Go — `CGO_ENABLED=0`
  still holds.
- `coding.Options.Repo` lets an embedder supply any `session.Repo`. Nil is
  still JSONL files under `~/.tau/sessions`, which is what the binary uses.
- `session.Index`, `session.NewID` and `session.EntriesToFork` are now exported.
  They are the reusable half of implementing a backend — label last-write-wins,
  the leaf derived from the entries, the fork prefix rules — and a second
  backend restating them in its own idiom would be a second set of answers to
  drift apart. Entries are stored as the bytes they arrived as and replayed
  through the same decoder, so the two backends cannot disagree about what a
  session means.

## [0.29.0] - 2026-08-10

- `/settings` opens a menu, the way Pi's does. Enter flips a toggle, or opens a
  list or a field for the rest. Every change closes the menu, writes, and opens
  it again on a freshly read file, so the new value is on screen because it was
  read back rather than assumed.
- The menu shows what is in effect, defaults included — a row reading `off`
  because nobody set it looks the same as one set to `off`, which is the point.
- Emptying a text field unsets the key rather than writing an empty string. The
  two mean different things to every setting with a default.
- The menu is curated, not generated from the known keys. Nested configuration
  is left to the typed form: a row that could only be edited as pasted JSON
  would be worse than typing the command.
- `/settings <key> ...` is unchanged and is still the only form that works
  headless, where a bare `/settings` remains the report.

## [0.28.0] - 2026-08-10

- Images. `ctrl+v` attaches one from the clipboard to your next message, so a
  screenshot of the thing you are asking about can go with the question rather
  than be described. `app.clipboard.pasteImage` has been bound since P9 and did
  nothing until now.
- Images in the transcript are drawn inline in kitty, Ghostty, WezTerm and
  iTerm2. Everywhere else — and under tmux, and in CI — they are described as
  `[png 1024×768, 240 kB]`, because knowing an image was there matters more than
  seeing it. `TAU_NO_IMAGES=1` forces the description form.
- A tool result that is only an image is no longer silently blank. An MCP server
  answering with a screenshot has something to show.
- An image is sized in terminal cells against the transcript width and capped at
  twenty rows: a screenshot in a conversation is a reference, not the
  conversation.
- The escape sequences are unit-tested against both protocols' documented
  shapes, but no one has yet watched an image appear in a real terminal. That
  first look is still outstanding.

## [0.27.0] - 2026-08-10

- `@` completes a file path, anywhere in a line. It goes through fd when fd is
  on disk, so it skips what `.gitignore` skips; without it, it reads the one
  directory you named, which is instant and never downloads anything on a
  keystroke. Accepting a directory offers what is inside it.
- `Tab` now completes a command's *argument*, not just its name. The registry
  has had a `Complete` method since P3 and nothing in the interface ever called
  it, so `/model`'s own completer and every extension's `ArgumentCompletions`
  were unreachable — both work now.
- A completion replaces the token under the caret rather than the whole line,
  and leaves the caret where the token ended, so `@` in the middle of a sentence
  works and the next keystroke lands where you were.
- Accepting a completion is one undo step.

## [0.26.0] - 2026-08-10

- `/tree` opens a picker over the whole session tree instead of a list of your
  own messages. A tool result is where a session went wrong often enough that it
  has to be reachable, and a compaction is a place you can be sitting on.
- Five views, cycled with `ctrl+o` or reached directly: default, no tool
  results, your messages only, labelled only, and everything. Each key toggles
  back to the default, so no view is a trap. `ctrl+left` and `ctrl+right` fold
  and unfold a branch, and move a row when there is nothing to fold.
- `shift+l` labels the highlighted entry from inside the picker; `shift+t` shows
  when each label was set. Typing searches summaries and labels, and the first
  `escape` takes the search back rather than closing the picker.
- A capital letter and `shift+<letter>` are now understood as one key. A
  terminal cannot spell the second — it sends the character shift produced — so
  Pi's `shift+` defaults previously could not fire at all. `A` and `a` remain
  different keys, and `Ctrl+Q` still means `ctrl+q`, because case in an
  identifier that spells its own modifiers is a human writing a config.
- Narrowing a view leaves the cursor on the nearest visible ancestor of the row
  it was on, rather than at the top.
- Indentation in the tree marks branch points rather than message count, so a
  conversation that never forked stays flat instead of walking off the right
  edge one message at a time.

## [0.25.0] - 2026-08-10

- `/scoped-models` opens a checklist. Space ticks a model, `ctrl+p` ticks a
  whole provider, `ctrl+a` and `ctrl+x` tick everything and nothing,
  `alt+↑`/`alt+↓` reorder — the order is the order cycling walks — and Enter
  saves. Typing patterns still works and is still the only headless form.
- Ticking everything and ticking nothing both clear the setting rather than
  writing a pattern per model, which would put the whole catalog into
  settings.json to describe the default.
- Reordering refuses to run while a filter is up: "up" would mean the previous
  visible row while the swap happened between rows that are not adjacent.

## [0.24.1] - 2026-08-10

- `ctrl+l` opens the model picker, which until now needed `/model` typed out.

## [0.24.0] - 2026-08-10

- The session picker acts on the highlighted row: `ctrl+s` flips the order,
  `ctrl+p` shows full paths, `ctrl+r` renames, `ctrl+d` deletes after a
  confirmation, and `ctrl+backspace` deletes while the filter is empty.
- `ctrl+backspace` and `ctrl+h` are now understood as one key. A terminal sends
  the same byte for both, so Pi's `ctrl+backspace` defaults could never have
  fired — nothing tau receives is ever spelled that way.
- `app.session.new`, `.resume`, `.tree` and `.fork` are dispatched, so binding
  them works. They ship unbound and run the matching slash command.
- Deleting the session you are in is refused rather than half-done, and a
  session is renamed by appending to its own file — so the name survives being
  read by Pi.

## [0.23.0] - 2026-08-10

- `!` runs a prompt in your shell instead of sending it to the model, and the
  prompt gutter changes colour the moment you type it. What the command printed
  is recorded in the session and shown to the model on the next turn: run the
  test, and it can see why it failed without you pasting anything.
- `!!` runs the command without telling the model — for the one you are about
  to run twenty times while iterating. It is still in the transcript and in an
  export; it is just never sent.
- The `user_bash` extension hook fires, so a sandbox or a remote executor can
  answer instead of the local shell. It was declared in P3 and never called
  until now.

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
