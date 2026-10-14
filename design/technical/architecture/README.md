# Architecture

Status: current implementation (hal 0.1.0).

This document is the technical description of hal: how the program is put
together. It is written coarse to fine — package layout first, then the
interfaces between packages, then the details of concurrency, data formats and
policy. What hal *does* is `design/features/basic-functions`; the two documents
are deliberately separate, and neither should absorb the other.

## 1. Shape of the program

hal is a single Go module (`github.com/Rushera/hal`) producing one static
binary. `main` is the only package with a `main()`, and it is the only place
that wires the parts together.

```
main.go                    CLI: flags, precedence, mode selection, wiring
internal/agent/            the agent loop (the core of the program)
internal/provider/         OpenAI-compatible streaming client
internal/tools/            the seven built-in tools + registry
internal/session/          JSONL session storage
internal/tui/              bubbletea terminal UI
internal/web/              HTTP + SSE console (index.html embedded)
internal/config/           config file + XDG path resolution
```

Dependency direction is a straight line, with `internal/agent` as the centre:

```
main ──▶ agent ◀── provider
  │        ▲
  ├──▶ tools ┘ (tools import agent for the Tool interface and messages)
  ├──▶ session ──▶ agent, config
  ├──▶ tui  ──▶ agent, session
  ├──▶ web  ──▶ agent, session
  └──▶ config
```

`agent` does not import any of the other internal packages: it defines the
types (`Message`, `ToolCall`, `ToolSpec`, `Usage`, `Event`) and the interfaces
(`Tool`, `Provider`) that everything else implements or consumes. `provider`
and `tools` both import `agent` for those types; `tui` and `web` import `agent`
and `session`; `session` imports `agent` (messages) and `config` (paths). No
package imports `main`, and `tui` and `web` do not import each other.

The front ends are adapters. `tui` and `web` each declare a small `Agent`
interface (the same method set: `RunPrompt`, `ModelName`, `SetModelName`,
`ProviderName`, `Messages`, `Reset`, `ToolNames`) rather than take
`*agent.Agent`, so both are unit-testable against a scripted fake. Sessions and
the console's session switching are injected as callbacks
(`SessionInfo`, `SessionID`, `NewSession`, `ListSessions`, `OpenSession`) built
once in `main` (`sessionHooks`), so the front ends know nothing about session
files.

## 2. The agent loop

`internal/agent/agent.go` is the whole core, and its structure is the design:

- `Agent` holds the `Provider`, the enabled `Tools`, the `SystemPrompt`, the
  conversation `History`, `MaxTurns`, the retry policy, and two callbacks:
  `OnEvent` (the interface draws from it) and `OnMessage` (sessions persist
  through it).
- `RunPrompt(ctx, prompt, emit)` appends the user message, then loops up to
  `MaxTurns` times. Each iteration streams one assistant message, appends it,
  executes every requested tool call in order (appending a `tool` message per
  call), and continues. An assistant message with no tool calls ends the run with
  a `done` event; exhausting the budget ends it with an error.
- Events are the single observation channel. `text_delta`, `tool_start`,
  `tool_result`, `turn_end`, `usage`, `message`, `retry`, `error`, `done`. Every
  front end is a fold over this stream, and the `message` event is what makes an
  interface able to track history without reading `History` itself.
- `append` is the one place history grows, so `OnMessage` (persistence) and the
  `message` event can never diverge from the conversation.

The request sent to the provider is rebuilt for each turn: the system prompt
(when non-empty) followed by `History` (`requestMessages`), plus the tool schemas
(`toolSpecs`). Tool results are capped at `MaxToolResultChars` (30 000) with a
head/tail truncation before they enter the history, so one noisy command cannot
consume the context window.

Tool execution (`runTool`) resolves the tool by name, defaults empty arguments
to `{}`, times the call, and turns a returned error into a result with `IsError`
rather than failing the run. An unknown tool name becomes an error result naming
the available tools — the model gets a chance to correct itself.

### 2.1 Retry policy

The policy lives in `agent`, not in the provider: the provider classifies, the
loop decides.

