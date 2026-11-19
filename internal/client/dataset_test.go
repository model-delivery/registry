// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/model-delivery/registry/internal/kit"
)

// The repository types keep the values the registry's API gives them.
func TestRepoTypeWireValues(t *testing.T) {
	t.Parallel()
	if Model != "model" || Dataset != "dataset" {
		t.Fatalf("the wire values changed: %q, %q", Model, Dataset)
	}
	for in, want := range map[string]RepoType{"": Model, "model": Model, "dataset": Dataset} {
		if got, err := ParseRepoType(in); err != nil || got != want {
			t.Errorf("ParseRepoType(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"space", "datasets", "Model"} {
		if _, err := ParseRepoType(bad); err == nil || !strings.Contains(err.Error(), "model or dataset") {
			t.Errorf("ParseRepoType(%q) = %v, want it refused", bad, err)
		}
	}
	if RepoKey(Dataset, "acme/x") != "datasets/acme/x" || RepoKey(Model, "acme/x") != "acme/x" {
		t.Error("RepoKey does not give the Hub's paths")
	}
}

// Every repository method addresses a dataset at /api/v1/datasets, and a
// model where it always has.
func TestDatasetRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const ds = "datasets/acme/table"

	tests := []struct {
		name   string
		call   func(*Client) error
		method string
		path   string
	}{
		{"list datasets", func(c *Client) error { _, err := c.ListRepositories(ctx, Dataset); return err }, http.MethodGet, "/api/v1/datasets"},
		{"list models", func(c *Client) error { _, err := c.ListRepositories(ctx, Model); return err }, http.MethodGet, "/api/v1/repos"},
		{"inspect", func(c *Client) error { _, err := c.Repo(ctx, ds); return err }, http.MethodGet, "/api/v1/datasets/acme/table"},
		{"delete", func(c *Client) error { _, err := c.DeleteRepo(ctx, ds); return err }, http.MethodDelete, "/api/v1/datasets/acme/table"},
		{"revision", func(c *Client) error { _, err := c.Revision(ctx, ds, "v1"); return err }, http.MethodGet, "/api/v1/datasets/acme/table/revisions/v1"},
		{"model revision", func(c *Client) error { _, err := c.Revision(ctx, "acme/table", "v1"); return err }, http.MethodGet, "/api/v1/repos/acme/table/revisions/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl, got := answering(t, http.StatusOK, `{"repos":[],"files":[]}`)
			if err := tc.call(cl); err != nil {
				t.Fatal(err)
			}
			if r := got(); len(r) != 1 || r[0].method != tc.method || r[0].path != tc.path {
				t.Fatalf("requests: %+v, want %s %s", r, tc.method, tc.path)
			}
		})
	}

	t.Run("a listing says which are datasets", func(t *testing.T) {
		t.Parallel()
		cl, _ := answering(t, http.StatusOK, `{"repos":[{"name":"acme/table","type":"dataset","created_at":"2026-11-19T10:00:00Z","updated_at":"2026-11-19T10:00:00Z"}]}`)
		out, err := cl.ListRepositories(ctx, Dataset)
		if err != nil || len(out.Repos) != 1 || out.Repos[0].Type != Dataset {
			t.Fatalf("decoded %+v, %v", out.Repos, err)
		}
	})
}

// A dataset's mirror request names its type; a model's is what it always
// was, with no type field.
func TestMirrorRequestNamesTheType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for key, want := range map[string]map[string]string{
		"datasets/acme/table": {"repo": "acme/table", "type": "dataset", "revision": "main", "upstream_token": ""},
		"acme/model":          {"repo": "acme/model", "revision": "main", "upstream_token": ""},
	} {
		cl, got := answering(t, http.StatusOK, `{"event":"done","repo":"x","sha":"`+strings.Repeat("a", 40)+`"}`+"\n")
		if _, err := cl.Mirror(ctx, key, "main", "", nil); err != nil {
			t.Fatal(err)
		}
		r := got()
		var body map[string]string
		if len(r) != 1 || json.Unmarshal([]byte(r[0].body), &body) != nil {
			t.Fatalf("%s: requests %+v", key, r)
		}
		if len(body) != len(want) {
			t.Errorf("%s: sent %v, want %v", key, body, want)
		}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("%s: sent %v, want %v", key, body, want)
			}
		}
	}
}

// A push to a dataset records its revision under /api/v1/datasets, and a
// pull of one downloads from the dataset's own files, at /datasets/.
func TestPushAndPullADataset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := "a,b\n1,2\n"
	digest := kit.SHA256Hex(content)
	sha := strings.Repeat("b", 40)

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/api/v1/datasets/acme/table/commits":
			_, _ = io.WriteString(w, `{"repo":"acme/table","repo_type":"dataset","sha":"`+sha+`"}`)
		case r.URL.Path == "/api/v1/datasets/acme/table/revisions/main":
			_, _ = io.WriteString(w, `{"repo":"acme/table","repo_type":"dataset","sha":"`+sha+`","files":[{"path":"train.csv","size":8,"sha256":"`+digest+`","lfs":true}]}`)
		case r.URL.Path == "/datasets/acme/table/resolve/"+sha+"/train.csv":
			_, _ = io.WriteString(w, content)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cl := New(srv.URL, "md_token")

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "train.csv"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	pushed, err := cl.Push(ctx, src, "datasets/acme/table", PushOptions{Branch: "main"})
	if err != nil || pushed.RepoType != Dataset || pushed.SHA != sha {
		t.Fatalf("push: %+v, %v", pushed, err)
	}
	dst := t.TempDir()
	rev, err := cl.Pull(ctx, "datasets/acme/table", "main", dst, nil)
	if err != nil || rev.Key() != "datasets/acme/table" {
		t.Fatalf("pull: %+v, %v", rev, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "train.csv")); err != nil || string(got) != content {
		t.Fatalf("pulled %q, %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(paths, "\n"), "GET /datasets/acme/table/resolve/"+sha+"/train.csv") {
		t.Fatalf("the file was not downloaded from the dataset's address: %v", paths)
	}
}
