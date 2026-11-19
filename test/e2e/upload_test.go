// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// runPython runs a script with the real huggingface_hub library pointed at
// endpoint, a fresh cache, and every other host unreachable.
func runPython(t *testing.T, endpoint, token, script string, extraEnv []string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python(t), append([]string{path}, args...)...)
	cmd.Env = append(os.Environ(),
		"HF_ENDPOINT="+endpoint,
		"HF_HOME="+t.TempDir(),
		"HF_TOKEN="+token,
		"HF_HUB_DISABLE_TELEMETRY=1",
		"HF_HUB_DISABLE_PROGRESS_BARS=1",
		"HF_HUB_DISABLE_SYMLINKS_WARNING=1",
		"PYTHONUTF8=1", "PYTHONIOENCODING=utf-8", // the hf command prints symbols a legacy console codec cannot encode
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9",
		"http_proxy=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9",
		"NO_PROXY="+reachable(), "no_proxy="+reachable(),
	)
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "HF_HUB_DISABLE_SYMLINKS=1")
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err = cmd.Run()
	return out.String(), errBuf.String(), err
}

// lastJSON decodes the last line a script printed.
func lastJSON(t *testing.T, stdout string, v any) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), v); err != nil {
		t.Fatalf("unreadable script output %q: %v", stdout, err)
	}
}

