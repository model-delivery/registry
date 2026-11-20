# Roadmap

What exists today is listed in the [README](../README.md). This is what
comes next, roughly in order. It is a statement of intent, not a promise
of dates. The order follows what people ask for: if an item matters to
you, an issue saying how you would use it moves it up. Pull requests that
move an item forward are welcome.

## Next

The first is about keeping independent copies of models, and the data
they were trained on, that do not depend on one host. (Dataset
repositories and keeping mirrors up to date, the items that used to head
this list, are in the registry now: see the [README](../README.md#what-it-does)
and [keeping mirrors up to date](operations.md#keeping-mirrors-up-to-date).)

- **Several upstreams.** A registry can already mirror from another
  registry in place of Hugging Face. Let it have a list, tried in order,
  so a chain of independent mirrors keeps working when one of them, or
  the original, cannot be reached. Every file is checked against its
  digest wherever it came from.
- **Mirror selected files.** `modelctl mirror --include "*.gguf"` to fetch
  chosen files of a large repository ahead of time. Pull-through caching
  already fetches only the files that are downloaded; this is the same for
  the explicit command.

## After that

- **Xet uploads.** Uploads from Hugging Face clients work today, but a
  stock client needs `HF_HUB_DISABLE_XET=1` for large files. Speaking the
  Xet protocol would remove that step.
- **Send a repository to another registry.** Mirroring pulls. The other
  direction, copying a repository with its revision ids intact from one
  registry to another you can write to, needs `modelctl pull` and
  `modelctl push` today and gives the copy new revision ids.
- **Resumable uploads** for `modelctl push`.
- **A migration command** to move stored files between local disk and S3.
- **Helm chart** for Kubernetes.
- **A second metadata backend (PostgreSQL)**, so several registry instances
  can share one database. SQLite is enough for a single node; this is what
  running more than one needs.
- **OIDC sign-in** for the web UI.
- **Per-repository permissions.**
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
