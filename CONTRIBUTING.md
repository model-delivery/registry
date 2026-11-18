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
runs the common commands. `task check` also needs
[golangci-lint](https://golangci-lint.run) v2.14.0, the version the checks
pin. The compatibility tests also need Python with `huggingface_hub`
installed.

```bash
task check      # format check, vet, lint, unit tests
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
how you tested it. The checks must pass.

## Automation

Workflows in `.github/workflows` are named for what starts them, so you
can find the one that ran from the event you caused:

| File | Starts when | Does |
|---|---|---|
| `pull-request.yaml` | A pull request is opened or updated | Runs the checks |
| `merge.yaml` | A commit lands on `main` | Runs the checks again, plus the test against huggingface.co, then publishes the `edge` container image |
| `release.yaml` | A version tag (`v1.2.3`) is pushed | Runs the checks, publishes the container image, the binaries and the GitHub release |
| `release-cut.yaml` | A maintainer presses **Cut release** | Picks the next version, tags `main`, starts `release.yaml` |
| `quality.yaml` | Called by the three above | The checks themselves, in one place |

The checks are: formatting, `go vet`, golangci-lint, govulncheck, unit
tests on Linux, macOS and Windows (with the race detector on Linux), the
real `huggingface_hub` client against the registry, the storage tests
against a real S3-compatible server, and a build-and-start of the
container image. `task check`, `task test:e2e`, `task test:s3` and
`task docker` run the same things locally.

## Releasing (maintainers)

1. Make sure `main` is green.
2. Actions → **Cut release** → Run workflow, choosing patch, minor or
   major. Or from a terminal: `task release -- -f bump=minor`. For a
   pre-release, give the exact version instead: `-f version=v0.2.0-rc.1`.
3. `release.yaml` does the rest. A pre-release does not move the `latest`
   image tag and is marked as a pre-release on GitHub.

Pushing a tag by hand (`git tag v0.2.0 && git push origin v0.2.0`) starts
the same release.

## Licence

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE), the same licence as the project.
