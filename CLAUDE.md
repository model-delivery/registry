# CLAUDE.md

Context for AI assistants working in this repository.

## What this is

Model Delivery Registry: a self-hosted registry for AI model files that
Hugging Face clients can download from. One Go binary (`modelctl`), SQLite
for metadata, a content-addressed file store on disk. Apache 2.0.

Read `docs/architecture.md` before changing anything structural, and
`docs/compatibility.md` before touching a Hugging Face compatible route.

## Layout

- `cmd/modelctl` — entry point; everything else is in `internal/`.
- `internal/blob` — content-addressed store. Files are named by SHA-256.
  Two backends, `FS` and `S3`, behind one interface. `store_test.go` runs
  the same contract against both; a new backend joins that list.
  `blobtest` is an in-memory S3 for tests.
- `internal/registry` — repositories, immutable revisions, refs, tokens.
- `internal/mirror` — imports a revision from an upstream hub.
- `internal/server` — HTTP: `hf.go` (Hugging Face routes), `api.go`
  (`/api/v1`), `ui.go` plus `templates/` and `static/` (web UI).
- `internal/client`, `internal/cli` — Go client and the command line.
- `test/e2e` — runs the real `huggingface_hub` Python library against the
  registry. Build tag `integration`.

## Commands

Use the Taskfile: `task check` (format, vet, unit tests), `task test:e2e`
(needs Python with `huggingface_hub`), `task test:s3` (real S3 server in
Docker; the only place multipart uploads are exercised), `task build`,
`task docker`.

## Rules that are easy to break

- **Nothing is stored or served unverified.** Every write goes through
  `blob.Store.Stage`, which hashes while streaming and rejects mismatches.
  Do not add a path that writes under `blobs/` or to the bucket any other
  way; the S3 backend stages locally for exactly this reason.
- **Publishing and removing a blob hold that digest's lock**
  (`Registry.locks`). Removal re-checks "unreferenced" in the same
  statement that deletes the record. Do not split that check from the
  delete.
- **All SQL stays in `internal/registry`.** That is what keeps a second
  metadata backend a contained change.
- **Revisions are immutable.** Never update a `commits` or `files` row.
  New content is a new revision; only refs move, and tags never move.
- **A failed operation leaves nothing half-visible.** Recording a revision
  is one transaction. A mirror records nothing unless every file verified.
- **Stream, never buffer.** Model files are many gigabytes. No
  `io.ReadAll` on file content, in the server or the client.
- **The upstream host comes from server configuration only**, never from a
  request.
- **Every route declares the role it needs** via `Server.need`. A new route
  without it is a bug; `TestAccessControl` is where to add the case.
- **Compatibility claims need a real-client test.** If the README or
  `docs/compatibility.md` says a `huggingface_hub` call works, `test/e2e`
  must exercise it.
- `go test ./...` must pass with no network, no Python and no Docker.

## Style

Standard library first. Runtime dependencies are the pure-Go SQLite driver
and the minio-go S3 client; neither needs cgo, which keeps the binary easy
to cross-compile. The
web UI is server-rendered templates with no JavaScript and no build step.
Errors shown to users say what to do next. Docs change in the same commit
as the behaviour they describe.
