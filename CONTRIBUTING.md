# Contributing

Thanks for considering a contribution. Issues and pull requests are welcome.

## Ground rules

- Keep the tool set small. hal deliberately ships one provider protocol
  (OpenAI chat completions) and seven tools; features that need a plugin system
  or a vendor matrix are out of scope (see the README's *Limitations*).
- Match the existing style: standard library first, small interfaces, doc
  comments on exported identifiers, errors that name what failed.
- Tests come with the change. The suite already runs the real agent loop
  against a scripted SSE server and the real tools against temp directories, so
  a test that covers the new path end to end is usually straightforward.

## Working on the code

```bash
git clone https://github.com/Rushera/hal && cd hal
go build ./...
go vet ./...
go test ./...
go test -race ./...   # Linux/macOS
gofmt -l .            # must print nothing
```

CI runs all of the above on Linux, macOS and Windows; keep the tests portable
(no `/tmp` literals, no assumption about the host shell beyond what the `bash`
tool already documents).

## Pull requests

- One logical change per pull request, with a commit message that explains why
  the change is needed, not just what changed.
- Note the user-visible behaviour change in the description; if it is a feature,
  update the README section that documents it.
- Do not add a `LICENSE` header to individual files.

## License

hal is GPL-3.0-or-later. By submitting a contribution you agree to license it
under the same terms.