- `provider` wraps a transient failure in `*agent.RetryableError` (carrying an
  optional `RetryAfter` from the `Retry-After` header). Permanent failures are
  returned as plain errors.
- `streamWithRetry` retries only when the error is a `RetryableError`, the run is
  not cancelled, and the attempt budget is not spent. It waits with exponential
  backoff (`RetryBaseDelay`, doubling, capped at `maxRetryDelay` = 10s), or the
  server's `Retry-After` when that is larger, emits a `retry` event, and waits
  through an injectable `Sleep` (a field, so tests do not actually sleep;
  `sleepCtx` makes an abort during the wait return immediately).
- Nothing is retried once part of the answer has been streamed: the loop counts
  the deltas it has already forwarded and returns the failure annotated with the
  character count, because those characters have already been displayed.
- `MaxRetries` semantics: `0` = `DefaultMaxRetries` (4), negative = never retry.
  `main` resolves the same convention for `--max-retries` / `max_retries`.

## 3. Provider layer

`internal/provider/openai.go` implements `agent.Provider` for
`POST {base}/chat/completions` with `stream: true` and
`stream_options.include_usage`.

- **Request building.** `convertMessages` maps the neutral `agent.Message` to the
  wire shape: tool results default an empty body to `(no output)`, an assistant
  message may carry only tool calls, and empty non-assistant content is dropped.
  Tool schemas are mapped to `tools[].function` with a JSON Schema parameter
  object (defaulted to `{"type":"object","properties":{}}` when missing).
- **Headers.** `Content-Type`, `Accept: text/event-stream`, `Authorization:
  Bearer <key>` when a key is set, `User-Agent` when set, and the session header
  when both a header name and a session id are configured.
- **Streaming.** `readStream` scans `data:` lines, ignores non-JSON noise and
  keep-alives, accumulates text (calling `onText` per delta), accumulates tool
  calls per index (id, name and argument fragments arrive split across chunks and
  are only complete at the end), and stops at `[DONE]`. Usage arrives on a chunk
  and is attached to the returned message. Tool calls with no id get a synthetic
  one.
- **Failure classification.** A transport error, a stream that dies mid-way, and
  an unfinished stream that delivered nothing become `RetryableError`s when they
  look transient (`isTransientNetError` matches `net.Error`, `*net.OpError`,
  `io.EOF`/`io.ErrUnexpectedEOF` and a list of message fragments such as
  "connection reset", "broken pipe", "goaway"); `429`, `408` and `5xx` statuses
  are transient (`isTransientStatus`) and carry `Retry-After`. Everything else,
  including a malformed URL or a TLS failure, is permanent. A cancelled context
  is returned as `ctx.Err()` and never retried. The body of a failed response is
  read (bounded) for the message and then drained so the connection can be reused
  by the retry.
- **Timeouts.** `newHTTPClient` bounds only `ResponseHeaderTimeout` (60s) and
  sets no client-level `Timeout`, because `http.Client.Timeout` also covers
  reading the body and would truncate a long streamed answer. The transport keeps
  idle connections per host (`MaxIdleConnsPerHost` = 8) so a retry against a
  flaky gateway does not pay for a new connection.
- `WithHeaderTimeout` returns a copy with a different header timeout (used by
  tests); `Ping` is a cheap credential check.

The provider holds the mutable state that `/model` and session affinity change:
`SetModelName` and `SetSessionID` satisfy the optional `agent.ModelSetter` and
`agent.SessionIDSetter` interfaces, which `Agent.SetModelName` /
`Agent.SetSessionID` reach through a type assertion. That is the extension point
for runtime-switchable providers without widening the `Provider` interface.

## 4. Tools

`internal/tools` is a flat package: `registry.go` holds the catalogue and the
shared schema helper, `files.go` the filesystem tools (`read`, `write`, `edit`,
`ls`), `shell.go` the `bash` tool, `search.go` `grep` and `find`.

- Every tool embeds `toolDef` (name, description, parameter schema) and
  implements `agent.Tool`. `Parameters()` marshals a `map[string]any` schema, and
  `objSchema` builds the common object shape.
