// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// Datasets with the real huggingface_hub, and the real datasets library
// when it is installed. The upstream is a local registry; nothing leaves
// this machine.

// The dataset acme/reviews, and a model of the same name with other
// content. They share one file.
var (
	reviewsData = map[string]string{
		"README.md":           "# reviews\n",
		"data/train.csv":      "text,label\ngreat,1\nawful,0\nfine,1\n",
		"data/test.csv":       "text,label\nmeh,0\n",
		"data/extra file.txt": "a name with a space",
		"shared.txt":          "in both the model and the dataset",
	}
	reviewsModel = map[string]string{
		"config.json":       `{"model_type":"reviews"}`,
		"model.safetensors": strings.Repeat("\x00weights\xff", 50_000),
		"shared.txt":        "in both the model and the dataset",
	}
)

const reviews = "acme/reviews"

// datasetReadPy reads a dataset with every huggingface_hub call the
// documents claim for datasets, then the model of the same name, and
// reports what it got: each file by its SHA-256.
const datasetReadPy = `
import hashlib, json, os, sys
import huggingface_hub
from huggingface_hub import snapshot_download, hf_hub_download, HfApi
from huggingface_hub.hf_api import RepoFile, RepoFolder

repo, one_file, revision = sys.argv[1], sys.argv[2], sys.argv[3]
api = HfApi()

def digests(root):
    out = {}
    for dirpath, _, names in os.walk(root):
        for n in names:
            p = os.path.join(dirpath, n)
            rel = os.path.relpath(p, root).replace(os.sep, "/")
            if rel.startswith(".cache/"):
                continue
            with open(p, "rb") as f:
                out[rel] = hashlib.sha256(f.read()).hexdigest()
    return out

snap = snapshot_download(repo, repo_type="dataset", revision=revision)
single = hf_hub_download(repo, one_file, repo_type="dataset", revision=revision)
info = api.dataset_info(repo, revision=revision)
tree = list(api.list_repo_tree(repo, repo_type="dataset", revision=revision, recursive=True))
top = list(api.list_repo_tree(repo, repo_type="dataset", revision=revision))
refs = api.list_repo_refs(repo, repo_type="dataset")
paths = api.get_paths_info(repo, [one_file, "data", "no/such/file"], repo_type="dataset", revision=revision)
model_paths = api.get_paths_info(repo, ["config.json", "data/train.csv"])
model = snapshot_download(repo)
print(json.dumps({
    "version": huggingface_hub.__version__,
    "snapshot": os.path.basename(snap),
    "files": digests(snap),
    "single": open(single, "rb").read().decode(),
    "id": info.id,
    "sha": info.sha,
    "siblings": sorted(s.rfilename for s in info.siblings),
    "tree_files": {e.path: e.size for e in tree if isinstance(e, RepoFile)},
    "tree_folders": sorted(e.path for e in tree if isinstance(e, RepoFolder)),
    "top_level": sorted(e.path for e in top),
    "branches": {b.name: b.target_commit for b in refs.branches},
    "tags": {t.name: t.target_commit for t in refs.tags},
    "paths_info": [[p.path, "file" if isinstance(p, RepoFile) else "folder"] for p in paths],
    "model_paths_info": [p.path for p in model_paths],
    "model_files": digests(model),
}))
`

type datasetRead struct {
	Version        string            `json:"version"`
	Snapshot       string            `json:"snapshot"`
	Files          map[string]string `json:"files"`
	Single         string            `json:"single"`
	ID             string            `json:"id"`
	SHA            string            `json:"sha"`
	Siblings       []string          `json:"siblings"`
	TreeFiles      map[string]int64  `json:"tree_files"`
	TreeFolders    []string          `json:"tree_folders"`
	TopLevel       []string          `json:"top_level"`
	Branches       map[string]string `json:"branches"`
	Tags           map[string]string `json:"tags"`
	PathsInfo      [][]string        `json:"paths_info"`
	ModelPathsInfo []string          `json:"model_paths_info"`
	ModelFiles     map[string]string `json:"model_files"`
}

// sameFiles reports how got (path -> SHA-256) differs from want (path ->
// content), or "" when it does not.
func sameFiles(got map[string]string, want map[string]string) string {
	var diff []string
	for p, content := range want {
		if got[p] != kit.SHA256Hex(content) {
			diff = append(diff, p+" differs or is missing")
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			diff = append(diff, p+" was not expected")
		}
	}
	return strings.Join(diff, "; ")
}

