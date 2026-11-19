// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// datasetHub is an upstream holding the model acme/tiny and the dataset
// acme/tiny, behind a front that notes every path it is asked for and can
// alter what it sends.
type datasetHub struct {
	inst   *regtest.Instance
	url    string
	model  client.PushResult
	data   client.PushResult
	mu     sync.Mutex
	asked  []string
	tamper func(path string) bool // when it says so, the file is sent altered
}

func newDatasetHub(t *testing.T, extra map[string]string) *datasetHub {
	t.Helper()
	h := &datasetHub{inst: regtest.Start(t, server.Config{AnonymousRead: true})}
	files := map[string]string{}
	for k, v := range datasetFiles {
		files[k] = v
	}
	for k, v := range extra {
		files[k] = v
	}
	h.model = push(t, h.inst, "acme/tiny", modelFiles, client.PushOptions{})
	h.data = push(t, h.inst, "datasets/acme/tiny", files, client.PushOptions{})
	target, err := url.Parse(h.inst.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.asked = append(h.asked, r.URL.Path)
		tamper := h.tamper
		h.mu.Unlock()
		if tamper != nil && tamper(r.URL.Path) {
			_, _ = w.Write([]byte("not what the listing says"))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	h.url = front.URL
	return h
}

func (h *datasetHub) paths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

// A dataset the registry does not hold is recorded from the upstream's
// dataset addresses on first request, as a dataset, and its files fetched
// one at a time.
func TestPullThroughADataset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newDatasetHub(t, nil)
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	resp := do(t, http.MethodGet, local.URL+"/datasets/acme/tiny/resolve/main/data/train.csv", "", nil, nil)
	if got := body(t, resp); resp.StatusCode != http.StatusOK || got != datasetFiles["data/train.csv"] || resp.Header.Get("X-Repo-Commit") != hub.data.SHA {
		t.Fatalf("status %d, %q", resp.StatusCode, got)
	}
	repo, err := local.Reg.GetRepo(ctx, "datasets/acme/tiny")
	if err != nil || repo.Type != registry.TypeDataset || repo.Upstream != hub.url {
		t.Fatalf("recorded %+v, %v", repo, err)
	}
	c, _ := local.Reg.Resolve(ctx, "datasets/acme/tiny", "main")
	files, _ := local.Reg.Files(ctx, c)
	pending := 0
	for _, f := range files {
		if f.Pending {
			pending++
		}
	}
	if pending != len(datasetFiles)-1 {
		t.Errorf("%d files pending, want all but the one fetched", pending)
	}
	if _, err := local.Reg.GetRepo(ctx, "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Errorf("the model of the same name was recorded: %v", err)
	}
	for _, p := range hub.paths() {
		if strings.HasPrefix(p, "/api/models/") || strings.HasPrefix(p, "/acme/") {
			t.Errorf("the upstream was asked for the model: %s", p)
		}
	}
	// dataset_info and the registry's own API start the same fetch.
	var doc map[string]any
	decode(t, do(t, http.MethodGet, local.URL+"/api/datasets/acme/tiny/revision/main", "", nil, nil), &doc)
	if doc["sha"] != hub.data.SHA {
		t.Errorf("dataset_info: %v", doc)
	}
}

// The allow list tells a dataset from a model by its key: "acme/*" names
// models, as it always has, and "datasets/acme/*" names datasets.
func TestPullThroughAllowListByType(t *testing.T) {
	t.Parallel()
	hub := newDatasetHub(t, nil)
	tests := []struct {
		name        string
		allow       []string
		model, data int
	}{
		{"a model pattern", []string{"acme/*"}, http.StatusOK, http.StatusNotFound},
		{"a dataset pattern", []string{"datasets/acme/*"}, http.StatusNotFound, http.StatusOK},
		{"one of each", []string{"acme/*", "datasets/*/*"}, http.StatusOK, http.StatusOK},
		{"no list: everything", nil, http.StatusOK, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true, PullThroughAllow: tc.allow})
			if resp := do(t, http.MethodGet, local.URL+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != tc.model {
				t.Errorf("the model: %d, want %d", resp.StatusCode, tc.model)
			}
			resp := do(t, http.MethodGet, local.URL+"/datasets/acme/tiny/resolve/main/README.md", "", nil, nil)
			if resp.StatusCode != tc.data {
				t.Errorf("the dataset: %d, want %d", resp.StatusCode, tc.data)
			}
			if tc.data == http.StatusNotFound && resp.Header.Get("X-Error-Code") != "RepoNotFound" {
				t.Errorf("a dataset not allowed: code %q", resp.Header.Get("X-Error-Code"))
			}
		})
	}
}

