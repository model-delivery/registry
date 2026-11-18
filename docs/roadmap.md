# Roadmap

What exists today is listed in the [README](../README.md). This is what
comes next, roughly in order. It is a statement of intent, not a promise
of dates. Issues and pull requests that move an item forward are welcome.

## Next

- **Mirror selected files.** `modelctl mirror --include "*.gguf"` to fetch
  chosen files of a large repository ahead of time. Pull-through caching
  already fetches only the files that are downloaded; this is the same for
  the explicit command.
- **Xet uploads.** Uploads from Hugging Face clients work today, but a
  stock client needs `HF_HUB_DISABLE_XET=1` for large files. Speaking the
  Xet protocol would remove that step.
- **Datasets.** The same registry for dataset repositories.

## After that

- **Scheduled sync.** Keep chosen mirrors up to date with upstream.
- **Serve while fetching.** Today the first download of a file through
  pull-through waits until the whole file is stored and verified. Stream
  it to that first client as it arrives, holding back the end until the
  digest has been checked.
- **Mirror every branch and tag** of a repository in one command.
- **Resumable uploads** for `modelctl push`.
- **A migration command** to move stored files between local disk and S3.
- **Helm chart** for Kubernetes.
- **A second metadata backend (PostgreSQL)**, so several registry instances
  can share one database. SQLite is enough for a single node; this is what
  running more than one needs.
- **OIDC sign-in** for the web UI.
- **Per-repository permissions.**
- **Metrics endpoint** for Prometheus.
- **More in the web UI:** rendered model cards, mirror from the browser,
  token management.

## Not planned

Model Delivery is a registry: it stores, versions and distributes model
files. It is not trying to be a community hub. These are out of scope:

- Discussions, likes, profiles and other social features.
- Hosted inference or model serving.
- Training, evaluation or experiment tracking.

## Open source and commercial

The registry in this repository is licensed under Apache 2.0 and is meant
to be complete and production-capable by itself: storage, the Hugging Face
compatible API, mirroring, the CLI, the REST API, the web UI, and token
authentication. It has no limits on models, storage or use, and none will
be added.

A separate commercial edition is planned for needs that come with large
organisations, such as SAML single sign-on, SCIM provisioning, fine-grained
roles, audit reporting, policy enforcement, quotas and replication. It
will build on this registry rather than replace it.