- `New(name, cwd)` maps a name to a tool bound to a working directory;
  `Select(cwd, list)` implements the `--tools` allowlist (empty = all built-ins,
  unknown names = one error listing the valid names).
- Paths from the model go through `resolve()`: `~` expansion, then joining with
  the working directory, then `Clean`. Output paths are rendered relative to the
  working directory by `display()` when possible, which is what keeps tool
  results short and readable.
- `bash` is the only tool that spawns a process. It picks `bash` when available
  and `sh` otherwise, runs `exec.Command(shell, "-c", command)` in the working
  directory (or the validated `workdir`) with `HAL=1` appended to the
  environment, and captures stdout and stderr into one `cappedBuffer` that keeps
  a fixed head and tail (64 KiB total) with a dropped-byte marker. The timeout is
  a `context.WithTimeout` (default 120s, capped at 600s) around the run; on
  expiry the process *group* is killed (`proctree_unix.go`, build-tagged, uses
  `Setpgid` and `kill(-pgid, SIGKILL)` so the shell's children die too; the
  non-unix build only kills the child). The result ends with
  `[exit code N in Xms]` (or the timeout message), and a non-zero exit is
  reported both in that text and as a tool error.
- `read` caps the file at 400 KiB, rejects a NUL byte as binary, and streams
  1-based numbered lines with a header giving the file, the range and the total.
  `write` and `edit` preserve the existing file mode on edit and default to
  `0644`. `grep`/`find` share the `skipDirs` list and never descend into it
  (VCS metadata, dependency trees, caches); both honour a result cap and say when
  they truncated. `grep` compiles the pattern, falling back to
  `regexp.QuoteMeta` when it is not valid regex.

## 5. Sessions

`internal/session` persists conversations as JSONL, and the on-disk format is
the contract:

- Each line is an `Entry` with a `type` (`session`, `message`, `name`,
  `model_change`), its own id, an optional `parentId`, a timestamp, and the
  type-specific payload. The header line records the session id, working
  directory, model and name; a `message` entry embeds the neutral
  `agent.Message`, which is why a session can be replayed straight into the
  agent's history. Parent links are written (the tree shape is kept for
  compatibility) but only the linear branch is used.
- Writes go through one `os.File` opened append-only, guarded by a mutex, so the
  format is self-describing line by line and a process that dies mid-write leaves
  a file whose truncated last line `Load` skips. `Close` is deferred in `main`.
- Sessions live under `RootWith(dir)` (explicit `--session-dir`/`session_dir`,
  then `$HAL_SESSION_DIR`, then `config.Resolve().SessionDir`), in a per-project
  bucket whose name is `ProjectDir(root, cwd)` — the absolute working directory
  with separators turned into dashes. `ProjectDir` is what makes the grouping
  stable across a rename of anything but the directory itself.
- `List(root, cwd, all)` is the query. `all=false` reads the one project bucket;
  `all=true` walks every bucket and filters by the recorded cwd (or keeps
  everything when the wanted cwd is empty). `Latest`/`LatestIn` pick the newest
  by the newest entry timestamp (falling back to file mtime) with a deterministic
  tie-break, and `ErrNoSessions` is what `main` turns into "start a fresh
  session" for `-c` and `--continue-all`. `FindByID` resolves a full id or the
  8-character prefix used in file names by walking the root, which is what lets
  the web console reopen the session named in a page URL.
- `stat` summarises a file by fully parsing it (id, name, cwd, model, message
  count, newest timestamp), which is why listing is O(total bytes) and is only
  used by the switchers and `--list-sessions`.

## 6. Front ends

### 6.1 Terminal UI (`internal/tui`)

A bubbletea model with a `viewport` (transcript), `textarea` (editor) and
`spinner`. Its design points:

- **The run runs off the update loop.** `startRun` launches a goroutine running
  `Agent.RunPrompt` and forwards every event into a buffered channel (512);
  the goroutine sends a private `__run_done` event at the end (with a 2s timeout
  so a wedged UI cannot leak the goroutine). The model consumes the channel with
  a `waitForEvent` command, so the terminal stays responsive and the spinner
  ticks while the model streams.