// A dataset created here is never added to from upstream, whatever the
// upstream holds under its name; a model created here does not stop the
// dataset of the same name from being pulled through.
func TestPullThroughNeverFillsALocalDataset(t *testing.T) {
	t.Parallel()
	hub := newDatasetHub(t, nil)
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	push(t, local, "datasets/acme/tiny", map[string]string{"mine.csv": "a\n"}, client.PushOptions{})
	push(t, local, "acme/tiny", map[string]string{"mine.bin": "m"}, client.PushOptions{})

	// The upstream's revision of the dataset is not fetched into the local one.
	resp := do(t, http.MethodGet, local.URL+"/datasets/acme/tiny/resolve/"+hub.data.SHA+"/README.md", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != "RevisionNotFound" {
		t.Fatalf("status %d, code %q", resp.StatusCode, resp.Header.Get("X-Error-Code"))
	}
	for _, p := range hub.paths() {
		if strings.Contains(p, "/datasets/") {
			t.Fatalf("the upstream was asked about the local dataset: %s", p)
		}
	}
	// The model is local too, and is left alone the same way.
	resp = do(t, http.MethodGet, local.URL+"/acme/tiny/resolve/"+hub.model.SHA+"/config.json", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the local model: status %d", resp.StatusCode)
	}

	// A registry with only a local model still pulls the dataset through.
	other := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	push(t, other, "acme/tiny", map[string]string{"mine.bin": "m"}, client.PushOptions{})
	if resp := do(t, http.MethodGet, other.URL+"/datasets/acme/tiny/resolve/main/README.md", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the dataset beside a local model: status %d", resp.StatusCode)
	}
}

// The size limit applies to a dataset's files, and says how to fetch one
// ahead of time.
func TestPullThroughSizeLimitForADataset(t *testing.T) {
	t.Parallel()
	hub := newDatasetHub(t, nil)
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true, PullThroughMaxFileBytes: 10})
	resp := do(t, http.MethodGet, local.URL+"/datasets/acme/tiny/resolve/main/data/train.csv", "", nil, nil)
	if got := body(t, resp); resp.StatusCode != http.StatusForbidden || !strings.Contains(got, "modelctl mirror --repo-type dataset acme/tiny") {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
}

// A large file of a dataset is sent while it is fetched, like a model's,
// and arrives whole and correct.
func TestPullThroughStreamsALargeDatasetFile(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("0123456789abcdef", 3<<16) // 3 MiB
	hub := newDatasetHub(t, map[string]string{"data/big.parquet": big})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	resp := do(t, http.MethodGet, local.URL+"/datasets/acme/tiny/resolve/main/data/big.parquet", "", nil, nil)
	if got := body(t, resp); resp.StatusCode != http.StatusOK || got != big {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(got))
	}
}

// What the upstream gets wrong is its failure, for a dataset as for a
// model: 502 with the reason, and nothing recorded as stored.
func TestPullThroughUpstreamFailuresForADataset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("content that does not match its digest", func(t *testing.T) {
		t.Parallel()
		hub := newDatasetHub(t, nil)
		local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
		hub.mu.Lock()
		hub.tamper = func(p string) bool { return strings.HasSuffix(p, "/data/train.csv") }
		hub.mu.Unlock()
		resp := do(t, http.MethodGet, local.URL+"/datasets/acme/tiny/resolve/main/data/train.csv", "", nil, nil)
		if got := body(t, resp); resp.StatusCode != http.StatusBadGateway || !strings.Contains(got, "upstream") {
			t.Fatalf("status %d: %s", resp.StatusCode, got)
		}
		c, err := local.Reg.Resolve(ctx, "datasets/acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		if f, err := local.Reg.File(ctx, c, "data/train.csv"); err != nil || !f.Pending {
			t.Fatalf("after the failed fetch: %+v, %v", f, err)
		}
	})

	t.Run("an upstream that cannot be reached", func(t *testing.T) {
		t.Parallel()
		hub := newDatasetHub(t, nil)
		local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
		hub.inst.StopServing()
		resp := do(t, http.MethodGet, local.URL+"/api/datasets/acme/tiny/revision/main", "", nil, nil)
		if got := body(t, resp); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status %d: %s", resp.StatusCode, got)
		}
	})
}
