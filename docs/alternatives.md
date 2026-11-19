# Alternatives

Other open-source projects solve nearby problems, and some of them will
fit your case better than this one. This page says what each is for, in
its own project's words where possible, and when to pick it.

It was last checked in November 2026. Projects move; follow the links for
what each does today. If something here is wrong or out of date, a pull
request correcting it is welcome, including from the projects named.

## What this project is for

Model Delivery is a registry for the models you depend on. It is one Go
binary with SQLite inside, and it:

- mirrors model repositories from Hugging Face, or caches them on demand,
  and checks every file against the digest the hub publishes before
  storing it;
- keeps revisions immutable, with the ids they have upstream, so a pinned
  revision is the same files on every registry that holds it;
- serves them to existing Hugging Face clients through `HF_ENDPOINT`, and
  accepts uploads of your own models from the same clients;
- stores files on disk or in an S3-compatible bucket, backs up its own
  metadata, and can re-check everything it holds.

It stores models only. Dataset repositories are on the
[roadmap](roadmap.md), not in the product, and several projects below
handle them today. It is a single server; see
[what is not supported yet](../README.md#what-is-not-supported-yet).

## Caching mirrors of Hugging Face

These sit in front of Hugging Face and keep what passes through.

- **[Olah](https://github.com/vtuber-plan/olah)** (Python, MIT) describes
  itself as a self-hosted lightweight Hugging Face mirror service that
  caches files in chunks as users download them. It lists model, dataset
  and Spaces mirroring, and cache clean-up policies.

  Choose it if you want a download cache that also covers datasets and
  Spaces, with eviction when the disk fills. Model Delivery keeps what it
  stores until you delete it, records each revision, and takes uploads.

- **[DingoSpeed](https://github.com/dingodb/dingo-hfmirror)** (Go)
  describes itself as a self-hosted Hugging Face mirror service for models
  and datasets, with several mirror nodes that share what they have
  cached, cache clean-up policies and download monitoring.

  Choose it if you want a cache spread over several cooperating nodes
  today. Model Delivery is one server; several upstreams are on the
  roadmap.

## Self-hosted hubs

These aim to be a hub of your own, with users, organisations and a web
interface for working in.

- **[MatrixHub](https://github.com/matrixhub-ai/matrixhub)** (Go, Apache
  2.0) describes itself as an open-source, self-hosted AI model registry
  engineered for large-scale enterprise inference, with Hugging Face
  compatibility and integrations for vLLM and SGLang. It installs with
  Docker Compose or Helm.

  Choose it if you are serving models to a large inference fleet on
  Kubernetes and want the integrations it ships. Model Delivery is the
  smaller piece: one binary with no other services to run.

- **[KohakuHub](https://github.com/KohakuBlueleaf/KohakuHub)** (Python,
  AGPL-3.0) describes itself as a self-hosted Hugging Face alternative
  with Git-like versioning for models and datasets, built on LakeFS and S3
  storage, with `git clone`, organisations, quotas and a full web
  interface. Its README marks it as an alpha release.

  Choose it if you want a place for people to publish and browse models
  and datasets, with accounts and `git clone`. Model Delivery has tokens
  and a small web interface for looking, and does not speak the git
  protocol.

## Model lifecycle registries

- **[MLflow Model Registry](https://mlflow.org/docs/latest/ml/model-registry)**
  (Apache 2.0) is, in its own words, a centralized model store, set of
  APIs and UI for managing the full lifecycle of a model: versions,
  aliases, tags, and lineage back to the run that produced each one.

  Choose it for the models you train: which version came from which
  experiment, and which one is in production. Model Delivery does none of
  that. It answers a different question, about getting the same files of
  a published model every time, from a server you run. The two work side
  by side.

## Models as OCI artifacts

- **[KitOps](https://github.com/kitops-ml/kitops)** (Apache 2.0) packages
  models, datasets, code and configuration as OCI artifacts called
  ModelKits, which can be stored in any OCI 1.1 registry such as
  **[Harbor](https://goharbor.io/)**.

  Choose this if your delivery is already built on container registries
  and you can change how models are packaged and pulled. You get the
  signing, scanning and replication your registry already has. Model
  Delivery takes the other route: nothing changes on the client except
  `HF_ENDPOINT`.

## Peer-to-peer distribution

- **[Dragonfly](https://d7y.io/)** (Apache 2.0, a CNCF project)
  distributes large files across a cluster peer to peer, and its client
  can download from Hugging Face directly.

  Choose it when the problem is getting one large model onto hundreds of
  nodes at once without each of them downloading from the origin. It
  moves files quickly; it is not a versioned store of them. A registry
  can be the origin it fetches from.

## Also worth knowing

Repository managers that many companies already run can proxy Hugging
Face as one more package format. Sonatype Nexus Repository does so in its
free Community Edition, which has usage limits. If you already run one,
look there first.
