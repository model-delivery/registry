// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// Whoever holds a name on the upstream hub must not be able to put content
// into a repository of the same name that was created in this registry.
func TestPullThroughLeavesLocalRepositoriesAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	// The upstream's acme/tiny has a branch the local repository lacks.
	theirs := push(t, hub.inst, "acme/tiny", map[string]string{"config.json": "THEIRS"}, client.PushOptions{Branch: "dev"})
	cfg := server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true}

	t.Run("a pushed repository is never added to from upstream", func(t *testing.T) {
		local := start(t, cfg)
		mine := push(t, local, "acme/tiny", map[string]string{"config.json": "MINE"}, client.PushOptions{})
		lookups, downloads := hub.lookups.Load(), hub.files.Load()

		for _, path := range []string{
			"/acme/tiny/resolve/dev/config.json",
			"/acme/tiny/resolve/" + theirs.SHA + "/config.json",
			"/api/models/acme/tiny/revision/dev",
			"/api/models/acme/tiny/revision/" + theirs.SHA,
			"/api/models/acme/tiny/tree/dev",
			"/api/v1/repos/acme/tiny/revisions/dev",
			"/api/v1/repos/acme/tiny/revisions/" + theirs.SHA,
		} {
			resp := do(t, http.MethodGet, local.url+path, "", nil, nil)
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != "RevisionNotFound" {
				t.Errorf("%s: status %d code %q, want 404 RevisionNotFound", path, resp.StatusCode, resp.Header.Get("X-Error-Code"))
			}
		}
		if hub.lookups.Load() != lookups || hub.files.Load() != downloads {
			t.Errorf("the upstream was contacted for a repository that was pushed here: lookups %d -> %d, files %d -> %d",
				lookups, hub.lookups.Load(), downloads, hub.files.Load())
		}
		repo, err := local.reg.GetRepo(ctx, "acme/tiny")
		if err != nil || repo.Upstream != "" {
			t.Fatalf("upstream = %q (%v), want it left empty", repo.Upstream, err)
		}
		refs, _ := local.reg.Refs(ctx, repo)
		if len(refs) != 1 || refs[0].Name != "main" || refs[0].SHA != mine.SHA {
			t.Fatalf("refs = %+v, want only main at %s", refs, mine.SHA)
		}
		if got := body(t, do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)); got != "MINE" {
			t.Fatalf("main serves %q", got)
		}
	})

	t.Run("nor is one an upload has created but not committed to yet", func(t *testing.T) {
		local := start(t, cfg)
		resp := do(t, http.MethodPost, local.url+"/api/repos/create", local.write,
			jsonBody(t, map[string]any{"name": "tiny", "organization": "acme"}), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create: %d", resp.StatusCode)
		}
		lookups := hub.lookups.Load()
		// A reader asks for main before the upload's commit arrives.
		for _, path := range []string{"/acme/tiny/resolve/main/config.json", "/api/models/acme/tiny/revision/main"} {
			resp := do(t, http.MethodGet, local.url+path, "", nil, nil)
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != "RevisionNotFound" {
				t.Fatalf("%s: status %d code %q", path, resp.StatusCode, resp.Header.Get("X-Error-Code"))
			}
		}
		if hub.lookups.Load() != lookups {
			t.Fatal("the upstream was contacted for a repository that was created here")
		}
		// The upload then commits as if nothing had happened.
		if status, out := commitTo(t, local, "acme/tiny", "main", local.write, header("first", ""), inlineFile("config.json", "MINE")); status != http.StatusOK {
			t.Fatalf("commit: %d %s", status, out)
		}
		if got := body(t, do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)); got != "MINE" {
			t.Fatalf("main serves %q", got)
		}
		if repo, err := local.reg.GetRepo(ctx, "acme/tiny"); err != nil || repo.Upstream != "" {
			t.Fatalf("upstream = %q (%v), want it left empty", repo.Upstream, err)
		}
	})

	t.Run("a listed file in a repository created here is not fetched", func(t *testing.T) {
		// Data written before this rule existed could hold such a revision.
		local := start(t, cfg)
		push(t, local, "acme/tiny", map[string]string{"config.json": "MINE"}, client.PushOptions{})
		if _, err := local.reg.CommitLazy(ctx, registry.LazyInput{Repo: "acme/tiny", SHA: theirs.SHA,
			Files: []registry.File{{Path: "config.json", Size: int64(len("THEIRS")), SHA256: sha("THEIRS"), GitOID: strings.Repeat("a", 40), LFS: true}}}); err != nil {
			t.Fatal(err)
		}
		downloads := hub.files.Load()
		resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/"+theirs.SHA+"/config.json", "", nil, nil)
		if msg := body(t, resp); resp.StatusCode != http.StatusNotFound || !strings.Contains(msg, "modelctl mirror acme/tiny") {
			t.Fatalf("status %d: %s", resp.StatusCode, msg)
		}
		if hub.files.Load() != downloads {
			t.Fatal("the upstream was asked for a file of a repository that was created here")
		}
	})

	t.Run("a repository that came from upstream still gains revisions", func(t *testing.T) {
		local := start(t, cfg)
		for rev, want := range map[string]string{"main": modelFiles["config.json"], "dev": "THEIRS"} {
			resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/"+rev+"/config.json", "", nil, nil)
			if got := body(t, resp); resp.StatusCode != http.StatusOK || got != want {
				t.Fatalf("%s: status %d body %q", rev, resp.StatusCode, got)
			}
		}
	})
}

