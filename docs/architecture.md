# Architecture

The registry is one Go process and one data directory. There is no
database server, queue, or second service to run. The one thing that can
live outside the data directory is the model files themselves, which can
be kept in an S3-compatible bucket instead of on local disk.

```
              huggingface_hub, transformers, hf CLI        modelctl          browser
                             |                                |                 |
                             v                                v                 v
        +------------------------------------------------------------------------------+
        |  internal/server      one HTTP handler                                       |
        |    server.go       the route table, the access gate, how errors are written  |
        |    hf.go           Hugging Face downloads    /api/models/...  /.../resolve   |
        |    hfupload.go     Hugging Face uploads      preupload, lfs batch, commit    |
        |    pullthrough.go  fetching from upstream what a client asks for             |
        |    stream.go       sending a file to clients while it is fetched             |
        |    api.go          registry REST API         /api/v1/...                     |
        |    backup.go       metadata backups          /api/v1/backups...              |
        |    ui.go           web UI                    /  /ui/...                      |
        |    limits.go       bounds on the size and time of request bodies             |
        |    metrics.go      what all of the above did, counted        /metrics        |
        +------------------------------------------------------------------------------+
                             |                                |
                             v                                v
        +-------------------------------------+   +------------------------------+
        |  internal/registry                  |   |  internal/mirror             |
        |  repositories, revisions, refs,     |<--|  reads an upstream hub,      |
        |  tokens, backups, verify (SQLite)   |   |  verifies, records a revision|
        +-------------------------------------+   +------------------------------+
                             |
                             v
        +-------------------------------------+
        |  internal/blob                      |
        |  content-addressed file store,      |
        |  on local disk or in an S3 bucket   |
        +-------------------------------------+
```

`internal/cli` is the `modelctl` command line and `internal/client` is the
Go client it uses: each client command is one call on the client. The
server never calls the client; they share only the HTTP API.