// revisionFiles is what the registry holds for a revision: path -> content.
func revisionFiles(t *testing.T, in *regtest.Instance, repo, rev string) (string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	c, err := in.Reg.Resolve(ctx, repo, rev)
	if err != nil {
		t.Fatal(err)
	}
	files, err := in.Reg.Files(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		rc, _, err := in.Reg.OpenBlob(ctx, f.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		_ = rc.Close()
		out[f.Path] = buf.String()
	}
	return c.SHA, out
}

func paths(m map[string]string) string {
	var p []string
	for k := range m {
		p = append(p, k)
	}
	sort.Strings(p)
	return strings.Join(p, ",")
}

// A large file is anything over the size the registry asks clients to send
// through the large-file protocol (10 MiB).
var largeContent = strings.Repeat("\x00weights\xff", 1_200_000) // about 11 MiB

const uploadPy = `
import json, sys
from huggingface_hub import HfApi, CommitOperationAdd, CommitOperationDelete

repo, first_dir, second_dir = sys.argv[1], sys.argv[2], sys.argv[3]
api = HfApi()

# What "hf upload" does: make sure the repository exists, then upload a folder.
url = api.create_repo(repo, exist_ok=True)
again = api.create_repo(repo, exist_ok=True)
first = api.upload_folder(folder_path=first_dir, repo_id=repo, commit_message="first upload")

# One file on its own.
one = api.upload_file(path_or_fileobj=b'{"v": 2}', path_in_repo="config.json", repo_id=repo, commit_message="update config")

# A folder upload that also deletes what is no longer there.
third = api.upload_folder(folder_path=second_dir, repo_id=repo, delete_patterns=["old/*"], commit_message="replace old")

# An explicit commit that adds and deletes.
fourth = api.create_commit(repo, commit_message="explicit", operations=[
    CommitOperationAdd(path_in_repo="notes/readme.txt", path_or_fileobj=b"notes"),
    CommitOperationDelete(path_in_repo="tokenizer/vocab.txt"),
])

# Uploading the same content again must not make a new revision.
same = api.upload_file(path_or_fileobj=b"notes", path_in_repo="notes/readme.txt", repo_id=repo)

print(json.dumps({
    "repo_id": url.repo_id,
    "revisions": [first.oid, one.oid, third.oid, fourth.oid, same.oid],
    "files": sorted(api.list_repo_files(repo)),
}))
`

// TestHubClientUpload uploads with the real huggingface_hub library, the
// way "hf upload" and training scripts do, and then reads everything back.
func TestHubClientUpload(t *testing.T) {
	python(t)
	ctx := context.Background()
	in := regtest.Start(t, server.Config{AnonymousRead: true})

	first := regtest.WriteTree(t, map[string]string{
		"config.json":         `{"v": 1}`,
		"model.safetensors":   largeContent,
		"tokenizer/vocab.txt": "a\nb\n",
		"old/a.bin":           "old a",
		"old/b.bin":           "old b",
		"empty.txt":           "",
		"name with space.txt": "spaced",
	})
	second := regtest.WriteTree(t, map[string]string{
		"model.safetensors": largeContent, // unchanged: must not be sent again
		"adapter.bin":       "adapter",
	})

	// The older large-file protocol, which is what the registry speaks.
	stdout, stderr, err := runPython(t, in.URL, in.Write, uploadPy, []string{"HF_HUB_DISABLE_XET=1"}, "team/uploaded", first, second)
	if err != nil {
		t.Fatalf("upload failed: %v\n%s", err, stderr)
	}
	var res struct {
		RepoID    string   `json:"repo_id"`
		Revisions []string `json:"revisions"`
		Files     []string `json:"files"`
	}
	lastJSON(t, stdout, &res)
	if res.RepoID != "team/uploaded" {
		t.Errorf("create_repo returned repository %q", res.RepoID)
	}
	for i, sha := range res.Revisions {
		if !registry.IsSHA(sha) {
			t.Fatalf("revision %d is %q, not a 40-character id", i, sha)
		}
	}
	rv := res.Revisions
	if rv[0] == rv[1] || rv[1] == rv[2] || rv[2] == rv[3] {
		t.Errorf("each change should be a new revision: %v", rv)
	}
	if rv[4] != rv[3] {
		t.Errorf("uploading identical content created a new revision: %s after %s", rv[4], rv[3])
	}

	// The first revision holds exactly what was uploaded, byte for byte.
	_, got := revisionFiles(t, in, "team/uploaded", rv[0])
	if paths(got) != "config.json,empty.txt,model.safetensors,name with space.txt,old/a.bin,old/b.bin,tokenizer/vocab.txt" {
		t.Errorf("first revision files: %s", paths(got))
	}
	if got["model.safetensors"] != largeContent || got["config.json"] != `{"v": 1}` || got["empty.txt"] != "" {
		t.Error("first revision content differs from what was uploaded")
	}

	// The latest revision reflects every later change.
	head, got := revisionFiles(t, in, "team/uploaded", "main")
	if head != rv[3] {
		t.Errorf("main is at %s, want %s", head, rv[3])
	}
	want := "adapter.bin,config.json,empty.txt,model.safetensors,name with space.txt,notes/readme.txt"
	if paths(got) != want {
		t.Errorf("latest files: %s\nwant:         %s", paths(got), want)
	}
	if got["config.json"] != `{"v": 2}` || got["adapter.bin"] != "adapter" || got["model.safetensors"] != largeContent {
		t.Error("latest revision content is wrong")
	}
	if strings.Join(res.Files, ",") != want {
		t.Errorf("list_repo_files says %v", res.Files)
	}

	// Earlier revisions are untouched by later uploads.
	if _, old := revisionFiles(t, in, "team/uploaded", rv[0]); old["config.json"] != `{"v": 1}` || old["old/a.bin"] != "old a" {
		t.Error("an earlier revision changed")
	}

	// And a standard download gets the uploaded model back.
	checkSnapshot(t, in, "team/uploaded", rv[3], hfDownload(t, in.URL, "team/uploaded", "main", "config.json"))

	// The unchanged large file was stored once.
	st, _ := in.Reg.Stats(ctx)
	if st.StoredBytes > int64(len(largeContent))+10_000 {
		t.Errorf("stored %d bytes; the large file (%d bytes) appears to be stored more than once", st.StoredBytes, len(largeContent))
	}
}

const smallUploadPy = `
import json, sys
from huggingface_hub import HfApi
api = HfApi()
api.create_repo(sys.argv[1], exist_ok=True)
info = api.upload_folder(folder_path=sys.argv[2], repo_id=sys.argv[1])
print(json.dumps({"oid": info.oid}))
`

// A stock client has hf_xet installed and sends large files through Xet
// without asking. The registry does not speak Xet, so: small files work
// as they are, and a large file fails with an error that says what to set.
func TestHubClientUploadWithXetInstalled(t *testing.T) {
	python(t)
	if err := exec.Command(python(t), "-c", "import hf_xet").Run(); err != nil {
		t.Skip("hf_xet is not installed, so this client already uses the protocol the registry speaks")
	}
	in := regtest.Start(t, server.Config{AnonymousRead: true})

	t.Run("small files upload with no extra setting", func(t *testing.T) {
		dir := regtest.WriteTree(t, map[string]string{"config.json": "{}", "README.md": "# small\n", "weights.bin": strings.Repeat("w", 200_000)})
		stdout, stderr, err := runPython(t, in.URL, in.Write, smallUploadPy, nil, "team/small", dir)
		if err != nil {
			t.Fatalf("upload of small files failed: %v\n%s", err, stderr)
		}
		var res struct{ OID string }
		lastJSON(t, stdout, &res)
		if _, got := revisionFiles(t, in, "team/small", res.OID); paths(got) != "README.md,config.json,weights.bin" {
			t.Fatalf("files: %s", paths(got))
		}
	})

	t.Run("a large file fails and says how to fix it", func(t *testing.T) {
		dir := regtest.WriteTree(t, map[string]string{"config.json": "{}", "model.safetensors": largeContent})
		_, stderr, err := runPython(t, in.URL, in.Write, smallUploadPy, nil, "team/large", dir)
		if err == nil {
			t.Fatal("the upload succeeded; if the registry now handles this, update the docs and this test")
		}
		t.Logf("client error (last lines):\n%s", tail(stderr, 6))
		if !strings.Contains(stderr, "HF_HUB_DISABLE_XET") {
			t.Errorf("the error the user sees does not mention HF_HUB_DISABLE_XET")
		}
		// Nothing half-uploaded is visible.
		if _, err := in.Reg.Resolve(context.Background(), "team/large", "main"); err == nil {
			t.Error("a revision was recorded for an upload that failed")
		}
	})
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Uploading needs a token with the write role.
func TestHubClientUploadNeedsWriteAccess(t *testing.T) {
	python(t)
	ctx := context.Background()
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	read, err := in.Reg.CreateToken(ctx, "reader", registry.RoleRead)
	if err != nil {
		t.Fatal(err)
	}
	dir := regtest.WriteTree(t, map[string]string{"config.json": "{}"})
	for name, token := range map[string]string{"no token": "", "read-only token": read, "unknown token": "md_0000000000000000000000000000000000000000"} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runPython(t, in.URL, token, smallUploadPy, []string{"HF_HUB_DISABLE_XET=1"}, "team/denied", dir); err == nil {
				t.Fatal("the upload succeeded")
			}
			if _, err := in.Reg.GetRepo(ctx, "team/denied"); err == nil {
				t.Fatal("a repository was created by a caller who may not write")
			}
		})
	}
}

