# CLAUDE.md

Context for AI assistants working in this repository.

## What this is

Model Delivery Registry: a self-hosted registry for AI model files that
Hugging Face clients can download from and upload to. One Go binary
(`modelctl`), SQLite for metadata, and a content-addressed file store on
local disk or in an S3-compatible bucket. Apache 2.0.

Read `docs/architecture.md` before changing anything structural, and
`docs/compatibility.md` before touching a Hugging Face compatible route.

## Layout

- `cmd/modelctl` — entry point; everything else is in `internal/`.
- `internal/blob` — content-addressed store. Files are named by SHA-256.
  Two backends, `FS` (`store.go`) and `S3` (`s3.go`), behind one interface.
  `store_test.go` runs the same contract against both; a new backend joins
  that list. `archive.go` keeps metadata backups beside the blobs,
  `check.go` re-reads a blob against its digest, and `tail.go` lets
  readers follow a blob while it is staged. `blobtest` is an in-memory
  S3 for tests.
- `internal/datadir` — the names of the files and directories in the data
  directory, and clearing `tmp/` at server start. Nothing else joins those
  names itself.
- `internal/registry` — repositories, immutable revisions, refs and tokens:
  `registry.go` (opening, blobs, garbage collection, tokens), `repo.go`
  (reading), `commit.go` (recording a revision), `pending.go` (files listed
  from upstream and not fetched yet), `follow.go` (the follow list:
  repositories kept up to date with their upstream), `backup.go` (metadata
  backup and restore), `verify.go` (re-checking every stored file).
- `internal/mirror` — imports a revision from an upstream hub: `mirror.go`
  (the plan and its execution), `refs.go` (every branch and tag, or one
  branch, as the upstream has them now: what `--all-refs` and a sync run),
  `upstream.go` (the hub's API, resumed downloads), `guard.go` (where the
  upstream may send the registry).
- `internal/server` — HTTP. `server.go` has the route table, the access
  gate and the error writers. Then one file per surface: `hf.go` (Hugging
  Face download routes), `hfupload.go` (Hugging Face upload routes),
  `pullthrough.go` (fetching from upstream on demand), `stream.go`
  (sending a file to clients while it is fetched), `api.go` and
  `backup.go` (`/api/v1`), `ui.go` plus `templates/` and `static/` (web
  UI), `limits.go` (bounds on request bodies), `metrics.go` (every metric
  of `GET /metrics`, and the handler that counts requests).
- `internal/metrics` — counters, gauges and histograms written in the
  Prometheus text format. Standard library only; it knows nothing about
  the registry.
- `internal/client` — the Go client for `/api/v1`. It shares only the HTTP
  API with the server and imports no other package here but `internal/kit`.
- `internal/cli` — the command line: `serve.go` (the server and its
  settings), `cli.go`, `backup.go` and `verify.go` (the client commands,
  each a call on `internal/client`, and `restore`, which works on a data
  directory with the registry stopped).
- `internal/kit` — the few helpers more than one package needs (showing a
  size, escaping a path for a URL, the git blob id, writing a file
  atomically). Standard library only. A helper moves here when a second
  package needs it, not before.
- `internal/regtest` — helpers for tests: a registry served over HTTP,
  `modelctl serve` on a free port, the fake-S3 environment. Test code only.
- `test/e2e` — runs the real `huggingface_hub` Python library, and the
  `datasets` library when it is installed, against the registry. Build tag
  `integration`.

- `.github/workflows` — named for the event that starts them:
  `pull-request.yaml`, `merge.yaml` (push to main), `release.yaml` (version
  tag), `release-cut.yaml` (manual button). The checks live once, in
  `quality.yaml`, which the others call. A new check goes in `quality.yaml`;
  a new workflow is named for its trigger. Actions are pinned by commit SHA.

## Commands

Use the Taskfile: `task check` (format, vet, headers, lint, unit tests,
tidy modules), `task test:e2e` (needs Python with `huggingface_hub`, and
`datasets` for the `load_dataset` tests, which are skipped without it),
`task test:s3` (real S3 server in Docker; the only place multipart uploads
are exercised), `task build`, `task docker`.

## Rules that are easy to break

