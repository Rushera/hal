# Security policy

## What hal does to your machine

hal is an agent: it sends your prompt and conversation to the model you
configure and executes the tool calls the model returns. `bash` runs with your
user's privileges, `write`/`edit` modify files, and the web console exposes the
same capability over HTTP.

Treat everything the model produces as untrusted input. A file hal reads can
contain text that tells the model to run a command (prompt injection); hal does
not sandbox it. Run hal only in directories you are willing to have modified,
and reduce the surface when you can:

- `--tools read,grep,find,ls` for read-only work, or `--print` for one-shot runs.
- Leave `--web-addr` on its default `127.0.0.1`; the console authenticates every
  request with a random per-run token, which is what stops a page on another
  origin from reaching the API through your browser's loopback access.
- Keep credentials in `api_key_env`/the environment rather than in a config file,
  and keep that file mode 600 (hal warns when it is group- or world-readable).

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub's
[security advisories](https://github.com/Rushera/hal/security/advisories/new)
("Report a vulnerability" on the *Security* tab), not in a public issue.

Please include the version (`hal --version`), what you did, what happened, and
what you expected. You should get an acknowledgement within a few days; if the
report is valid, a fix and an advisory will follow once a patch is available.
