// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/kit"
)

// The commands that name a repository take --repo-type, as the hf CLI
// does: a dataset is addressed under /api/v1/datasets, and without the flag
// every command addresses a model exactly as before.
func TestRepoTypeFlag(t *testing.T) {
	t.Parallel()
	const sha = "5a1f000000000000000000000000000000000000"
	datasetDoc := `{"name":"acme/table","type":"dataset","upstream":"https://huggingface.co","created_at":"2026-11-17T09:00:00Z","refs":[],"revisions":[]}`
	listDoc := `{"repos":[{"name":"acme/table","type":"dataset","created_at":"2026-11-17T09:00:00Z","revisions":1,"stored_bytes":8,"updated_at":"2026-11-17T09:00:00Z"}]}`
	tests := []struct {
		name   string
		args   []string
		route  string
		answer answer
		out    string // expected in standard output
	}{
		{"list datasets", []string{"list", "--repo-type", "dataset"}, "GET /api/v1/datasets", answer{body: listDoc}, "acme/table"},
		{"no datasets", []string{"list", "--repo-type=dataset"}, "GET /api/v1/datasets", answer{body: `{"repos":[]}`}, "modelctl mirror --repo-type dataset"},
		{"list models", []string{"list", "--repo-type", "model"}, "GET /api/v1/repos", answer{body: `{"repos":[]}`}, "No repositories yet"},
		{"inspect a dataset", []string{"inspect", "acme/table", "--repo-type", "dataset"}, "GET /api/v1/datasets/acme/table", answer{body: datasetDoc},
			"Repository: dataset acme/table\nMirror of:  https://huggingface.co/datasets/acme/table"},
		{"delete a dataset", []string{"delete", "--repo-type", "dataset", "acme/table", "--yes"}, "DELETE /api/v1/datasets/acme/table", answer{body: freedDoc},
			"Deleted dataset acme/table"},
		{"mirror a dataset", []string{"mirror", "acme/table", "--repo-type", "dataset"}, "POST /api/v1/mirror",
			answer{body: `{"event":"done","repo":"acme/table","repo_type":"dataset","sha":"` + sha + `","files":1}` + "\n"}, "Mirrored dataset acme/table at revision " + sha},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newScripted(t, map[string]answer{tc.route: tc.answer})
			code, out, errOut := reg.run(t, nil, tc.args...)
			if code != 0 || !strings.Contains(out, tc.out) {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s\nwant in stdout: %s", code, out, errOut, tc.out)
			}
			if got := reg.requests(); len(got) != 1 || got[0] != tc.route {
				t.Fatalf("asked %v, want %s", got, tc.route)
			}
		})
	}
}

// push and pull address a dataset's revisions under /api/v1/datasets and
// its files at /datasets/.
func TestPushAndPullWithRepoType(t *testing.T) {
	t.Parallel()
	const sha = "5a1f000000000000000000000000000000000000"
	content := "a,b\n1,2\n"
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "train.csv"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := kit.SHA256Hex(content)
	reg := newScripted(t, map[string]answer{
		"HEAD /api/v1/blobs/" + digest:             {},
		"POST /api/v1/datasets/acme/table/commits": {status: 201, body: `{"repo":"acme/table","repo_type":"dataset","sha":"` + sha + `"}`},
		"GET /api/v1/datasets/acme/table/revisions/main": {body: `{"repo":"acme/table","repo_type":"dataset","sha":"` + sha + `","files":[{"path":"train.csv","size":8,"sha256":"` +
			digest + `","lfs":true}]}`},
		"GET /datasets/acme/table/resolve/" + sha + "/train.csv": {body: content},
	})
	code, out, errOut := reg.run(t, map[string]string{"MODELCTL_TOKEN": "md_x"}, "push", src, "acme/table", "--repo-type", "dataset")
	if code != 0 || !strings.Contains(out, "Pushed dataset acme/table at revision "+sha) {
		t.Fatalf("push: exit %d\n%s\n%s", code, out, errOut)
	}
	dst := filepath.Join(t.TempDir(), "out")
	code, out, errOut = reg.run(t, nil, "pull", "acme/table", "--repo-type", "dataset", "--dir", dst)
	if code != 0 || !strings.Contains(out, "Pulled dataset acme/table") {
		t.Fatalf("pull: exit %d\n%s\n%s", code, out, errOut)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "train.csv")); err != nil || string(got) != content {
		t.Fatalf("pulled %q, %v", got, err)
	}
}

// A type that is not one is a usage error, before any request.
func TestRepoTypeFlagRefusesOtherTypes(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"list", "--repo-type", "space"},
		{"inspect", "acme/x", "--repo-type", "datasets"},
		{"mirror", "acme/x", "--repo-type", "Dataset"},
		{"delete", "acme/x", "--repo-type", "space", "--yes"},
		{"pull", "acme/x", "--repo-type", "kernel"},
	} {
		reg := newScripted(t, nil)
		code, _, errOut := reg.run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "model or dataset") || len(reg.requests()) != 0 {
			t.Errorf("%v: exit %d, %s, asked %v", args, code, errOut, reg.requests())
		}
	}
}
