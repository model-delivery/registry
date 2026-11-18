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

`blob.Store` is a four-method interface. The filesystem implementation is
the only one today. An object-store backend implements the same interface.

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
2. For each file, skip the download if a blob with that digest is already
   stored. Otherwise download it and verify it against the digest. A
   dropped connection is resumed with a range request, up to five times
   per file.
3. Once every file is stored and verified, record the revision under its
   upstream id, with the upstream's model description kept as provenance.

If any file fails, nothing is recorded. The repository does not appear, or
keeps pointing at its previous revision. Blobs downloaded before the
failure stay, so a retry does not fetch them again; `modelctl gc` removes
them if the mirror is abandoned.

The upstream address is set by the operator when the server starts. It is
never taken from a request, so an API caller cannot make the server fetch
from an arbitrary host.

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

Blob removal and blob publication take turns on one lock, so a blob cannot
be deleted between being written and being recorded.

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