- **Transcript as blocks.** A `block` is `{kind, title, body, done, isErr}` with
  kinds `user`, `assistant`, `tool`, `system`, `error`. `handleEvent` folds the
  agent's events into blocks: a `text_delta` opens (or appends to) one streaming
  assistant block, a `tool_start` opens a tool block, and the matching
  `tool_result` fills in its body, error flag and duration. `refresh` re-renders
  all blocks into the viewport and pins it to the bottom.
- **Abort tracking.** An error reaches the UI as a *string* over the event
  channel, so `errors.Is(err, context.Canceled)` no longer matches. The model
  therefore keeps its own `aborting` flag, set when `esc` (or a session switch)
  cancels a run, and `finishRun` suppresses the failure notice when it is set
  (also for a genuine `context.Canceled`).
- **Session identity guard.** `startRun` captures the session id at launch; when
  the run finishes it compares it with the current id and, if a switch happened
  in between, discards the run's result so a stale turn cannot land in the
  conversation the console has moved to.
- **Picker.** `/switch` builds `choices` from `ListSessions()`, minus the current
  session, and renders a bordered list in place of the transcript. The list is
  windowed to the terminal height (`pickerWindow`, `pickerRange`) and each entry
  is clipped to the available width, so a short or narrow terminal cannot
  overflow; `/switch <id>` skips the picker entirely.
- **Layout.** `layout()` sizes the viewport from the terminal height minus the
  editor and footer (leaving a single row while picking) and the textarea from
  the width; `View` draws header (version, model, provider, cwd), then either the
  transcript plus editor or the picker, then the footer (cwd, model, status,
  `↑/↓` token counts, turns).

### 6.2 Web console (`internal/web`)

The same console as an HTTP server; `index.html` is embedded with `go:embed`, so
there is no build step and no external asset.

- **Transport.** `GET /api/events` is a server-sent-event stream; every other
  endpoint is JSON. Each subscriber gets a buffered channel (4096); a client that
  cannot keep up is dropped and its `EventSource` reconnects, which is safe
  because every connection starts with a full snapshot
  (`statusLocked("state")`, carrying the whole transcript, the session menu, the
  footer state and the session id). A 20s comment ping keeps idle connections
  alive from intermediary proxies.
- **State.** The `Server` owns the transcript (`[]Block`), the run state
  (`running`, `cancel`, `queued`, `aborting`), the counters (turns, usage) and
  the subscriber set, all under one mutex. `applyEvent` folds agent events into
  blocks *and* into an `outEvent` delta under that mutex, so the snapshot and the
  deltas can never disagree.
- **Run management.** `Submit` either starts a run or queues the prompt;
  `claimRunLocked` marks the console busy and hands back a cancellable context;
  `run` drives `RunPrompt` and `finishRun` clears the state and starts the next
  queued prompt. `Abort` marks the run as aborted (so the failure is not
  reported) and cancels it. `handlePrompt`, `handleAbort`, `handleClear`,
  `handleNew`, `handleModel`, `handleSessions` and `handleOpen` are thin
  wrappers over those operations.
- **Auth.** Every route is wrapped in `auth`, which compares the `X-Hal-Token`
  header or the `token` query parameter against the server token with
  `subtle.ConstantTimeCompare`. The index page additionally adopts the session
  named in the URL before the page subscribes, so its first snapshot already
  holds that conversation.
- **Token persistence.** `loadOrCreateToken` reads and reuses
  `$XDG_STATE_HOME/hal/web-token`, creating it `0600` when missing, which is what
  lets an already-open page reconnect to a restarted process. An explicit token
  (`--web-token`, `$HAL_WEB_TOKEN`) short-circuits this.
- **Session switching.** `OpenSession` aborts a run in flight, waits (bounded,
  500 × 10ms) for it to finish so its remaining events cannot land in the new
  session, swaps the session through the injected callback, rebuilds the
  transcript from the agent's history (`historyBlocks`) and broadcasts a fresh
  snapshot. `historyBlocks` reuses the live block shapes and folds stored tool
  results back into their call blocks (matching on the call id); since the
  session file has no error flag, a result is treated as failed when its text
  starts with `Error: `. `Abort`/switch semantics match the TUI: the queue is
  dropped with the conversation it belonged to.
