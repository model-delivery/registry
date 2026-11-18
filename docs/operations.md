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

## The first admin token

On its first start against an empty data directory the server creates an
admin token and prints it once, to standard error. With Docker, read it
from `docker logs`.

To choose the token yourself, set `REGISTRY_ADMIN_TOKEN` (at least 16
characters). It is applied at every start, so changing the variable and
restarting rotates it.

If you lose every admin token, start the server once with
`REGISTRY_ADMIN_TOKEN` set.

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
  filesystem (NFS, SMB) is unreliable.
- There is no built-in rate limiting. Use the reverse proxy for that if the
  registry is public.
