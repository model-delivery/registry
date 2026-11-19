# Operations

## Settings

`modelctl serve` takes each setting as a flag or as an environment
variable, and `modelctl serve --help` lists them with their defaults. The
[README](../README.md#configuration) has the table. Three things hold for
all of them:

- **A flag wins over its variable.** This document mostly shows the
  variables, since that is how a container or a service unit is configured;
  the flag of the same name does the same (`REGISTRY_BACKUP_INTERVAL` is
  `--backup-interval`, `REGISTRY_S3_BUCKET` is `--s3-bucket`).
- **Secrets have no flag.** The admin token, the upstream token and the S3
  keys are read from the environment only, because a command line shows in
  the process list and in shell history.
- **A value the server cannot read stops it.** It exits with an error that
  names the setting, instead of starting with something other than what
  you wrote. That includes the on/off settings: `true`, `false`, `1`, `0`,
  `yes`, `no`, `on` and `off` are accepted, in any case, and anything else
  is an error. Before v0.3.1 any other value, `yes` and `on` among them,
  was read as off without a word: `REGISTRY_REQUIRE_AUTH=yes` started a
  registry open to everyone. If you upgrade and the server now refuses to
  start over one of these, the setting was not in effect before.

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

Everything here but the two keys can also be given as a flag:
`--storage s3 --s3-bucket my-models --s3-endpoint ... --s3-region us-east-1`.

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
bucket to the client. Set `REGISTRY_S3_REDIRECT=true` (or pass
`--s3-redirect`) to have it answer
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

To choose the token yourself, set `REGISTRY_ADMIN_TOKEN`. It is applied at
every start, so changing the variable and restarting rotates it. Generate
it; do not type one:

```bash
openssl rand -hex 24
```

The registry stores a SHA-256 of each token, not the token, and that hash
is in the database and in every backup. For the tokens the registry
generates, which are 192 random bits, the hash gives nothing away. A token
you choose is only as strong as it is hard to guess: someone who can read a
backup or the bucket can try candidates against the hash at their leisure.
The server accepts 16 characters or more, and logs a warning at every
start while the supplied token is shorter than 32.

If you lose every admin token, start the server once with
`REGISTRY_ADMIN_TOKEN` set.

### Where tokens end up

- **In the database**, as hashes. The registry keeps `registry.db`, its
  write-ahead log, snapshots and restored databases readable by the
  account it runs as and no other (mode `0600`). Stored blobs and backups
  have always been written that way.
- **On the command line, if you put them there.** `--token` works, but
  command-line arguments are visible to every account on the machine
  through the process list, and are kept in shell history. Use the
  `MODELCTL_TOKEN` environment variable instead; the same goes for
  `--upstream-token` and `MODELCTL_UPSTREAM_TOKEN`.
- **In the browser, after signing in to the web UI.** The sign-in cookie
  (`md_token`) holds the access token itself and lasts 30 days. Scripts
  cannot read it (`HttpOnly`) and it is not sent on requests from other
  sites (`SameSite=Strict`). It is marked `Secure`, so that it is never
  sent over plain HTTP, when the sign-in arrived over HTTPS; see
  [HTTPS](#https). Signing out removes the cookie but does not revoke the
  token: to end a browser session for good, revoke the token with
  `modelctl token revoke`. Sign in with a token made for the purpose, not
  with the admin token, on a machine you share.
- **Never in the log**, apart from the one generated at first start, which
  is printed once so that you can read it.

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

- **The first download of a file takes as long as fetching it, and starts
  at once.** The registry sends a file to the client while it is still
  arriving from the upstream, so the first download of a 20 GB file takes
  about as long as fetching 20 GB and the client sees bytes from the
  start. See [the first download](#the-first-download-of-a-file) below
  for what that changes and how to turn it off.
- **Downloads of the same file share one fetch.** Twenty workers starting
  at once cause one download from upstream, not twenty, and each is sent
  the file from that one fetch as it arrives.
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

### The first download of a file

A file the registry already stores was verified before it was stored, and
is served as it is. The first download of a file is different, because
the file is not there yet.

**What the first client gets.** The registry starts fetching the file and
passes it on as it arrives. The client receives the first bytes as soon as
the upstream sends them, and the download finishes when the fetch does.
Clients that arrive while the fetch is running get the file from the same
fetch, from its beginning. Clients that arrive after it are served from
storage.

**What is guaranteed, in plain words.** For every other download, no byte
leaves the registry that has not been checked against its digest. For
this first one, bytes are sent before the check, because the check needs
the whole file. What still holds:

- The last mebibyte of the file is not sent until the whole file has
  arrived and matched its digest and size. A client can therefore never
  receive a complete file that failed the check.
- If the check fails, the registry cuts the connection. The client sees a
  download that ended short of the length it was told, which every HTTP
  client reports as an error. The server logs `aborted a download` with
  the reason. The same happens if the upstream fails part-way, or the
  registry is stopped.
- Nothing that failed the check is stored, recorded or served again.
- A client that was cut off holds the start of a file that was never
  verified, and will usually ask for the rest. The registry does not send
  it the rest: until it restarts, it answers a request for part of that
  file with the whole file, and it fetches and verifies the file
  completely before sending any of it. `huggingface_hub` then discards
  what it had and takes the whole file; while the upstream's content is
  still wrong it gets an error (`502`) and keeps nothing.
- Files of a mebibyte or less (configuration, tokenizer files) are never
  sent before they are verified.

**What is not guaranteed.** Two cases remain, both needing the upstream to
send bytes that do not match its own listing:

- *A client that is not `huggingface_hub`.* The rule above relies on the
  client replacing its partial file when a request for a range is
  answered with the whole file (`200`). `huggingface_hub` does, and
  `test/e2e` runs it; so does `modelctl pull`, which drops its `.part`
  file and takes the whole answer (before v0.4.0 it stopped with "registry
  did not honour the resume request" until the `.part` file was deleted
  by hand). A download tool that appends the answer to what it
  has regardless would end up with a damaged file. A tool that ignores
  the announced length and keeps a cut-off download as if it were whole
  has a file missing its last mebibyte, of unverified content.
- *A restart in between.* Which files are distrusted is kept in memory.
  If the registry restarts after cutting a download and before the client
  asks for the rest, and the upstream then sends correct content, the
  client is sent the rest and ends with a file whose start is unverified.
  `huggingface_hub` asks about a second after being cut off and fails if
  the registry is down then, so the registry would have to be back within
  that second.

If either matters to you more than the first download's speed, turn it
off.

**A slow or stalled upstream.** The registry passes on what it has and
sends nothing else, so a client waits exactly as long as the upstream
makes the registry wait. If the upstream sends nothing for longer than the
client's timeout (ten seconds for `huggingface_hub`), the client gives up
on that request; the fetch carries on. The same applies at the end of
the file: the last mebibyte is sent once the file is complete and checked,
so an upstream slower than about 100 kB/s keeps the client waiting more
than ten seconds for it. A client that then asks for the rest is served
it, from the same fetch or from storage.

**Turning it off.** `--pull-through-stream=false` (or
`REGISTRY_PULL_THROUGH_STREAM=false`) restores the strict behaviour: the
registry hands a file to a client only once all of it is stored and
verified, without exception. The first download of a large file then
sends nothing until the fetch is complete, clients with short timeouts
give up and report an error (`huggingface_hub` raises a read timeout),
the fetch carries on regardless, and a retry once it has finished is
served at once.

**Your own repositories are never filled from upstream.** Pull-through
looks upstream for two kinds of repository only: one the registry does not
hold at all, and one that itself came from the upstream (by `modelctl
mirror` or by pull-through). A repository created here, by `modelctl
push`, `hf upload` or `create_repo`, is served as it is: a branch or
revision id it does not have is "not found", whatever a repository of the
same name on the hub contains, and the web UI keeps showing it as pushed
here. To bring upstream revisions into such a repository on purpose, run
`modelctl mirror`, which needs a token with the write role.

That rule protects names the registry already holds. For a name it does
not hold yet, **the allow list is the control**: without one, the first
client to ask for `my-team/model` before you have pushed it gets whatever
the hub has under that name, stored under your internal name. If you push
your own models to a registry that also has pull-through on, set
`REGISTRY_PULL_THROUGH_ALLOW` to the upstream namespaces you use and leave
your own namespaces out of it. The list is checked against the name in the
request and against the name the upstream reports after a rename.

**Who can trigger a fetch.** Anyone who is allowed to download. On a
private network that is what you want. Anywhere else, an open registry
with pull-through lets any visitor make it download and store any public
file, which fills your disk and uses your bandwidth. Each revision id a
visitor asks about is also recorded with its full file listing, so the
metadata database grows with every upstream commit that is asked for,
whether or not a file is ever downloaded. Close that with either or both
of:

- `--require-auth`, so only token holders can read, and therefore fetch;
- `--pull-through-allow` (or `REGISTRY_PULL_THROUGH_ALLOW`), a
  comma-separated list of patterns over the repository id, such as
  `meta-llama/*,google/*,*/*-GGUF`. Repositories that match nothing are
  served only if already stored.

The server logs a warning at start if pull-through is on with neither.

An allow list limits which repositories can be fetched, not how much of
them. Two further bounds:

- `REGISTRY_PULL_THROUGH_MAX_FILE_BYTES` (or
  `--pull-through-max-file-bytes`) is the largest file fetched on demand,
  in bytes; unset or `0` means no limit. A download of a larger
  file is refused with an error that names this setting and the
  `modelctl mirror` command that fetches the file deliberately. Listings
  and file metadata for such a file still answer.
- At most four fetches run at a time and at most 64 more wait their turn.
  A fetch carries on after the client that started it has gone, so this
  queue is what stops a client from stacking up fetches by asking and
  hanging up. When it is full, a request that would start another fetch
  is answered `503` with `Retry-After`, saying the registry is busy
  fetching; requests for files already stored, or already being fetched,
  are not affected.

There is no total quota: watch free disk space, and see
[Limits](#limits).

If you turn pull-through off later, revisions it recorded stay listed.
Their fetched files are served; a request for one that was never fetched
gets an error naming the `modelctl mirror` command that would fetch it.

## Mirroring from somewhere other than Hugging Face

`--upstream` (or `REGISTRY_UPSTREAM`) sets the hub to mirror from. Any
server that implements the same download API works, including another
Model Delivery registry, which is how you copy models between registries.

The upstream may be on a private network or on the same machine. What it
may not do is redirect the registry to a *different* host on a loopback,
private or link-local address: such a redirect is refused, so that a hub
which is compromised or impersonated cannot make the registry fetch from
services only your network can reach. Redirects to public hosts, which is
how hubs hand out file content, are followed, without the upstream token.

One setup runs into this: mirroring from another registry that has
`REGISTRY_S3_REDIRECT=true` and a bucket on a private address. Its
downloads are redirects to that bucket, and the mirror fails with "the
upstream pointed at an address the registry does not follow". Turn
redirects off on the source registry while you copy from it, so that it
streams the files itself.

Use an `https://` upstream unless the network between the two is yours.

## Backup and restore

There are two things to protect:

- **Model files** (`blobs/`, or the bucket). They are named by their
  SHA-256 and never change once written.
- **The metadata database** (`registry.db`). It is small, and it is the
  only record of which files make up which revision, plus the tokens.
  Without it the files are a pile of digests.

The registry backs up the database itself. It does not copy model files.

### Scheduled backups

```bash
REGISTRY_BACKUP_INTERVAL=1h   # off unless set; at least 1m (--backup-interval)
REGISTRY_BACKUP_KEEP=48       # how many to keep (default 48; 0 keeps all) (--backup-keep)
```

Every interval the server takes a consistent snapshot while it keeps
serving, runs SQLite's integrity check on it, and stores it next to the
model files:

- with S3 storage, in the same bucket under `backups/` (inside
  `REGISTRY_S3_PREFIX` if one is set), which puts it off the server;
- with filesystem storage, in `backups/` in the data directory. That
  protects against a damaged database, not a lost disk: copy the directory
  elsewhere, or use `modelctl backup download` from another machine.

Backups are named by time, `registry-20261118T140000Z.db`, and the oldest
beyond the keep count are removed, so that exactly that many are left. A
second backup taken in the same second is named
`registry-20261118T140000Z_2.db`; it does not replace the first, and a
name is not used again once its backup has been removed. From the tenth
of one second on, the number is preceded by a letter that says how many
digits it has (`_9`, `_a10`, ... `_a99`, `_b100`), which keeps the names
in the order the backups were taken wherever they are sorted as text.
Versions before v0.4.0 wrote `_10` there, and a later backup in a second
whose first had been removed took the plain name again, was listed as the
oldest, and left one backup more than the keep count. Backups with the
older names are listed, restored and removed like any other, in the order
they were taken. A failed backup is logged as an error and the schedule carries on;
alert on `scheduled metadata backup failed`. A backup that was stored but
could not be followed by the removal of old ones is not a failure: it is
logged as a warning (`old backups beyond the number to keep could not be
removed`), and `modelctl backup now` prints the warning.

A stored backup contains token hashes (not the tokens) and the full
catalogue. Anyone who can read the bucket can read it.

### On demand

```bash
modelctl backup now                 # take one and store it, as above
modelctl backup list
modelctl backup download snap.db    # a fresh snapshot to a local file, verified on arrival
```

These need an admin token. `download` is the way to keep copies somewhere
of your own choosing, for example from a scheduled job.

### Restore

Stop the registry, then:

```bash
modelctl restore --latest               # newest stored backup
modelctl restore --name registry-20261118T140000Z.db
modelctl restore snap.db                # a downloaded snapshot
```

`restore` takes `--data` and the same storage settings as `serve`
(`--storage` and the `--s3-*` flags, or `REGISTRY_STORAGE` and the
`REGISTRY_S3_*` variables), so it finds the stored backups. It
verifies the snapshot first, and refuses a file that is damaged, is not a
registry database, or was written by a newer version. It will not
overwrite an existing database unless you pass `--replace`, and then the
old one is kept beside it as `registry.db.before-restore-<time>`, together
with its write-ahead log (`...-wal`, `...-shm`) if the registry did not
stop cleanly, so nothing it had committed is lost from the kept copy. If
the restore fails, the old database is put back where it was.

### A server whose disk does not outlive it

With S3 storage the server's disk holds only the database. Set

```bash
REGISTRY_RESTORE_IF_EMPTY=true   # or pass --restore-if-empty
```

and a server that starts with no database restores the newest stored
backup before serving; with no backup either, it starts as a new registry.
A data directory that already has a database is never touched. In
Kubernetes this means a replaced volume recovers by itself:

```yaml
env:
  - name: REGISTRY_STORAGE
    value: s3
  - name: REGISTRY_BACKUP_INTERVAL
    value: 1h
  - name: REGISTRY_RESTORE_IF_EMPTY
    value: "true"
```

Run one replica. Two servers restoring the same backup would each record
revisions the other cannot see.

### What a restore loses

A backup is a point in time. Revisions, tokens and deletions recorded
after it are not in it: at most one interval of changes with scheduled
backups. The files of lost revisions are still stored, but the registry no
longer knows them; push or mirror those revisions again. With pull-through
caching, lost entries are fetched again on the next request.

If you ran `modelctl gc` or `modelctl delete` after the backup you are
restoring, some restored revisions may list files that were removed.
Downloads of those files fail until the revision is pushed or mirrored
again.

### What a restore brings back

**Tokens you revoked after the backup was taken work again.** A backup
holds the tokens as they were at that moment, so restoring it undoes every
revocation made since, and that includes the automatic restore of
`REGISTRY_RESTORE_IF_EMPTY`. After any restore, run `modelctl token list`
and revoke again whatever should not be there. If a token was revoked
because it leaked, do this before the registry is reachable again.

An admin token supplied with `REGISTRY_ADMIN_TOKEN` or
`REGISTRY_ADMIN_TOKEN_FILE` is not affected: it is applied at every start
and replaces the restored one.

### Copying everything by hand

Stop the server, copy the data directory, start the server. Without
stopping, copy `blobs/` first and take the database second with
`modelctl backup download`, so every file the database mentions is in the
copy.

## Freeing space

```bash
modelctl delete acme/old-model --yes   # removes its files unless another repository uses them
modelctl gc                            # removes files nothing references (failed mirrors, abandoned pushes)
```

`gc` leaves files newer than one hour alone, in case a push is still in
progress. `delete` leaves a file alone for the same hour only if a push or
mirror has just stored it again or asked about it; a later `gc` removes it
if nothing came to use it.

If storage refuses to remove some of the files, the command has still done
what it was asked (the repository is deleted, the rest is freed): it exits
`0` and prints a warning on standard error, and the server's log has the
reason, one line for each file that could not be removed, naming it by
its digest. The counts the command prints are the files that were really
removed. What was left behind is wasted space and nothing more, and the
registry still has its record of those files: once the storage is put
right, run `modelctl gc` and it removes them. Nothing has to be removed
by hand. (Before v0.4.0 the registry dropped its record of such a file
even though the file stayed, counted it as freed, and no later `gc` found
it; files left behind by an earlier version still have to be removed from
storage by hand.)

Two rare cases are different. If storage reported a failure and the file
turns out to be gone, it is counted as removed. If the record itself
cannot be put back, because the database fails at that moment, the log
line says so, and that one file is not found by a later `gc`.

## Checking stored files

Every file is checked against its digest when it is stored. After that the
registry trusts its storage: a download compares the stored size with the
recorded one and nothing more. `modelctl verify` asks the question again
of everything the registry holds:

```bash
modelctl verify
```

```
Checking 1204 stored files, 86.3 GiB. This reads every one of them in full.
damaged     9f2c...e41a  4.2 GiB
            blob verification failed: its content now has sha256 77ab...
            used by meta-llama/Llama-3.2-1B
modelctl verify: 1 of 1204 stored files failed the check. ...
```

It needs an admin token. It exits `0` when every file is present and
matches its digest and `1` otherwise, so it can run from a scheduled job
that alerts on failure; `--json` gives the totals and the list of problems.

Each file is reported as one of:

| | |
|---|---|
| `missing` | The registry has a record of the file and storage holds nothing under its name. |
| `damaged` | The stored bytes are the wrong size, or no longer hash to the file's digest: a failing disk, an interrupted copy, or something other than the registry writing to the bucket or to `blobs/`. |
| `unreadable` | The file could not be read, so nothing is known about its content. An unreachable bucket looks like this. Fix the storage and run the check again. |

What to expect:

- **It reads everything.** On local disk that is I/O for the whole store.
  With S3 storage every object is downloaded through the registry, which
  your provider may charge for. Run it when the cost is acceptable, not on
  every deploy.
- **The registry keeps serving.** The check takes no lock, reads one file
  at a time, and uses a fixed amount of memory whatever the file sizes.
- **Stopping the command stops the check.** It can simply be run again.
- **It changes nothing.** Damaged files are reported and left in place.
- **It checks what the registry has a record of.** Objects in the bucket
  that the registry does not know are not looked at, and files that
  pull-through has listed but never fetched have nothing stored to check.

To put a file right you need its correct content again, from wherever it
came from.

1. If the file is `damaged`, remove the stored copy by hand:
   `data/blobs/sha256/<first two characters>/<digest>` on disk, or the
   object `<prefix>/sha256/<first two characters>/<digest>` in the bucket.
   The registry never replaces what is stored under a digest, because the
   same name means the same content, so a damaged copy has to go first. A
   `missing` file needs nothing removed.
2. Upload the correct content under the same digest. It is accepted only
   if it hashes to that digest:

   ```bash
   curl -X PUT -H "Authorization: Bearer $MODELCTL_TOKEN" --data-binary @model.safetensors \
     http://localhost:8080/api/v1/blobs/<digest>
   ```

3. Run `modelctl verify` again.

`modelctl push` and `modelctl mirror` do not repair a file: both skip
content the registry already has a record of. For a file that belongs to
mirrored repositories only, the other way is to `modelctl delete` each
repository that uses it, which removes the stored copy, and mirror them
again.

## Stopping the server

`modelctl serve` stops on an interrupt or `SIGTERM`. It stops accepting
requests, cancels the pull-through fetches in progress and a scheduled
backup if one is running, and waits up to 30 seconds for those and for
transfers in flight before it exits. A client that was waiting on a fetch
is answered `503` with `Retry-After`; the fetch starts again on its next
request. A client that was being sent a file while it was fetched has its
download cut short, as it would be by a failed fetch (see
[the first download](#the-first-download-of-a-file)). A backup that was
cut short stores nothing.

A transfer still running after 30 seconds is cut off. Uploads cut off this
way leave nothing visible: the part received is cleared from `tmp/` at the
next start. Give the process at least that long before it is killed (on
Kubernetes, the default `terminationGracePeriodSeconds` of 30 is the
minimum that makes sense; raise it a little).

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
- There is no built-in rate limiting and no cap on connections. Use the
  reverse proxy for both if the registry is public.
- There is no storage quota. The settings under
  [Limits on what a request may send](#limits-on-what-a-request-may-send)
  bound single files and requests, not the total.

## Limits on what a request may send

File transfers have no time limit, because model files are large. By
default they have no size limit either. Everything else a client sends is
small, and is held to that:

| What | Limit |
|---|---|
| One uploaded file | `REGISTRY_MAX_BLOB_BYTES` (`--max-blob-bytes`), in bytes. Unset or `0`: no limit. |
| One file fetched by pull-through | `REGISTRY_PULL_THROUGH_MAX_FILE_BYTES` (`--pull-through-max-file-bytes`). Unset or `0`: no limit. See [pull-through caching](#pull-through-caching). |
| A commit from a Hugging Face client (`hf upload`, `create_commit`) | 4 GiB for the whole request, which carries only the files of 10 MiB or less; larger files travel separately and are not counted. |
| A JSON request to the API | 64 MiB, and the body must arrive within 30 seconds. |
| A web UI form | 64 KiB, and the body must arrive within 30 seconds. |
| Any other request that announces a body (one to a route that reads none, or that is refused) | The server waits at most 30 seconds for it, then answers and closes the connection. |

`REGISTRY_MAX_BLOB_BYTES` applies to every way of uploading: `modelctl
push`, `PUT /api/v1/blobs`, and both the small-file and large-file paths of
a Hugging Face client. An upload over it is refused with `413` and a
message naming the setting, and nothing of it is kept. It does not apply
to `modelctl mirror`, which stores what the upstream holds.

A token with the write role can still fill the disk with many files under
the limit. Give write tokens to people and jobs you would trust with the
disk.
