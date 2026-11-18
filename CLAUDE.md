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
- `internal/registry` — repositories, immutable revisions, refs, tokens,
  and metadata backup and restore (`backup.go`).
- `internal/mirror` — imports a revision from an upstream hub.
- `internal/server` — HTTP: `hf.go` (Hugging Face routes), `api.go`
  (`/api/v1`), `ui.go` plus `templates/` and `static/` (web UI).
- `internal/client`, `internal/cli` — Go client and the command line.
- `test/e2e` — runs the real `huggingface_hub` Python library against the
  registry. Build tag `integration`.

- `.github/workflows` — named for the event that starts them:
  `pull-request.yaml`, `merge.yaml` (push to main), `release.yaml` (version
  tag), `release-cut.yaml` (manual button). The checks live once, in
  `quality.yaml`, which the others call. A new check goes in `quality.yaml`;
  a new workflow is named for its trigger. Actions are pinned by commit SHA.

## Commands

Use the Taskfile: `task check` (format, vet, lint, unit tests), `task test:e2e`
(needs Python with `huggingface_hub`), `task test:s3` (real S3 server in
Docker; the only place multipart uploads are exercised), `task build`,
`task docker`.

## Rules that are easy to break

- **Nothing is stored or served unverified.** Every write goes through
  `blob.Store.Stage`, which hashes while streaming and rejects mismatches.
  Do not add a path that writes under `blobs/` or to the bucket any other
  way; the S3 backend stages locally for exactly this reason. The one
  other thing stored beside blobs is metadata backups (`blob.Archive`,
  under `backups/`), which are never served as model content.
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
- **`files` rows always point at stored, verified content.** A file that
  pull-through has listed but not fetched is a row in `pending_files`, and
  moves to `files` only in `Registry.Fulfil`, after its content matched its
  digest. Never insert into `files` for content that is not stored, and
  never serve a pending file's content.
- **Stream, never buffer.** Model files are many gigabytes. No
  `io.ReadAll` on file content, in the server or the client.
- **The upstream host comes from server configuration only**, never from a
  request. What the upstream answers is trusted less than its address:
  every upstream request goes through `Upstream.get`, which follows
  redirects itself, sends the token only to the configured origin, and
  refuses hops to local and private addresses (`internal/mirror/guard.go`).
  Do not let an `http.Client` follow upstream redirects by itself, and do
  not put an upstream response body in an error.
- **Pull-through never adds to a repository created here.** It goes
  upstream only for a repository that is absent or whose `upstream` field
  is set; `Server.resolve` checks, and `CommitLazy` checks again inside
  its transaction (`OnlyFromUpstream`). A stored blob is matched by git
  blob id only through `FindMirroredBlob`.
- **Small request bodies are read with `Server.readJSON` or
  `Server.formValue`**, which bound their size and the time the client has
  to send them. Blob and file transfers are deliberately unbounded in
  time, and `isFileTransfer` (`limits.go`) is the list of them: every
  other request that announces a body gets a read deadline. A new route
  whose body is model content must be added there, or slow uploads to it
  are cut off; `TestFileTransfersHaveNoBodyDeadline` is where to add the
  case.
- **Every route declares the role it needs** via `Server.need`. A new route
  without it is a bug; `TestAccessControl` is where to add the case.
- **Compatibility claims need a real-client test.** If the README or
  `docs/compatibility.md` says a `huggingface_hub` call works, `test/e2e`
  must exercise it.
- `go test ./...` must pass with no network, no Python and no Docker.

## Ownership

A Spectrum Labs LLC project; the LLC holds the copyright (`NOTICE`). Every Go file starts with
`// Copyright 2026 Spectrum Labs LLC` then `// SPDX-License-Identifier: Apache-2.0`; `task headers`
checks it and CI runs it. Keep the year a file was first written; do not bump it.

## Style

Standard library first. Runtime dependencies are the pure-Go SQLite driver
and the minio-go S3 client; neither needs cgo, which keeps the binary easy
to cross-compile. The
web UI is server-rendered templates with no JavaScript and no build step.
Errors shown to users say what to do next. Docs change in the same commit
as the behaviour they describe.
