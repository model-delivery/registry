# Contributing

Thank you for helping. Bug reports, compatibility reports and pull
requests are all welcome.

## Reporting a problem

Open an issue with:

- The registry version (`modelctl version`).
- What you ran and what happened, including the full error message.
- For a compatibility problem: the client library and its version (for
  example `huggingface_hub 1.24.0`, `transformers 5.x`) and the call that
  failed.

Do not report security problems in public issues. See
[SECURITY.md](SECURITY.md).

## Development setup

You need Go 1.26 or newer. [Task](https://taskfile.dev) is optional but
runs the common commands. The compatibility tests also need Python with
`huggingface_hub` installed.

```bash
task check      # format check, vet, unit tests
task test:e2e   # run the real huggingface_hub client against the registry
task run        # run a registry on http://localhost:8080 with data in ./data
```

Without Task: `go test ./...` and
`go test -tags=integration ./test/e2e/`.

## What a good change looks like

- **It comes with a test** that fails without the change. Unit tests must
  pass with no network, no Python and no Docker (`go test ./...`). Tests
  that need Python or the network go in `test/e2e` behind the
  `integration` build tag.
- **Compatibility claims are tested against the real client.** If you add
  or change a Hugging Face compatible route, extend `test/e2e` so the real
  `huggingface_hub` library exercises it, and update
  `docs/compatibility.md`.
- **It stays simple.** One binary and one data directory, no required
  external services. A change that adds a dependency or a moving part
  should say why it is worth it.
- **Integrity comes first.** Nothing may be stored or served without being
  verified against its digest, and a failed operation must leave nothing
  half-visible.
- **Docs move with the code.** Update the README, `docs/` and command help
  in the same pull request.

Code is formatted with `gofmt`. Keep comments about why, not what.

## Pull requests

Keep them focused; separate unrelated changes. Describe what changed and
how you tested it. CI must pass.

## Licence

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE), the same licence as the project.
