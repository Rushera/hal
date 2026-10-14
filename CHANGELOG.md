# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Session switching.** `/switch` in the TUI opens a picker of the sessions
  stored for the directory (`↑`/`↓`, `enter`, `esc`), `/switch <id>` opens one
  directly, and `/sessions` lists them; switching aborts a turn in flight and
  replays the conversation it switched to. The web console gets a session menu in
  the header and the same `/switch` / `/sessions` commands. `hal
  --list-sessions` prints the stored conversations with the ids `--session`
  accepts, and `hal --continue-all` resumes the most recent session for the
  machine rather than only for the working directory.
- **Retries for transient provider failures.** A reset connection
  (`read: connection reset by peer`), a timeout, a `429`, or a `5xx` is now
  retried with exponential backoff (0.5s to 10s, four attempts by default)
  instead of failing the turn. A server `Retry-After` wins over the backoff, an
  abort during a backoff stops immediately, and a failure after part of the
  answer was streamed is not retried (that text has already been shown). Every
  retry is reported in the TUI, the web console, and print mode. The budget is
  `--max-retries` / `max_retries`.
- The provider HTTP client reuses connections per host and bounds only the wait
  for response headers.

### Fixed

- A dropped connection in the middle of a stream is no longer presented as an
  empty answer: an unfinished stream that delivered nothing is treated as a
  transient failure and retried.
- A long streamed answer is no longer cut off: only the wait for response headers
  is bounded, which a whole-request timeout would have covered too.
- The TUI session picker fits the terminal: entries are clipped and the list is
  windowed to the height, so a narrow or short terminal no longer overflows.
- The web console reuses its access token from
  `$XDG_STATE_HOME/hal/web-token` (overridable with `--web-token` or
  `$HAL_WEB_TOKEN`), so a restarted process keeps serving a page that is already
  open: reload the tab instead of reopening the URL.
- The console URL carries the session id. Loading a URL that names another
  session reopens it, and the transcript is rebuilt from the stored conversation
  — including tool calls and their output, marked with a `restored` note — so a
  reload or a restart shows the whole conversation again. `hal --web -c` resumes
  the latest session for the directory, which is what a rebuild-and-reload loop
  wants.

## [0.1.0] - 2026-10-12

First release.

### Added

- Agent loop over any OpenAI-compatible `/chat/completions` endpoint, with a
  bounded number of turns per prompt and streamed responses.
- Seven built-in tools: `read`, `write`, `edit`, `bash`, `grep`, `find`, `ls`,
  selectable with `--tools`.
- Four interfaces: interactive bubbletea TUI, `--web` browser console, `--print`
  one-shot mode, and `--mode json` for a JSONL event stream.
- JSONL sessions with parent links, grouped per working directory and resumable
  with `-c`; sessions can be named and stored in an explicit directory.
- Configuration from flags, environment variables, and a TOML or JSON file,
  resolved with flags > environment > file > default and inspectable with
  `--show-config`; credentials can be read from a named environment variable.
- `AGENTS.md`/`CLAUDE.md` context files, `--system-prompt` and
  `--append-system-prompt`, and per-conversation session headers for gateways
  that route agent traffic.

[Unreleased]: https://github.com/Rushera/hal/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Rushera/hal/releases/tag/v0.1.0
