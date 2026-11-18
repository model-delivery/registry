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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// These tests mirror from one registry into another. The upstream registry
// speaks the same hub API that huggingface.co does, so this exercises the
// whole mirror path without the network.

// upstreamHub starts a registry holding acme/tiny and returns it together
// with a URL that serves it through wrap (for injecting faults).
func upstreamHub(t *testing.T, cfg server.Config, wrap func(http.Handler) http.Handler) (*instance, string, client.PushResult) {
	t.Helper()
	hub := start(t, cfg)
	pushed := push(t, hub, "acme/tiny", modelFiles, client.PushOptions{})
	if wrap == nil {
		return hub, hub.url, pushed
	}
	target, err := url.Parse(hub.url)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(wrap(httputil.NewSingleHostReverseProxy(target)))
	t.Cleanup(front.Close)
	return hub, front.URL, pushed
}

func TestMirrorThenServeWithoutUpstream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub, hubURL, pushed := upstreamHub(t, server.Config{AnonymousRead: true}, nil)
	local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL})

	var events []client.MirrorEvent
	done, err := client.New(local.url, local.write).Mirror(ctx, "acme/tiny", "main", "", func(e client.MirrorEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	// The upstream revision id is preserved exactly.
	if done.SHA != pushed.SHA || done.Repo != "acme/tiny" || done.Files != len(modelFiles) {
		t.Fatalf("done = %+v, want revision %s", done, pushed.SHA)
	}
	if len(events) != 1+len(modelFiles) || events[0].Event != "start" {
		t.Fatalf("got %d progress events", len(events))
	}

	// Take the upstream away entirely. Everything must still be served.
	_ = hub.reg.Close()

	dir := t.TempDir()
	rev, err := client.New(local.url, "").Pull(ctx, "acme/tiny", "main", dir, nil)
	if err != nil {
		t.Fatalf("pull after upstream went away: %v", err)
	}
	if rev.SHA != pushed.SHA {
		t.Fatalf("main resolves to %s, want upstream revision %s", rev.SHA, pushed.SHA)
	}
	for p, want := range modelFiles {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || string(got) != want {
			t.Errorf("%s differs from upstream (%v)", p, err)
		}
	}

	// Provenance is recorded.
	repo, err := local.reg.GetRepo(ctx, "acme/tiny")
	if err != nil || repo.Upstream != hubURL {
		t.Fatalf("repo upstream = %q (%v), want %s", repo.Upstream, err, hubURL)
	}
	c, _ := local.reg.Resolve(ctx, "acme/tiny", pushed.SHA)
	if c.Source != registry.SourceMirror {
		t.Fatalf("revision source = %q", c.Source)
	}

	// The same headers a hub client pins on are identical to upstream's.
	resp := do(t, http.MethodHead, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	if resp.Header.Get("X-Repo-Commit") != pushed.SHA || resp.Header.Get("ETag") != `"`+sha(modelFiles["model.safetensors"])+`"` {
		t.Fatalf("headers = %v", resp.Header)
	}
}

func TestMirrorTwiceDownloadsNothingNew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var fileRequests atomic.Int64
	_, hubURL, _ := upstreamHub(t, server.Config{AnonymousRead: true}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/resolve/") {
				fileRequests.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL})
	cl := client.New(local.url, local.write)

	first, err := cl.Mirror(ctx, "acme/tiny", "main", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReusedBytes != 0 || fileRequests.Load() != int64(len(modelFiles)) {
		t.Fatalf("first mirror: reused %d bytes, %d file requests", first.ReusedBytes, fileRequests.Load())
	}
	before, _ := local.reg.Stats(ctx)

	second, err := cl.Mirror(ctx, "acme/tiny", "main", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := local.reg.Stats(ctx)
	if second.DownloadedBytes != 0 || fileRequests.Load() != int64(len(modelFiles)) {
		t.Fatalf("second mirror downloaded %d bytes with %d total file requests", second.DownloadedBytes, fileRequests.Load())
	}
	if after != before {
		t.Fatalf("second mirror changed storage: %+v -> %+v", before, after)
	}
}

func TestMirrorRejectsCorruptedContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The upstream lists the right digests but sends different bytes for
	// one file, as a broken proxy or a tampering middlebox would.
	_, hubURL, _ := upstreamHub(t, server.Config{AnonymousRead: true}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/model.safetensors") {
				body := strings.Repeat("tampered", len(modelFiles["model.safetensors"])/8)
				w.Header().Set("Content-Length", itoa(len(body)))
				_, _ = w.Write([]byte(body))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL})

	_, err := client.New(local.url, local.write).Mirror(ctx, "acme/tiny", "main", "", nil)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("mirror of tampered content: %v, want a verification failure", err)
	}
	// Nothing of the failed mirror is visible: no repository, no revision.
	if _, err := local.reg.GetRepo(ctx, "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Fatalf("a repository exists after a failed mirror: %v", err)
	}
	resp := do(t, http.MethodHead, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("tampered file is being served: status %d", resp.StatusCode)
	}
}

func TestMirrorResumesDroppedConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var dropped atomic.Bool
	var rangeSeen atomic.Value
	_, hubURL, pushed := upstreamHub(t, server.Config{AnonymousRead: true}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/model.safetensors") {
				next.ServeHTTP(w, r)
				return
			}
			if dropped.CompareAndSwap(false, true) {
				// Promise the whole file, send part of it, then cut the line.
				full := modelFiles["model.safetensors"]
				w.Header().Set("Content-Length", itoa(len(full)))
				_, _ = w.Write([]byte(full[:9000]))
				panic(http.ErrAbortHandler)
			}
			rangeSeen.Store(r.Header.Get("Range"))
			next.ServeHTTP(w, r)
		})
	})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL})

	done, err := client.New(local.url, local.write).Mirror(ctx, "acme/tiny", "main", "", nil)
	if err != nil {
		t.Fatalf("mirror did not survive a dropped connection: %v", err)
	}
	if done.SHA != pushed.SHA {
		t.Fatalf("revision %s, want %s", done.SHA, pushed.SHA)
	}
	if got, _ := rangeSeen.Load().(string); got != "bytes=9000-" {
		t.Fatalf("retry asked for Range %q, want it to continue from byte 9000", got)
	}
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	if body(t, resp) != modelFiles["model.safetensors"] {
		t.Fatal("resumed file differs from the original")
	}
}

func TestMirrorPrivateUpstream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub, hubURL, pushed := upstreamHub(t, server.Config{AnonymousRead: false}, nil)
	local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL})
	cl := client.New(local.url, local.write)

	tests := []struct {
		name    string
		token   string
		wantErr string
	}{
		{"no upstream token", "", "private or gated"},
		{"wrong upstream token", "md_not_a_real_token_at_all", "rejected or lacks access"},
		{"valid upstream token", hub.read, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, err := cl.Mirror(ctx, "acme/tiny", "main", tc.token, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
				}
				if _, err := local.reg.GetRepo(ctx, "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
					t.Fatalf("repository created despite upstream refusing: %v", err)
				}
				return
			}
			if err != nil || done.SHA != pushed.SHA {
				t.Fatalf("mirror = %+v, %v", done, err)
			}
		})
	}
}

func TestMirrorErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, hubURL, pushed := upstreamHub(t, server.Config{AnonymousRead: true}, nil)
	local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL})
	cl := client.New(local.url, local.write)

	tests := []struct {
		name, repo, rev, wantErr string
	}{
		{"unknown repository", "acme/absent", "main", "not found upstream"},
		{"unknown revision", "acme/tiny", "no-such-branch", "not found upstream"},
		{"malformed repository", "not-a-repo-id", "main", "namespace/name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cl.Mirror(ctx, tc.repo, tc.rev, "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}

	t.Run("mirroring by revision id records it without moving a branch", func(t *testing.T) {
		done, err := cl.Mirror(ctx, "acme/tiny", pushed.SHA, "", nil)
		if err != nil || done.SHA != pushed.SHA || done.Ref != "" {
			t.Fatalf("done = %+v, %v", done, err)
		}
		if _, err := local.reg.Resolve(ctx, "acme/tiny", "main"); !errors.Is(err, registry.ErrRevisionNotFound) {
			t.Fatalf("a branch was created: %v", err)
		}
		if _, err := local.reg.Resolve(ctx, "acme/tiny", pushed.SHA); err != nil {
			t.Fatalf("revision id does not resolve: %v", err)
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }
