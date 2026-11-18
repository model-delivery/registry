# REST API

The registry's own API lives under `/api/v1`. `modelctl` uses nothing else
for administration. The Hugging Face compatible routes are described in
[compatibility.md](compatibility.md).

## Conventions

- **Authentication**: `Authorization: Bearer <token>`.
- **Roles**: each route needs `read`, `write` or `admin`. `read` routes are
  open to everyone unless the server runs with `--require-auth`.
- **Errors**: a non-2xx status with `{"error": "what went wrong"}`.
  `401` means no valid token was sent; `403` means the token's role is too
  low.
- **Request bodies**: a JSON body may be at most 64 MiB and must arrive
  within 30 seconds, or the request is refused with `400`. File uploads
  have no time limit. If the operator has set `REGISTRY_MAX_BLOB_BYTES`, a
  larger file is refused with `413`.
- **Repository ids** are `namespace/name` and appear in paths as two
  segments.
- **Revision** means a branch name, a tag name, or a 40-character revision
  id.

Examples assume:

```bash
export R=http://localhost:8080
export T=md_your_token
```

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

Returns its refs (`name`, `kind` of `branch` or `tag`, `sha`) and its
revisions, newest first (`sha`, `parent`, `message`, `source` of `push` or
`mirror`, `files`, `bytes`, `created_at`).

### Inspect a revision

`GET /api/v1/repos/{namespace}/{name}/revisions/{revision}` (read)

```bash
curl $R/api/v1/repos/acme/tiny/revisions/main
```

```json
{"repo": "acme/tiny", "sha": "409918bc...", "source": "push", "created_at": "...",
 "files": [{"path": "config.json", "size": 21, "sha256": "...", "git_oid": "...", "lfs": true}]}
```

### Delete a repository

`DELETE /api/v1/repos/{namespace}/{name}` (admin)

Removes every revision and frees the stored files no other repository
uses. Returns `{"blobs": 3, "bytes": 28183891}`: what was freed.

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
is stored. Uploading a file that is already stored succeeds.

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

Returns the revision, including its `sha`. The repository is created if it
does not exist. Recording the same content the branch already has returns
the existing revision.

## Mirroring

`POST /api/v1/mirror` (write)

```bash
curl -N -X POST -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  $R/api/v1/mirror -d '{"repo": "hf-internal-testing/tiny-random-bert", "revision": "main"}'
```

| Field | |
|---|---|
| `repo` | Required. The repository id on the upstream hub. |
| `revision` | Branch, tag or revision id. Default `main`. |
| `upstream_token` | Token for the upstream hub, for private or gated repositories. Used for this request only and not stored. |

If the upstream refuses (unknown repository, missing access) the response
is an ordinary error. Otherwise it is `200` with a stream of JSON lines:

```json
{"event":"start","repo":"hf-internal-testing/tiny-random-bert","sha":"f171d7baecaf37b5da5a3616d8833b9969753535","files":10,"bytes":28183891}
{"event":"file","path":"config.json","status":"downloaded","size":548}
{"event":"file","path":"model.safetensors","status":"reused","size":520212}
{"event":"done","repo":"hf-internal-testing/tiny-random-bert","sha":"f171d7baecaf37b5da5a3616d8833b9969753535","ref":"main","files":10,"bytes":28183891,"downloaded_bytes":27663679,"reused_bytes":520212}
```

(Eight more `file` lines are omitted here. `reused` means the registry
already held those bytes and did not download them.)

A failure after the stream has started arrives as a final
`{"event":"error","error":"..."}` line, and no revision is recorded. Always
check the last line.

The upstream hub itself is fixed by the server's `--upstream` setting.

## Tokens

All `admin`.

| | |
|---|---|
| `GET /api/v1/tokens` | List tokens: `name`, `role`, `created_at`. Secrets are never returned. |
| `POST /api/v1/tokens` | Body `{"name": "ci", "role": "write"}`. Returns the token secret, once. |
| `DELETE /api/v1/tokens/{name}` | Revoke. |

## Backups

All `admin`. See [operations.md](operations.md#backup-and-restore).

| | |
|---|---|
| `GET /api/v1/backups` | List stored backups, oldest first: `name`, `size`, `created_at`. |
| `POST /api/v1/backups` | Take a backup and store it next to the model files. Returns `backup` and `contents` (`schema_version`, `repos`, `revisions`, `size`). |
| `GET /api/v1/backup/snapshot` | Download a fresh, verified snapshot of the metadata database (`application/vnd.sqlite3`). |

## Maintenance

| | |
|---|---|
| `GET /api/v1/stats` (read) | `repos`, `revisions`, `blobs`, `stored_bytes` (on disk), `logical_bytes` (sum over every file of every revision). |
| `POST /api/v1/gc` (admin) | Remove stored files no revision references and that are more than an hour old. Returns what was freed. |
| `GET /healthz` | `{"status": "ok", "version": "..."}`. No authentication. |