- **Nothing is stored unverified, and nothing is served from the store
  unverified.** (The one case of bytes sent before they are verified is a
  file still being fetched; see the rule on pending files below.) Every
  write goes through
  `blob.Store.Stage`, which hashes while streaming and rejects mismatches.
  Do not add a path that writes under `blobs/` or to the bucket any other
  way; the S3 backend stages locally for exactly this reason. The one
  other thing stored beside blobs is metadata backups (`blob.Archive`,
  under `backups/`), which are never served as model content.
- **Publishing and removing a blob hold that digest's lock**
  (`Registry.locks`). Removal re-checks "unreferenced" in the same
  statement that deletes the record. Do not split that check from the
  delete. The record goes before the bytes; if storage then refuses to
  remove them, `dropBlob` puts the record back before it releases the
  lock, so that a later `gc` finds the blob again. Only what storage
  removed is counted as freed.
- **All SQL stays in `internal/registry`.** That is what keeps a second
  metadata backend a contained change.
- **A repository's type is part of its identity.** A model and a dataset
  of the same name are two repositories. `internal/registry` addresses a
  repository by its key (`registry.RepoKey`: `ns/name`, or
  `datasets/ns/name`); every query on `repos` names both the type and the
  name. Outside it, a type travels as `registry.RepoType`, never a bare
  string, and models and datasets go through the same code with the type
  as a parameter: no second copy of a route, handler or command for
  datasets.
- **A schema change is one more migration step** (`migrate` in
  `registry.go`), run in the same transaction as the others and the new
  version number, and it gets a test that opens a database written in the
  previous layout (`TestUpgradeFromSchemaVersion3` is the pattern).
- **Revisions are immutable.** A revision's id and what it lists (each
  path with its size and digest) never change; new content is a new
  revision; only refs move, and tags never move. Never update or delete a
  `files` row, and never change a `commits` row, with one exception that
  changes neither of those things: a revision that pull-through recorded
  from an upstream listing has its files in `pending_files` until they are
  fetched. `fulfil` (`pending.go`) then inserts the file's `files` row, the
  only place one is written for a revision that already exists, and when
  none is left pending `sealIfComplete` sets `commits.tree`, once. Do not
  add another write to an existing revision.
- **A failed operation leaves nothing half-visible.** Recording a revision
  is one transaction. A mirror records nothing unless every file verified.
- **`files` rows always point at stored, verified content.** A file that
  pull-through has listed but not fetched is a row in `pending_files`, and
  moves to `files` only through `fulfil` (`Registry.Fulfil` for one file,
  `Registry.CompleteRevision` for the rest of a revision in one
  transaction), after its content matched its digest. Never insert into
  `files` for content that is not stored.
- **A pending file's content leaves the registry in one way only, and
  never whole.** While pull-through is fetching it, `stream.go` may send
  it to the requests waiting for it, under these rules; do not add another
  path that serves a pending file, and do not loosen one of these:
  - *What may be sent:* bytes of the fetch in flight, read through its
    `blob.Tail`, to a `GET` that runs to the end of the file (the whole
    file or `Range: bytes=N-`, no conditional header), for a file larger
    than `streamHoldBack`.
  - *What is held back:* the last `streamHoldBack` bytes (1 MiB), until
    `Stage` has matched the digest and the size. If that fails, or the
    fetch fails any other way after a byte was sent, the response is
    aborted (`panic(http.ErrAbortHandler)`), never ended cleanly, and the
    reason is logged.
  - *What is never stored:* anything unverified. Nothing changes in how
    a blob is staged, published or recorded; a failed fetch leaves no
    blob, no `files` row and no staging file.
  - *What follows a failure:* the file is distrusted (`puller.distrust`,
    by ETag, until restart). It is then verified and stored before any
    of it is sent, and a request for part of it is answered with all of
    it (`200`), including where a redirect to the object store would
    otherwise be sent. That is what stops a client from completing
    unverified bytes with verified ones; `TestStreamAbortsWhenVerificationFails`
    and the wrong-bytes test in `test/e2e` are where a change to it is
    checked.
  - With `PullThroughNoStream` (`--pull-through-stream=false`) none of
    this applies and a pending file's content is never served.
