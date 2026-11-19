// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package e2e proves the registry against the real huggingface_hub Python
// library. Run with: go test -tags=integration ./test/e2e/
//
// It needs Python with huggingface_hub installed (set REGISTRY_E2E_PYTHON
// to pick an interpreter). The test that mirrors from huggingface.co also
// needs the network and only runs when REGISTRY_E2E_NETWORK=1.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// The registry each test runs against comes from internal/regtest, which
// the server's own tests use too.

func python(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("REGISTRY_E2E_PYTHON"), "python3", "python"}
	if runtime.GOOS == "windows" {
		candidates = []string{os.Getenv("REGISTRY_E2E_PYTHON"), "python", "py"}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if err := exec.Command(c, "-c", "import huggingface_hub").Run(); err == nil {
			return c
		}
	}
	t.Skip("no Python with huggingface_hub found; pip install huggingface_hub or set REGISTRY_E2E_PYTHON")
	return ""
}

// download.py uses only the public huggingface_hub API, exactly as a user
// script would, and reports what it got.
const downloadPy = `
import json, sys
import huggingface_hub
from huggingface_hub import snapshot_download, hf_hub_download, HfApi
from huggingface_hub.hf_api import RepoFile, RepoFolder

repo, revision, one_file = sys.argv[1], sys.argv[2], sys.argv[3]
snapshot = snapshot_download(repo, revision=revision)
single = hf_hub_download(repo, one_file, revision=revision)
api = HfApi()
info = api.model_info(repo, revision=revision)
refs = api.list_repo_refs(repo)
tree = list(api.list_repo_tree(repo, revision=revision, recursive=True))
top = list(api.list_repo_tree(repo, revision=revision))
print(json.dumps({
    "version": huggingface_hub.__version__,
    "snapshot": snapshot,
    "single": single,
    "sha": info.sha,
    "siblings": sorted(s.rfilename for s in info.siblings),
    "listed": sorted(api.list_repo_files(repo, revision=revision)),
    "tree_files": {e.path: e.size for e in tree if isinstance(e, RepoFile)},
    "tree_folders": sorted(e.path for e in tree if isinstance(e, RepoFolder)),
    "top_level": sorted(e.path for e in top),
    "branches": {b.name: b.target_commit for b in refs.branches},
    "tags": {t.name: t.target_commit for t in refs.tags},
}))
`

type pyResult struct {
	Version  string   `json:"version"`
	Snapshot string   `json:"snapshot"`
	Single   string   `json:"single"`
	SHA      string   `json:"sha"`
	Siblings []string `json:"siblings"`
	Listed   []string `json:"listed"`
	// What list_repo_tree returned: with recursive=True, every file with its
	// size and every directory; without, the entries at the top.
	TreeFiles   map[string]int64  `json:"tree_files"`
	TreeFolders []string          `json:"tree_folders"`
	TopLevel    []string          `json:"top_level"`
	Branches    map[string]string `json:"branches"`
	Tags        map[string]string `json:"tags"`
}

// hfDownload runs the script with HF_ENDPOINT pointed at endpoint and a
// fresh cache. Every proxy variable points at a dead port, so any attempt
// to reach a host other than the local registry fails loudly.
func hfDownload(t *testing.T, endpoint, repo, revision, oneFile string) pyResult {
	t.Helper()
	res, stderr, err := runDownload(t, endpoint, "", repo, revision, oneFile)
	if err != nil {
		t.Fatalf("huggingface_hub download failed: %v\n%s", err, stderr)
	}
	t.Logf("huggingface_hub %s downloaded %s@%s", res.Version, repo, res.SHA)
	return res
}

// runDownload is hfDownload with an access token, returning the failure
// instead of ending the test.
func runDownload(t *testing.T, endpoint, token, repo, revision, oneFile string) (pyResult, string, error) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "download.py")
	if err := os.WriteFile(script, []byte(downloadPy), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python(t), script, repo, revision, oneFile)
	cmd.Env = append(os.Environ(),
		"HF_ENDPOINT="+endpoint,
		"HF_HOME="+t.TempDir(),
		"HF_HUB_DISABLE_TELEMETRY=1",
		"HF_HUB_DISABLE_PROGRESS_BARS=1",
		"HF_HUB_DISABLE_SYMLINKS_WARNING=1",
		"HF_TOKEN="+token,
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9",
		"http_proxy=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9",
		"NO_PROXY="+reachable(), "no_proxy="+reachable(),
	)
	if runtime.GOOS == "windows" {
		// Creating symlinks on Windows needs a privilege most accounts lack,
		// and the client's own probe for it is unreliable when downloads run
		// in parallel. Have it copy files into the snapshot folder instead;
		// what the registry is asked for is the same either way.
		cmd.Env = append(cmd.Env, "HF_HUB_DISABLE_SYMLINKS=1")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return pyResult{}, stderr.String(), err
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var res pyResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("unreadable script output %q: %v", stdout.String(), err)
	}
	return res, stderr.String(), nil
}