// hubWithReviews is an upstream holding the dataset and the model
// acme/reviews, the dataset tagged v1.
func hubWithReviews(t *testing.T) (*regtest.Instance, client.PushResult, client.PushResult) {
	t.Helper()
	ctx := context.Background()
	hub := regtest.Start(t, server.Config{AnonymousRead: true})
	cl := client.New(hub.URL, hub.Write)
	data, err := cl.Push(ctx, regtest.WriteTree(t, reviewsData), client.RepoKey(client.Dataset, reviews), client.PushOptions{Branch: "main", Tags: []string{"v1"}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := cl.Push(ctx, regtest.WriteTree(t, reviewsModel), reviews, client.PushOptions{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return hub, data, model
}

// TestHubClientDatasetAgainstMirror mirrors a dataset and the model of the
// same name, takes the upstream away, and reads both with huggingface_hub
// and the hf command.
func TestHubClientDatasetAgainstMirror(t *testing.T) {
	python(t)
	ctx := context.Background()
	hub, data, model := hubWithReviews(t)
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.URL})
	cl := client.New(local.URL, local.Write)
	for key, want := range map[string]string{client.RepoKey(client.Dataset, reviews): data.SHA, reviews: model.SHA} {
		done, err := cl.Mirror(ctx, key, "main", "", nil)
		if err != nil || done.SHA != want {
			t.Fatalf("mirror %s: %+v, %v", key, done, err)
		}
	}
	if _, err := cl.Mirror(ctx, client.RepoKey(client.Dataset, reviews), "v1", "", nil); err != nil {
		t.Fatalf("mirror the tag: %v", err)
	}
	hub.StopServing()

	for _, rev := range []string{"main", data.SHA} {
		t.Run("at "+rev, func(t *testing.T) {
			stdout, stderr, err := runPython(t, local.URL, "", datasetReadPy, nil, reviews, "data/train.csv", rev)
			if err != nil {
				t.Fatalf("the script failed: %v\n%s", err, tail(stderr, 20))
			}
			var got datasetRead
			lastJSON(t, stdout, &got)
			t.Logf("huggingface_hub %s read dataset %s@%s", got.Version, reviews, got.SHA)
			if diff := sameFiles(got.Files, reviewsData); diff != "" || got.Snapshot != data.SHA {
				t.Errorf("snapshot_download(repo_type=dataset) in %s: %s", got.Snapshot, diff)
			}
			if got.Single != reviewsData["data/train.csv"] {
				t.Errorf("hf_hub_download(repo_type=dataset): %q", got.Single)
			}
			if got.ID != reviews || got.SHA != data.SHA || len(got.Siblings) != len(reviewsData) {
				t.Errorf("dataset_info: id %q sha %q siblings %v", got.ID, got.SHA, got.Siblings)
			}
			if len(got.TreeFiles) != len(reviewsData) || got.TreeFiles["data/train.csv"] != int64(len(reviewsData["data/train.csv"])) ||
				strings.Join(got.TreeFolders, ",") != "data" || strings.Join(got.TopLevel, ",") != "README.md,data,shared.txt" {
				t.Errorf("list_repo_tree: files %v folders %v top %v", got.TreeFiles, got.TreeFolders, got.TopLevel)
			}
			// The upstream's tag v1 is a tag here too, as it is for a model.
			if got.Branches["main"] != data.SHA || got.Tags["v1"] != data.SHA {
				t.Errorf("list_repo_refs: branches %v tags %v", got.Branches, got.Tags)
			}
			if len(got.PathsInfo) != 2 || got.PathsInfo[0][0] != "data/train.csv" || got.PathsInfo[1][1] != "folder" {
				t.Errorf("get_paths_info(repo_type=dataset): %v", got.PathsInfo)
			}
			// The model of the same name is another repository.
			if strings.Join(got.ModelPathsInfo, ",") != "config.json" {
				t.Errorf("get_paths_info of the model: %v", got.ModelPathsInfo)
			}
			if diff := sameFiles(got.ModelFiles, reviewsModel); diff != "" {
				t.Errorf("snapshot_download of the model of the same name: %s", diff)
			}
		})
	}

	t.Run("hf download --repo-type dataset", func(t *testing.T) {
		whole := t.TempDir()
		if _, stderr, err := runPython(t, local.URL, "", hfCommandPy, nil, "download", reviews, "--repo-type", "dataset", "--local-dir", whole); err != nil {
			t.Fatalf("hf download failed: %v\n%s", err, tail(stderr, 12))
		}
		for p, want := range reviewsData {
			if got, err := os.ReadFile(filepath.Join(whole, filepath.FromSlash(p))); err != nil || string(got) != want {
				t.Errorf("%s: %q, %v", p, got, err)
			}
		}
		if _, err := os.Stat(filepath.Join(whole, "config.json")); !os.IsNotExist(err) {
			t.Errorf("the model's file came with the dataset: %v", err)
		}
		one := t.TempDir()
		if _, stderr, err := runPython(t, local.URL, "", hfCommandPy, nil, "download", reviews, "data/test.csv", "--repo-type", "dataset", "--revision", "v1", "--local-dir", one); err != nil {
			t.Fatalf("hf download of one file at a tag failed: %v\n%s", err, tail(stderr, 12))
		}
		if got, err := os.ReadFile(filepath.Join(one, "data", "test.csv")); err != nil || string(got) != reviewsData["data/test.csv"] {
			t.Errorf("one file at a tag: %q, %v", got, err)
		}
	})
}

// datasetUploadPy uploads to a dataset the way a script and the hub do,
// then reads it back.
const datasetUploadPy = `
import hashlib, json, os, sys
from huggingface_hub import HfApi, snapshot_download

repo, folder = sys.argv[1], sys.argv[2]
api = HfApi()
url = api.create_repo(repo, repo_type="dataset", exist_ok=True)
again = api.create_repo(repo, repo_type="dataset", exist_ok=True)
first = api.upload_folder(folder_path=folder, repo_id=repo, repo_type="dataset", commit_message="first upload")
second = api.upload_file(path_or_fileobj=b"text,label\nnew,1\n", path_in_repo="data/validation.csv", repo_id=repo, repo_type="dataset")
snap = snapshot_download(repo, repo_type="dataset")
files = {}
for dirpath, _, names in os.walk(snap):
    for n in names:
        p = os.path.join(dirpath, n)
        with open(p, "rb") as f:
            files[os.path.relpath(p, snap).replace(os.sep, "/")] = hashlib.sha256(f.read()).hexdigest()
print(json.dumps({"repo_type": url.repo_type, "repo_id": url.repo_id, "revisions": [first.oid, second.oid], "files": files}))
`

// TestHubClientDatasetUpload uploads a dataset with upload_folder,
// upload_file and `hf upload --repo-type dataset`, one file of it large
// enough for the large-file protocol, and reads it back.
func TestHubClientDatasetUpload(t *testing.T) {
	python(t)
	ctx := context.Background()
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	env := []string{"HF_HUB_DISABLE_XET=1"}
	folder := map[string]string{"data/train.csv": "text,label\na,1\n", "data/big.parquet": largeContent}

	stdout, stderr, err := runPython(t, in.URL, in.Write, datasetUploadPy, env, "team/uploaded", regtest.WriteTree(t, folder))
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, tail(stderr, 20))
	}
	var got struct {
		RepoType  string            `json:"repo_type"`
		RepoID    string            `json:"repo_id"`
		Revisions []string          `json:"revisions"`
		Files     map[string]string `json:"files"`
	}
	lastJSON(t, stdout, &got)
	want := map[string]string{"data/train.csv": folder["data/train.csv"], "data/big.parquet": largeContent, "data/validation.csv": "text,label\nnew,1\n"}
	if got.RepoType != "dataset" || got.RepoID != "team/uploaded" || sameFiles(got.Files, want) != "" {
		t.Fatalf("create_repo gave %s %s; read back: %s", got.RepoType, got.RepoID, sameFiles(got.Files, want))
	}
	head, stored := revisionFiles(t, in, "datasets/team/uploaded", "main")
	if head != got.Revisions[1] || stored["data/big.parquet"] != largeContent {
		t.Fatalf("the registry holds %s at %s", paths(stored), head)
	}
	if _, err := in.Reg.GetRepo(ctx, "team/uploaded"); err == nil {
		t.Fatal("uploading a dataset created a model")
	}

	t.Run("hf upload --repo-type dataset", func(t *testing.T) {
		dir := regtest.WriteTree(t, map[string]string{"train.jsonl": `{"a": 1}` + "\n"})
		if _, stderr, err := runPython(t, in.URL, in.Write, hfCommandPy, env, "upload", "team/cli-data", dir, "--repo-type", "dataset"); err != nil {
			t.Fatalf("hf upload failed: %v\n%s", err, tail(stderr, 12))
		}
		one := filepath.Join(regtest.WriteTree(t, map[string]string{"notes.md": "# notes\n"}), "notes.md")
		if _, stderr, err := runPython(t, in.URL, in.Write, hfCommandPy, env, "upload", "team/cli-data", one, "docs/notes.md", "--repo-type", "dataset"); err != nil {
			t.Fatalf("hf upload of one file failed: %v\n%s", err, tail(stderr, 12))
		}
		_, stored := revisionFiles(t, in, "datasets/team/cli-data", "main")
		if paths(stored) != "docs/notes.md,train.jsonl" {
			t.Fatalf("after hf upload: %s", paths(stored))
		}
		down := t.TempDir()
		if _, stderr, err := runPython(t, in.URL, "", hfCommandPy, nil, "download", "team/cli-data", "--repo-type", "dataset", "--local-dir", down); err != nil {
			t.Fatalf("hf download failed: %v\n%s", err, tail(stderr, 12))
		}
		if b, err := os.ReadFile(filepath.Join(down, "docs", "notes.md")); err != nil || string(b) != "# notes\n" {
			t.Fatalf("downloaded back: %q, %v", b, err)
		}
	})
}

// recorder is a registry behind a handler that notes every request it is
// asked, as the client sent it.
type recorder struct {
	url   string
	reg   *registry.Registry
	write string // a token with the write role

	mu   sync.Mutex
	seen []string // "METHOD host path?query -> status"
	gets map[string]int
	// firstByte is when the first byte of a 2xx body went to a GET whose
	// path ends with watch.
	watch     string
	firstByte time.Time
}

type recordingWriter struct {
	http.ResponseWriter
	status int
	wrote  func(n int)
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.wrote(n)
	return n, err
}

func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func startRecorder(t *testing.T, cfg server.Config, watch string) *recorder {
	t.Helper()
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AnonymousRead = true
	handler := server.New(reg, cfg)
	rec := &recorder{reg: reg, gets: map[string]int{}, watch: watch}
	if rec.write, err = reg.CreateToken(context.Background(), "write", registry.RoleWrite); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recordingWriter{ResponseWriter: w}
		rw.wrote = func(n int) {
			if n > 0 && r.Method == http.MethodGet && watch != "" && strings.HasSuffix(r.URL.Path, watch) && rw.status/100 == 2 {
				rec.mu.Lock()
				if rec.firstByte.IsZero() {
					rec.firstByte = time.Now()
				}
				rec.mu.Unlock()
			}
		}
		rec.mu.Lock()
		if r.Method == http.MethodGet {
			rec.gets[r.URL.Path]++
		}
		rec.mu.Unlock()
		handler.ServeHTTP(rw, r)
		rec.mu.Lock()
		rec.seen = append(rec.seen, r.Method+" "+r.Host+" "+r.URL.RequestURI()+" -> "+http.StatusText(rw.status))
		rec.mu.Unlock()
	}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = handler.Shutdown(ctx)
		srv.Close()
		_ = reg.Close()
	})
	rec.url = srv.URL
	return rec
}