- **Stream, never buffer.** Model files are many gigabytes. No
  `io.ReadAll` on file content, in the server or the client. The few
  `io.ReadAll` calls there are read metadata through an `io.LimitReader`.
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
- **Every route says how it is protected, where it is registered.** Either
  `Server.gate`, with the role it needs and the style a refusal is written
  in (`asAPI`, `asHF`, `asUI`), or `public`, with a comment saying why no
  role is asked (the signed upload URL, whoami, sign-in, the health check).
  Paths the mux cannot match are routed by `Server.root` and
  `Server.hfRepos`, which gate each route they find the same way. The gate
  is the only place a role is compared: a handler never checks one itself.
  A new route registered any other way is a bug; `TestAccessControl` is
  where to add the case.
- **Whose failure it is decides what a client is told.** A failure of the
  registry's own (its disk, its database) is `500`, logged in full, and
  described to the client no further. A failure the upstream caused is
  `502` with the reason, for mirror and pull-through alike; only what
  `mirror.FromUpstream` vouches for is reported that way. An error value
  is answered through `Server.fail` (registry API) or `Server.hfFail`
  (Hugging Face routes), which apply this; `writeError` and `hfError` are
  for fixed messages only.
- **Work that outlives a request runs on the server's lifetime**, never on
  `context.Background()`. Pull-through jobs do (`puller.ctx`), and
  `Server.Shutdown` cancels and waits for them; `serve` in
  `internal/cli/serve.go` waits for that and for the backup loop before the
  registry is closed. `TestServeStopsItsOwnWorkBeforeTheRegistryCloses` is
  where a new kind of background work joins.
- **A client command is one call on `internal/client`.** It builds no
  path and decodes no response itself; a new request is a new typed method
  there, with a named result. `--json` prints the registry's document as
  it arrived (the result's `Raw`), so do not re-encode a result for it.
- **Every server setting has a flag and an environment variable**, the
  variable being the flag's default, unless it is a secret. Secrets (the
  admin token, the upstream token, the S3 keys) are read from the
  environment only. A value that cannot be read stops the server with a
  usage error naming the setting; never fall back to a default in silence.
  `TestServeHelpListsEverySetting` is where a new setting joins, and the
  table in the README.
- **`docs/api.md` lists every `/api/v1` route**, and nothing else.
  `TestAPIDocumentMatchesTheRoutes` fails when a route is added, renamed
  or removed without the document following.
- **No metric is labelled by anything a request carries.** Not a
  repository, revision, path, token name or address: the metrics must not
  leak the catalogue or grow with it. A label is declared with all of its
  values (`metrics.L`), and `internal/metrics` does not count a value
  that was not declared, so a label's values are constants in
  `server/metrics.go`. Counting is an atomic add on a handle looked up
  beforehand, never a lock or a lookup per byte, and never changes an
  answer: a handler's `ResponseWriter` may be wrapped, so
  `http.MaxBytesReader` is given `baseWriter(w)`. A new metric gets a row
  in the table in `docs/operations.md` (`TestEveryMetricIsDocumented`),
  and `TestMetricsCountWhatTheRegistryDoes` is where what it counts is
  checked.
- **Compatibility claims need a real-client test.** If the README or
  `docs/compatibility.md` says a `huggingface_hub` or `datasets` call
  works, `test/e2e` must exercise it, for each repository type claimed.
- **A test that needs a clock or a schedule supplies it.** Hand the code
  under test its clock or its ticks (`server.Config.Now`, the tick channel
  of `runBackups`) and wait on what it does, rather than sleeping until it
  has probably happened. `TestScheduledBackupsRunAndPrune` is the pattern.
- `go test ./...` must pass with no network, no Python and no Docker.

## Ownership

A Spectrum Labs LLC project; the LLC holds the copyright (`NOTICE`). Every Go file starts with
`// Copyright 2026 Spectrum Labs LLC` then `// SPDX-License-Identifier: Apache-2.0`; `task headers`
checks it and CI runs it. Keep the year a file was first written; do not bump it.

## Style

Standard library first. Runtime dependencies are the pure-Go SQLite driver
and the minio-go S3 client; neither needs cgo, which keeps the binary easy
to cross-compile. (`gofakes3` is in `go.mod` for tests only.) The
web UI is server-rendered templates with no JavaScript and no build step.
Errors shown to users say what to do next. Docs change in the same commit
as the behaviour they describe.
