# Architecture

The registry is one Go process and one data directory. There is no
database server, queue, or second service to run.

```
              huggingface_hub, transformers, hf CLI        modelctl          browser
                             |                                |                 |
                             v                                v                 v
        +---------------------------------------------------------------------------+
        |  internal/server      one HTTP handler                                    |
        |    hf.go    Hugging Face compatible routes   /api/models/...  /.../resolve |
        |    api.go   registry REST API                /api/v1/...                   |
        |    ui.go    web UI                           /  /ui/...                    |
        +---------------------------------------------------------------------------+
                             |                                |
                             v                                v
        +-------------------------------------+   +------------------------------+
        |  internal/registry                  |   |  internal/mirror             |
        |  repositories, revisions, refs,     |<--|  reads an upstream hub,      |
        |  tokens (SQLite)                    |   |  verifies, records a revision|
        +-------------------------------------+   +------------------------------+
                             |
                             v
        +-------------------------------------+
        |  internal/blob                      |
        |  content-addressed file store       |
        +-------------------------------------+
```

`internal/cli` is the `modelctl` command line and `internal/client` is the
Go client it uses. The server never calls the client; they share only the
HTTP API.

## Data directory

```
data/
  registry.db              SQLite: repositories, revisions, files, refs, tokens
  blobs/sha256/ab/abcd...  one file per unique content, named by its SHA-256
  tmp/                     uploads in progress
```

With S3 storage, `blobs/` is absent: the same `sha256/ab/abcd...` names are
object keys in the bucket. `registry.db` and `tmp/` stay local.

## Content-addressed storage

Every file's bytes are stored once, under the SHA-256 of the content. A
revision does not contain files; it lists `path -> SHA-256`. So:

- Two revisions that share a 5 GB weights file store it once.
- Two repositories that contain the same file store it once.
- Re-mirroring or re-pushing unchanged content transfers nothing.
- A stored file can always be re-checked against its own name.

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

`blob.Store` is a three-method interface (stage, open, delete) with two
implementations, and one test suite that both must pass.

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
and where it came from. It is never edited after it is recorded.

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
`pending_files` to `files` in one transaction. When the last pending file
of a revision arrives, the revision is complete and indistinguishable from
a mirrored one.

A small coordinator keeps one job per thing being fetched, whether a
revision's listing or one file. A request for something already being
fetched waits on the existing job. Jobs run on the server's own context,
not the request's, so a client that disconnects does not stop one. Four
run at a time and a bounded number wait; past that, a request that would
start another is answered `503`.

**Which repositories.** The lookup goes upstream only for a repository the
registry does not hold, or one whose `upstream` field is set, meaning it
was created by mirroring or pull-through. A repository created here by a
push or an upload is never added to on a reader's request, and its
`upstream` field is never set by pull-through. The same check is repeated
inside the transaction that records the revision, against the canonical
name the upstream answered with, so neither a rename upstream nor a push
that lands in between gets around it. For names not held at all, the
operator's allow list is the control.

Two rules follow from "nothing unverified is served":

- A file is handed to a client only after all of it is stored and has
  matched its digest. The first download of a file therefore waits for the
  whole file.
- A revision cannot be built on (by an upload that merges into its branch)
  while it has pending files, because that would record a new revision
  referring to content the registry has never seen.

A lookup that hits is never sent upstream, so pull-through cannot change
what a recorded branch points at. `modelctl mirror` on a partly fetched
revision fetches whatever is still pending and completes it.

## Serving downloads

`GET /{repo}/resolve/{revision}/{path}` resolves the revision, looks up the
file's digest, opens the blob and hands it to Go's `http.ServeContent`,
which streams it and implements `Range`, `If-Range` and `If-None-Match`.
The `ETag` is the file's digest and `X-Repo-Commit` is the revision id,
which is what Hugging Face clients use to name files in their cache.

## Deleting and garbage collection

Deleting a repository removes its revisions and then removes the blobs that
no other repository references. `modelctl gc` removes blobs that nothing
references at all, skipping any younger than an hour, since those may
belong to a push that has uploaded files but not yet recorded its revision.

Publishing a blob and removing it take turns on a lock held per digest, so
a blob cannot be deleted between being written and being recorded, and a
long upload of one file never holds up work on another. Removal re-checks,
in the same statement that deletes the record, that the blob is still
unreferenced and has not been re-sent or asked about since it was picked.

## Access control

A token is a random secret shown once; only its SHA-256 is stored. Each
token has one role:

| Role | Allows |
|---|---|
| `read` | Download and browse |
| `write` | The above, plus push and mirror |
| `admin` | The above, plus delete, garbage-collect and manage tokens |

Reading is open to anyone unless the server runs with `--require-auth`.
Writing always needs a token. The web UI signs in with the same tokens,
held in an `HttpOnly`, `SameSite=Strict` cookie, and state-changing
requests from other origins are refused.

## Extension points

These are the seams where larger deployments would plug in. None is a
plugin system; each is a small interface or a single function.

- **Storage backend**: `blob.Store`.
- **Authentication**: `Server.roleOf` maps a request to a role. A different
  identity source replaces that one function.
- **Authorization**: every route declares the role it needs through
  `Server.need`. Finer-grained rules replace that one check.
- **Audit**: every state change goes through a handful of handlers in
  `api.go` that already log what changed.

## What it deliberately is not

No microservices, no message broker, no Kubernetes dependency, no separate
database. One process against one data directory. Running two processes
against the same directory is not supported.

Metadata is SQLite on purpose. It is small (one row per file per revision)
and SQLite serves it comfortably on one node, with nothing to operate. What
SQLite rules out is several registry instances sharing one database, which
is what high availability needs. All SQL is confined to
`internal/registry`, so a second metadata backend is a contained change
when that is wanted.
