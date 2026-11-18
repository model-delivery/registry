// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// countingHub is an upstream hub that counts what it is asked for.
type countingHub struct {
	inst     *instance // the registry playing the upstream
	url      string
	pushed   client.PushResult
	lookups  atomic.Int64  // revision lookups: one per fetch started
	files    atomic.Int64  // file downloads
	hold     chan struct{} // when set, file downloads wait until it is closed
	tampered atomic.Bool   // when set, model.safetensors is served altered
}

func newCountingHub(t *testing.T, cfg server.Config) *countingHub {
	t.Helper()
	h := &countingHub{}
	h.inst, h.url, h.pushed = upstreamHub(t, cfg, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/revision/"):
				h.lookups.Add(1)
			case strings.Contains(r.URL.Path, "/resolve/"):
				h.files.Add(1)
				if h.hold != nil {
					select {
					case <-h.hold:
					case <-r.Context().Done():
						return
					}
				}
				if h.tampered.Load() && strings.HasSuffix(r.URL.Path, "/model.safetensors") {
					_, _ = w.Write([]byte("not the real weights"))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	return h
}

func TestPullThroughFetchesOnFirstRequestThenServesLocally(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	// Nothing is in the registry. A client simply asks for a file.
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/tokenizer/vocab.txt", "", nil, nil)
	if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["tokenizer/vocab.txt"] {
		t.Fatalf("first request: status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Repo-Commit"); got != hub.pushed.SHA {
		t.Fatalf("served revision %s, upstream is %s", got, hub.pushed.SHA)
	}

	// The whole revision was stored, verified, under its upstream id.
	c, err := local.reg.Resolve(ctx, "acme/tiny", "main")
	if err != nil || c.SHA != hub.pushed.SHA || c.Source != registry.SourceMirror {
		t.Fatalf("stored revision = %+v, %v", c, err)
	}
	files, _ := local.reg.Files(ctx, c)
	if len(files) != len(modelFiles) {
		t.Fatalf("stored %d files, upstream has %d", len(files), len(modelFiles))
	}

	// Every later request is answered locally: the upstream is not asked again.
	lookups, downloads := hub.lookups.Load(), hub.files.Load()
	for _, path := range []string{
		"/acme/tiny/resolve/main/model.safetensors",
		"/acme/tiny/resolve/" + hub.pushed.SHA + "/config.json",
		"/api/models/acme/tiny/revision/main",
		"/api/models/acme/tiny/tree/" + hub.pushed.SHA + "?recursive=true",
		"/api/v1/repos/acme/tiny/revisions/main",
	} {
		if resp := do(t, http.MethodGet, local.url+path, "", nil, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
	}
	if hub.lookups.Load() != lookups || hub.files.Load() != downloads {
		t.Fatalf("upstream was contacted again: lookups %d -> %d, files %d -> %d",
			lookups, hub.lookups.Load(), downloads, hub.files.Load())
	}
}

// Each kind of request a client might make first has to trigger the fetch.
func TestPullThroughStartsFromAnyEntryPoint(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	entry := map[string]string{
		"model info":       "/api/models/acme/tiny/revision/main",
		"model info, bare": "/api/models/acme/tiny",
		"file listing":     "/api/models/acme/tiny/tree/main?recursive=true",
		"file metadata":    "/acme/tiny/resolve/main/config.json",
		"pinned revision":  "/acme/tiny/resolve/" + hub.pushed.SHA + "/config.json",
		"registry API":     "/api/v1/repos/acme/tiny/revisions/main",
	}
	for name, path := range entry {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
			method := http.MethodGet
			if name == "file metadata" {
				method = http.MethodHead
			}
			resp := do(t, method, local.url+path, "", nil, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d (%s)", resp.StatusCode, body(t, resp))
			}
			if _, err := local.reg.Resolve(context.Background(), "acme/tiny", hub.pushed.SHA); err != nil {
				t.Fatalf("revision was not stored: %v", err)
			}
		})
	}
}

func TestPullThroughConcurrentRequestsShareOneFetch(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	hub.hold = make(chan struct{})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	const clients = 12
	var wg sync.WaitGroup
	statuses := make(chan int, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Different files and both ref forms: all the same revision.
			path := "/acme/tiny/resolve/main/config.json"
			if i%2 == 1 {
				path = "/api/models/acme/tiny/tree/main"
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, local.url+path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	// Let every client arrive while the upstream is still holding files back.
	deadline := time.Now().Add(5 * time.Second)
	for hub.files.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	close(hub.hold)
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != http.StatusOK {
			t.Errorf("a waiting client got status %d", s)
		}
	}
	if got := hub.lookups.Load(); got != 1 {
		t.Fatalf("upstream revision was looked up %d times for one revision, want 1", got)
	}
	if got := hub.files.Load(); got != int64(len(modelFiles)) {
		t.Fatalf("upstream served %d files, want each of the %d once", got, len(modelFiles))
	}
}

func TestPullThroughSurvivesTheClientLeaving(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	hub.hold = make(chan struct{})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	// A client with a short timeout, as download libraries have.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("request finished with %d before the upstream released any file", resp.StatusCode)
	}

	// The client is gone. The fetch must still complete.
	close(hub.hold)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := local.reg.Resolve(context.Background(), "acme/tiny", "main"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fetch stopped when the client that started it disconnected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The retry is answered at once, from the registry.
	lookups := hub.lookups.Load()
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
	if resp.StatusCode != http.StatusOK || hub.lookups.Load() != lookups {
		t.Fatalf("retry: status %d, upstream lookups %d -> %d", resp.StatusCode, lookups, hub.lookups.Load())
	}
}

func TestPullThroughIsOffByDefaultAndHonoursTheAllowList(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	tests := []struct {
		name   string
		cfg    server.Config
		want   int
		pulled bool
	}{
		{"off by default", server.Config{}, http.StatusNotFound, false},
		{"on, no allow list", server.Config{PullThrough: true}, http.StatusOK, true},
		{"on, repository matches", server.Config{PullThrough: true, PullThroughAllow: []string{"other/*", "acme/*"}}, http.StatusOK, true},
		{"on, repository does not match", server.Config{PullThrough: true, PullThroughAllow: []string{"other/*", "acme/large-*"}}, http.StatusNotFound, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.AnonymousRead, tc.cfg.Upstream = true, hub.url
			local := start(t, tc.cfg)
			before := hub.lookups.Load()
			resp := do(t, http.MethodHead, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if !tc.pulled {
				// Refused without asking the upstream, and reported the usual way.
				if resp.Header.Get("X-Error-Code") != "RepoNotFound" {
					t.Errorf("error code %q", resp.Header.Get("X-Error-Code"))
				}
				if hub.lookups.Load() != before {
					t.Error("the upstream was contacted for a repository pull-through does not cover")
				}
			}
			_, err := local.reg.GetRepo(context.Background(), "acme/tiny")
			if (err == nil) != tc.pulled {
				t.Fatalf("repository stored = %v, want %v", err == nil, tc.pulled)
			}
		})
	}
}

func TestPullThroughNeedsReadAccess(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	local := start(t, server.Config{AnonymousRead: false, Upstream: hub.url, PullThrough: true})

	for name, token := range map[string]string{"no token": "", "unknown token": "md_unknown_token_000000000000"} {
		resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", token, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
	if hub.lookups.Load() != 0 {
		t.Fatal("a caller without read access made the registry contact the upstream")
	}
	// A read token is enough: fetching is part of reading.
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", local.read, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read token: status %d", resp.StatusCode)
	}
}

func TestPullThroughWhenUpstreamDoesNotHaveIt(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	tests := []struct {
		path, code string
	}{
		{"/acme/absent/resolve/main/config.json", "RepoNotFound"},
		{"/acme/tiny/resolve/no-such-branch/config.json", "RepoNotFound"},
	}
	for _, tc := range tests {
		before := hub.lookups.Load()
		for range 3 {
			resp := do(t, http.MethodHead, local.url+tc.path, "", nil, nil)
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != tc.code {
				t.Fatalf("%s: status %d code %q", tc.path, resp.StatusCode, resp.Header.Get("X-Error-Code"))
			}
		}
		// A wrong name asked for three times reaches the upstream once.
		if got := hub.lookups.Load() - before; got != 1 {
			t.Errorf("%s: upstream asked %d times for three requests, want 1", tc.path, got)
		}
	}

	// A file that is not in a revision the registry does hold is a plain
	// 404; it is not a reason to contact the upstream.
	if resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	before := hub.lookups.Load()
	resp := do(t, http.MethodHead, local.url+"/acme/tiny/resolve/main/absent.bin", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != "EntryNotFound" || hub.lookups.Load() != before {
		t.Fatalf("missing file: status %d code %q, upstream lookups %d -> %d",
			resp.StatusCode, resp.Header.Get("X-Error-Code"), before, hub.lookups.Load())
	}
}

func TestPullThroughRejectsTamperedContentAndCanRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	hub.tampered.Store(true)
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	// Even a request for a file that is intact fails: a revision is
	// all-or-nothing, and one of its files does not verify.
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if msg := body(t, resp); !strings.Contains(msg, "could not fetch from upstream") || !strings.Contains(msg, "verification failed") {
		t.Fatalf("error does not explain itself: %s", msg)
	}
	if _, err := local.reg.GetRepo(ctx, "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Fatalf("a repository exists after a failed fetch: %v", err)
	}

	// A failure is not remembered: once the upstream is sound, the next
	// request succeeds.
	hub.tampered.Store(false)
	resp = do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["model.safetensors"] {
		t.Fatalf("retry: status %d", resp.StatusCode)
	}
}

func TestPullThroughFromAPrivateUpstream(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: false})

	t.Run("without an upstream token the cause is reported", func(t *testing.T) {
		local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
		resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body(t, resp), "private or gated") {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("with the operator's upstream token it works", func(t *testing.T) {
		local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, UpstreamToken: hub.inst.read, PullThrough: true})
		resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d (%s)", resp.StatusCode, body(t, resp))
		}
	})
}
