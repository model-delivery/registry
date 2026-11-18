# Model Delivery Registry

**A self-hosted registry for AI models that existing Hugging Face tools can download from.**

Your models. Your infrastructure. Your rules.

Think of a Docker registry or Artifactory, built for model files. Mirror
repositories from Hugging Face, or push your own, and serve them from a
machine you control. Tools built on `huggingface_hub` (`transformers`,
`diffusers`, the `hf` CLI and others) download from it after changing one
environment variable:

```bash
export HF_ENDPOINT=http://localhost:8080
```

It is one binary. Out of the box it needs nothing else: metadata in SQLite,
model files on disk. For larger deployments the model files can live in
any S3-compatible bucket instead. It runs the same on a Raspberry Pi, a
homelab box or a server.

> **Status: early release.** The download path, mirroring, the CLI and the
> web UI work and are tested against the real `huggingface_hub` library.
> Read [what is not supported yet](#what-is-not-supported-yet) before
> relying on it.

## Quick start

Mirror a model, cut the connection to Hugging Face, and download it with a
standard Hugging Face client.

**1. Start the registry.** Download `modelctl` from the
[releases page](https://github.com/model-delivery/registry/releases), or
use Docker (below).

```bash
modelctl serve
```

On first start it prints an admin token. Copy it:

```bash
export MODELCTL_TOKEN=md_...
```

**2. Mirror a model from Hugging Face.**

```bash
modelctl mirror hf-internal-testing/tiny-random-bert
```

```
Mirroring hf-internal-testing/tiny-random-bert at revision f171d7baecaf...: 10 files, 26.9 MiB
  downloaded    25.4 MiB  tf_model.h5
  ...
Mirrored hf-internal-testing/tiny-random-bert at revision f171d7baecaf37b5da5a3616d8833b9969753535
10 files verified: 26.9 MiB downloaded, 0 B already stored
```

Every file is checked against the digest Hugging Face publishes for it, and
the revision keeps its upstream id.

**3. Disconnect from Hugging Face** (unplug, or block `huggingface.co`).

**4. Download with a standard client**, pointed at your registry:

```bash
export HF_ENDPOINT=http://localhost:8080
python -c "from huggingface_hub import snapshot_download; print(snapshot_download('hf-internal-testing/tiny-random-bert'))"
```

The files arrive from your registry, under the same revision id they had
upstream. Browse what you have at <http://localhost:8080>.

This exact flow runs as an automated test (`task test:e2e:network`).

### With Docker

```bash
docker run -d --name registry -p 8080:8080 -v registry-data:/data ghcr.io/model-delivery/registry
docker logs registry        # the admin token is printed here on first start
```

Or `docker compose up -d` with the [`compose.yaml`](compose.yaml) in this
repository. The image contains the same `modelctl` binary:

```bash
docker exec -e MODELCTL_TOKEN=md_... registry /modelctl mirror openai-community/gpt2
```

## What it does

| | |
|---|---|
| **Mirror** | Copy a repository from Hugging Face (or any compatible hub) at a branch, tag or revision id. Private and gated repositories work with your Hugging Face token. |
| **Cache on demand** | Optionally, fetch a model from Hugging Face the first time a client asks for it, then serve it from the registry from then on. Point your tools at the registry and it fills itself. |
| **Serve** | Existing clients download through the Hugging Face compatible API, including resumable range requests. |
| **Push and pull** | Store your own models: `modelctl push ./dir team/model`, `modelctl pull team/model`. |
| **Version** | Every revision is immutable and addressed by id. Branches move, tags never do. |
| **Verify** | Files are stored under the SHA-256 of their content and verified on the way in. Identical files are stored once, across revisions and repositories. |
| **Store** | Local disk, or an S3-compatible object store. Verified against Cloudflare R2 and SeaweedFS (which CI runs on every change). It uses the standard S3 API, so AWS S3, Backblaze B2, Ceph and MinIO are expected to work but have not been tested. With S3, downloads can be redirected to signed bucket URLs so the bytes never pass through the registry. |
| **Control access** | Access tokens with read, write and admin roles. Downloads can be open or require a token. |
| **Browse** | A small web UI lists repositories, revisions, files and storage use. |

## Command line

```
modelctl serve                      run the registry
modelctl mirror <repo>              copy a repository from the upstream hub
modelctl push <dir> <repo>          upload a directory as a new revision
modelctl pull <repo>                download a revision and verify it
modelctl list                       list repositories
modelctl inspect <repo>             show revisions, refs and files
modelctl delete <repo> --yes        delete a repository and free its storage
modelctl token create|list|revoke   manage access tokens
modelctl gc                         remove stored files nothing references
```

Client commands read `MODELCTL_ENDPOINT` (default `http://localhost:8080`)
and `MODELCTL_TOKEN`. Every one accepts `--json` for scripts.

```bash
modelctl mirror meta-llama/Llama-3.2-1B --upstream-token "$HF_TOKEN"   # gated repository
modelctl mirror openai-community/gpt2 --revision <branch, tag or 40-character revision id>
modelctl push ./my-model my-team/my-model --tag v1.0 --message "first release"
modelctl pull my-team/my-model --revision v1.0 --dir ./out
modelctl inspect my-team/my-model --revision main
modelctl token create ci --role write
```

The REST API behind these commands is described in [docs/api.md](docs/api.md).

## Configuration

`modelctl serve` is configured by flags or environment variables.

| Flag | Environment | Default | |
|---|---|---|---|
| `--addr` | `REGISTRY_ADDR` | `:8080` | Listen address |
| `--data` | `REGISTRY_DATA` | `./data` | Database and stored files |
| `--upstream` | `REGISTRY_UPSTREAM` | `https://huggingface.co` | Hub to mirror from |
| `--require-auth` | `REGISTRY_REQUIRE_AUTH` | `false` | Require a token for downloads and browsing too |
| `--pull-through` | `REGISTRY_PULL_THROUGH` | `false` | Fetch a model from the upstream hub the first time a client asks for one the registry does not hold |
| | `REGISTRY_PULL_THROUGH_ALLOW` | all | Comma-separated patterns limiting which repositories pull-through may fetch, for example `meta-llama/*,google/*` |
| `--storage` | `REGISTRY_STORAGE` | `filesystem` | Where model files are kept: `filesystem` or `s3` |
| | `REGISTRY_ADMIN_TOKEN` | | Use this admin token instead of generating one |
| | `REGISTRY_ADMIN_TOKEN_FILE` | | Read the admin token from this file (a mounted secret) |
| `--require-admin-token` | `REGISTRY_REQUIRE_ADMIN_TOKEN` | `false` | Refuse to start without a supplied admin token instead of generating one and printing it. Use on Kubernetes and anywhere logs are collected |
| | `REGISTRY_UPSTREAM_TOKEN` | | Default token for the upstream hub |

**S3 storage.** Set `REGISTRY_STORAGE=s3` and:

| Environment | |
|---|---|
| `REGISTRY_S3_BUCKET` | Required. The bucket must already exist. |
| `REGISTRY_S3_ENDPOINT` | Service URL. Default `https://s3.amazonaws.com`. For other services, for example `https://<account>.r2.cloudflarestorage.com` or `http://seaweedfs:8333`. |
| `REGISTRY_S3_REGION` | Region, when the service needs one. |
| `REGISTRY_S3_ACCESS_KEY`, `REGISTRY_S3_SECRET_KEY` | Credentials. `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` are used if these are unset; with neither, the instance or pod role is used. |
| `REGISTRY_S3_PREFIX` | Key prefix, to share a bucket. |
| `REGISTRY_S3_REDIRECT` | `true` sends downloads to signed bucket URLs instead of streaming them through the registry. Clients must be able to reach the bucket. |

Metadata stays in SQLite in the data directory, so keep that directory on a
persistent disk and back it up. See [docs/operations.md](docs/operations.md#s3-storage).

**Pull-through caching.** With `--pull-through`, a request for a model the
registry does not have makes it fetch that revision from the upstream hub,
verify and store it, and then answer. The first request for a large model
waits for the whole download; if the client gives up, the fetch carries on
and the next request is served from the registry. Anyone allowed to
download can trigger a fetch, so on a registry others can reach, combine it
with `--require-auth` or an allow list. See
[docs/operations.md](docs/operations.md#pull-through-caching).

**Access.** Writing (push, mirror) always needs a token with the `write`
role; deleting and managing tokens needs `admin`. By default anyone who can
reach the registry can download, which suits a home network. Set
`--require-auth` to require a `read` token for downloads too; clients then
send it the usual way:

```bash
export HF_ENDPOINT=https://models.example.com
export HF_TOKEN=md_...
```

**TLS.** The registry speaks plain HTTP. Put it behind a reverse proxy
(Caddy, nginx, Traefik) for HTTPS, and do that before exposing it beyond a
network you trust. See [docs/operations.md](docs/operations.md).

**Backups.** Everything is in the data directory. See
[docs/operations.md](docs/operations.md) for how to copy it safely.

## Hugging Face compatibility

Tested against the real `huggingface_hub` Python library in CI.

| Capability | Status |
|---|---|
| `snapshot_download`, `hf_hub_download`, `hf download` | Supported |
| `model_info`, `list_repo_tree`, `list_repo_files`, `list_repo_refs` | Supported |
| Branches, tags and pinned revision ids | Supported |
| Resuming interrupted downloads | Supported |
| Private access with `HF_TOKEN` | Supported |
| Libraries that download through `huggingface_hub` (`transformers`, `diffusers`, ...) | Expected to work; not individually tested yet |
| Uploading with `hf upload`, `upload_file`, `upload_folder`, `create_commit` | Supported. Files over 10 MiB need `HF_HUB_DISABLE_XET=1` on the client, because the registry does not speak Xet |
| Datasets and Spaces | Not supported |
| Model search and listing (`list_models`) | Not supported |
| `git clone` / git-lfs access | Not supported |
| Xet accelerated transfer | Not used; clients fall back to plain HTTP |
| Hub-only features (discussions, likes, inference, gating workflows) | Out of scope |

Details, including the exact routes, are in
[docs/compatibility.md](docs/compatibility.md).

## What is not supported yet

- **One server.** Model files can be in S3, but metadata is a SQLite file
  on the server's disk, so you run a single instance. No replication or
  high availability yet.
- **A mirror is a point-in-time copy.** Run `modelctl mirror` again to pick
  up a newer upstream revision. There is no automatic sync. Pull-through
  caching fetches what the registry lacks, but it never refreshes a branch
  it already holds.
- **A mirror copies whole revisions.** You cannot yet mirror only some
  files of a repository.
- **Repository ids are `namespace/name`**, and are case-sensitive.
- **Uploads are not resumable.** An interrupted `modelctl push` re-sends the
  file it was in the middle of; files that finished are skipped.
- **No per-repository permissions.** A token's role applies to the whole
  registry.

The [roadmap](docs/roadmap.md) says what is planned and in what order.

## How it works

One process serves three things: the Hugging Face compatible routes, its
own REST API, and the web UI. Metadata is a SQLite database; file content
is stored once per unique SHA-256, on disk or in a bucket. A revision is a list of
`path -> digest` entries, so revisions that share files share storage. See
[docs/architecture.md](docs/architecture.md).

## Building from source

Needs Go 1.27 or newer. [Task](https://taskfile.dev) runs the common
commands:

```bash
task build        # bin/modelctl
task check        # format, vet, lint, unit tests
task test:e2e     # the huggingface_hub compatibility tests (needs Python)
task test:s3      # storage and compatibility tests against a real S3 server (needs Docker)
task docker       # container image
```

## Contributing

Bug reports and pull requests are welcome. See
[CONTRIBUTING.md](CONTRIBUTING.md). To report a security problem, see
[SECURITY.md](SECURITY.md).

## License

Copyright 2026 [Spectrum Labs LLC](https://spectrumlabs.tech). Licensed under the
[Apache License 2.0](LICENSE); see [NOTICE](NOTICE).

Model Delivery is a Spectrum Labs project. It is not affiliated with or
endorsed by Hugging Face. "Hugging Face" is a trademark of Hugging Face, Inc.