- **Client.** `index.html` is plain ES5-era JavaScript: it subscribes with
  `EventSource`, applies snapshots and deltas, re-renders only the streaming
  block on a delta (`renderOpen`) to avoid flicker on long transcripts, keeps the
  session id in the URL with `history.replaceState` (`syncSession`), renders the
  session menu in the header, and implements the same slash commands as the TUI
  by calling the corresponding endpoints.

## 7. Configuration

`internal/config` (file + XDG paths) and `main` (resolution) split the work:

- `config.Resolve()` computes `ConfigDir`, `DataDir`, `StateDir` and
  `SessionDir` from `XDG_CONFIG_HOME`/`XDG_DATA_HOME`/`XDG_STATE_HOME`, ignoring
  relative values as the specification requires and defaulting to `~/.config`,
  `~/.local/share` and `~/.local/state`.
- `config.Load(explicit, paths)` reads an explicit path (which must exist) or the
  first existing candidate (`config.toml`, then `config.json`; TOML wins).
  Decoding is TOML via `BurntSushi/toml` or JSON by extension. Two warnings are
  produced deliberately: TOML keys the schema does not know (so a typo is
  visible), and a config file containing an `api_key` that is readable by other
  users. A missing config file is not an error.
- The schema (`config.File`) uses snake_case keys matching the flag names. Two
  booleans are pointers (`NoSession`, `NoContextFiles`) so "unset" is
  distinguishable from `false`; that pointer-ness is what lets `pickBool` apply
  the config value only when the key was present.
- **Precedence lives in `main.resolveOptions`**, implemented once as three
  helpers (`pickString`, `pickBool`, `pickInt`) that consult, in order: the set
  of flags the user actually typed (collected with `flag.FlagSet.Visit`), the
  environment, the config file, and the default. Every resolved setting records a
  `settingSource` (key, value, source) so `--show-config` can print exactly where
  each value came from, with credentials masked (`maskedValue`).
  `--max-retries` is resolved separately because its `0` means "use the default"
  rather than "was set to zero", so it needs the raw check that the generic
  helpers deliberately hide.
- The web token is deliberately excluded from the printed settings (it is a local
  secret); only `--web-token`/`$HAL_WEB_TOKEN` are read here, and the state-file
  fallback happens in the web server.

## 8. Composition and mode selection (`main.go`)

`runWithArgs(args)` is the whole program, parameterized by `argv` so the tests
can drive it:

1. `parseArgs` parses flags, recording which were set (the basis of precedence),
   splitting positional arguments into messages and `@file` references, and
   validating `--mode`.
2. `config.Resolve` + `config.Load`, then `resolveOptions` and
   `validateResolved` (only the `openai` provider is accepted; anything else is
   an error naming the config file).
3. `--show-config`, `--version` and `--list-sessions` print and exit.
4. Non-terminal stdin is read (bounded to 4 MiB) and merged with `@file`
   contents and the positional message by `buildPrompt`.
5. The provider is built (`provider.NewFromEnv`) and stamped with
   `User-Agent: hal/<version>`, the session header decided by
   `sessionHeaderFor(configured, baseURL)`, and a run id (`newRunID`) as the
   starting affinity id. Tools are selected (`tools.Select`) and the system
   prompt assembled (`buildSystemPrompt`: default prompt, then `--system-prompt`,
   then discovered context files, then `--append-system-prompt`; a
   `--system-prompt` or `--append-system-prompt` value naming an existing file is
   read from disk, which would otherwise be ambiguous with a literal prompt).
6. The session is selected: an explicit file path (`--session`), `--continue-all`,
   `-c` (falling back to a fresh session when there is nothing to continue), or a
   new one. Once chosen, `ag.OnMessage` is pointed at `sess.AppendMessage` and the
   session id is pushed to the provider as the affinity id. A session *id* (rather
   than a path) is resolved by the front ends' `/switch`, through
   `session.FindByID`.
7. `signal.NotifyContext` turns SIGINT/SIGTERM into a cancelled run context, and
   the mode decides the front end: `--web`, `--mode json`, interactive (both
   stdin and stdout are terminals), or print. Interactive detection is what makes
   a piped or redirected stdout fall back to print mode.

