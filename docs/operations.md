# Operations

## Sizing

The registry itself is small: one process, tens of megabytes of memory,
and it streams files rather than loading them. What you need is disk.
Plan for the total size of the models you keep, once each: identical
files are stored a single time however many revisions or repositories
contain them.

Free space equal to the largest single file is needed on the same volume
while that file is being received, because uploads are written to
`data/tmp/` and then moved into place.

## S3 storage

To keep model files in an object store instead of on the server's disk:

```bash
export REGISTRY_STORAGE=s3
export REGISTRY_S3_BUCKET=my-models
export REGISTRY_S3_ENDPOINT=https://s3.us-east-1.amazonaws.com   # or your provider's URL
export REGISTRY_S3_REGION=us-east-1
export REGISTRY_S3_ACCESS_KEY=...
export REGISTRY_S3_SECRET_KEY=...
modelctl serve
```

For Cloudflare R2, the endpoint is `https://<account id>.r2.cloudflarestorage.com` and the region
is `auto`. R2 does not charge for downloads, which makes it a good fit for redirected downloads
(below).

The bucket must exist; the registry does not create it. It checks the
bucket at start and exits with a clear message if it cannot reach it.

Credentials: `REGISTRY_S3_ACCESS_KEY` and `REGISTRY_S3_SECRET_KEY`, else
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (with `AWS_SESSION_TOKEN`
if set), else the role of the instance or pod the registry runs on.

The registry needs these permissions on the bucket, and nothing more:
`s3:ListBucket` on the bucket, and `s3:GetObject`, `s3:PutObject`,
`s3:DeleteObject` and `s3:AbortMultipartUpload` on its objects (or on
`<prefix>/*` if you set `REGISTRY_S3_PREFIX`).

**What still lives on the server.** Two things, both in the data directory:

- `registry.db`, the metadata. It is small, but it is the only record of
  which objects make up which revision. Put the data directory on a
  persistent disk and back it up (see below). A bucket without its
  database is a pile of unnamed files.
- `tmp/`, scratch space. Each upload or mirrored file is received and
  verified locally before it is sent to the bucket, so you need free space
  for the largest file being received, times the number received at once
  (a mirror fetches four files in parallel).

**Redirected downloads.** By default the registry streams files from the
bucket to the client. Set `REGISTRY_S3_REDIRECT=true` to have it answer
with a redirect to a signed bucket URL instead, valid for one hour, so
clients download directly from the object store and the registry carries
no file traffic. Use this when:

- clients can reach the bucket endpoint (it is not on a private network
  they cannot see), and
- you are comfortable that anyone holding a signed URL can download that
  one file until it expires, without a registry token. The registry only
  issues a URL after checking the caller's access.

If the registry reaches the bucket by an internal address that clients
cannot use, leave redirects off.

**Bucket settings worth having.** A lifecycle rule that aborts incomplete
multipart uploads after a day or two cleans up after a crash mid-upload.
Do not add a rule that expires or deletes objects: the registry decides
what is unreferenced, with `modelctl gc`.

**Moving existing data into S3.** There is no migration command yet. To
move, copy `data/blobs/` into the bucket keeping the layout
(`blobs/sha256/ab/abcd...` becomes `<prefix>/sha256/ab/abcd...`), then
restart with S3 storage against the same `registry.db`.

## The first admin token

On its first start against an empty data directory the server creates an
admin token and prints it once, to standard error. With Docker, read it
from `docker logs`.

To choose the token yourself, set `REGISTRY_ADMIN_TOKEN` (at least 16
characters). It is applied at every start, so changing the variable and
restarting rotates it.

If you lose every admin token, start the server once with
`REGISTRY_ADMIN_TOKEN` set.

### On Kubernetes, or anywhere logs are collected

Do not let the registry print a token there: the first-start message would
put an admin credential in your log store. Supply the token and forbid
generating one:

- `REGISTRY_ADMIN_TOKEN_FILE=/path/to/file` reads the token from a file,
  which is how a mounted secret arrives. A trailing newline is ignored.
  `REGISTRY_ADMIN_TOKEN` does the same from an environment variable; set
  one or the other, not both.