// reachable lists the hosts the Python client may contact directly. Every
// other host goes to the dead proxy. A real S3 service is included when
// one is configured, because a redirected download fetches from it.
func reachable() string {
	hosts := "127.0.0.1,localhost"
	if u, err := url.Parse(os.Getenv("REGISTRY_TEST_S3_ENDPOINT")); err == nil && u.Hostname() != "" {
		hosts += "," + u.Hostname()
	}
	return hosts
}

// checkSnapshot compares what huggingface_hub wrote to disk with what the
// registry says the revision contains.
func checkSnapshot(t *testing.T, in *regtest.Instance, repo, wantSHA string, res pyResult) {
	t.Helper()
	if res.SHA != wantSHA {
		t.Errorf("huggingface_hub saw revision %s, want %s", res.SHA, wantSHA)
	}
	// The client names the snapshot folder after the revision id.
	if filepath.Base(res.Snapshot) != wantSHA {
		t.Errorf("snapshot folder is %s, want it named after revision %s", res.Snapshot, wantSHA)
	}
	c, err := in.Reg.Resolve(context.Background(), repo, wantSHA)
	if err != nil {
		t.Fatal(err)
	}
	files, err := in.Reg.Files(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Listed) != len(files) {
		t.Errorf("list_repo_files returned %d files, registry has %d", len(res.Listed), len(files))
	}
	if len(res.Siblings) != len(files) {
		t.Errorf("client lists %d files, registry has %d", len(res.Siblings), len(files))
	}
	// list_repo_tree: every file with its size, every directory that holds
	// one, and at the top level the first segment of each path.
	folders, top := map[string]bool{}, map[string]bool{}
	if len(res.TreeFiles) != len(files) {
		t.Errorf("list_repo_tree returned %d files, registry has %d", len(res.TreeFiles), len(files))
	}
	for _, f := range files {
		got := regtest.FileSHA256(t, filepath.Join(res.Snapshot, filepath.FromSlash(f.Path)))
		if got != f.SHA256 {
			t.Errorf("%s: downloaded sha256 %s, registry has %s", f.Path, got, f.SHA256)
		}
		if size, listed := res.TreeFiles[f.Path]; !listed || size != f.Size {
			t.Errorf("%s: list_repo_tree says size %d (listed: %v), registry has %d", f.Path, size, listed, f.Size)
		}
		segs := strings.Split(f.Path, "/")
		top[segs[0]] = true
		for i := 1; i < len(segs); i++ {
			folders[strings.Join(segs[:i], "/")] = true
		}
	}
	if got, want := strings.Join(res.TreeFolders, ","), strings.Join(slices.Sorted(maps.Keys(folders)), ","); got != want {
		t.Errorf("list_repo_tree directories: %s, want %s", got, want)
	}
	if got, want := strings.Join(res.TopLevel, ","), strings.Join(slices.Sorted(maps.Keys(top)), ","); got != want {
		t.Errorf("list_repo_tree at the top level: %s, want %s", got, want)
	}
	if _, err := os.Stat(res.Single); err != nil {
		t.Errorf("hf_hub_download result missing: %v", err)
	}
}

// TestHfDownloadCommand runs the actual "hf download" command: a whole
// repository, then one file at a tag.
func TestHfDownloadCommand(t *testing.T) {
	python(t)
	files := map[string]string{
		"config.json":         `{"model_type":"tiny"}`,
		"model.safetensors":   strings.Repeat("\x00weights\xff", 200_000),
		"tokenizer/vocab.txt": "a\nb\nc\n",
	}
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	if _, err := client.New(in.URL, in.Write).Push(context.Background(), regtest.WriteTree(t, files), "team/own-model",
		client.PushOptions{Branch: "main", Tags: []string{"v1.0"}}); err != nil {
		t.Fatal(err)
	}
	hf := func(args ...string) {
		t.Helper()
		if _, stderr, err := runPython(t, in.URL, "", hfCommandPy, nil, args...); err != nil {
			t.Fatalf("hf %s failed: %v\n%s", strings.Join(args, " "), err, tail(stderr, 12))
		}
	}

	whole := t.TempDir()
	hf("download", "team/own-model", "--local-dir", whole)
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(whole, filepath.FromSlash(p)))
		if err != nil || string(got) != want {
			t.Errorf("%s after hf download: %d bytes, %v; want the %d bytes pushed", p, len(got), err, len(want))
		}
	}

	one := t.TempDir()
	hf("download", "team/own-model", "tokenizer/vocab.txt", "--revision", "v1.0", "--local-dir", one)
	if got, err := os.ReadFile(filepath.Join(one, "tokenizer", "vocab.txt")); err != nil || string(got) != files["tokenizer/vocab.txt"] {
		t.Errorf("one file at a tag: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(one, "config.json")); !os.IsNotExist(err) {
		t.Errorf("asking for one file fetched another: %v", err)
	}
}