`sessionHooks` is the single implementation of the session controls shared by the
TUI and the web console. It closes over the current `*session.Session` in a
variable that `adopt` replaces, so `/new` and `/switch` rewire the agent's
history, its `OnMessage` persistence hook and its affinity id in one place. Both
front ends receive the same four callbacks and know nothing about files.

## 9. Concurrency model

- One run per process at a time, in both front ends. The run executes on its own
  goroutine; the interface's update loop never blocks on the model.
- **TUI:** goroutine → buffered channel → bubbletea update. Cancellation is a
  `context.CancelFunc` held by the model.
- **Web:** goroutine → `applyEvent` under the server mutex → per-subscriber
  channels → `http.ResponseWriter`. Cancellation is a `context.CancelFunc` held
  by the server.
- Shared mutable state is small and mutex-guarded: the TUI is single-threaded by
  construction (bubbletea serializes updates) and only the run goroutine touches
  the agent, via events; the web server guards its state with one mutex and never
  calls the agent while holding it (except through the agent's own callbacks,
  which run on the run goroutine).
- Cancellation propagates through `context`, including into the retry wait
  (`sleepCtx`) and into `bash` (which kills the process group on expiry), so an
  abort is prompt rather than polled.

## 10. Testing strategy

The suite is layered, and the layers exist to keep the real loop under test:

- **End to end (`e2e_test.go`, `config_integration_test.go`, `headers_test.go`,
  `examples_test.go`, root package).** An `httptest` server scripts SSE
  responses (text chunks, tool-call chunks, usage, resets) and the real agent
  loop runs against it with the real tools in `t.TempDir()`: tool-call round
  trips, chained tools, tool failure fed back to the model, allowlists, provider
  errors, print and JSON output, context-file discovery, session persistence,
  XDG path resolution, config precedence (flag > env > config), `--show-config`
  output and masking, retry budgets, and the identity headers. This layer is the
  reason changes are cheap to verify: it exercises what the user actually runs.
- **Unit tests per package.** `agent` covers the retry policy with an injected
  `Sleep` (transient success, budget exhaustion, permanent failure, partial
  stream, `Retry-After`, backoff cap, disabled retries, abort during backoff).
  `provider` covers failure classification and header timeouts against a fake
  server. `tools` covers each tool's behaviour against temp directories,
  including the `bash` timeout. `session` covers the JSONL round trip, grouping,
  listing, `FindByID` and `LatestIn`. `config` covers resolution, TOML/JSON
  parsing, precedence between the two files, warnings and path expansion.
- **Front-end tests with fakes.** `tui` and `web` drive their models/servers with
  a scripted fake agent (the same small `Agent` interface the real one satisfies)
  and `httptest`, so folding events, aborting, queueing, retry notices, session
  switching, the picker windowing and the auth checks are all covered without a
  terminal or a socket.
- **Race detector** on Linux/macOS (CI), plus `go vet` and `gofmt -l` on every
  platform. Tests are kept portable: temp directories rather than `/tmp` literals,
  no assumptions about the host shell beyond what `bash` documents.

## 11. Extension points, and what is deliberately absent

The seams a design change would use, without redesigning the core:

- A new provider implements `agent.Provider`; runtime mutation (model, session
  id) is optional through `ModelSetter`/`SessionIDSetter` and a type assertion,
  so a provider that cannot change model at runtime simply is not asked to.
- A new tool implements `agent.Tool` (name, description, JSON Schema, `Run`) and
  is added to `tools.BuiltinNames`/`DefaultNames`/`New`/`Select`.
- A new front end implements the same small `Agent` interface and consumes the
  same event stream; sessions come in as the `sessionHooks` callbacks, so it
  needs no knowledge of session files.

All three are constrained by `design/features/basic-functions`: the tool set is
fixed at seven, one provider protocol is packaged, and the four interfaces are
the supported ones. Anything beyond that — extensions, sub-agents, MCP, other
vendors — is out of scope until a design document says otherwise, not something
to be added as a local convenience.
