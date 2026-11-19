// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// Datasets against the real huggingface.co. Each test reads a dataset
// twice with the same script: once straight from huggingface.co, which
// gives the expected answer, and once through the registry with every
// other host blocked. Nothing about the datasets is written down here, so
// the tests keep passing when their owners change them.
//
// The two datasets have the two layouts most datasets on the Hub have,
// and are small (under 1 MB each):
//
//   - cornell-movie-review-data/rotten_tomatoes: train.parquet,
//     validation.parquet and test.parquet at the top, and a dataset card
//     that declares no configs. The splits come from the file names.
//   - google-research-datasets/mbpp: two configs, full (the default) and
//     sanitized, each a directory of files named after their split
//     (full/train-00000-of-00001.parquet), declared in the dataset card's
//     YAML; one split is called prompt. This is how push_to_hub writes a
//     dataset, so it is how most popular datasets are laid out.
//
// Their parquet files are stored in Xet on huggingface.co.
// REGISTRY_E2E_DATASET names one more dataset to mirror and load the same
// way, such as stanfordnlp/imdb (84 MB).
const (
	flatDataset   = "cornell-movie-review-data/rotten_tomatoes"
	configDataset = "google-research-datasets/mbpp"
)

// fromHub is the environment for a script that reads from huggingface.co
// itself: HF_ENDPOINT and the proxies unset again.
var fromHub = []string{
	"HF_ENDPOINT=https://huggingface.co",
	"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=",
}

func needsNetwork(t *testing.T) {
	t.Helper()
	if os.Getenv("REGISTRY_E2E_NETWORK") != "1" {
		t.Skip("set REGISTRY_E2E_NETWORK=1 to read datasets from huggingface.co")
	}
}

// hubDatasetPy reads a dataset with huggingface_hub and reports what it
// got: the revision, every file by size and SHA-256, the tree.
const hubDatasetPy = `
import hashlib, json, os, sys
import huggingface_hub
from huggingface_hub import snapshot_download, HfApi
from huggingface_hub.hf_api import RepoFile, RepoFolder

repo = sys.argv[1]
api = HfApi()
info = api.dataset_info(repo, files_metadata=True)
snap = snapshot_download(repo, repo_type="dataset", revision=info.sha)
files = {}
for dirpath, _, names in os.walk(snap):
    for n in names:
        p = os.path.join(dirpath, n)
        rel = os.path.relpath(p, snap).replace(os.sep, "/")
        if rel.startswith(".cache/"):
            continue
        h = hashlib.sha256()
        with open(p, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                h.update(chunk)
        files[rel] = h.hexdigest()
tree = list(api.list_repo_tree(repo, repo_type="dataset", revision=info.sha, recursive=True))
top = list(api.list_repo_tree(repo, repo_type="dataset", revision=info.sha))
print(json.dumps({
    "version": huggingface_hub.__version__,
    "id": info.id,
    "sha": info.sha,
    "snapshot": os.path.basename(snap),
    "siblings": {s.rfilename: s.size for s in info.siblings},
    "files": files,
    "tree_files": {e.path: e.size for e in tree if isinstance(e, RepoFile)},
    "tree_folders": sorted(e.path for e in tree if isinstance(e, RepoFolder)),
    "top_level": sorted(e.path for e in top),
}))
`

type hubDataset struct {
	Version     string            `json:"version"`
	ID          string            `json:"id"`
	SHA         string            `json:"sha"`
	Snapshot    string            `json:"snapshot"`
	Siblings    map[string]int64  `json:"siblings"`
	Files       map[string]string `json:"files"`
	TreeFiles   map[string]int64  `json:"tree_files"`
	TreeFolders []string          `json:"tree_folders"`
	TopLevel    []string          `json:"top_level"`
}

// loadSplitsPy loads a dataset by its id alone and reports each split's
// row count and columns.
const loadSplitsPy = `
import json, sys
import datasets
from datasets import load_dataset
ds = load_dataset(sys.argv[1])
print(json.dumps({
    "version": datasets.__version__,
    "splits": {name: split.num_rows for name, split in ds.items()},
    "columns": {name: sorted(split.column_names) for name, split in ds.items()},
}))
`