// TestHubClientAgainstMirror is the offline demonstration, without the
// internet: mirror a repository from one hub into the registry, remove the
// hub, then download with stock huggingface_hub.
func TestHubClientAgainstMirror(t *testing.T) {
	python(t)
	ctx := context.Background()
	files := map[string]string{
		"config.json":            `{"model_type":"tiny"}`,
		"README.md":              "# tiny\n",
		"model.safetensors":      strings.Repeat("\x00weights\xff", 200_000),
		"tokenizer/vocab.txt":    "a\nb\nc\n",
		"onnx/model with sp.bin": "space in the name",
	}
	dir := t.TempDir()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	hub := regtest.Start(t, server.Config{AnonymousRead: true})
	pushed, err := client.New(hub.URL, hub.Write).Push(ctx, dir, "acme/tiny", client.PushOptions{Branch: "main", Tags: []string{"v1.0"}})
	if err != nil {
		t.Fatal(err)
	}

	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.URL})
	done, err := client.New(local.URL, local.Write).Mirror(ctx, "acme/tiny", "main", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if done.SHA != pushed.SHA {
		t.Fatalf("mirror recorded revision %s, upstream is %s", done.SHA, pushed.SHA)
	}
	hub.StopServing() // the upstream is gone

	t.Run("branch", func(t *testing.T) {
		checkSnapshot(t, local, "acme/tiny", pushed.SHA, hfDownload(t, local.URL, "acme/tiny", "main", "config.json"))
	})
	t.Run("pinned revision id", func(t *testing.T) {
		checkSnapshot(t, local, "acme/tiny", pushed.SHA, hfDownload(t, local.URL, "acme/tiny", pushed.SHA, "tokenizer/vocab.txt"))
	})
}

