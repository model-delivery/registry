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

## What the registry does and does not protect

Knowing the design helps you deploy it safely.

**It does:**

- Verify every stored file against a SHA-256 digest when it is written, and
  mirrored files against the digests the upstream hub publishes.
- Store access tokens only as SHA-256 hashes. A token is shown once, when
  created.
- Require a token for every write, and optionally for every read.
- Refuse state-changing browser requests that come from another site.
- Fetch only from the upstream hub the operator configured. API callers
  cannot point it at other hosts.

**It does not:**

- Terminate TLS. Put it behind an HTTPS reverse proxy before exposing it
  beyond a network you trust; otherwise tokens cross the network in the
  clear.
- Scan model files. A model file can contain code that runs when it is
  loaded (Python pickle files are the common case). The registry stores and
  serves bytes faithfully; it does not judge whether they are safe. Mirror
  and load only models you trust, and prefer formats such as safetensors.
- Limit request rates or lock out repeated bad tokens. Use your reverse
  proxy for that if the registry is reachable from the internet.
- Encrypt data at rest. Use an encrypted volume if you need that.
- Restrict access per repository. A token's role applies to the whole
  registry.

See [docs/operations.md](docs/operations.md) for deployment guidance.
