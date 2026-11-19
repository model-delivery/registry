# Security

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub: on the repository page, open the
**Security** tab and choose **Report a vulnerability**. Include what you
found, how to reproduce it, and the version affected.

You should get a reply within a few days. Fixes for confirmed problems are
released as soon as they are ready, and you will be credited unless you
ask not to be.

## Supported versions

The project is young. Security fixes go into the latest release only.

## Verifying a release

Every release lists a SHA-256 for each archive in `checksums.txt`:

```bash
sha256sum -c checksums.txt --ignore-missing
```

That shows the download is intact. To show it was built by this
repository's release workflow from the tagged commit, releases after
v0.3.1 also carry a signed build attestation, checked with the GitHub CLI:

```bash
gh attestation verify modelctl_<version>_linux_amd64.tar.gz -R model-delivery/registry
```

The container image carries its build provenance and a list of what it
contains (an SBOM), from the same release on:

```bash
docker buildx imagetools inspect ghcr.io/model-delivery/registry:<version> --format '{{ json .Provenance }}'
```

The image's base images are pinned by digest in the `Dockerfile`.

## What the registry does and does not protect

Knowing the design helps you deploy it safely.

**It does:**

- Verify every stored file against a SHA-256 digest when it is written, and
  mirrored files against the digests the upstream hub publishes: a SHA-256
  for files tracked with LFS, and a git blob id (a SHA-1, the only digest a
  hub offers for them) for the small files it keeps in git.
- Store access tokens only as SHA-256 hashes. A token is shown once, when
  created.
- Require a token for every write, and optionally for every read. The one
  write that carries no token in the request is a large-file upload from a
  Hugging Face client, which goes to a signed URL: the URL is issued to a
  token with the write role, covers one file of one digest and size,
  expires, and stops working when that token is revoked.
- Refuse state-changing browser requests that come from another site.
- Bound every request body that is not a file transfer, in size and in the
  time the client has to send it. File transfers have no time limit, and
  no size limit unless the operator sets one (`--max-blob-bytes`,
  `--pull-through-max-file-bytes`).
- Stop at start, with an error, on a setting it cannot read, rather than
  run with a default the operator did not choose. `REGISTRY_REQUIRE_AUTH`
  is the one that matters most here.
- Fetch only from the upstream hub the operator configured. API callers
  cannot point it at other hosts, the upstream token is sent to that hub
  and nowhere else, and a redirect from the hub to a loopback or private
  address is refused.
- Keep pull-through caching out of repositories that were created in the
  registry: a repository you pushed is never added to from the upstream.

**It does not:**

- Terminate TLS. Put it behind an HTTPS reverse proxy before exposing it
  beyond a network you trust; otherwise tokens cross the network in the
  clear.
- Scan model files. A model file can contain code that runs when it is
  loaded (Python pickle files are the common case). The registry stores and
  serves bytes faithfully; it does not judge whether they are safe. Mirror
  and load only models you trust, and prefer formats such as safetensors.
- Re-check stored files each time they are served. Whoever can write to
  the bucket or the data directory can change what is served under a
  file's name. `modelctl verify` re-reads everything and reports files
  that no longer match their digest; `modelctl pull` checks what it
  downloads, and Hugging Face clients mostly do not.
- Limit request rates or lock out repeated bad tokens. Use your reverse
  proxy for that if the registry is reachable from the internet.
- Encrypt data at rest. Use an encrypted volume if you need that.
- Restrict access per repository. A token's role applies to the whole
  registry.

See [docs/operations.md](docs/operations.md) for deployment guidance.
