# hal

[![CI](https://github.com/Rushera/hal/actions/workflows/ci.yml/badge.svg)](https://github.com/Rushera/hal/actions/workflows/ci.yml)

A minimal coding agent written in Go, with a
[bubbletea](https://github.com/charmbracelet/bubbletea) terminal UI.

hal is an agent loop over an LLM with a small set of built-in file/shell tools,
a terminal interface, and JSONL sessions. It deliberately leaves out extension,
skill, package, and provider-matrix machinery.

## What it does

- **Agent loop.** System prompt + conversation history + tool definitions are
  sent to the model; the streamed response is rendered, every requested tool call
  is executed, results are fed back, and the loop repeats until the model answers
  without tool calls (bounded by `--max-turns`, default 32).
- **Seven built-in tools:** `read`, `bash`, `edit`, `write`, `grep`, `find`,
  `ls`.
- **Terminal UI.** Startup header, scrollable transcript showing each tool call
  and its output, multi-line editor, and a footer with directory, model, session
  status, and token usage.
- **Four interfaces:** interactive TUI, `--web` browser console, `--print`
  one-shot, and `--mode json` for a machine-readable event stream.
- **JSONL sessions** with parent links, grouped per working directory, resumable
  with `-c`.
- **OpenAI-compatible provider** — OpenAI, OpenRouter, Groq, Ollama, llama.cpp,
  vLLM, LM Studio, and anything else speaking `/chat/completions`.

## Install

```bash
go install github.com/Rushera/hal@latest   # or:
git clone https://github.com/Rushera/hal && cd hal && go build -o hal .
```

Building needs Go 1.27.1 or newer (see `go.mod`); `go build` produces a single
static binary with no runtime dependencies.

## Configure

Settings resolve with the precedence **flag > environment > config file >
default**, and `hal --show-config` prints the resolved value and the source of
every setting:

```
$ hal --show-config
hal 0.1.0

paths:
  config file   ~/.config/hal/config.toml
  config dir    ~/.config/hal
  session dir   ~/.local/share/hal/sessions
  data dir      ~/.local/share/hal
  state dir     ~/.local/state/hal

settings:
  provider             openai
                         ← default
  model                gpt-4o-mini
                         ← default
  api_key              sk-c…og (masked)
                         ← env $HAL_API_KEY
```

### Config file

TOML at `$XDG_CONFIG_HOME/hal/config.toml` (`XDG_CONFIG_HOME` defaults to
`~/.config`). JSON is also accepted as `config.json` in the same directory, and
TOML wins when both exist. `--config <path>` or `$HAL_CONFIG` selects a
specific file instead.

```toml
# ~/.config/hal/config.toml
model    = "gpt-4o-mini"
base_url = "https://api.openai.com/v1"
api_key  = ""                  # prefer the HAL_API_KEY environment variable
tools    = "read,bash,edit,write,grep,find,ls"
max_turns = 32

system_prompt        = ""      # text, or a path to a file
append_system_prompt = ""
session_dir = ""               # default $XDG_DATA_HOME/hal/sessions
session_header = ""            # "" = auto (see below)
name        = ""               # default session display name
no_session      = false        # do not persist sessions
no_context_files = false       # do not load AGENTS.md / CLAUDE.md
```

Unknown TOML keys are reported as warnings rather than silently ignored, and a
config file that contains an `api_key` while being readable by other users
produces a permissions warning.

### Credentials without copying secrets

`api_key_env` names the environment variable holding the key, so the secret
stays in one place:

```toml
api_key     = ""
api_key_env = "GATEWAY_API_KEY"
```

The named variable is checked before `HAL_API_KEY` / `OPENAI_API_KEY`; a
literal `api_key` in the file is the last resort, and `--api-key` overrides
everything.

### Gateway identity headers

Some gateways route coding-agent traffic and require clients to identify
themselves and to send a stable per-conversation id. hal always sends
`User-Agent: hal/<version>`, and sends a session id header when the endpoint is
known to want one — currently `x-opencode-session` for `opencode.ai`. Set
`session_header` to name a different header, or to `""` to keep the automatic
choice. The id is the conversation's session id, so resuming with `-c` keeps the
same routing affinity; runs with no stored session get a fresh one.

### Example: a third-party gateway

`examples/config.gateway.toml` shows the shape of a service that speaks the
OpenAI protocol but wants its own credential variable and session header:

```toml
provider       = "openai"                    # the protocol; the URL picks the service
model          = "example-model"
base_url       = "https://gateway.example.com/v1"
api_key        = ""
api_key_env    = "GATEWAY_API_KEY"
session_header = "x-session-id"              # "" = automatic
```

### Environment variables

```
HAL_API_KEY      / OPENAI_API_KEY       credentials
HAL_BASE_URL     / OPENAI_BASE_URL      endpoint (default https://api.openai.com/v1)
HAL_MODEL        / OPENAI_MODEL         model (default gpt-4o-mini)
HAL_CONFIG                              config file path
HAL_SESSION_DIR                         session storage root
XDG_CONFIG_HOME / XDG_DATA_HOME / XDG_STATE_HOME
```

`HAL_*` wins over the `OPENAI_*` aliases. Relative values in the `XDG_*`
variables are ignored, as the XDG specification requires.

### XDG layout

```
$XDG_CONFIG_HOME/hal/config.toml        (or config.json)   configuration
$XDG_DATA_HOME/hal/sessions/                              session files
$XDG_STATE_HOME/hal/                                      reserved
```

## Usage

```bash
hal                                  # interactive
hal "list the go files here"         # interactive with an initial prompt
hal -p "summarize README.md"         # one-shot, prints the response
cat main.go | hal -p "review this"   # pipe into a one-shot run
hal -p @notes.md "extract the todos" # include a file
hal --mode json "inspect this repo"  # JSONL event stream
hal --web                            # browser console on 127.0.0.1:8080
hal -c                               # resume the most recent session here
hal --tools read,grep,find,ls -p "audit this repo"   # read-only run
```

### Web console

`--web` serves the same console in a browser instead of the terminal: the same
agent, tools, sessions, and event stream, with a plain page (no ASCII art).
`--web-addr` picks the listen address; it defaults to `127.0.0.1:8080` and
stays on loopback unless you widen it.

```
$ hal --web
hal web console: http://127.0.0.1:8080/?token=b20e4552…
```

The URL carries a random token generated at startup; every request must present
it. That is what keeps a page on another origin from reaching the API through
your browser's loopback access, since the console can run shell commands. The
page is served from the binary, so there is no build step and no extra files.

`xterm`-free transcript: user turns, streamed assistant text, each tool call
with its output and duration, errors, and a footer with model, status, token
usage, session, and turns. The editor supports the same slash commands as the
TUI (`/help /model /new /session /tools /clear`); `enter` sends, `shift+enter`
inserts a newline, `esc` aborts, and a prompt submitted while the agent is busy
is queued. Server-sent events carry the stream; each reconnect resynchronises
from a full snapshot, so a dropped connection never duplicates or loses blocks.

### Editor and keys

| Key | Action |
|---|---|
| `enter` | submit (queues the message if the agent is working) |
| `alt+enter` | newline |
| `esc` | abort the current run |
| `pgup`/`pgdn`, `alt+up`/`alt+down` | scroll the transcript |
| `ctrl+l` | clear the transcript |
| `ctrl+c` | quit |

### Slash commands

```
/help      /model [name]   /new      /session
/tools     /clear          /quit
```

### Context files

`AGENTS.md` or `CLAUDE.md` in the config directory (`$XDG_CONFIG_HOME/hal`) is
loaded first. Then, for each directory from the filesystem root down to the
working directory, the first of `AGENTS.override.md`, `AGENTS.md`, or
`CLAUDE.md` that exists is added, so the working directory wins. Disable with
`--no-context-files`. `--system-prompt` replaces the default prompt;
`--append-system-prompt` adds to it.

## Tools

| Tool | Purpose |
|---|---|
| `read` | Read a text file with 1-based line numbers, `offset`/`limit` paging |
| `write` | Create or overwrite a file, creating parent directories |
| `edit` | Exact text replacement; requires a unique match unless `replaceAll` |
| `bash` | Run a shell command, capture stdout+stderr and exit code, timeout |
| `grep` | Regex content search, `glob` filter, skips VCS/dependency dirs |
| `find` | Glob path search, `**` crosses directories |
| `ls` | List a directory, directories marked with `/` |

Guardrails: tool results are capped before being fed back to the model, `read`
refuses binary files, `bash` keeps at most 64 KiB of output (head and tail),
runs with a timeout (default 120s, max 600s) and kills the whole process group
on timeout, and `grep`/`find` skip `.git`, `node_modules`, `vendor`, and
similar directories.

## Security

`bash` executes commands with your user's privileges and the model decides what
to run, so run hal in a directory you are willing to have modified. Model output
is untrusted input: prompt injection from files hal reads can request arbitrary
commands. Use `--tools` to drop `bash`/`write`/`edit` for read-only work, and
`--web-addr` to keep the console on loopback.

The web console binds to `127.0.0.1` by default and authenticates every request
with the random per-run token in its URL; widening `--web-addr` exposes an
interface that can run shell commands, so put it behind your own access control.
Report vulnerabilities privately rather than in a public issue — see
[SECURITY.md](SECURITY.md).

## Development

```bash
go test ./...          # full suite
go test -race ./...    # race detector (Linux/macOS)
go vet ./...
gofmt -l .             # must print nothing
```

The suite runs the real agent loop against a scripted SSE server over loopback
and executes the real tools against temp directories, so the tool-call round
trip, error handling, session persistence, and the TUI's event handling are
covered end to end.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the ground rules, and
[CHANGELOG.md](CHANGELOG.md) for what changed in each release. Security issues
should go through a private advisory, as described in [SECURITY.md](SECURITY.md).

## Limitations

Deliberate, given "basic functions only":

- Only the OpenAI-compatible provider is implemented; there is no native
  Anthropic or Google client and no `/login` OAuth flow. Other vendors are
  reachable through a compatible gateway.
- No extensions, skills, prompt templates, packages, themes, sub-agents, MCP,
  plan mode, or background bash.
- No session tree navigation (`/tree`, `/fork`, `/clone`), compaction, or
  HTML export; sessions are linear and resumable only.
- No prompt caching, cost accounting, or context-window tracking; the footer
  reports token counts only.
- `grep` uses Go's `regexp` rather than ripgrep, and neither search tool reads
  `.gitignore`.

## License

Copyright (C) 2026 the hal authors.

hal is free software: you can redistribute it and/or modify it under the terms
of the GNU General Public License as published by the Free Software Foundation,
either version 3 of the License, or (at your option) any later version.

hal is distributed in the hope that it will be useful, but **without any
warranty**; without even the implied warranty of merchantability or fitness for
a particular purpose. See the [GNU General Public License](LICENSE) for more
details.

The full text is in [LICENSE](LICENSE). Third-party Go modules keep their own
licenses (all MIT/BSD); see `go.sum` and each module's `LICENSE` file.