Four small packages sit beside these. `internal/datadir` is the one place
that names the files and directories of the data directory. `internal/kit`
holds the few helpers more than one package needs and imports only the
standard library, which is what lets the client use it. `internal/metrics`
keeps counters, gauges and histograms and writes them in the Prometheus
text format; it too is standard library only (see [Metrics](#metrics)).
`internal/regtest` is test helpers.

## Data directory

```
data/
  registry.db              SQLite: repositories, revisions, files, pending files,
                           refs, tokens (and its write-ahead log beside it)
  blobs/sha256/ab/abcd...  one file per unique content, named by its SHA-256
  backups/registry-*.db    metadata backups
  tmp/                     uploads being received, snapshots being taken
```

With S3 storage, `blobs/` and `backups/` are absent: the same
`sha256/ab/abcd...` and `backups/...` names are object keys in the bucket.
`registry.db` and `tmp/` stay local.

The tables of `registry.db` are `repos`, `commits` (revisions), `files`
(one row per file of a revision, pointing at a blob), `pending_files`
(files a revision lists whose content has not been fetched; see
[Pull-through caching](#pull-through-caching)), `refs`, `blobs` (what is
stored, and when it was last written or asked about) and `tokens`.

## Content-addressed storage

Every file's bytes are stored once, under the SHA-256 of the content. A
revision does not contain files; it lists `path -> SHA-256`. So:

- Two revisions that share a 5 GB weights file store it once.
- Two repositories that contain the same file store it once.
- Re-mirroring or re-pushing unchanged content transfers nothing.
- A stored file can always be re-checked against its own name, which is
  what `modelctl verify` does for everything in storage.

Writing a blob (`internal/blob`):

1. Stream the incoming bytes to a file in `tmp/`, hashing as they arrive.
   Nothing is buffered in memory beyond a copy buffer, so file size is
   bounded by disk, not RAM.
2. Compare the result with what the sender declared (SHA-256, size, and for
   mirrored non-LFS files the git blob id). On any mismatch the temporary
   file is deleted and the write fails.
3. Rename the temporary file to its digest path. A rename is atomic, so a
   blob is either fully present under its name or absent; a reader can
   never see a partial file.

Staging can be followed while it runs: a caller that hands `Stage` a
`blob.Tail` gets readers over the temporary file that see the bytes
received so far, and are told when step 2 has passed or that it never
will. Pull-through uses it to send a file while fetching it; it changes
nothing about what is published.

`blob.Store` is a small interface (stage, open, delete, plus the four
methods of `blob.Archive` that keep metadata backups beside the blobs)
with two implementations, and one test suite that both must pass. Opening
a store removes nothing; `modelctl serve` clears what a previous run left
in `tmp/` once, at start (`internal/datadir`, which is also the one place
that names the files and directories of the data directory).

- **`blob.FS`** keeps blobs as files in the data directory, as above.
- **`blob.S3`** keeps them as objects in an S3-compatible bucket, under the
  same `sha256/ab/abcd...` names. An upload is still received and verified
  on local disk first (step 1 and 2), and only then sent to the bucket, as
  a multipart upload when it is large. The object store makes an object
  visible only when the upload completes, which gives the same
  all-or-nothing guarantee as the rename. The cost is local scratch space
  for the largest file being received.

Reading from S3 goes one of two ways. By default the registry opens the
object and streams it, turning client range requests into ranged reads of
the object. With redirects enabled, the registry answers the file request
with a `302` to a signed, time-limited URL for the object, plus the
headers a Hugging Face client needs to identify the file. The client then
downloads straight from the bucket, and the registry carries no file
traffic. The registry still checks the caller's access before it signs.

## Revisions and refs

A **revision** is immutable: an id (40 hex characters), a list of files,
and where it came from. Its id and what it lists (each path, with its size
and digest) never change after it is recorded. The one thing that happens
to a recorded revision is that a file pull-through listed for it, and had
not fetched, gets fetched: its row moves from `pending_files` to `files`
(see [Pull-through caching](#pull-through-caching)). No file of a revision
is ever replaced or removed.

- A mirrored revision keeps the id it has upstream, so pinning a revision
  id works identically against Hugging Face and against the registry.
- A pushed revision gets an id derived from its content, parent, message
  and time.

A **ref** is a name for a revision. Branches move when a new revision is
pushed or mirrored. Tags never move: creating a tag that exists and points
elsewhere is refused.

Recording a revision is one SQLite transaction: the revision, its file
list and its ref updates become visible together or not at all. The
transaction refuses to reference a blob that is not stored, and the
database enforces that with a foreign key.

Pushing content identical to the branch's current revision does not create
a new revision.

## Uploads

A revision is recorded from files that are already stored, so every way of
uploading has the same two steps: store the files, each verified against
its SHA-256, then record a revision that names them. A file the registry
already stores is not sent again.

- **`modelctl push`** uses the registry's own API: `PUT /api/v1/blobs/{sha256}`
  for each file it does not find with `HEAD`, then
  `POST /api/v1/repos/{repo}/commits`.
- **Hugging Face clients** (`hf upload`, `upload_file`, `upload_folder`,
  `create_commit`) drive the steps the Hub defines, in `hfupload.go`. The
  client asks, per file, how to send it (`preupload`). Files up to 10 MiB
  travel inside the commit request, base64-encoded, one JSON line each.
  Larger files use the git-lfs batch protocol: the client names each file
  by its SHA-256 and the registry answers with a signed URL to `PUT` it
  to, or says it is already stored. Then the commit request records the
  revision, merged onto the branch's current files. If the client named
  the revision it started from and the branch has moved since, the commit
  is refused with `412`.

The signed upload URL is the one route that takes model content without a
token in the request, because clients send to it as they would to a hub's
object store. The URL itself is the grant: it is signed with a key made at
server start, names the digest, the size and the token it was issued to,
expires after twelve hours, and stops working when that token is revoked
or loses the write role. What arrives is still verified against the
digest.

Clients with the `hf_xet` package send large files through the Hub's Xet
protocol, which the registry does not speak; `docs/compatibility.md` says
what that means for users.

## Mirroring

`internal/mirror` copies one upstream revision:

1. Ask the upstream which revision id the requested branch or tag is, and
   list that revision's files. The listing carries a digest for every file:
   a SHA-256 for LFS files, a git blob id for the rest.
2. For each file, skip the download if its content is already stored (see
   below for what counts). Otherwise download it and verify it against
   the digest. A dropped connection is resumed with a range request, up
   to five times per file.
3. Once every file is stored and verified, record the revision under its
   upstream id, with the upstream's model description kept as provenance
   (unless it is over 1 MiB, in which case none is kept).

If any file fails, nothing is recorded. The repository does not appear, or
keeps pointing at its previous revision. Blobs downloaded before the
failure stay, so a retry does not fetch the LFS files again;
`modelctl gc` removes them if the mirror is abandoned. A blob the mirror
reuses is marked as just used, so a `gc` running at the same time leaves
it alone.

**What a file is verified against.** The upstream publishes one digest per
file, and which one depends on how it stores the file:

- A file tracked with LFS (weights, and anything large) has a SHA-256 of
  its content. That is also the name the registry stores it under, so
  "already stored" is a lookup by name.
- A file kept in git itself (configuration, tokenizer files, the model
  card) has only its git blob id, which is a SHA-1 over a short header and
  the content. The registry verifies the download against that, because
  it is the only digest the upstream offers. SHA-1 is no longer collision
  resistant, so this is a weaker check than the one LFS files get: it
  catches corruption and truncation, not someone able to construct two
  files with the same SHA-1 who also controls the upstream copy.

  For the same reason a stored blob stands in for such a file only if a
  mirrored revision already uses it, that is, only if it reached the
  registry from an upstream under this same id. Content that was pushed
  is never matched by git blob id; the file is downloaded instead.

Once stored, every file is named and served by the SHA-256 of what was
actually received, whichever digest it was checked against on arrival.

**Where the upstream can send the registry.** The upstream address is set
by the operator when the server starts. It is never taken from a request,
so an API caller cannot make the server fetch from an arbitrary host. The
upstream's own answers are held to rules as well (`internal/mirror/guard.go`):

- The upstream token is sent to the configured scheme, host and port and
  nowhere else, not to wherever a redirect leads.
- The next page of a file listing must be on the configured host.
- A redirect to another host is followed, since hubs keep file content on
  separate storage hosts, but not if that host is a loopback, private,
  link-local or unspecified address. The check is made against the
  address the connection is actually opened to, so a host name that
  resolves to a private address is refused too.
- What the upstream writes in the body of an error answer is logged by the
  server and is not included in the error returned to the client.

## Pull-through caching

Pull-through fetches what is asked for and nothing more, at two levels.

**Revisions.** The three Hugging Face routes and the registry's own
revision lookup all resolve a revision through one function. When that
lookup misses, and pull-through is on and the repository is allowed, the
registry asks the upstream for the revision's file listing and records it:
the revision under its upstream id, with every file's path, size and
digest, and no content. Such files are *pending*.

**Files.** Pending files live in their own table, `pending_files`. The
`files` table keeps its guarantee that every row points at stored,
verified content. A request for a pending file's metadata (`HEAD`) is
answered from the listing. A request for its content fetches it from the
upstream, verifies it against the listed digest (a SHA-256 for LFS files,
a git blob id for the rest) while storing it, and then moves the row from
`pending_files` to `files` in one transaction. While it is being fetched
it is sent to the clients waiting for it, all but its end (see
[Serve while fetching](#serve-while-fetching)). When the last pending file
of a revision arrives, the revision is complete and indistinguishable from
a mirrored one.

A small coordinator keeps one job per thing being fetched, whether a
revision's listing or one file. A request for something already being
fetched joins the existing job. Jobs run on the server's lifetime, not
on the request's, so a client that disconnects does not stop one. What
does stop them is the server stopping: `Server.Shutdown` cancels every job
and returns once the last has ended, and `modelctl serve` waits for that,
and for a scheduled backup in progress, before it closes the registry.

Four file fetches run at a time. Listings have slots of their own, since
a listing is one small request and a file fetch can run for hours: a first
request for another repository is not held up by downloads in progress. A
bounded number of jobs wait for a slot; past that, a request that would
start another is answered `503`. The time a job is allowed starts when it
gets its slot.

An error from a job is attributed before it is reported. What the upstream
caused (unreachable, an error answer, content that does not match its
digest) is answered `502` with the reason. Anything else, such as a full
disk while storing what was fetched, is the registry's own failure: `500`,
logged in full, and not described to the client.

**Which repositories.** The lookup goes upstream only for a repository the
registry does not hold, or one whose `upstream` field is set, meaning it
was created by mirroring or pull-through. A repository created here by a
push or an upload is never added to on a reader's request, and its
`upstream` field is never set by pull-through. The same check is repeated
inside the transaction that records the revision, against the canonical
name the upstream answered with, so neither a rename upstream nor a push
that lands in between gets around it. For names not held at all, the
operator's allow list is the control.

A revision cannot be built on (by an upload that merges into its branch)
while it has pending files, because that would record a new revision
referring to content the registry has never seen.

### Serve while fetching

Fetching a large file takes minutes, and download clients give up after
seconds without a byte (`huggingface_hub` after ten). So a file that is
being fetched is sent to the clients waiting for it while it arrives
(`stream.go`), and the first download takes about as long as the fetch.

The fetch itself is unchanged. The download is staged like any other
write: `blob.Store.Stage` streams it to a file in `tmp/` and hashes it as
it goes. Staging announces its progress on a `blob.Tail`, and each waiting
request follows that: it reads the staging file at its own position, sends
what is there, flushes, and waits for more. One fetch feeds the request
that started it and every request that joins while it runs. A request
that arrives after the file is stored is served from the store.

This is the one place where bytes leave the registry before they are
verified. The rules that keep an unverified file from ever being complete,
on the registry or on a client:

- **The end is held back.** Every response sent from a fetch runs to the
  end of the file, and the last mebibyte is sent only after the digest and
  the size have been checked. If the check fails, or the fetch fails for
  any other reason after bytes were sent, the connection is aborted, not
  closed cleanly: the client was given a `Content-Length` and received
  less. The reason is logged. The amount is `streamHoldBack`; the comment
  there says why it is a mebibyte and not a byte.
- **Small files are not streamed.** A file of a mebibyte or less is all
  end, so nothing of it is sent before it is stored, as before.
- **Storing is untouched.** Nothing is published, recorded in `files` or
  served from the store that did not match its digest. The staging file
  of a failed fetch is deleted.
- **Only what runs to the end of the file is streamed:** a `GET` for the
  whole file, or for the rest of it from some byte (`Range: bytes=N-`),
  with no conditional header. A bounded range, a suffix or a conditional
  request waits for the stored file and is answered by `ServeContent`.
  `HEAD` is answered from the listing and starts no fetch.
- **A failure before the first byte is still a status.** The response
  begins when there is a byte to send. A fetch that fails before that is
  answered `502` or `500` as described above; so is a file over the size
  limit, and a full queue.
- **A file whose streamed fetch failed is distrusted.** A client keeps
  what it received and may ask for the rest. While the same fetch is in
  flight that is safe, and is served from it. After the fetch has failed
  it is not: the rest, from a later and sound fetch, would complete a file
  whose start was never verified, and `huggingface_hub` does not check
  digests. So the registry remembers the file by its ETag, and from then
  until it restarts (a) fetches, verifies and stores that file before
  sending any of it, so a second failure is a status, and (b) answers any
  request for part of it with the whole file, `200` and not `206`, also
  when it would otherwise redirect to the object store. A client resuming
  a download then replaces what it has; `huggingface_hub` truncates its
  temporary file when a ranged request is answered `200`, and `test/e2e`
  runs that case. The memory is bounded; if it ever fills, every ranged
  request is answered with the whole file.

The response headers are those of a stored file (`ETag`, `Content-Length`,
`X-Repo-Commit`, `Accept-Ranges`, and the rest), because the client
decides what to do from them. With redirected downloads the first request
cannot be redirected, since nothing is in the bucket yet: it is streamed
from the registry with the `X-Linked-*` headers, and later requests are
redirected.

What remains open is in
[operations.md](operations.md#pull-through-caching): a client other than
`huggingface_hub` that mishandles a `200` to a ranged request, and a
registry restarted between the failure and the client's retry, since the
distrust is kept in memory.

`--pull-through-stream=false` (`REGISTRY_PULL_THROUGH_STREAM`) turns all
of this off: a file is then handed to a client only after all of it is
stored and verified, and the first download waits for the whole fetch.

A lookup that hits is never sent upstream, so pull-through cannot change
what a recorded branch points at. `modelctl mirror` on a partly fetched
revision fetches whatever is still pending and completes it.

## Serving downloads

`GET /{repo}/resolve/{revision}/{path}` resolves the revision, looks up the
file's digest, opens the blob and hands it to Go's `http.ServeContent`,
which streams it and implements `Range`, `If-Range` and `If-None-Match`.
The `ETag` is the file's digest and `X-Repo-Commit` is the revision id,
which is what Hugging Face clients use to name files in their cache.

Content is verified when it is stored, not each time it is served: a
download compares the stored size with the recorded one and streams.
Hashing a multi-gigabyte file before every download would make serving
cost what storing does. The check on demand is `blob.Check`, which re-reads
a blob through `blob.Store.Open` and so works the same on every backend;
`Registry.Verify` runs it over every recorded blob, a page of the listing
at a time and holding no lock, and `POST /api/v1/verify` streams the
results.

## Metadata backups

The database is the only record of which stored files make up which
revision, so the registry backs it up itself (`registry/backup.go`). Model
files are not copied: they are already stored under their digests and
never change.

A backup is a snapshot taken with SQLite's `VACUUM INTO` while the
registry keeps serving, which gives a consistent copy from one read
transaction. The snapshot is written to `tmp/`, checked (SQLite's
integrity check, and that it is a registry database of a schema this
version reads), and only then stored through `blob.Archive`, the four
methods every storage backend has for keeping named files beside the
blobs: under `backups/` in the data directory, or under `backups/` in the
bucket. With object storage that puts the backup off the server's disk.
Backups are named by time, with a number when several fall in one second,
and are ordered by name; the name of a new one follows every name stored
for its second and is never one that was used before. The oldest beyond
the number to keep are removed after the new one is stored, never before.

Three things take one: the schedule in `modelctl serve` (a loop that takes
a backup on each tick of a ticker), `POST /api/v1/backups`, and
`GET /api/v1/backups/snapshot`, which streams a fresh snapshot to the
caller instead of storing it.

Restoring is done with the registry stopped (`modelctl restore`), or by
`modelctl serve` itself at start when it is told to and finds no database.
The snapshot is verified again before it replaces anything, a database
that is already there is put aside with its write-ahead log rather than
overwritten, and a failed restore puts it back.

## Background work and shutdown

Two things in the server process are not requests: pull-through fetches,
which outlive the request that started them, and the scheduled-backup
loop. Each has an owner that stops it. `Server.Shutdown` cancels every
fetch and returns when the last has ended, which also ends, by an abort,
any response that was being sent from one; the backup loop runs on a
context that `modelctl serve` cancels. On a signal, `serve` stops taking
requests, cancels both, and waits for requests in flight, the fetches and
a backup in progress, up to thirty seconds, before it closes the
registry. Nothing is started on a context that nobody cancels.

## Metrics

`GET /metrics` reports what the registry is doing in the Prometheus text
format. It is registered through the access gate with the read role, like
browsing.

The format is written by `internal/metrics`, a small package with no
dependency: a metric is declared with its labels and *every value each
label can take*, and a value that was not declared is not counted. That is
what makes two promises structural, not a matter of care at each call
site: nothing from a request (a repository, a path, a token's name, an
address) can become a label value, and the output cannot grow while the
process runs. Counting is one atomic operation on a handle the caller
looked up once; a handle that was never made counts nothing, so code that
counts does not depend on metrics having been set up.

`internal/server/metrics.go` declares every metric and is the only place
that knows their names. There are three ways a value gets there:

- **Counted where it happens.** A handler or the pull-through coordinator
  adds to a counter at the point it already logs: a file stored from an
  upload, a fetch started and how it ended, a response sent from a fetch
  in flight. Bytes are counted once per write of up to 256 KiB, never per
  byte.
- **Counted around every request.** The outermost handler notes each
  request's class of route (decided from the method and the shape of the
  path), its method and the class of its status, and times the routes that
  are not transfers. The writer it hands down passes everything through to
  the server's own, including `ReadFrom`, so a file is still sent the way
  it was; `http.MaxBytesReader` is given the server's own writer
  (`baseWriter`), because it tells the server to close the connection
  through a method no wrapper can forward.
- **Read when asked.** What the registry holds is one query over the
  database (`Registry.Totals`), made at most every fifteen seconds
  however often the endpoint is read. How backups and verify runs have
  gone is kept by the registry itself, in memory (`Registry.Activity`),
  so that scheduled backups, which the server's handlers never see, are
  counted with the ones asked for over the API. The newest stored backup
  is looked up once, so the age of the last backup is right after a
  restart. A scrape never lists or reads stored files.

## Request limits

Model files are large and their transfers long, so a file transfer has no
time limit and, unless the operator sets one, no size limit. Every other
request body is small and is held to that: `limits.go` bounds the size of
JSON bodies and forms and gives the client thirty seconds to send them,
and a request that announces a body to a route that reads none is not
waited on for longer than that. The list of routes that count as file
transfers is one function, `isFileTransfer`. A download carries no body,
so no deadline is set for it, whether it is served from the store or sent
while the file is fetched.

## Deleting and garbage collection

Deleting a repository removes its revisions and then removes the blobs that
no other repository references. `modelctl gc` removes blobs that nothing
references at all, skipping any younger than an hour, since those may
belong to a push that has uploaded files but not yet recorded its revision.
Deleting a repository makes the same allowance more narrowly: it leaves a
blob alone if, within the last hour, a push or mirror stored it again or
was told it is already stored and has not yet recorded a revision naming
it. Such a blob goes at a later `gc` if nothing came to use it. Everything
else the repository alone held is removed at once, which is what makes
"delete and mirror again" a repair for a damaged file.

Publishing a blob and removing it take turns on a lock held per digest, so
a blob cannot be deleted between being written and being recorded, and a
long upload of one file never holds up work on another. Removal re-checks,
in the same statement that deletes the record, that the blob is still
unreferenced and has not been re-sent or asked about since it was picked.
The record goes before the bytes, so that nothing can come to reference
bytes that are about to go. If storage then refuses to remove them, the
record is put back as it was, still under the lock: the blob is again
what it was, stored and unreferenced, and the next garbage collection
tries it again. Only what storage really removed is counted as freed.

## Access control

A token is a random secret shown once; only its SHA-256 is stored. Each
token has one role:

| Role | Allows |
|---|---|
| `read` | Download and browse |
| `write` | The above, plus push, upload and mirror |
| `admin` | The above, plus delete, garbage-collect, verify, back up and manage tokens |

Reading is open to anyone unless the server runs with `--require-auth`.
Writing always needs a token. The web UI signs in with the same tokens,
held in an `HttpOnly`, `SameSite=Strict` cookie, and state-changing
requests from other origins are refused.

The token a request carries is looked up once, before any handler runs.
Every route is registered with the role it needs, through one gate that
compares roles and answers a refusal in the style of the surface it guards
(the registry API's JSON, a Hugging Face client's headers, or the sign-in
page). The few routes that ask for no role say why where they are
registered: the health check, whoami, signing in, and the signed upload
URL described under [Uploads](#uploads).

## Extension points

These are the seams where larger deployments would plug in. None is a
plugin system; each is a small interface or a single function.

- **Storage backend**: `blob.Store`.
- **Authentication**: `Server.identify` maps a request to a token, once,
  before any handler runs. A different identity source replaces that one
  function.
- **Authorization**: every route is registered through `Server.gate` with
  the role it needs, and the gate is the only place a role is compared.
  Finer-grained rules replace that one check.
- **Audit**: there is no audit trail beyond the server's log. The handlers
  that change what the registry records each log one line saying what
  changed: a revision recorded (`api.go`, `hfupload.go`), a repository
  created (`hfupload.go`) or deleted (`api.go`, `ui.go`), a mirror
  completed, garbage collected, a token created or revoked (`api.go`), a
  backup stored or a snapshot downloaded (`backup.go`). Pull-through logs
  each revision it records and each file it fetches (`pullthrough.go`),
  and the scheduled-backup loop logs each backup (`internal/cli/serve.go`).
  Two changes are not logged: storing a file that no revision names yet,
  and creating a branch through the Hugging Face route. An audit log would
  start from these lines.

## What it deliberately is not

No microservices, no message broker, no Kubernetes dependency, no separate
database. One process against one data directory. Running two processes
against the same directory is not supported, and neither is pointing two
registries at one bucket and prefix: each would remove files the other
still refers to.

Metadata is SQLite on purpose. It is small (one row per file per revision)
and SQLite serves it comfortably on one node, with nothing to operate. What
SQLite rules out is several registry instances sharing one database, which
is what high availability needs. All SQL is confined to
`internal/registry`, so a second metadata backend is a contained change
when that is wanted.