func (r *recorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *recorder) getsOf(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets[path]
}

func (r *recorder) firstByteAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstByte
}

// fetchDatasetPy downloads one file of a dataset with hf_hub_download.
const fetchDatasetPy = `
import hashlib, json, sys
from huggingface_hub import hf_hub_download
path = hf_hub_download(sys.argv[1], sys.argv[2], repo_type="dataset")
with open(path, "rb") as f:
    print(json.dumps({"sha256": hashlib.sha256(f.read()).hexdigest()}))
`

// TestHubClientDatasetPullThrough points huggingface_hub at an empty
// registry with pull-through on and asks for a dataset, including a file
// the upstream takes about three seconds to send against a client timeout
// of one: it arrives in one request, sent while it is fetched.
func TestHubClientDatasetPullThrough(t *testing.T) {
	python(t)
	ctx := context.Background()
	big := numbered(8 * mib)
	hub := regtest.Start(t, server.Config{AnonymousRead: true})
	files := map[string]string{"data/train.csv": reviewsData["data/train.csv"], "data/big.parquet": big}
	pushed, err := client.New(hub.URL, hub.Write).Push(ctx, regtest.WriteTree(t, files), client.RepoKey(client.Dataset, reviews), client.PushOptions{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// The upstream sends the large file a quarter of a mebibyte every tenth
	// of a second, and notes when it has sent the last byte.
	target, _ := url.Parse(hub.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var finishedMu sync.Mutex
	var finished time.Time
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data/big.parquet") {
			if sendPaced(w, r, big, mib/4, 100*time.Millisecond) {
				finishedMu.Lock()
				finished = time.Now()
				finishedMu.Unlock()
			}
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	local := startRecorder(t, server.Config{Upstream: front.URL, PullThrough: true}, "/data/big.parquet")
	stdout, stderr, err := runPython(t, local.url, "", fetchDatasetPy, []string{"HF_HUB_DOWNLOAD_TIMEOUT=1"}, reviews, "data/big.parquet")
	if err != nil {
		t.Fatalf("hf_hub_download of the large file: %v\n%s", err, tail(stderr, 20))
	}
	var got struct {
		SHA256 string `json:"sha256"`
	}
	lastJSON(t, stdout, &got)
	if got.SHA256 != kit.SHA256Hex(big) {
		t.Fatalf("the client ended with sha256 %s, want %s", got.SHA256, kit.SHA256Hex(big))
	}
	if n := local.getsOf("/datasets/" + reviews + "/resolve/main/data/big.parquet"); n != 1 {
		t.Errorf("the registry received %d GETs for the file, want one", n)
	}
	finishedMu.Lock()
	done := finished
	finishedMu.Unlock()
	if first := local.firstByteAt(); first.IsZero() || done.IsZero() || done.Sub(first) < time.Second {
		t.Errorf("the first byte was sent at %v and the upstream finished at %v: not while it was fetched", first, done)
	}
	repo, err := local.reg.GetRepo(ctx, client.RepoKey(client.Dataset, reviews))
	if err != nil || repo.Type != registry.TypeDataset || repo.Upstream != front.URL {
		t.Fatalf("recorded %+v, %v", repo, err)
	}

	// With the upstream gone, the file that was fetched is served from the
	// store.
	front.Close()
	stdout, _, err = runPython(t, local.url, "", fetchDatasetPy, nil, reviews, "data/big.parquet")
	if err != nil {
		t.Fatalf("the stored file with the upstream gone: %v", err)
	}
	lastJSON(t, stdout, &got)
	if got.SHA256 != kit.SHA256Hex(big) {
		t.Fatalf("served from the store: sha256 %s", got.SHA256)
	}
	if c, err := local.reg.Resolve(ctx, client.RepoKey(client.Dataset, reviews), "main"); err != nil || c.SHA != pushed.SHA {
		t.Fatalf("the revision: %+v, %v", c, err)
	}
}

// datasetsLibrary is the Python interpreter, when it has the datasets
// library; the test is skipped, saying why, when it has not.
func datasetsLibrary(t *testing.T) string {
	t.Helper()
	py := python(t)
	if err := exec.Command(py, "-c", "import datasets").Run(); err != nil {
		t.Skip("the datasets library is not installed (pip install datasets); load_dataset is not tested")
	}
	return py
}

// loadDatasetPy loads a dataset by its id alone, as a user does, and
// reports each split's row count and first row.
const loadDatasetPy = `
import json, sys
import datasets
from datasets import load_dataset
ds = load_dataset(sys.argv[1])
print(json.dumps({
    "version": datasets.__version__,
    "splits": {name: split.num_rows for name, split in ds.items()},
    "first": {name: split[0] for name, split in ds.items()},
}, default=str))
`

// writeParquetPy writes a train and a test split as parquet into a folder.
const writeParquetPy = `
import sys
import pyarrow as pa, pyarrow.parquet as pq
out = sys.argv[1]
pq.write_table(pa.table({"text": ["red", "green", "blue", "cyan"], "label": [0, 1, 2, 3]}), out + "/train.parquet")
pq.write_table(pa.table({"text": ["black", "white"], "label": [4, 5]}), out + "/test.parquet")
`

// TestLoadDatasetAgainstMirror runs the datasets library's load_dataset on
// mirrored datasets of csv, jsonl and parquet files, split into train and
// test by file name, with the upstream gone. The library also asks the
// Hub's dataset viewer for exported dataset infos at the datasets-server
// subdomain of HF_ENDPOINT; the request is sent to the registry through a
// proxy, as wildcard DNS would, and the registry's 404 makes the library
// read the repository's own files.
func TestLoadDatasetAgainstMirror(t *testing.T) {
	py := datasetsLibrary(t)
	ctx := context.Background()

	parquet := t.TempDir()
	if out, err := exec.Command(py, "-c", writeParquetPy, parquet).CombinedOutput(); err != nil {
		t.Fatalf("writing the parquet files: %v\n%s", err, out)
	}
	readFile := func(name string) string {
		b, err := os.ReadFile(filepath.Join(parquet, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	layouts := map[string]map[string]string{
		"acme/csv-splits": {
			"README.md": "# csv\n",
			"train.csv": "text,label\nhello,1\nbye,0\nmaybe,1\n",
			"test.csv":  "text,label\nyes,1\nno,0\n",
		},
		"acme/jsonl-splits": {
			"train.jsonl": `{"text": "one", "label": 1}` + "\n" + `{"text": "two", "label": 2}` + "\n",
			"test.jsonl":  `{"text": "three", "label": 3}` + "\n",
		},
		"acme/parquet-splits": {
			"train.parquet": readFile("train.parquet"),
			"test.parquet":  readFile("test.parquet"),
		},
	}
	want := map[string]struct {
		train, test int
		firstTrain  string
	}{
		"acme/csv-splits":     {3, 2, "hello"},
		"acme/jsonl-splits":   {2, 1, "one"},
		"acme/parquet-splits": {4, 2, "red"},
	}

	hub := regtest.Start(t, server.Config{AnonymousRead: true})
	for name, files := range layouts {
		if _, err := client.New(hub.URL, hub.Write).Push(ctx, regtest.WriteTree(t, files), client.RepoKey(client.Dataset, name), client.PushOptions{Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	local := startRecorder(t, server.Config{Upstream: hub.URL}, "")
	for name := range layouts {
		if _, err := client.New(local.url, local.write).Mirror(ctx, client.RepoKey(client.Dataset, name), "main", "", nil); err != nil {
			t.Fatalf("mirror %s: %v", name, err)
		}
	}
	hub.StopServing()

	for name, w := range want {
		t.Run(name, func(t *testing.T) {
			// The viewer request goes to http://datasets-server.127.0.0.1:port,
			// which the proxy delivers to the registry; the registry itself
			// is reached directly.
			env := []string{"HTTP_PROXY=" + local.url, "http_proxy=" + local.url}
			stdout, stderr, err := runPython(t, local.url, "", loadDatasetPy, env, name)
			if err != nil {
				t.Fatalf("load_dataset(%q) failed: %v\n%s", name, err, tail(stderr, 25))
			}
			var got struct {
				Version string                    `json:"version"`
				Splits  map[string]int            `json:"splits"`
				First   map[string]map[string]any `json:"first"`
			}
			lastJSON(t, stdout, &got)
			t.Logf("datasets %s loaded %s: %v", got.Version, name, got.Splits)
			if got.Splits["train"] != w.train || got.Splits["test"] != w.test || len(got.Splits) != 2 {
				t.Errorf("splits %v, want train %d and test %d", got.Splits, w.train, w.test)
			}
			if got.First["train"]["text"] != w.firstTrain {
				t.Errorf("the first row of train is %v, want text %q", got.First["train"], w.firstTrain)
			}
		})
	}

	// What the library asked of the registry, for the record and for two
	// checks: it asked the viewer and was told 404, and it read the files.
	asked := local.requests()
	for _, line := range asked {
		t.Log(line)
	}
	viewer := false
	for _, line := range asked {
		if strings.Contains(line, " datasets-server.") && strings.Contains(line, "/info?dataset=") {
			viewer = true
			if !strings.HasSuffix(line, "-> Not Found") {
				t.Errorf("the viewer request was answered: %s", line)
			}
		}
	}
	if !viewer {
		t.Error("the library did not ask the dataset viewer through the proxy; the fallback was not exercised")
	}
}
