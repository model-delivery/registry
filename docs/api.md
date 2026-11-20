# REST API

The registry's own API lives under `/api/v1`. `modelctl` uses it for
everything except the content of files it pulls, which it downloads from
the same `resolve` route Hugging Face clients use. That route and the
other Hugging Face compatible ones are described in
[compatibility.md](compatibility.md).

## Conventions

- **Authentication**: `Authorization: Bearer <token>`.
- **Roles**: each route needs `read`, `write` or `admin`. `read` routes are
  open to everyone unless the server runs with `--require-auth`.
- **Errors**: a non-2xx status with `{"error": "what went wrong"}`.
  `401` means no valid token was sent; `403` means the token's role is too
  low. The other statuses are listed under [Statuses](#statuses).
- **Request bodies**: a JSON body may be at most 64 MiB and must arrive
  within 30 seconds, or the request is refused with `400`. File uploads
  have no time limit. If the operator has set `REGISTRY_MAX_BLOB_BYTES`, a
  larger file is refused with `413`.
- **Unknown fields are refused.** A JSON body with a field these routes do
  not define is answered `400`, naming the field, and nothing is done. A
  misspelt `branch` is an error, not a commit to `main`. (The Hugging Face
  compatible routes ignore fields they do not use, as the Hub does.)
- **Warnings**: an operation that did what was asked and then could not
  finish tidying up answers with its usual success status and body, plus a
  `warning` string. The field is absent otherwise. Its reason is in the
  server's log.
- **Repository ids** are `namespace/name` and appear in paths as two
  segments.
- **Models and datasets.** A repository is a model or a dataset, and a
  model and a dataset of the same name are two repositories. Models are
  under `/api/v1/repos`, as they have always been; datasets have the same
  routes under `/api/v1/datasets` (see [Datasets](#datasets)). A document
  that describes a dataset says so with `"type": "dataset"` (a repository)
  or `"repo_type": "dataset"` (a revision or a mirror's progress); for a
  model the field is absent, so model documents are unchanged.
- **Revision** means a branch name, a tag name, or a 40-character revision
  id.

Examples assume:

```bash
export R=http://localhost:8080
export T=md_your_token
```

### Statuses

| Status | Meaning |
|---|---|
| `400` | The request is malformed: invalid JSON, an unknown field, a name or path that is not allowed, a file that was never uploaded, or an upload that does not hash to its digest. |
| `401`, `403` | No valid token, or a token whose role is too low. |
| `404` | No such repository, revision, file, token or backup, or a repository that is not followed. Looking up a revision also sets the `X-Error-Code` header to `RepoNotFound` or `RevisionNotFound`. |
| `409` | The request conflicts with what is recorded: a tag that already names another revision, a branch name that is a tag, a token name that is taken, a commit onto a branch whose revision still has files that were never fetched (see `pending` below), or following a repository that was not created from an upstream. Nothing is changed. |
| `413` | A file larger than `REGISTRY_MAX_BLOB_BYTES`. |
| `500` | The registry itself failed, for example its disk or database. The body says only `internal error`; the server's log has the reason. |
| `502` | The upstream hub is at fault: it cannot be reached, it answered with an error or with something that is not a hub's answer, it denies access, or it redirects somewhere the registry does not follow. Mirroring answers this, and so does looking up a revision with pull-through on. The body says which. |
| `503` | The registry is fetching as much from the upstream as it will at once, or is shutting down. `Retry-After` says when to try again. |

Two of these changed in v0.3.1. A failure of the registry's own while it
fetches from the upstream is a `500`; before, it was reported as `502`. A
mirror request whose upstream cannot be reached, or answers with a server
error, is a `502` with the reason; before, it was `500` with
`internal error`.

## Repositories

### List repositories

`GET /api/v1/repos` (read)

```bash
curl $R/api/v1/repos
```

```json
{"repos": [{"name": "acme/tiny", "upstream": "https://huggingface.co",
            "created_at": "...", "revisions": 2, "stored_bytes": 28183891, "updated_at": "..."}]}
```

`upstream` is present for mirrored repositories. `stored_bytes` counts each
distinct file once.

### Inspect a repository

`GET /api/v1/repos/{namespace}/{name}` (read)

Returns the repository's `name`, `upstream` (if it was mirrored) and
`created_at`, with its `refs` (`name`, `kind` of `branch` or `tag`, `sha`,
`updated_at`) and its `revisions`, newest first (`repo`, `sha`, `parent`,
`message`, `source` of `push` or `mirror`, `files`, `bytes`, `created_at`;
`parent` and `message` are absent when empty).

### Inspect a revision

`GET /api/v1/repos/{namespace}/{name}/revisions/{revision}` (read)

```bash
curl $R/api/v1/repos/acme/tiny/revisions/main
```

```json
{"repo": "acme/tiny", "sha": "409918bc...", "source": "push", "created_at": "...",
 "files": [{"path": "config.json", "size": 21, "sha256": "...", "git_oid": "...", "lfs": true}]}
```

Two things about `files` are easy to trip over:

- Here it is the list of files. In the revisions of
  [Inspect a repository](#inspect-a-repository), and in the last line of a
  mirror, `files` is a number: how many there are.
- A file can carry `"pending": true`. That is a file of a revision that
  pull-through has listed from the upstream but whose content the registry
  has not fetched yet. Its `size` is known, and so is its `sha256` if the
  upstream tracks it with LFS; otherwise `sha256` is empty until the file
  is fetched. The field is absent for a stored file. A pending file is
  fetched the first time it is downloaded, or all at once by
  `modelctl mirror`.

With pull-through on, a revision the registry does not hold is looked up
upstream, so this route can also answer `502` or `503` (see
[Statuses](#statuses)).

### Delete a repository

`DELETE /api/v1/repos/{namespace}/{name}` (admin)

Removes every revision and frees the stored files no other repository
uses. Returns `{"blobs": 3, "bytes": 28183891}`: what was freed.

If the repository was deleted and some of its files could not then be
removed from storage, the answer is still `200`, with a `warning` beside
the counts. The repository is gone either way; the files left behind take
up space, are referenced by nothing, and are not in the counts. The
registry keeps its record of them, so a later garbage collection removes
them once storage allows it.

## Datasets

Every route above has a dataset twin under `/api/v1/datasets`, which
takes the same requests and answers the same documents for a dataset:

| | |
|---|---|
| `GET /api/v1/datasets` (read) | List datasets, as [List repositories](#list-repositories) lists models. Each has `"type": "dataset"`. |
| `GET /api/v1/datasets/{namespace}/{name}` (read) | Inspect a dataset. |
| `GET /api/v1/datasets/{namespace}/{name}/revisions/{revision}` (read) | Inspect a revision of a dataset; `repo_type` is `dataset`. |
| `POST /api/v1/datasets/{namespace}/{name}/commits` (write) | Record a revision of a dataset, from files uploaded as under [Pushing](#pushing). Creates the dataset if it does not exist. |
| `DELETE /api/v1/datasets/{namespace}/{name}` (admin) | Delete a dataset and free the stored files nothing else uses. |

```bash
curl $R/api/v1/datasets/acme/reviews/revisions/main
```

```json
{"repo": "acme/reviews", "repo_type": "dataset", "sha": "5a1f...", "source": "mirror", "created_at": "...",
 "files": [{"path": "data/train.csv", "size": 4120, "sha256": "...", "git_oid": "...", "lfs": false}]}
```

Stored files are shared: a file a model and a dataset both contain is
stored once, and deleting either leaves it for the other. A dataset and a
model of the same name never see each other's revisions, refs or files.

A dataset is mirrored through `POST /api/v1/mirror` with `"type":
"dataset"` (see [Mirroring](#mirroring)).

`/api/v1/repos` lists and serves models only. A registry older than this
one answers `404` to everything under `/api/v1/datasets`, so a dataset
request never reaches a model of the same name by mistake.

## Pushing

A push is two steps: upload the files, then record a revision that names
them. Because files are addressed by SHA-256, a file the registry already
has does not need to be uploaded.

### Check whether a file is stored

`HEAD /api/v1/blobs/{sha256}` (write) returns `200` or `404`.

### Upload a file

`PUT /api/v1/blobs/{sha256}` (write)

The body is the raw bytes. The upload is accepted only if the body hashes
to the digest in the URL; otherwise it is rejected with `400` and nothing
is stored. Uploading a file that is already stored succeeds. The answer is
`201` with `{"sha256": "...", "size": 1234}`.

```bash
SHA=$(sha256sum model.bin | cut -d' ' -f1)
curl -X PUT -H "Authorization: Bearer $T" --data-binary @model.bin $R/api/v1/blobs/$SHA
```

### Record a revision

`POST /api/v1/repos/{namespace}/{name}/commits` (write)

```bash
curl -X POST -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  $R/api/v1/repos/acme/tiny/commits \
  -d '{"branch": "main", "tags": ["v1.0"], "message": "first release",
       "files": [{"path": "model.bin", "sha256": "'$SHA'"}]}'
```

| Field | |
|---|---|
| `files` | Required. `path` and `sha256` for each file. Every file must already be uploaded. A path uses `/` between segments, is at most 1024 bytes, and may not contain a backslash, a control character, or an empty, `.` or `..` segment. |
| `branch` | Branch to move to the new revision. Default `main`. |
| `tags` | Tags to create at the new revision. A tag that already exists at a different revision is refused with `409`. |
| `message` | Free text. |
| `merge` | `false` (default): `files` is the complete revision. `true`: keep the branch's current files and add or replace these. |

Returns the revision, including its `sha`, with status `201`. The
repository is created if it does not exist. Recording the same content the
branch already has returns the existing revision, also with `201`, although
nothing new was recorded: compare `sha` with the branch's previous one to
tell.

## Mirroring

`POST /api/v1/mirror` (write)

```bash
curl -N -X POST -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  $R/api/v1/mirror -d '{"repo": "hf-internal-testing/tiny-random-bert", "revision": "main"}'
```

| Field | |
|---|---|
| `repo` | Required. The repository id on the upstream hub. |
| `type` | `dataset` to mirror a dataset; `model` (the default) for a model. Anything else is refused with `400`. |
| `revision` | Branch, tag or revision id. Default `main`. |
| `all_refs` | `true` to mirror every branch and tag instead of one revision; see [below](#every-branch-and-tag). |
| `upstream_token` | Token for the upstream hub, for private or gated repositories. Used for this request only and not stored. |

If the upstream cannot provide the revision, the response is an ordinary
error: `404` for a repository or revision it does not have, `502` when it
denies access, cannot be reached or answers with an error. Otherwise it is
`200` with a stream of JSON lines:

```json
{"event":"start","repo":"hf-internal-testing/tiny-random-bert","sha":"f171d7baecaf37b5da5a3616d8833b9969753535","files":10,"bytes":28183891}
{"event":"file","path":"config.json","status":"downloaded","size":548}
{"event":"file","path":"model.safetensors","status":"reused","size":520212}
{"event":"done","repo":"hf-internal-testing/tiny-random-bert","sha":"f171d7baecaf37b5da5a3616d8833b9969753535","ref":"main","files":10,"bytes":28183891,"downloaded_bytes":27663679,"reused_bytes":520212}
```

(Eight more `file` lines are omitted here. `reused` means the registry
already held those bytes and did not download them.) For a dataset the
`start` and `done` lines also carry `"repo_type": "dataset"`.

A failure after the stream has started arrives as a final
`{"event":"error","error":"..."}` line, and no revision is recorded. Always
check the last line. The line gives the reason when the upstream caused the
failure (a file that did not match its digest, a connection that kept
dropping). When the registry itself could not store the revision, it says
so and no more; the reason is in the server's log.

The upstream hub itself is fixed by the server's `--upstream` setting.

### Every branch and tag

With `"all_refs": true` (and no `revision`), the request mirrors every
branch and every tag the upstream lists for the repository, model or
dataset. A revision that several refs name is fetched once, and files
already stored are not fetched again. Each upstream branch is recorded as
a branch and each tag as a tag.

The upstream's list of refs is asked for before the answer starts, so a
repository it does not have is a `404` and a refusal a `502`, as above.
The stream then has the `start` and `file` lines of each revision fetched,
one `ref` line per ref, and a last `done` line that repeats every ref's
result in `refs`:

```json
{"event":"ref","ref":"main","kind":"branch","outcome":"mirrored","sha":"f171d7ba..."}
{"event":"ref","ref":"v1.0","kind":"tag","outcome":"tag_moved","sha":"9a0e33c1...","previous":"5b2c8e10...","detail":"tag \"v1.0\" of acme/tiny has moved upstream, from 5b2c8e10... to 9a0e33c1...; the registry's tag stays at 5b2c8e10..., because a tag never moves here"}
{"event":"done","repo":"acme/tiny","bytes":28183891,"downloaded_bytes":27663679,"reused_bytes":520212,"refs":[...]}
```

| `outcome` | |
|---|---|
| `mirrored` | The ref is new, or a branch moved, or its revision had files that were not stored; the revision is now stored in full and the ref points at it. |
| `unchanged` | The registry already had the ref at that revision, with every file stored. |
| `tag_moved` | The upstream's tag names another revision than the registry's. Tags never move here, so the registry's tag stays; `previous` is where it is, `sha` where the upstream's is. |
| `conflict` | The name is a branch on one side and a tag on the other, or the upstream has both a branch and a tag of that name (the branch is mirrored). The registry's ref is left as it is. |
| `deleted_upstream` | The registry has the ref and the upstream no longer does. It stays. |
| `failed` | The ref could not be mirrored, and nothing of it was recorded; `detail` says why, by the same rule as an `error` line. The other refs are not affected. |

A `done` line means the run finished; look at each ref's `outcome` for
how it went. An `error` line is sent only when the run could not go
through the refs at all.

A revision mirrored by name (without `all_refs`) is likewise recorded as
the kind of ref it is upstream: mirroring a tag creates a tag. Up to
v0.4.1 it became a branch.

## Keeping mirrors up to date

A repository created by mirroring or pull-through can be *followed*: the
registry checks what its followed refs point at upstream, on the schedule
set by `REGISTRY_SYNC_INTERVAL` or when asked, and mirrors whatever has
moved. See [operations.md](operations.md#keeping-mirrors-up-to-date).
Every route here has a dataset twin under `/api/v1/datasets`.

| | |
|---|---|
| `POST /api/v1/repos/{namespace}/{name}/follow` (write) | Follow a repository, or change what it follows. Body `{"branch": "main"}` or `{"all_refs": true}`; `{}` follows the repository's only branch, or `main`. Answers `200` with the entry (below). A repository created by a push or an upload is refused with `409`, and so is a branch name that is a tag here. |
| `DELETE /api/v1/repos/{namespace}/{name}/follow` (write) | Stop following. Nothing the repository holds changes. Answers `204`, or `404` if it was not followed. |
| `GET /api/v1/follows` (read) | The follow list, models and datasets: `{"follows": [...], "interval_seconds": 3600}` (`0` when there is no schedule). |
| `POST /api/v1/repos/{namespace}/{name}/sync` (write) | Check one followed repository now and answer when the check is over. `404` if it is not followed. |
| `POST /api/v1/sync` (write) | Start a check of every followed repository and answer at once, `202` with `{"queued": ["acme/tiny", "datasets/acme/reviews"]}`. The checks take their turns, one at a time. |
| `POST /api/v1/datasets/{namespace}/{name}/follow` (write) | Follow a dataset. |
| `DELETE /api/v1/datasets/{namespace}/{name}/follow` (write) | Stop following a dataset. |
| `POST /api/v1/datasets/{namespace}/{name}/sync` (write) | Check one followed dataset now. |

An entry of the follow list:

```json
{"name": "acme/tiny", "branch": "main", "created_at": "...",
 "last_check": "...", "last_success": "...", "last_change": "...",
 "last_error": "upstream returned 503", "last_error_at": "...", "failures": 1, "next_check": "..."}
```

`branch` is absent and `all_refs` is `true` for a repository that follows
every branch and tag; `type` is `dataset` for a dataset. `last_check` is
when the upstream was last asked, whatever came of it; `last_success`
when that last went well; `last_change` when a check last added or moved
a ref. `last_error`, `last_error_at` and `failures` (checks in a row that
failed) are cleared by a check that succeeds. `next_check` is when the
schedule will next check it, and is absent with no schedule. A time is
absent until it has happened.

The answer to a sync of one repository:

```json
{"name": "acme/tiny", "checked_at": "...", "changed": true, "downloaded_bytes": 520212,
 "refs": [{"name": "main", "kind": "branch", "outcome": "mirrored", "sha": "9a0e...", "previous": "f171..."}]}
```

`refs` is as for a mirror of every ref, for the refs followed. A check
that failed answers `502` (the upstream's doing: unreachable, an error
answer, content that did not match its digest, a followed branch the
upstream no longer has) or `500` (the registry's own), with the same
document and the reason in `error`; it is also recorded in the entry's
`last_error`. A tag that moved upstream, or a ref deleted upstream, is not
a failure: it is reported in `refs` and left as it is.

## Tokens

All `admin`.

| | |
|---|---|
| `GET /api/v1/tokens` | List tokens: `name`, `role`, `created_at`. Secrets are never returned. |
| `POST /api/v1/tokens` | Body `{"name": "ci", "role": "write"}`. Answers `201` with `name`, `role` and `token`: the secret, which is shown this once and not kept. |
| `DELETE /api/v1/tokens/{name}` | Revoke. Answers `204` with no body. |

## Backups

All `admin`. See [operations.md](operations.md#backup-and-restore).

| | |
|---|---|
| `GET /api/v1/backups` | List stored backups, oldest first: `name`, `size`, `created_at`. |
| `POST /api/v1/backups` | Take a backup and store it next to the model files. Returns `backup` and `contents` (`schema_version`, `repos`, `revisions`, `size`), and a `warning` string when the backup was stored but old backups beyond the number to keep could not be removed. |
| `GET /api/v1/backups/snapshot` | Download a fresh, verified snapshot of the metadata database (`application/vnd.sqlite3`). |

The snapshot's address was `GET /api/v1/backup/snapshot` in v0.3.0. That
address still works and answers the same; new clients should use
`/api/v1/backups/snapshot`, beside the other two.

## Maintenance

| | |
|---|---|
| `GET /api/v1/stats` (read) | `repos`, `revisions`, `blobs`, `stored_bytes` (on disk), `logical_bytes` (sum over every file of every revision). |
| `POST /api/v1/gc` (admin) | Remove stored files no revision references and that are more than an hour old. Returns what was freed, as `{"blobs": 3, "bytes": 28183891}`, with a `warning` string when some files could not be removed from storage. Those are not counted, and the next run tries them again. |
| `POST /api/v1/verify` (admin) | Re-read every stored file and check it against its digest. See below. |
| `GET /healthz` | `{"status": "ok", "version": "..."}`. No authentication. |
| `GET /metrics` (read) | What the registry is doing, in the Prometheus text format (`text/plain; version=0.0.4`), not JSON. Like every `read` route it is open unless the server runs with `--require-auth`; a scraper then sends a token as a bearer token. [operations.md](operations.md#monitoring) lists the metrics. |

### Check stored files

`POST /api/v1/verify` (admin)

```bash
curl -N -X POST -H "Authorization: Bearer $T" $R/api/v1/verify
```

The response is `200` with a stream of JSON lines, one per stored file as
it is checked, so it works on a store of any size:

```json
{"event":"start","blobs":3,"bytes":28183891}
{"event":"blob","sha256":"1f0c...","size":548,"status":"ok"}
{"event":"blob","sha256":"9f2c...","size":27663679,"status":"damaged","detail":"blob verification failed: its content now has sha256 77ab...","repos":["acme/tiny"]}
{"event":"blob","sha256":"c41d...","size":519664,"status":"missing","detail":"storage holds nothing under this name","repos":["acme/tiny"]}
{"event":"done","blobs":3,"bytes":28183891,"failed":2}
```

`status` is `ok`, `missing`, `damaged` (wrong size, or content that no
longer hashes to the digest) or `unreadable` (the file could not be read).
For anything but `ok`, `detail` says what was found and `repos` names up
to ten repositories with a revision that uses the file. `failed` counts
the files that were not `ok`.

If the run cannot finish, the last line is `{"event":"error","error":"..."}`
instead of `done`. Always check the last line. Closing the connection
stops the run. Nothing is changed by it, and the registry serves as usual
while it runs; [operations.md](operations.md#checking-stored-files) covers
cost and repair.
