# Roadmap

What exists today is listed in the [README](../README.md). This is what
comes next, roughly in order. It is a statement of intent, not a promise
of dates. Issues and pull requests that move an item forward are welcome.

## Next

- **S3-compatible storage.** A second `blob.Store` backend for MinIO, S3,
  R2 and similar, with downloads redirected to presigned URLs.
- **Partial mirrors.** `modelctl mirror --include "*.gguf"` to copy only
  some files of a large repository, recorded honestly as a subset.
- **Uploads from Hugging Face clients.** `upload_file`, `upload_folder` and
  `hf upload` against the registry, so pushing needs no extra tool.
- **Integrity check.** `modelctl verify` to re-hash stored files against
  their names and report any damage.
- **Datasets.** The same registry for dataset repositories.

## After that

- **Scheduled sync.** Keep chosen mirrors up to date with upstream.
- **Pull-through cache.** Fetch from upstream on first request, serve from
  the registry afterwards.
- **Mirror every branch and tag** of a repository in one command.
- **Resumable uploads** for `modelctl push`.
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