// TestHubClientAgainstPush covers repositories pushed straight to the
// registry, including a tag.
func TestHubClientAgainstPush(t *testing.T) {
	python(t)
	ctx := context.Background()
	dir := t.TempDir()
	for p, content := range map[string]string{"config.json": "{}", "weights.bin": strings.Repeat("w", 300_000)} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	pushed, err := client.New(in.URL, in.Write).Push(ctx, dir, "team/own-model", client.PushOptions{Branch: "main", Tags: []string{"v1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	res := hfDownload(t, in.URL, "team/own-model", "v1.0", "config.json")
	checkSnapshot(t, in, "team/own-model", pushed.SHA, res)
	if res.Branches["main"] != pushed.SHA || res.Tags["v1.0"] != pushed.SHA {
		t.Errorf("list_repo_refs: branches %v tags %v, want main and v1.0 at %s", res.Branches, res.Tags, pushed.SHA)
	}
}

// TestHubClientPullThrough points huggingface_hub at an empty registry with
// pull-through on. The client asks for a model the registry has never seen,
// and gets it: the registry fetches it from its upstream, verifies it,
// stores it, and answers.
func TestHubClientPullThrough(t *testing.T) {
	python(t)
	ctx := context.Background()
	dir := t.TempDir()
	for p, content := range map[string]string{
		"config.json":       `{"model_type":"tiny"}`,
		"model.safetensors": strings.Repeat("\x00weights\xff", 100_000),
		"sub/vocab.txt":     "a\nb\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hub := regtest.Start(t, server.Config{AnonymousRead: true})
	pushed, err := client.New(hub.URL, hub.Write).Push(ctx, dir, "acme/on-demand", client.PushOptions{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}

	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.URL, PullThrough: true})
	if _, err := local.Reg.GetRepo(ctx, "acme/on-demand"); err == nil {
		t.Fatal("the registry already has the repository before anyone asked")
	}
	res := hfDownload(t, local.URL, "acme/on-demand", "main", "sub/vocab.txt")
	checkSnapshot(t, local, "acme/on-demand", pushed.SHA, res)

	// With the upstream gone, a second client still downloads everything.
	hub.StopServing()
	checkSnapshot(t, local, "acme/on-demand", pushed.SHA, hfDownload(t, local.URL, "acme/on-demand", pushed.SHA, "config.json"))
}

// s3Stores returns the S3 backends to test against: always the in-memory
// fake, plus a real S3-compatible service when REGISTRY_TEST_S3_ENDPOINT
// (and _BUCKET, _ACCESS_KEY, _SECRET_KEY) point at one.
func s3Stores(t *testing.T) map[string]blob.Store {
	t.Helper()
	out := map[string]blob.Store{"in-memory": blobtest.NewFakeS3(t).Store(t)}
	if endpoint := os.Getenv("REGISTRY_TEST_S3_ENDPOINT"); endpoint != "" {
		real, err := blob.NewS3(context.Background(), blob.S3Config{
			Endpoint:   endpoint,
			Region:     "us-east-1",
			Bucket:     os.Getenv("REGISTRY_TEST_S3_BUCKET"),
			Prefix:     "e2e-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000000"),
			AccessKey:  os.Getenv("REGISTRY_TEST_S3_ACCESS_KEY"),
			SecretKey:  os.Getenv("REGISTRY_TEST_S3_SECRET_KEY"),
			StagingDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		out["real service"] = real
	}
	return out
}

// TestHubClientAgainstS3 downloads with huggingface_hub from a registry
// that keeps its files in an S3 bucket, both streamed through the registry
// and redirected to signed bucket URLs.
func TestHubClientAgainstS3(t *testing.T) {
	python(t)
	ctx := context.Background()
	dir := t.TempDir()
	files := map[string]string{
		"config.json":       `{"model_type":"tiny"}`,
		"empty.txt":         "",
		"model.safetensors": strings.Repeat("\x00weights\xff", 300_000),
		"sub/vocab.txt":     "a\nb\n",
	}
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, redirect := range map[string]bool{"streamed through the registry": false, "redirected to the bucket": true} {
		for backend, store := range s3Stores(t) {
			t.Run(name+"/"+backend, func(t *testing.T) {
				// Reads need a token, so the redirected case also shows the
				// client fetching from the bucket with only the signed URL.
				in := regtest.StartWith(t, server.Config{AnonymousRead: false, RedirectDownloads: redirect}, store)
				pushed, err := client.New(in.URL, in.Write).Push(ctx, dir, "team/in-bucket", client.PushOptions{Branch: "main"})
				if err != nil {
					t.Fatal(err)
				}
				read, err := in.Reg.CreateToken(ctx, "reader", registry.RoleRead)
				if err != nil {
					t.Fatal(err)
				}
				res, stderr, err := runDownload(t, in.URL, read, "team/in-bucket", "main", "sub/vocab.txt")
				if err != nil {
					t.Fatalf("download failed: %v\n%s", err, stderr)
				}
				checkSnapshot(t, in, "team/in-bucket", pushed.SHA, res)
			})
		}
	}
}

// TestHubClientWithToken covers a registry that requires a token to read:
// huggingface_hub sends HF_TOKEN, and nothing is served without it.
func TestHubClientWithToken(t *testing.T) {
	python(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := regtest.Start(t, server.Config{AnonymousRead: false})
	pushed, err := client.New(in.URL, in.Write).Push(ctx, dir, "team/private", client.PushOptions{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := in.Reg.CreateToken(ctx, "reader", registry.RoleRead)
	if err != nil {
		t.Fatal(err)
	}

	res, stderr, err := runDownload(t, in.URL, read, "team/private", "main", "config.json")
	if err != nil {
		t.Fatalf("download with a read token failed: %v\n%s", err, stderr)
	}
	checkSnapshot(t, in, "team/private", pushed.SHA, res)

	for name, token := range map[string]string{"no token": "", "wrong token": "md_0000000000000000000000000000000000000000"} {
		if _, _, err := runDownload(t, in.URL, token, "team/private", "main", "config.json"); err == nil {
			t.Errorf("%s: download from a private registry succeeded", name)
		}
	}
}

// TestMirrorFromHuggingFace is the real thing: mirror a public model from
// huggingface.co, then download it with huggingface_hub while every route
// to huggingface.co is blocked.
func TestMirrorFromHuggingFace(t *testing.T) {
	if os.Getenv("REGISTRY_E2E_NETWORK") != "1" {
		t.Skip("set REGISTRY_E2E_NETWORK=1 to mirror from huggingface.co")
	}
	python(t)
	ctx := context.Background()
	repo := os.Getenv("REGISTRY_E2E_REPO")
	if repo == "" {
		repo = "hf-internal-testing/tiny-random-bert"
	}

	// What does the real hub call this revision?
	resp, err := http.Get("https://huggingface.co/api/models/" + repo + "/revision/main")
	if err != nil {
		t.Fatal(err)
	}
	var upstream struct {
		SHA string `json:"sha"`
	}
	err = json.NewDecoder(resp.Body).Decode(&upstream)
	_ = resp.Body.Close()
	if err != nil || upstream.SHA == "" {
		t.Fatalf("could not read upstream revision: %v", err)
	}

	local := regtest.Start(t, server.Config{AnonymousRead: true})
	done, err := client.New(local.URL, local.Write).Mirror(ctx, repo, "main", os.Getenv("HF_TOKEN"), func(e client.MirrorEvent) {
		if e.Event == "file" {
			t.Logf("%-10s %9d  %s", e.Status, e.Size, e.Path)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.SHA != upstream.SHA {
		t.Fatalf("mirror recorded revision %s, huggingface.co reports %s", done.SHA, upstream.SHA)
	}
	t.Logf("mirrored %s@%s: %d files, %d bytes", done.Repo, done.SHA, done.Files, done.Bytes)

	files, err := local.Reg.Files(ctx, mustResolve(t, local, done.Repo, done.SHA))
	if err != nil || len(files) == 0 {
		t.Fatalf("no files recorded: %v", err)
	}
	checkSnapshot(t, local, done.Repo, upstream.SHA, hfDownload(t, local.URL, done.Repo, "main", files[0].Path))
}

func mustResolve(t *testing.T, in *regtest.Instance, repo, rev string) registry.Commit {
	t.Helper()
	c, err := in.Reg.Resolve(context.Background(), repo, rev)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestPullThroughOneFileFromHuggingFace asks a pull-through registry for a
// single file of a real model on huggingface.co, with the stock client,
// and checks that one file is all that was fetched.
func TestPullThroughOneFileFromHuggingFace(t *testing.T) {
	if os.Getenv("REGISTRY_E2E_NETWORK") != "1" {
		t.Skip("set REGISTRY_E2E_NETWORK=1 to fetch from huggingface.co")
	}
	python(t)
	ctx := context.Background()
	repo := os.Getenv("REGISTRY_E2E_REPO")
	if repo == "" {
		repo = "hf-internal-testing/tiny-random-bert"
	}
	local := regtest.Start(t, server.Config{AnonymousRead: true, PullThrough: true})

	const script = `
import json, sys
from huggingface_hub import hf_hub_download, HfApi
path = hf_hub_download(sys.argv[1], "config.json")
info = HfApi().model_info(sys.argv[1])
print(json.dumps({"path": path, "sha": info.sha, "files": len(info.siblings)}))
`
	stdout, stderr, err := runPython(t, local.URL, "", script, nil, repo)
	if err != nil {
		t.Fatalf("download through a pull-through registry failed: %v\n%s", err, stderr)
	}
	var res struct {
		Path  string `json:"path"`
		SHA   string `json:"sha"`
		Files int    `json:"files"`
	}
	lastJSON(t, stdout, &res)

	c := mustResolve(t, local, repo, "main")
	if c.SHA != res.SHA {
		t.Fatalf("client saw revision %s, registry recorded %s", res.SHA, c.SHA)
	}
	files, err := local.Reg.Files(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, f := range files {
		if !f.Pending {
			stored++
			if f.Path != "config.json" {
				t.Errorf("%s was fetched although nobody asked for it", f.Path)
			}
		}
	}
	if stored != 1 || len(files) != res.Files || len(files) < 2 {
		t.Fatalf("%d of %d files fetched; the client saw %d listed", stored, len(files), res.Files)
	}
	st, _ := local.Reg.Stats(ctx)
	t.Logf("%s@%s: %d files listed, 1 fetched, %d bytes stored", repo, c.SHA, len(files), st.StoredBytes)
	if got := regtest.FileSHA256(t, res.Path); got != files[indexOf(files, "config.json")].SHA256 {
		t.Errorf("downloaded config.json hashes to %s", got)
	}
}

func indexOf(files []registry.File, path string) int {
	for i, f := range files {
		if f.Path == path {
			return i
		}
	}
	return 0
}