type loadedSplits struct {
	Version string              `json:"version"`
	Splits  map[string]int      `json:"splits"`
	Columns map[string][]string `json:"columns"`
}

// runOrFail runs a script and decodes what it printed into v.
func runOrFail(t *testing.T, endpoint, script string, env []string, v any, args ...string) {
	t.Helper()
	stdout, stderr, err := runPython(t, endpoint, "", script, env, args...)
	if err != nil {
		t.Fatalf("against %s: %v\n%s", endpoint, err, tail(stderr, 30))
	}
	lastJSON(t, stdout, v)
}

// TestMirrorDatasetFromHuggingFace mirrors each dataset from huggingface.co
// and reads it through the registry with huggingface_hub, and then with
// load_dataset, comparing everything with what huggingface.co gives.
func TestMirrorDatasetFromHuggingFace(t *testing.T) {
	needsNetwork(t)
	python(t)
	ctx := context.Background()
	local := regtest.Start(t, server.Config{AnonymousRead: true})
	repos := []string{flatDataset, configDataset}
	if extra := os.Getenv("REGISTRY_E2E_DATASET"); extra != "" {
		repos = append(repos, extra) // stanfordnlp/imdb, say: larger, so not by default
	}
	for _, repo := range repos {
		t.Run(repo, func(t *testing.T) {
			var want hubDataset
			runOrFail(t, "https://huggingface.co", hubDatasetPy, fromHub, &want, repo)

			done, err := client.New(local.URL, local.Write).Mirror(ctx, client.RepoKey(client.Dataset, repo), "main", "", nil)
			if err != nil {
				t.Fatalf("mirror: %v", err)
			}
			t.Logf("mirrored %s@%s: %d files, %d bytes", done.Repo, done.SHA, done.Files, done.Bytes)
			if done.SHA != want.SHA {
				t.Fatalf("mirror recorded revision %s, huggingface.co reports %s", done.SHA, want.SHA)
			}

			t.Run("huggingface_hub", func(t *testing.T) {
				var got hubDataset
				runOrFail(t, local.URL, hubDatasetPy, nil, &got, repo)
				t.Logf("huggingface_hub %s read %s@%s: %d files", got.Version, got.ID, got.SHA, len(got.Files))
				if got.ID != want.ID || got.SHA != want.SHA || got.Snapshot != want.SHA {
					t.Errorf("dataset_info: id %q sha %q snapshot %q; huggingface.co: id %q sha %q", got.ID, got.SHA, got.Snapshot, want.ID, want.SHA)
				}
				if !maps.Equal(got.Siblings, want.Siblings) {
					t.Errorf("dataset_info siblings with sizes:\n got %v\nwant %v", got.Siblings, want.Siblings)
				}
				if !maps.Equal(got.Files, want.Files) || len(got.Files) == 0 {
					t.Errorf("snapshot_download(repo_type=dataset), path -> sha256:\n got %v\nwant %v", got.Files, want.Files)
				}
				if !maps.Equal(got.TreeFiles, want.TreeFiles) {
					t.Errorf("list_repo_tree(recursive=True) files:\n got %v\nwant %v", got.TreeFiles, want.TreeFiles)
				}
				if !slices.Equal(got.TreeFolders, want.TreeFolders) || !slices.Equal(got.TopLevel, want.TopLevel) {
					t.Errorf("list_repo_tree: folders %v top %v; huggingface.co: folders %v top %v", got.TreeFolders, got.TopLevel, want.TreeFolders, want.TopLevel)
				}
			})

			t.Run("load_dataset", func(t *testing.T) {
				datasetsLibrary(t)
				var want, got loadedSplits
				runOrFail(t, "https://huggingface.co", loadSplitsPy, fromHub, &want, repo)
				runOrFail(t, local.URL, loadSplitsPy, nil, &got, repo)
				t.Logf("datasets %s: huggingface.co %v, the registry %v", got.Version, want.Splits, got.Splits)
				if len(want.Splits) == 0 || !maps.Equal(got.Splits, want.Splits) {
					t.Errorf("split row counts: the registry %v, huggingface.co %v", got.Splits, want.Splits)
				}
				if !maps.EqualFunc(got.Columns, want.Columns, slices.Equal) {
					t.Errorf("columns: the registry %v, huggingface.co %v", got.Columns, want.Columns)
				}
			})
		})
	}
}

