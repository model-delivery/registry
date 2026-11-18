# Hugging Face compatibility

The registry implements the part of the Hugging Face Hub HTTP API that
clients use to **download models**. It does not implement the whole Hub
API, and this page lists exactly what is covered.

Compatibility is checked in CI by running the real `huggingface_hub`
Python library against the registry (`test/e2e`). The version tested is
whatever `pip install huggingface_hub` installs on the day; it was 1.24.0
when this was written.

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
| `snapshot_download` | By branch, tag and revision id |
| `hf_hub_download` | Including files in subdirectories and names with spaces |
| `HfApi.model_info` | `sha` and `siblings` are the registry's; other fields are passed through from the upstream for mirrored models |
| `HfApi.list_repo_files`, `list_repo_tree` | |
| `HfApi.list_repo_refs` | |
| Reading with `HF_TOKEN` from a registry that requires a token | And being refused without one |

Libraries that fetch files through `huggingface_hub` (`transformers`,
`diffusers`, `sentence-transformers` and others) use the calls above, so
they are expected to work. They are not tested individually yet. If one
does not work, please open an issue naming the library and the call.

## Routes

| Route | Notes |
|---|---|
| `GET /api/models/{repo}` | Same as revision `main` |
| `GET /api/models/{repo}/revision/{revision}` | |
| `GET /api/models/{repo}/tree/{revision}[/{directory}]` | `recursive` is honoured. All entries are returned in one response; there is no pagination |
| `GET /api/models/{repo}/refs` | |
| `HEAD`, `GET /{repo}/resolve/{revision}/{path}` | Sends `ETag`, `X-Repo-Commit`, `Content-Length`, `Accept-Ranges`; honours `Range` and `If-None-Match` |
| `GET /api/whoami-v2` | Reports the registry token's name |

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
| **Uploads from Hugging Face clients** (`upload_file`, `upload_folder`, `create_repo`, `hf upload`, `push_to_hub`) | Use `modelctl push` or the [REST API](api.md). Planned. |
| **Datasets and Spaces** | Only model repositories. `repo_type="dataset"` returns 404. |
| **Search and listing** (`list_models`, `GET /api/models`) | Use `modelctl list` or the web UI. |
| **Repository ids without a namespace** (`gpt2`) | Use the full id (`openai-community/gpt2`). When mirroring, the registry stores the repository under the canonical id the upstream reports. |
| **Case-insensitive ids** | Ids are matched exactly. |
| **git and git-lfs** | The registry is not a git server. `git clone` does not work. |
| **Xet transfer** | The registry does not advertise it, so clients use plain HTTP downloads. |
| **Commit history** (`list_repo_commits`), **file metadata** (`get_paths_info`) | Not implemented. |
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
- **The first pull-through request for a large model can time out.** The
  registry answers only once the whole revision is stored, and
  `huggingface_hub` waits about ten seconds for metadata. The fetch carries
  on after the client gives up; run the download again when it has
  finished, or warm the registry with `modelctl mirror`. Small models
  (tested in `test/e2e`) complete within the timeout.
- **Model cards are not rendered.** `README.md` is stored and served like
  any other file.
