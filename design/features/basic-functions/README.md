# Basic functions

Status: current implementation (hal 0.1.0).

This document is the feature specification of hal as it exists today: what the
program does, from the outside in. It is written coarse to fine — the overview
first, then each area, then the detail that matters to a user or to whoever
tests the behaviour. It says nothing about how any of it is implemented; that is
`design/technical/architecture`.

## 1. Overview

hal is a minimal coding agent for the terminal. It takes a request in natural
language, asks an LLM what to do, runs the file and shell tools the model asks
for, feeds the results back, and repeats until the model answers without asking
for another tool.

Everything hal does is one of four things:

1. **The agent loop** — the turn-by-turn cycle described above.
2. **Seven built-in tools** — `read`, `bash`, `edit`, `write`, `grep`, `find`,
   `ls`, which are the only way hal touches the machine.
3. **Four interfaces** — the interactive terminal UI, the browser console
   (`--web`), one-shot print mode (`--print`), and a machine-readable JSON event
   stream (`--mode json`). All four drive the same agent loop with the same
   tools; they differ only in how the run is presented and controlled.
4. **Sessions and configuration** — conversations are stored as JSONL files
   grouped per working directory and can be resumed or switched between; the
   settings that shape a run come from flags, environment variables, and a
   configuration file.

Deliberately out of scope: extensions, skills, prompt templates, packages,
themes, sub-agents, MCP, plan mode, background bash, session trees, compaction,
HTML export, prompt caching, cost accounting, context-window tracking, and every
provider that does not speak the OpenAI `/chat/completions` protocol. Section 9
repeats this list as a boundary, so a missing feature is visible here rather
than inferred from the code.

## 2. The agent loop

### 2.1 One prompt, one run

A *run* starts when a prompt is submitted and ends when the model stops asking
for tools. Within a run, each pass over the model is a *turn*.

The model is asked with the system prompt, the whole conversation so far, and
the definitions of the enabled tools. The answer is streamed to the interface as
it arrives. When the answer contains tool calls, every call is executed in the
order the model asked for it, each result is appended to the conversation, and
the model is asked again. An answer with no tool calls ends the run.

### 2.2 Turn budget

A run is bounded by a maximum number of turns, 32 by default (`--max-turns`,
`max_turns`). Reaching the limit without a tool-call-free answer ends the run
with an error saying the agent stopped without a final answer; the conversation
so far is kept.

### 2.3 What the user sees during a run

- Assistant text appears incrementally as it is streamed.
- Each tool call appears before it runs, with a one-line summary of its
  arguments (the `path`, `command`, `pattern` or `query` field, else the raw
  arguments, truncated), and its output is filled in when the tool returns.
- A failed tool is not a failed run: the error is recorded as the tool's result
  and given back to the model, which can adjust. A failed *provider* request
  fails the turn and ends the run with an error.
- Token usage is accumulated from the provider's per-request accounting and
  displayed as input/output counts.
- A completed turn increments a turn counter.

### 2.4 Interrupting and queueing

- **Abort.** In the terminal UI `esc` cancels the run in flight, in the web
  console `esc` or the Abort button does, and the cancellation is reported as an
  abort ("aborted by user"), not as a failure. Aborting during a retry wait stops
  immediately. Killing the process (ctrl+c, SIGTERM) also cancels the run.
- **Queue.** A prompt submitted while a run is in flight is queued and started
  when the current run ends, in submission order. The queue belongs to the
  conversation: switching sessions (or `/new`) aborts the run in flight and
  drops its queue, so the turns of one conversation stay together.

### 2.5 Transient provider failures

Connection resets (`read: connection reset by peer`), timeouts, and `429`/`5xx`
responses are treated as transient and retried with exponential backoff — 0.5s,
1s, 2s, 4s, capped at 10s, four attempts by default (`--max-retries` /
`max_retries`; a negative value disables retrying, `0` means the default). A
`Retry-After` header from the server wins over the computed backoff.

Retrying stops at the first failure that is not transient (a `400`, a bad
response body), at a cancelled run, and after part of the answer has already been
streamed — that text has already been shown, so a retry would duplicate it. Each
retry is reported in every interface. Only the wait for response headers is
bounded (60s), so a slow but healthy stream is never cut off.

A stream that ends after the headers but before a single byte or `[DONE]` marker
is also treated as transient, rather than being presented as an empty answer.

## 3. Tools

Seven tools are built in, and all seven are enabled by default. `--tools` /
`tools` takes a comma-separated allowlist (e.g. `read,grep,find,ls` for a
read-only run); an unknown name is an error that lists the available tools.

