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

It is one binary with no external services: metadata in SQLite, files on
disk. It runs the same on a Raspberry Pi, a homelab box or a server.

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
| **Serve** | Existing clients download through the Hugging Face compatible API, including resumable range requests. |
| **Push and pull** | Store your own models: `modelctl push ./dir team/model`, `modelctl pull team/model`. |
| **Version** | Every revision is immutable and addressed by id. Branches move, tags never do. |
| **Verify** | Files are stored under the SHA-256 of their content and verified on the way in. Identical files are stored once, across revisions and repositories. |
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
| | `REGISTRY_ADMIN_TOKEN` | | Use this admin token instead of generating one |
| | `REGISTRY_UPSTREAM_TOKEN` | | Default token for the upstream hub |

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
| Uploading with Hugging Face clients (`upload_file`, `hf upload`, `push_to_hub`) | Not supported; use `modelctl push` |
| Datasets and Spaces | Not supported |
| Model search and listing (`list_models`) | Not supported |
| `git clone` / git-lfs access | Not supported |
| Xet accelerated transfer | Not used; clients fall back to plain HTTP |
| Hub-only features (discussions, likes, inference, gating workflows) | Out of scope |

Details, including the exact routes, are in
[docs/compatibility.md](docs/compatibility.md).

## What is not supported yet

- **Storage is the local filesystem.** S3-compatible object storage is
  planned; the storage layer is built for it but only the filesystem
  backend exists.
- **One server, one data directory.** No replication or high availability.
- **A mirror is a point-in-time copy.** Run `modelctl mirror` again to pick
  up a newer upstream revision. There is no automatic sync and no
  pull-through caching.
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
is stored once per unique SHA-256. A revision is a list of
`path -> digest` entries, so revisions that share files share storage. See
[docs/architecture.md](docs/architecture.md).

## Building from source

Needs Go 1.26 or newer. [Task](https://taskfile.dev) runs the common
commands:

```bash
task build        # bin/modelctl
task check        # format, vet, unit tests
task test:e2e     # the huggingface_hub compatibility tests (needs Python)
task docker       # container image
```

## Contributing

Bug reports and pull requests are welcome. See
[CONTRIBUTING.md](CONTRIBUTING.md). To report a security problem, see
[SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).

Model Delivery is an independent project. It is not affiliated with or
endorsed by Hugging Face. "Hugging Face" is a trademark of Hugging Face, Inc.
