# Hugging Face compatibility

The registry implements the part of the Hugging Face Hub HTTP API that
clients use to **download and upload models and datasets**. It does not
implement the whole Hub API, and this page lists exactly what is covered.

Compatibility is checked in CI by running the real `huggingface_hub` and
`datasets` Python libraries against the registry (`test/e2e`). CI installs
the latest release of both on the day. The suite was last run locally
against `huggingface_hub` 2.4.0 and `datasets` 5.2.0. Where a client's
behaviour differs between versions in a way that matters here, this page
says so.

## Using it

```bash
export HF_ENDPOINT=http://localhost:8080   # your registry
export HF_TOKEN=md_...                     # only if the registry requires a token to read
```

```python
from huggingface_hub import snapshot_download, hf_hub_download

repo = "hf-internal-testing/tiny-random-bert"
snapshot_download(repo)
snapshot_download(repo, revision="f171d7baecaf37b5da5a3616d8833b9969753535")
hf_hub_download(repo, "config.json")
```

`HF_ENDPOINT` is read when `huggingface_hub` is imported, so set it before
starting Python.

If you leave a Hugging Face token in `HF_TOKEN` while pointing at a
registry that allows anonymous reads, downloads still work: a token the
registry does not recognise is ignored for reading. Note that your client
is then sending that token to your registry, so only do this with a
registry you run.

## Tested with huggingface_hub