| Tool | What it does |
|---|---|
| `read` | Read a text file with 1-based line numbers (`   12| text`), `offset`/`limit` paging. |
| `write` | Create or overwrite a file completely, creating parent directories. |
| `edit` | Exact text replacement; requires a unique match unless `replaceAll` is set. |
| `bash` | Run a shell command, returning combined stdout/stderr and the exit code. |
| `grep` | Regex content search with an optional `glob` filter; prints `path:line: text`. |
| `find` | Glob path search, where `**` crosses directories; without a slash the pattern matches file names at any depth. |
| `ls` | List a directory, directories marked with a trailing `/`. |

Paths are relative to the working directory or absolute; `~` expands to the home
directory. A tool's error is reported to the model as a failed result rather
than aborting the run, and the arguments it was given are validated (a missing
required argument is such an error).

Guardrails, all user-visible:

- `read` refuses binary files (detects a NUL byte), refuses directories, reports
  the line range and total, and truncates a file larger than 400 KiB.
- `bash` keeps at most 64 KiB of output (head and tail, with a marker for what
  was dropped), runs with a timeout (default 120s, capped at 600s) and, on
  timeout, kills the whole process group so a shell's children die with it. The
  reported status is the exit code, or that the command timed out and was
  killed. The child runs in the working directory (or `workdir`) with `HAL=1` in
  its environment.
- `edit` fails when `oldText` is absent, and when it matches more than once
  without `replaceAll`, telling the model to add context or read the file again.
- `grep` skips VCS metadata and dependency directories (`.git`, `node_modules`,
  `vendor`, virtualenvs, caches, …), skips files over 2 MiB, caps results
  (200 by default, 2000 maximum) and says when the cap was reached. An invalid
  regex is retried as a literal string.
- `find` applies the same skip list, does not follow into skipped directories,
  caps results at 1000 and says when it truncated.
- `ls` caps its listing at 1000 entries and says when it truncated.
- The result of any single tool is capped at 30 000 characters before it is fed
  back to the model, keeping head and tail with a truncation marker. Empty
  output is reported as `(no output)`.

## 4. Interfaces

### 4.1 Interactive terminal UI (default)

The default mode when both stdin and stdout are a terminal. Its screen is:

- a **header** line: `hal <version> — <model> (<provider>)` and the working
  directory, shortened with `~`;
- a scrollable **transcript** of blocks: user turns (`›`), assistant answers,
  tool calls (`⚙ name(summary)` with output and duration, or `(running…)` while
  it runs), system notes, and errors (`✗`);
- a multi-line **editor** whose border is highlighted while a run is in flight;
- a **footer** with the working directory on the left and model, status,
  `↑input ↓output` token counts and turn count on the right.

Startup prints the version/model line, the enabled tools, the key bindings, and
the command list as the first transcript block. A long tool result in the
transcript is previewed to 12 lines with a "… (N more lines)" marker.

Keys:

| Key | Action |
|---|---|
| `enter` | Submit; queues the message if a run is in flight |
| `alt+enter`, `shift+enter`, `ctrl+j` | Insert a newline |
| `esc` | Abort the run; with no run, clear the editor; cancel the session picker |
| `pgup`/`pgdn`, `alt+up`/`alt+down` | Scroll the transcript |
| `ctrl+l` | Clear the transcript (not the conversation) |
| `ctrl+c` | Quit (cancelling a run in flight first) |

An initial prompt given on the command line starts as the first run.

### 4.2 Browser console (`--web`)

`--web` serves the same console to a browser, with the same agent, tools,
sessions and event stream as the terminal UI, rendered as a plain page with no
build step and no external assets (it is served from the binary):

- `--web-addr` selects the listen address; it defaults to `127.0.0.1:8080` and
  stays on loopback.
- The startup line prints the URL, which carries an access token and the session
  the console was opened in. Every request must present the token (header or
  query parameter), which is what stops a page on another origin from reaching
  the API through the browser's loopback access — the console can run shell
  commands.
- The token is persisted in `$XDG_STATE_HOME/hal/web-token` (mode `0600`) and
  reused, so a page that is already open keeps working after the process is
  restarted: reload it and it reconnects. `--web-token` / `$HAL_WEB_TOKEN`
  override it with a fixed value (a development convenience).
- The page has a header (title, working directory, session menu), a transcript
  with the same block kinds as the terminal UI, a composer (`Send`, `Abort`,
  `enter` sends, `shift+enter` inserts a newline, `esc` aborts), and a footer
  with the session and token usage.