// renamedHub is an upstream on which acme/renamed has been renamed to
// internal/model: asked about the old name, it answers with the new one.
func renamedHub(t *testing.T) (url string, requests *atomic.Int64) {
	t.Helper()
	requests = &atomic.Int64{}
	sha := strings.Repeat("c", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models/acme/renamed/revision/main":
			_, _ = w.Write([]byte(`{"id":"internal/model","sha":"` + sha + `"}`))
		case "/api/models/internal/model/tree/" + sha:
			_, _ = w.Write([]byte(`[{"type":"file","path":"config.json","size":2,"oid":"` + strings.Repeat("d", 40) + `"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests
}

// The name a revision is stored under is the upstream's canonical one. The
// rules apply to that name too, not only to the one in the request.
func TestPullThroughAppliesItsRulesToTheCanonicalName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hubURL, _ := renamedHub(t)
	const path = "/api/models/acme/renamed/revision/main"

	t.Run("stored under the canonical name when nothing forbids it", func(t *testing.T) {
		local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL, PullThrough: true})
		if resp := do(t, http.MethodGet, local.url+path, "", nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d (%s)", resp.StatusCode, body(t, resp))
		}
		if _, err := local.reg.GetRepo(ctx, "internal/model"); err != nil {
			t.Fatalf("not stored under the canonical name: %v", err)
		}
	})
	t.Run("refused when the allow list does not cover the canonical name", func(t *testing.T) {
		local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL, PullThrough: true, PullThroughAllow: []string{"acme/*"}})
		if resp := do(t, http.MethodGet, local.url+path, "", nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d, want 404", resp.StatusCode)
		}
		if _, err := local.reg.GetRepo(ctx, "internal/model"); !errors.Is(err, registry.ErrRepoNotFound) {
			t.Fatalf("a repository outside the allow list was stored: %v", err)
		}
	})
	t.Run("refused when the canonical name is a repository created here", func(t *testing.T) {
		local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL, PullThrough: true})
		push(t, local, "internal/model", map[string]string{"config.json": "MINE"}, client.PushOptions{})
		if resp := do(t, http.MethodGet, local.url+path, "", nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d, want 404", resp.StatusCode)
		}
		repo, err := local.reg.GetRepo(ctx, "internal/model")
		if err != nil || repo.Upstream != "" {
			t.Fatalf("upstream = %q (%v), want it left empty", repo.Upstream, err)
		}
		if revs, _ := local.reg.Commits(ctx, repo); len(revs) != 1 {
			t.Fatalf("%d revisions, want only the pushed one", len(revs))
		}
	})
}

func TestPullThroughAsksUpstreamOnlyAboutValidRevisions(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	for name, rev := range map[string]string{
		"a space":            "not%20a%20ref",
		"two dots":           "a..b",
		"a tilde":            "main~1",
		"a control char":     "main%0d%0aX",
		"longer than a name": strings.Repeat("r", 300),
	} {
		resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/"+rev+"/config.json", "", nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, resp.StatusCode)
		}
	}
	if got := hub.lookups.Load(); got != 0 {
		t.Fatalf("the upstream was asked %d times about revisions that cannot exist", got)
	}
	if resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a valid revision: status %d", resp.StatusCode)
	}
}

// What the upstream writes in an error answer is not passed on to whoever
// asked the registry: it could be some other service's page.
func TestUpstreamErrorPagesAreNotRelayed(t *testing.T) {
	t.Parallel()
	const page = "internal status page: db-password=hunter2"
	_, hubURL, _ := upstreamHub(t, server.Config{AnonymousRead: true}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/revision/") {
				http.Error(w, page, http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hubURL, PullThrough: true})

	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
	msg := body(t, resp)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "upstream returned 500") {
		t.Fatalf("pull-through: status %d: %s", resp.StatusCode, msg)
	}
	if strings.Contains(msg, "hunter2") || strings.Contains(resp.Header.Get("X-Error-Message"), "hunter2") {
		t.Fatalf("pull-through relayed the upstream's error page: %s", msg)
	}

	resp = do(t, http.MethodPost, local.url+"/api/v1/mirror", local.write, jsonBody(t, map[string]any{"repo": "acme/tiny"}), nil)
	if msg := body(t, resp); resp.StatusCode < 400 || strings.Contains(msg, "hunter2") {
		t.Fatalf("mirror: status %d: %s", resp.StatusCode, msg)
	}
}

func TestPullThroughFileSizeLimit(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true, PullThroughMaxFileBytes: 100})

	// A file under the limit is fetched as usual.
	if resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("small file: status %d", resp.StatusCode)
	}
	// One over it is described, since that costs nothing, but not fetched.
	if resp := do(t, http.MethodHead, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD of a large file: status %d", resp.StatusCode)
	}
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	msg := body(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("large file: status %d, want 403: %s", resp.StatusCode, msg)
	}
	for _, want := range []string{"REGISTRY_PULL_THROUGH_MAX_FILE_BYTES", "modelctl mirror acme/tiny"} {
		if !strings.Contains(msg, want) || !strings.Contains(resp.Header.Get("X-Error-Message"), want) {
			t.Errorf("the refusal does not mention %q: %s", want, msg)
		}
	}
	if got := hub.files.Load(); got != 1 {
		t.Fatalf("upstream served %d files; only the small one should have been fetched", got)
	}
	// The command the error names does fetch it.
	if _, err := client.New(local.url, local.write).Mirror(context.Background(), "acme/tiny", "main", "", nil); err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["model.safetensors"] {
		t.Fatalf("after mirror: status %d", resp.StatusCode)
	}
}