| Call | |
|---|---|
| `snapshot_download` | By branch, tag and revision id, including files in subdirectories and names with spaces |
| `hf_hub_download` | One file, at a branch, tag or revision id, including one in a subdirectory |
| `hf download` | The command: a whole repository, and one file at a tag |
| `HfApi.model_info` | `sha` and `siblings` are the registry's; other fields are passed through from the upstream for mirrored models |
| `HfApi.list_repo_files` | |
| `HfApi.list_repo_tree` | With and without `recursive` |
| `HfApi.list_repo_refs` | |
| Reading with `HF_TOKEN` from a registry that requires a token | And being refused without one |
| `create_repo`, `upload_file`, `upload_folder`, `create_commit` | Adding, replacing and deleting files. **Large files need `HF_HUB_DISABLE_XET=1`**; see [Uploading](#uploading) |
| `hf upload` | The command: a folder, one file, and a folder to a new branch |
| `HfApi.get_paths_info` | For a model and for a dataset: files and directories by path. `expand` is ignored |
| All of the above with `repo_type="dataset"`, and `HfApi.dataset_info` | See [Datasets](#datasets) for exactly which |

All of these run with the `hf_xet` package installed, as a default
installation has it. Downloads are unaffected by it; uploads are covered
under [Uploading](#uploading).

Libraries that fetch files through `huggingface_hub` (`transformers`,
`diffusers`, `sentence-transformers` and others) use the calls above, so
they are expected to work. They are not tested individually yet. If one
does not work, please open an issue naming the library and the call.

## Datasets

A dataset repository is served, uploaded, mirrored and cached exactly as a
model is, at the Hub's dataset addresses. A model and a dataset of the
same name are two repositories with their own revisions and files. Point
the client at the registry and say `repo_type="dataset"`, as you would
against the Hub:

```python
from huggingface_hub import snapshot_download
snapshot_download("acme/reviews", repo_type="dataset")
```

```bash
hf download acme/reviews --repo-type dataset
```

```python
from datasets import load_dataset
load_dataset("acme/reviews")
```

### Tested

With `huggingface_hub`, against a mirrored dataset with the upstream gone
(`TestHubClientDatasetAgainstMirror`): `snapshot_download` and
`hf_hub_download` with `repo_type="dataset"`, by branch and by revision
id, including a file with a space in its name; `hf download --repo-type
dataset`, a whole dataset and one file; `dataset_info`; `list_repo_tree`
with and without `recursive`; `list_repo_refs`; `get_paths_info`; and the
model of the same name beside it, with different content.

Uploads (`TestHubClientDatasetUpload`): `create_repo(repo_type="dataset")`,
`upload_folder` with a file over 10 MiB (with `HF_HUB_DISABLE_XET=1`, as
for models), `upload_file`, and `hf upload --repo-type dataset` of a
folder and of one file, each read back.

Pull-through (`TestHubClientDatasetPullThrough`): `hf_hub_download` of a
dataset the registry has never seen, including an 8 MiB file that the
upstream takes about three seconds to send against a client timeout of
one second. It arrives in one request, sent while the registry fetches
it.

With `datasets` (`TestLoadDatasetAgainstMirror`): `load_dataset("org/name")`
with no other argument on three mirrored datasets, with the upstream gone:

| Layout | Result |
|---|---|
| `train.csv` and `test.csv`, and a `README.md` without metadata | `train` and `test` splits with the right rows |
| `train.jsonl` and `test.jsonl`, no `README.md` | `train` and `test` splits with the right rows |
| `train.parquet` and `test.parquet`, no `README.md` | `train` and `test` splits with the right rows |

These requests are what `load_dataset` made of the registry in that test:
`HEAD /datasets/{id}/resolve/main/README.md` (and, when that is not
there, `GET /api/datasets/{id}` for the revision id); `HEAD` of
`{name}.py` (a loading script, which `datasets` no longer runs) and of
`.huggingface.yaml` and `dataset_infos.json`, all optional;
`GET /api/datasets/{id}/revision/{sha}`; `GET /api/datasets/{id}/tree/{sha}`
at the top and in `data/`, not recursive; and `HEAD` and `GET` of each
data file at the revision id. It made no `paths-info` request.

### The dataset viewer is not implemented

On the Hub a separate service, the dataset viewer, converts datasets to
parquet and describes them. The registry does not implement it:
`/api/datasets/{id}/parquet`, `/croissant` and the like answer `404`.

`load_dataset` asks the viewer for one thing, a dataset's exported
description, when it is given no `data_files`, `data_dir` or revision
other than `main`. It sends that request not to `HF_ENDPOINT` but to the
host `datasets-server.` followed by `HF_ENDPOINT`'s host
(`datasets/utils/_dataset_viewer.py`): with `HF_ENDPOINT=https://models.example.com`
it asks `https://datasets-server.models.example.com/info?dataset=...`,
**with the user's token**. There is no setting to turn it off. Any
failure (the name does not resolve, nothing listens, `404`) makes the
library read the repository's own files instead, which is what the test
above shows: the request was delivered to the registry, answered `404`,
and the splits loaded. An operator who controls DNS for the registry's
domain should make sure `datasets-server.<registry host>` does not
resolve to anything they do not run, since that host would receive the
tokens of users who load datasets.

So the viewer is not needed to load a dataset from its files. The three
layouts above are tested; other layouts that `datasets` reads from a
repository's own files (other formats it reads, `data/` folders, splits
named in the dataset card) take the same requests and are expected to
work, but are not tested. What cannot work: anything that relies on the
viewer's conversion, such as reading the `refs/convert/parquet`
revision, and datasets that need a loading script, which `datasets`
5.2.0 refuses to run against any hub ("Dataset scripts are no longer
supported").

## Uploading

`hf upload`, `upload_file`, `upload_folder` and `create_commit` work
against the registry (each is run against it in `test/e2e`), so a training script that pushes to the Hub can push
to your registry instead. The token needs the `write` role.

```bash
export HF_ENDPOINT=http://localhost:8080
export HF_TOKEN=md_...              # a registry token with the write role
export HF_HUB_DISABLE_XET=1         # required for files over 10 MiB, see below
hf upload my-team/my-model ./my-model
```

**Files over 10 MiB need `HF_HUB_DISABLE_XET=1`.** `huggingface_hub`
installs the `hf_xet` package by default, and with it present the client
sends large files through Hugging Face's Xet storage protocol without
asking the server whether it supports that. The registry does not speak
Xet. With the variable set, the client uses its older large-file protocol,
which the registry implements. Without it:

- files up to 10 MiB upload normally, because they travel inside the
  commit request and never touch Xet;
- a larger file fails, and the error names the address
  `/api/xet-is-not-supported/set-HF_HUB_DISABLE_XET=1-and-upload-again`,
  which is the registry telling you what to do in the only place that
  client will print.

A client without `hf_xet` installed needs no setting.

What an upload does and does not do:

- **Each upload is a new revision** on the branch (default `main`); earlier
  revisions are unchanged. Uploading content identical to the branch's
  current revision makes no new revision.
- **Every file is verified.** A large file is uploaded under its SHA-256
  and rejected unless the bytes match.
- **A file already stored is not sent again**, whichever repository or
  revision it came from.
- **The repository is created on first use.** A repository id without a
  namespace is put under the name of the token that created it.
- **Datasets upload the same way**, with `repo_type="dataset"` or
  `--repo-type dataset`; see [Datasets](#datasets).
- **Not supported:** pull requests (`create_pr`), uploading to a revision
  id, Spaces, and `git push`.
- **Not tested:** `upload_large_folder`, and `push_to_hub` in libraries
  such as `transformers`. They are built on the calls above, so they are
  expected to work with the same setting.

## Routes

| Route | Notes |
|---|---|
| `GET /api/models/{repo}` | Same as revision `main` |
| `GET /api/models/{repo}/revision/{revision}` | |
| `GET /api/models/{repo}/tree/{revision}[/{directory}]` | `recursive` is honoured. All entries are returned in one response; there is no pagination |
| `GET /api/models/{repo}/refs` | |
| `POST /api/models/{repo}/paths-info/{revision}` | A form with `paths` (repeated); the entries `tree` gives, for the paths that exist. `expand` is ignored. Needs the read role, though it is a POST |
| `HEAD`, `GET /{repo}/resolve/{revision}/{path}` | Sends `ETag`, `X-Repo-Commit`, `Content-Length`, `Accept-Ranges`; honours `Range` and `If-None-Match` |
| `GET /api/whoami-v2` | Reports the registry token's name |
| `POST /api/repos/create` | `type` of `model` (or none) or `dataset`; `space` is refused. `409` if the repository exists, which `exist_ok` clients accept |
| `POST /api/models/{repo}/preupload/{branch}` | Files up to 10 MiB are sent inline, larger ones through the large-file protocol |
| `POST /{repo}.git/info/lfs/objects/batch` | git-lfs batch API, `upload` operation, `basic` transfer. Returns a signed, expiring URL per object, or nothing for an object already stored |
| `PUT /api/lfs/upload/{sha256}` | The signed URL. Accepts only the bytes it was issued for, for up to 12 hours, and only while the token that asked for it still exists and has the write role |
| `POST /api/models/{repo}/commit/{branch}` | Line-delimited commit: `file`, `lfsFile`, `deletedFile`, `deletedFolder`. `412` if `parentCommit` is no longer the branch's revision |
| `POST /api/models/{repo}/branch/{branch}` | Creates the branch from `startingPoint` (default `main`). On a repository with no revision yet it succeeds and creates nothing: the branch comes into being with its first commit |
| `POST /api/validate-yaml` | Always accepts; the registry stores `README.md` as a file |
| `GET /api/models/{repo}/xet-read-token/{revision}`, `xet-write-token` | Not Xet: a redirect to an address that answers `400` and whose path says to set `HF_HUB_DISABLE_XET=1`. See [Uploading](#uploading) |

Every route above has a dataset twin at the Hub's address for it:
`/api/datasets/{repo}/...` for each `/api/models/{repo}/...` route,
`/datasets/{repo}/resolve/{revision}/{path}` for files, and
`/datasets/{repo}.git/info/lfs/objects/batch` for large uploads. The
dataset viewer's routes (`/api/datasets/{repo}/parquet`, `/croissant`)
answer `404`; see [Datasets](#datasets).

When the registry is configured to redirect downloads to S3, `resolve`
answers `302` with `Location` (a signed bucket URL), `X-Linked-Etag`,
`X-Linked-Size` and `X-Repo-Commit`, which is how the Hub itself describes
files served from its CDN. `huggingface_hub` is tested in this mode too.

Errors carry `X-Error-Code` (`RepoNotFound`, `RevisionNotFound`,
`EntryNotFound`) so clients raise their usual exceptions.

**Identifiers match upstream for mirrored models.** The revision id is the
upstream commit id. The `ETag` is the SHA-256 for LFS files and the git
blob id for other files, exactly as Hugging Face reports them. A client
cache filled from the registry is therefore interchangeable with one filled
from Hugging Face.

For models pushed with `modelctl push`, every file is reported the way the
Hub reports LFS files, with its SHA-256 as the `ETag`.

## Not supported

| | |
|---|---|
| **Xet uploads** | Large files from a client with `hf_xet` installed need `HF_HUB_DISABLE_XET=1`. See [Uploading](#uploading). |
| **Pull requests, uploading to a revision id** | Upload to a branch, or use `modelctl push`. |
| **Spaces** | Models and datasets only. `repo_type="space"` returns 404, and `create_repo` of one 400. |
| **The dataset viewer** (parquet conversion, row previews, croissant) | See [Datasets](#datasets) for what that means for `load_dataset`. |
| **Search and listing** (`list_models`, `list_datasets`, `GET /api/models`, `GET /api/datasets`) | Use `modelctl list` (with `--repo-type dataset` for datasets) or the web UI. |
| **Repository ids without a namespace** (`gpt2`) | Use the full id (`openai-community/gpt2`). When mirroring, the registry stores the repository under the canonical id the upstream reports. |
| **Case-insensitive ids** | Ids are matched exactly. |
| **git and git-lfs** | The registry is not a git server. `git clone` does not work. |
| **Xet transfer** | The registry does not advertise it, so clients use plain HTTP downloads. |
| **Commit history** (`list_repo_commits`) | Not implemented. |
| **Hub features**: discussions, pull requests, likes, collections, inference, access requests | Out of scope. |

A request for an unimplemented Hub route returns `404` with a JSON error
saying so; it does not pretend to succeed.

## Differences worth knowing

- **Gated and private upstream models become ordinary models** in your
  registry. Who can read them is decided by your registry's access
  settings, not by the upstream's. Make sure your use complies with the
  model's licence and access terms.
- **A mirror is a snapshot.** `main` in the registry points at the revision
  that was current when you last ran `modelctl mirror`, or when
  pull-through first fetched it.
- **The first download of a file through pull-through is sent while the
  registry fetches it.** `hf_hub_download` of a file the registry has
  never fetched succeeds on the first attempt even when the fetch takes
  longer than the client's download timeout, because bytes reach the
  client as they arrive from the upstream (tested in `test/e2e` with a
  slow upstream). The last mebibyte is held back until the file has
  matched its digest. If it does not match, the download fails and the
  client keeps nothing, and a client that had already kept part of the
  file does not complete it from a later, sound fetch: its request for
  the rest is answered with the whole file, which `huggingface_hub`
  takes in place of what it had (also in `test/e2e`). Details and limits
  are in [operations.md](operations.md#the-first-download-of-a-file).
- **A stalled upstream stalls the client.** The registry sends no filler,
  so if the upstream sends nothing for longer than the client's timeout
  (ten seconds between bytes for `huggingface_hub`), that request times
  out. The fetch carries on; download again, or fetch everything ahead of
  time with `modelctl mirror`.
- **With `--pull-through-stream=false` the first request for a large
  file can time out.** The registry then hands over a file only once it
  has fetched and verified all of it, so the client's first request gets
  nothing within its timeout (tested in `test/e2e`). What happens next
  depends on the client's version: `huggingface_hub` 1.24.0 gives up with
  a read timeout, and you run the download again once the fetch has
  finished; recent versions (2.4.0, and from about 1.31 on) retry a
  timed-out download a few times by themselves and succeed if the fetch
  finishes within those retries. The fetch carries on either way.
  Listings and file metadata answer immediately, and small files (tested
  in `test/e2e`, including against huggingface.co) arrive within the
  timeout.
- **Model cards are not rendered.** `README.md` is stored and served like
  any other file.