- The session id is kept in the URL, so reloading a tab reopens the same
  conversation; `/new` rewrites the URL. Loading a URL naming another session
  reopens that stored session and rebuilds the transcript, marked as restored.
- The stream is server-sent events. Every (re)connection starts from a full
  snapshot of the transcript, so a dropped connection never duplicates or loses
  blocks; the client reconnects on its own.
- A restarted server starts a fresh session unless `-c`/`--continue` is given,
  which reattaches to the most recent session for the directory — the
  rebuild-and-reload loop.
- Restoring a transcript from a stored session brings tool output back whole,
  but per-call durations are not in the session file and are lost.

### 4.3 Print mode (`-p`, `--print`)

One-shot: run the prompt, stream the answer to stdout, exit. Tool activity is
written to stderr (`⚙ tool(summary)`, `  ↳ ok|error (Nms)`), as are errors and
retry notices, so stdout stays the answer alone. A trailing newline is added
only when something was printed. An empty prompt is an error.

### 4.4 JSON mode (`--mode json`)

The same run, with every agent event encoded as one JSON object per line on
stdout, for scripts and other programs: text deltas, tool calls, tool results,
turn ends, usage, retries, errors, and a final `done`. An empty prompt is an
error. `--mode text` selects the interactive text UI when stdout is a terminal
(even with stdin redirected); `--mode json` always takes precedence.

### 4.5 Piped input and `@file`

Non-terminal stdin is read (up to 4 MiB) and prepended to the prompt, so
`cat main.go | hal -p "review this"` works. Positional arguments starting with
`@` name files, whose contents are embedded as `<file path="...">…</file>`
blocks (each capped at 512 KiB) ahead of the written prompt. Message words are
joined into one prompt.

## 5. Sessions

### 5.1 Storage and grouping

Conversations are stored as JSONL files, one JSON object per line, under
`$XDG_DATA_HOME/hal/sessions/<slugified-cwd>/<timestamp>-<id8>.jsonl`
(`--session-dir`, `session_dir`, `$HAL_SESSION_DIR` override the root). Each
file starts with a session header (id, working directory, model, name) and then
records every message as the run appends it, so persistence is append-only and a
process killed mid-run leaves a readable file. `--no-session` disables storage
entirely; the conversation then lives only in memory.

Each stored session carries an id (16 hex characters, shown as its first 8), a
display name (default: the id prefix, or `--name`/`name`), a model, the working
directory, a message count and an update time.

### 5.2 Resuming from the command line

| Command | Effect |
|---|---|
| `hal -c`, `hal --continue` | Resume the most recent session for this directory; if there is none, start a fresh one (so `hal --web -c` works on a first run). |
| `hal --continue-all` | Same, but across every session recorded on the machine. |
| `hal --session <file>` | Open a specific session file by path. |
| `hal --list-sessions` | Print the stored conversations for this directory, newest first, with their ids, name, model, message count and time, and the resume command. |

### 5.3 Switching inside a running console

Both the terminal UI and the web console can switch conversations without a
restart:

- `/switch` opens a picker of the sessions stored for the directory, newest
  first (the current one is hidden in the terminal picker, flagged in the web
  menu). `↑`/`↓`/`j`/`k` choose, `enter` opens, `esc` cancels, and a digit jumps
  to that entry; the terminal list is windowed to the terminal height and its
  entries are clipped so a narrow or short terminal does not overflow.
- `/switch <id>` opens one directly; `/sessions` lists them.
- Switching aborts a run in flight and drops its queue, then replays the
  conversation it switched to. Tool output is not part of the stored history, so
  a replayed transcript shows tool calls without their results, plus a note of
  how many messages were restored.

## 6. Commands and configuration

### 6.1 Slash commands

Available in the terminal UI and the web console (the web console's `/quit`
explains that closing the tab is how to stop it):

```
/help  /model [name]  /new  /session  /switch [id]  /sessions  /tools  /clear  /quit
```

- `/model` shows the current model; `/model <name>` switches it for the rest of
  the process when the provider supports it.
- `/new` clears the conversation and starts a fresh session file.
- `/session` describes the current session (id, file, message count).
- `/tools` lists the enabled tools.
- `/clear` empties the visible transcript without touching the conversation.

### 6.2 Configuration resolution

Every setting resolves with the precedence **flag > environment > config file >
default**, and `--show-config` prints the resolved value and its source for each
one (credentials masked). `hal --version` prints the version.

### 6.3 Config file

TOML at `$XDG_CONFIG_HOME/hal/config.toml`, with JSON also accepted as
`config.json` in the same directory (TOML wins if both exist). `--config <path>`
or `$HAL_CONFIG` selects a specific file. Keys use snake_case and mirror the
flag names:

```toml
provider       = "openai"        # the protocol; the URL picks the service
model          = "gpt-4o-mini"
base_url       = "https://api.openai.com/v1"
api_key        = ""              # prefer the environment
api_key_env    = ""              # or name a variable, e.g. GATEWAY_API_KEY
tools          = "read,bash,edit,write,grep,find,ls"
max_turns      = 32
max_retries    = 4               # 0 = default, negative = never retry
system_prompt        = ""        # text, or a path to a file
append_system_prompt = ""
session_dir    = ""              # default $XDG_DATA_HOME/hal/sessions
session_header = ""              # "" = auto (see below)
name           = ""              # default session display name
no_session      = false
no_context_files = false
```

Unknown keys are reported as warnings rather than ignored silently, and a config
file that contains an `api_key` while being readable by other users produces a
permissions warning.

### 6.4 Credentials and environment

`api_key_env` names the environment variable holding the key (checked before
`HAL_API_KEY`/`OPENAI_API_KEY`), so the secret stays in one place; a literal
`api_key` is the last resort and `--api-key` overrides everything.

```
HAL_API_KEY / OPENAI_API_KEY          credentials
HAL_BASE_URL / OPENAI_BASE_URL / OPENAI_API_BASE   endpoint
HAL_MODEL / OPENAI_MODEL              model
HAL_CONFIG                            config file path
HAL_SESSION_DIR                       session storage root
HAL_WEB_TOKEN                         access token for --web
XDG_CONFIG_HOME / XDG_DATA_HOME / XDG_STATE_HOME   base directories
```

`HAL_*` wins over the `OPENAI_*` aliases. Relative values in the `XDG_*`
variables are ignored, as the specification requires.

### 6.5 Gateway identity

hal always sends `User-Agent: hal/<version>`, and sends a per-conversation
session id header when the endpoint is known to want one — currently
`x-opencode-session` for `opencode.ai`. `session_header` in the config file
names a different header, or `""` keeps the automatic choice. The id is the
conversation's session id, so `-c` keeps the same routing affinity; a run with a
stored session uses that session's id, and a run without one uses a fresh
random id.

### 6.6 System prompt and context files

The default system prompt describes hal, its working directory, the OS/arch and
the date, and gives the model its working guidelines. `--system-prompt` replaces
it (a value naming an existing file is read from disk);
`--append-system-prompt` adds to it.

Unless `--no-context-files` is given, instruction files are discovered and
appended:

1. `AGENTS.md` or `CLAUDE.md` in `$XDG_CONFIG_HOME/hal` (the first that exists).
2. For each directory from the filesystem root down to the working directory,
   the first of `AGENTS.override.md`, `AGENTS.md`, `CLAUDE.md` that exists — one
   file per directory, so the working directory's file wins.

## 7. Documentation and developer-facing behaviour

- `--help` prints usage, the modes, every flag, the config-file keys, the
  environment variables, and examples.
- The README documents the same surface for users, and the CHANGELOG records
  notable changes per release.
- The test suite covers the agent loop against a scripted SSE server and the
  real tools against temporary directories; see
  `design/technical/architecture`.

## 8. Security posture (as a feature)

`bash` runs with the user's privileges and the model chooses what to run, so hal
should be used in a directory one is willing to have modified, with model output
treated as untrusted input (a file hal reads can prompt-inject arbitrary
commands). The supported mitigations are `--tools` to drop `bash`/`write`/`edit`
for read-only work, and keeping `--web` on loopback. The web console binds to
`127.0.0.1` by default and authenticates every request with its token; widening
`--web-addr` exposes an interface that can run shell commands to whatever
network it is bound to.

## 9. Explicit non-features

Not implemented, deliberately, and not to be added without a design change:

- Providers other than OpenAI-compatible `/chat/completions` — no native
  Anthropic or Google client, no `/login` OAuth flow. Other vendors are reached
  through a compatible gateway.
- Extensions, skills, prompt templates, packages, themes, sub-agents, MCP, plan
  mode, background bash.
- Session-tree navigation (`/tree`, `/fork`, `/clone`), compaction, HTML export.
  Sessions are linear and resumable only.
- Prompt caching, cost accounting, context-window tracking (the footer reports
  token counts only).
- `grep` uses Go's regexp rather than ripgrep, and the search tools do not read
  `.gitignore`.
- Per-tool result persistence: tool output is stored in the session, but
  per-call durations and error flags are not, so they are lost across a restart.