// TestPullThroughDatasetFromHuggingFace points the clients at an empty
// registry with pull-through on: load_dataset of the dataset with configs,
// which reads the default config only, and hf_hub_download of one file of
// the other. The registry fetches what was asked for and nothing else.
func TestPullThroughDatasetFromHuggingFace(t *testing.T) {
	needsNetwork(t)
	python(t)
	ctx := context.Background()

	t.Run("load_dataset "+configDataset, func(t *testing.T) {
		datasetsLibrary(t)
		local := regtest.Start(t, server.Config{AnonymousRead: true, PullThrough: true})
		var want, got loadedSplits
		runOrFail(t, "https://huggingface.co", loadSplitsPy, fromHub, &want, configDataset)
		runOrFail(t, local.URL, loadSplitsPy, nil, &got, configDataset)
		t.Logf("huggingface.co %v, the registry %v", want.Splits, got.Splits)
		if len(want.Splits) == 0 || !maps.Equal(got.Splits, want.Splits) {
			t.Errorf("split row counts: the registry %v, huggingface.co %v", got.Splits, want.Splits)
		}
		key := client.RepoKey(client.Dataset, configDataset)
		files, err := local.Reg.Files(ctx, mustResolve(t, local, key, "main"))
		if err != nil {
			t.Fatal(err)
		}
		var fetched, pending []string
		for _, f := range files {
			if f.Pending {
				pending = append(pending, f.Path)
			} else {
				fetched = append(fetched, f.Path)
			}
		}
		t.Logf("fetched %v; still pending %v", fetched, pending)
		// The dataset card and the default config's files are read; the
		// other config's are not.
		for _, p := range fetched {
			if strings.HasPrefix(p, "sanitized/") {
				t.Errorf("%s was fetched although load_dataset reads the default config only", p)
			}
		}
		if !slices.Contains(fetched, "README.md") || !slices.ContainsFunc(fetched, func(p string) bool { return strings.HasPrefix(p, "full/") }) {
			t.Errorf("fetched %v: want README.md and the files of the default config", fetched)
		}
		if !slices.ContainsFunc(pending, func(p string) bool { return strings.HasPrefix(p, "sanitized/") }) {
			t.Errorf("pending %v: want the other config's files listed and not fetched", pending)
		}
	})

	t.Run("hf_hub_download "+flatDataset, func(t *testing.T) {
		local := regtest.Start(t, server.Config{AnonymousRead: true, PullThrough: true})
		var got struct {
			SHA256 string `json:"sha256"`
		}
		runOrFail(t, local.URL, fetchDatasetPy, nil, &got, flatDataset, "validation.parquet")
		files, err := local.Reg.Files(ctx, mustResolve(t, local, client.RepoKey(client.Dataset, flatDataset), "main"))
		if err != nil {
			t.Fatal(err)
		}
		stored := 0
		for _, f := range files {
			if f.Pending {
				continue
			}
			stored++
			if f.Path != "validation.parquet" {
				t.Errorf("%s was fetched although nobody asked for it", f.Path)
			} else if f.SHA256 != got.SHA256 {
				t.Errorf("the client got sha256 %s, the registry stored %s", got.SHA256, f.SHA256)
			}
		}
		if stored != 1 || len(files) < 2 {
			t.Errorf("%d of %d files fetched, want only validation.parquet", stored, len(files))
		}
	})
}