- `REGISTRY_REQUIRE_ADMIN_TOKEN=true` makes the server exit with an error
  if it has no tokens and none was supplied, instead of creating one and
  printing it. With it set, a deployment that forgot the secret fails to
  start rather than leaking a credential.

```yaml
env:
  - name: REGISTRY_REQUIRE_ADMIN_TOKEN
    value: "true"
  - name: REGISTRY_ADMIN_TOKEN_FILE
    value: /run/secrets/registry/admin-token
volumeMounts:
  - name: registry-admin
    mountPath: /run/secrets/registry
    readOnly: true
```

The supplied token is applied at every start, so changing the secret and
restarting the pod rotates it. The log records that a token was applied
and where it came from, never its value.

If a registry was ever started on such a platform without these, the
token it printed is in your logs: treat it as exposed, supply a new one
as above and restart, which replaces it.

Create narrower tokens for day-to-day use:

```bash
modelctl token create laptop --role read
modelctl token create ci --role write
```

## Open or private

By default, anyone who can reach the registry can download and browse,
and nobody can write without a token. That is a sensible default on a home
or lab network.

Start with `--require-auth` (or `REGISTRY_REQUIRE_AUTH=true`) to require a
token for downloads and the web UI as well. Do this before putting the
registry anywhere reachable from the internet.

## HTTPS

The registry serves plain HTTP. Tokens travel in request headers, so use
HTTPS for anything beyond a trusted network. Put a reverse proxy in front.

Caddy, which obtains certificates automatically:

```
models.example.com {
    reverse_proxy localhost:8080
}
```

nginx: model files are large, so lift the body limit and turn off
buffering:

