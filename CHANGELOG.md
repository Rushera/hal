# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