// hfCommandPy runs the `hf` command line in-process, the same entry point
// the installed `hf` executable calls.
const hfCommandPy = `
import sys
from huggingface_hub.cli.hf import main
sys.argv = ["hf"] + sys.argv[1:]
main()
`

// TestHfUploadCommand runs the actual "hf upload" command: a folder to a
// new repository, a single file, and a folder to a new branch.
func TestHfUploadCommand(t *testing.T) {
	python(t)
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	env := []string{"HF_HUB_DISABLE_XET=1"}
	dir := regtest.WriteTree(t, map[string]string{"config.json": `{"v": 1}`, "model.safetensors": largeContent, "sub/vocab.txt": "a\n"})
	hf := func(args ...string) {
		t.Helper()
		if _, stderr, err := runPython(t, in.URL, in.Write, hfCommandPy, env, args...); err != nil {
			t.Fatalf("hf %s failed: %v\n%s", strings.Join(args, " "), err, tail(stderr, 12))
		}
	}

	hf("upload", "team/cli-model", dir)
	first, got := revisionFiles(t, in, "team/cli-model", "main")
	if paths(got) != "config.json,model.safetensors,sub/vocab.txt" || got["model.safetensors"] != largeContent {
		t.Fatalf("after hf upload: %s", paths(got))
	}

	one := filepath.Join(regtest.WriteTree(t, map[string]string{"notes.md": "# notes\n"}), "notes.md")
	hf("upload", "team/cli-model", one, "docs/notes.md")
	second, got := revisionFiles(t, in, "team/cli-model", "main")
	if second == first || got["docs/notes.md"] != "# notes\n" || len(got) != 4 {
		t.Fatalf("after uploading one file: %s", paths(got))
	}

	// A branch that does not exist yet, on a repository that already has revisions.
	hf("upload", "team/cli-model", regtest.WriteTree(t, map[string]string{"experiment.txt": "x"}), "--revision", "experiment")
	_, got = revisionFiles(t, in, "team/cli-model", "experiment")
	if got["experiment.txt"] != "x" || got["config.json"] != `{"v": 1}` {
		t.Fatalf("new branch should start from main and add the file: %s", paths(got))
	}
	if head, _ := revisionFiles(t, in, "team/cli-model", "main"); head != second {
		t.Fatal("uploading to another branch moved main")
	}

	// A branch on a repository that has no revision at all.
	hf("upload", "team/cli-new", dir, "--revision", "dev")
	if _, got := revisionFiles(t, in, "team/cli-new", "dev"); len(got) != 3 {
		t.Fatalf("upload to a new branch of a new repository: %s", paths(got))
	}
}