```nginx
server {
    listen 443 ssl;
    server_name models.example.com;
    # ssl_certificate ...; ssl_certificate_key ...;

    client_max_body_size 0;
    proxy_request_buffering off;
    proxy_buffering off;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

The proxy must pass `X-Forwarded-Proto` so the sign-in cookie is marked
secure, and must not buffer responses, or `modelctl mirror` shows no
progress until it finishes.

## Mirroring private and gated models

Pass a Hugging Face token that has access to the model:

```bash
modelctl mirror meta-llama/Llama-3.2-1B --upstream-token hf_...
```

`modelctl mirror` also picks the token up from `MODELCTL_UPSTREAM_TOKEN`
or `HF_TOKEN`. The token is sent with that one request and is not stored.
Alternatively set `REGISTRY_UPSTREAM_TOKEN` on the server to use one token
for every mirror.

Once mirrored, the model is governed by your registry's access settings.
Honour the model's licence and access terms.

## Pull-through caching

With `--pull-through` (or `REGISTRY_PULL_THROUGH=true`) the registry fills
itself with what clients actually ask for.

```bash
REGISTRY_PULL_THROUGH=true modelctl serve
export HF_ENDPOINT=http://localhost:8080
hf download hf-internal-testing/tiny-random-bert config.json    # fetches that one file
```

It works at two levels:

1. **The first request for a revision** the registry has never seen makes
   it ask the upstream hub for that revision's file listing: every path,
   size and digest. That is one small request. From then on the registry
   answers listings and file metadata for that revision itself.
2. **The first download of a file** makes it fetch that file's content,
   check it against the digest from the listing, and store it. Later
   downloads of that file never reach the upstream.

So a client that wants one quantization out of a repository of twenty
costs you one file, not twenty. The web UI and `modelctl inspect` show
which files of a revision have been fetched.

What to expect:

- **The first download of a large file waits.** The registry hands a file
  to a client only once all of it is stored and verified, so the first
  download of a 20 GB file takes as long as fetching 20 GB. Clients with
  short timeouts give up and report an error. The fetch carries on
  regardless, and a retry once it finishes is served at once. Files that
  fetch within the client's timeout, which covers most configuration and
  tokenizer files, need no retry.
- **Downloads of the same file share one fetch.** Twenty workers starting
  at once cause one download from upstream, not twenty.
- **It fills gaps; it does not refresh.** A branch the registry already
  holds is served as recorded, even if it has moved upstream. Clients that
  pin a revision id always get exactly that revision. To pick up a newer
  `main`, run `modelctl mirror`.
- **A file that was never downloaded still depends on the upstream.** If
  the upstream is unreachable, files already fetched are served and the
  others fail with an error saying so. To make a whole revision available
  offline, run `modelctl mirror <repo>`: it fetches everything still
  missing and reuses what pull-through already stored.
- **Private and gated models** use the server's `REGISTRY_UPSTREAM_TOKEN`.
  Without one, such a request fails with an error saying so.
- **A model the upstream does not have** is an ordinary "not found". That
  answer is remembered for a minute, so a client retrying a wrong name does
  not become a stream of upstream requests.
- **A partly fetched revision cannot be uploaded onto.** An upload adds to
  the branch's current files, and the registry will not build a revision on
  content it has not verified. Run `modelctl mirror <repo>` first.

**Who can trigger a fetch.** Anyone who is allowed to download. On a
private network that is what you want. Anywhere else, an open registry
with pull-through lets any visitor make it download and store any public
file, which fills your disk and uses your bandwidth. Close that with
either or both of:

- `--require-auth`, so only token holders can read, and therefore fetch;
- `REGISTRY_PULL_THROUGH_ALLOW`, a comma-separated list of patterns over
  the repository id, such as `meta-llama/*,google/*,*/*-GGUF`. Repositories
  that match nothing are served only if already stored.

The server logs a warning at start if pull-through is on with neither.

At most four fetches run at a time; further requests queue.

If you turn pull-through off later, revisions it recorded stay listed.
Their fetched files are served; a request for one that was never fetched
gets an error naming the `modelctl mirror` command that would fetch it.

## Mirroring from somewhere other than Hugging Face

`--upstream` (or `REGISTRY_UPSTREAM`) sets the hub to mirror from. Any
server that implements the same download API works, including another
Model Delivery registry, which is how you copy models between registries.

## Backup and restore

Everything is in the data directory: `registry.db` (plus `registry.db-wal`
and `registry.db-shm` while running) and `blobs/`.

**Simplest:** stop the server, copy the directory, start the server.

**Without stopping:** files under `blobs/` never change once written, so
they can be copied at any time. Copy `blobs/` first and the database
second, so that every file the database mentions is in the copy. Take the
database copy with SQLite's online backup rather than a plain file copy:

```bash
rsync -a data/blobs/ backup/blobs/
sqlite3 data/registry.db ".backup 'backup/registry.db'"
```

To restore, put the directory back and start the server. Do not run
`modelctl gc` or delete repositories while a backup is being taken.

**With S3 storage,** the bucket holds the files and your provider's
durability covers them; what you back up is `registry.db`, with the same
`sqlite3 ... ".backup"` command, on a schedule. Keep the backups somewhere
other than the server. If you restore an older database, revisions
recorded after that backup are gone from the registry even though their
objects are still in the bucket; `modelctl gc` cannot see those objects,
because it only knows about files the database lists.

## Freeing space

```bash
modelctl delete acme/old-model --yes   # removes its files unless another repository uses them
modelctl gc                            # removes files nothing references (failed mirrors, abandoned pushes)
```

`gc` leaves files newer than one hour alone, in case a push is still in
progress.

## Upgrading

Replace the binary or the image and restart. The database schema is
versioned; a newer server upgrades it at start. A server refuses to open a
data directory written by a newer version than itself, so take a backup
before upgrading if you might need to go back.

## Limits

- Run one server per data directory. Two processes against the same
  directory are not supported.
- Keep the data directory on a local filesystem. SQLite over a network
  filesystem (NFS, SMB) is unreliable. This applies with S3 storage too,
  since the database stays in the data directory.
- There is no built-in rate limiting. Use the reverse proxy for that if the
  registry is public.
