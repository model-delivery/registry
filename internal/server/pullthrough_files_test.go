// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// Pull-through fetches what is asked for and nothing more: the file
// listing on the first request for a revision, then each file's content on
// that file's first download.

func pending(t *testing.T, in *instance, repo, rev string) int {
	t.Helper()
	c, err := in.reg.Resolve(context.Background(), repo, rev)
	if err != nil {
		t.Fatal(err)
	}
	n, err := in.reg.PendingCount(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPullThroughFetchesOnlyTheFilesAskedFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	// Nothing is in the registry. A client asks for one small file.
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/tokenizer/vocab.txt", "", nil, nil)
	if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["tokenizer/vocab.txt"] {
		t.Fatalf("first request: status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Repo-Commit"); got != hub.pushed.SHA {
		t.Fatalf("served revision %s, upstream is %s", got, hub.pushed.SHA)
	}
	// One file was downloaded from upstream, not the whole repository.
	if got := hub.files.Load(); got != 1 {
		t.Fatalf("upstream served %d files for a request for one", got)
	}
	// The revision is recorded under its upstream id with its complete listing.
	c, err := local.reg.Resolve(ctx, "acme/tiny", "main")
	if err != nil || c.SHA != hub.pushed.SHA || c.Source != registry.SourceMirror {
		t.Fatalf("recorded revision = %+v, %v", c, err)
	}
	files, _ := local.reg.Files(ctx, c)
	if len(files) != len(modelFiles) || pending(t, local, "acme/tiny", "main") != len(modelFiles)-1 {
		t.Fatalf("listing has %d files with %d pending; want %d and %d",
			len(files), pending(t, local, "acme/tiny", "main"), len(modelFiles), len(modelFiles)-1)
	}

	t.Run("listings and metadata are complete and cost nothing more", func(t *testing.T) {
		lookups, downloads := hub.lookups.Load(), hub.files.Load()

		// What a client asks before downloading: size, identity, revision.
		want := modelFiles["model.safetensors"]
		head := do(t, http.MethodHead, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
		if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(want)) ||
			head.Header.Get("ETag") != `"`+sha(want)+`"` || head.Header.Get("X-Repo-Commit") != hub.pushed.SHA ||
			head.Header.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("HEAD of a file not fetched yet: status %d length %d headers %v", head.StatusCode, head.ContentLength, head.Header)
		}

		tree := do(t, http.MethodGet, local.url+"/api/models/acme/tiny/tree/main?recursive=true", "", nil, nil)
		var entries []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			LFS  *struct {
				OID string `json:"oid"`
			} `json:"lfs"`
		}
		decode(t, tree, &entries)
		listed := map[string]string{}
		for _, e := range entries {
			if e.Type == "file" {
				if e.LFS == nil || e.Size != int64(len(modelFiles[e.Path])) {
					t.Errorf("listing entry %s: %+v", e.Path, e)
					continue
				}
				listed[e.Path] = e.LFS.OID
			}
		}
		for p, content := range modelFiles {
			if listed[p] != sha(content) {
				t.Errorf("listing has the wrong digest for %s", p)
			}
		}

		info := do(t, http.MethodGet, local.url+"/api/models/acme/tiny/revision/main", "", nil, nil)
		var doc struct {
			Siblings []json.RawMessage `json:"siblings"`
		}
		decode(t, info, &doc)
		if len(doc.Siblings) != len(modelFiles) {
			t.Errorf("model info lists %d files", len(doc.Siblings))
		}

		// The web page says which files are still to be fetched.
		page := do(t, http.MethodGet, local.url+"/ui/repo/acme/tiny", "", nil, nil)
		if got := body(t, page); page.StatusCode != http.StatusOK || strings.Count(got, "fetched on demand") != len(modelFiles)-1 {
			t.Errorf("repository page: status %d, marks %d files as fetched on demand, want %d",
				page.StatusCode, strings.Count(got, "fetched on demand"), len(modelFiles)-1)
		}

		// The file already fetched is served again without asking upstream.
		again := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/"+hub.pushed.SHA+"/tokenizer/vocab.txt", "", nil, nil)
		if again.StatusCode != http.StatusOK {
			t.Errorf("second download: %d", again.StatusCode)
		}

		if hub.lookups.Load() != lookups || hub.files.Load() != downloads {
			t.Fatalf("upstream was contacted: lookups %d -> %d, files %d -> %d",
				lookups, hub.lookups.Load(), downloads, hub.files.Load())
		}
	})

	t.Run("each further file is fetched once, when first downloaded", func(t *testing.T) {
		for range 3 {
			resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
			if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["model.safetensors"] {
				t.Fatalf("status %d", resp.StatusCode)
			}
		}
		if got := hub.files.Load(); got != 2 {
			t.Fatalf("upstream has served %d files after two distinct files were downloaded", got)
		}
		// A range request for a file not fetched yet works too.
		part := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, map[string]string{"Range": "bytes=2-11"})
		if part.StatusCode != http.StatusPartialContent || body(t, part) != modelFiles["config.json"][2:12] {
			t.Fatalf("range on a pending file: status %d", part.StatusCode)
		}
		if n := pending(t, local, "acme/tiny", "main"); n != 0 {
			t.Fatalf("%d files still pending after all were downloaded", n)
		}
	})

	t.Run("a revision fetched piece by piece ends up identical to a mirrored one", func(t *testing.T) {
		mirrored := start(t, server.Config{AnonymousRead: true, Upstream: hub.url})
		if _, err := client.New(mirrored.url, mirrored.write).Mirror(ctx, "acme/tiny", "main", "", nil); err != nil {
			t.Fatal(err)
		}
		a, _ := local.reg.Files(ctx, c)
		mc, _ := mirrored.reg.Resolve(ctx, "acme/tiny", "main")
		b, _ := mirrored.reg.Files(ctx, mc)
		if len(a) != len(b) {
			t.Fatalf("%d files against %d", len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("file %d differs:\n pulled   %+v\n mirrored %+v", i, a[i], b[i])
			}
		}
	})
}

func TestPullThroughConcurrentDownloadsOfOneFileShareOneFetch(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	hub.hold = make(chan struct{})
	// The registry under test, behind a handler that counts the requests
	// that have reached it.
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var arrived, downloadsAnswered atomic.Int64
	handler := server.New(reg, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		local.Close()
		_ = reg.Close()
	})

	const clients = 12
	var wg sync.WaitGroup
	statuses := make(chan int, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The same file by branch and by listing: all one revision.
			path, download := "/acme/tiny/resolve/main/model.safetensors", true
			if i%3 == 2 {
				path, download = "/api/models/acme/tiny/tree/main", false
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, local.URL+path, nil)
			resp, err := http.DefaultClient.Do(req)
			if download {
				downloadsAnswered.Add(1)
			}
			if err != nil {
				statuses <- 0
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	// The upstream is released only once every request is inside the
	// registry and the fetch has reached the upstream, which is holding the
	// file back. No sleep stands in for that: a request that arrived after
	// the fetch had finished would be served from storage and prove nothing
	// about sharing one.
	deadline := time.Now().Add(10 * time.Second)
	for arrived.Load() < clients || hub.files.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d requests reached the registry and %d fetches reached the upstream", arrived.Load(), clients, hub.files.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Every download is in, the file cannot be served before it is fetched,
	// and the fetch is held: so all of them are waiting, on that one fetch.
	if got := downloadsAnswered.Load(); got != 0 {
		t.Fatalf("%d downloads were answered while the upstream still held the file", got)
	}
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
	if got := hub.files.Load(); got != 1 {
		t.Fatalf("upstream served the file %d times to %d clients, want once", got, clients)
	}
}

func TestPullThroughVerifiesEachFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	hub.tampered.Store(true) // the upstream sends altered bytes for model.safetensors
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	// A file that arrives intact is served.
	if resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("intact file: status %d", resp.StatusCode)
	}

	// The altered one is refused, with the reason, and nothing of it is kept.
	resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("altered file: status %d, want 502", resp.StatusCode)
	}
	if msg := body(t, resp); !strings.Contains(msg, "could not fetch from upstream") || !strings.Contains(msg, "verification failed") {
		t.Fatalf("error does not explain itself: %s", msg)
	}
	c, _ := local.reg.Resolve(ctx, "acme/tiny", "main")
	if f, err := local.reg.File(ctx, c, "model.safetensors"); err != nil || !f.Pending {
		t.Fatalf("the altered file is recorded as stored: %+v, %v", f, err)
	}
	if _, ok, _ := local.reg.StatBlob(ctx, sha("not the real weights")); ok {
		t.Fatal("the altered bytes were stored")
	}
	if _, ok, _ := local.reg.StatBlob(ctx, sha(modelFiles["model.safetensors"])); ok {
		t.Fatal("content was stored under the genuine digest although it never arrived")
	}

	// A failure is not remembered: once the upstream is sound, the download works.
	hub.tampered.Store(false)
	resp = do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["model.safetensors"] {
		t.Fatalf("retry: status %d", resp.StatusCode)
	}
}

func TestMirrorCompletesARevisionPullThroughStarted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	// A client downloads one file; the rest of the revision is only listed.
	if resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if n := pending(t, local, "acme/tiny", "main"); n != len(modelFiles)-1 {
		t.Fatalf("%d pending", n)
	}

	// Until then, a file that was never fetched depends on the upstream.
	t.Run("an unfetched file needs the upstream", func(t *testing.T) {
		// Stand-in for "the upstream cannot supply this file": it sends bytes
		// that fail verification.
		hub.tampered.Store(true)
		defer hub.tampered.Store(false)
		resp := do(t, http.MethodGet, local.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", resp.StatusCode)
		}
	})

	// `modelctl mirror` fetches everything that is missing, and only that.
	before := hub.files.Load()
	done, err := client.New(local.url, local.write).Mirror(ctx, "acme/tiny", "main", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if done.SHA != hub.pushed.SHA || pending(t, local, "acme/tiny", "main") != 0 {
		t.Fatalf("after mirror: %+v, %d pending", done, pending(t, local, "acme/tiny", "main"))
	}
	if got := hub.files.Load() - before; got != int64(len(modelFiles)-1) {
		t.Fatalf("mirror downloaded %d files; %d were missing", got, len(modelFiles)-1)
	}
	if done.ReusedBytes != int64(len(modelFiles["config.json"])) {
		t.Errorf("mirror reused %d bytes, want the one file already fetched (%d)", done.ReusedBytes, len(modelFiles["config.json"]))
	}

	// Complete now: everything is served with the upstream out of the picture.
	lookups, downloads := hub.lookups.Load(), hub.files.Load()
	dir := t.TempDir()
	if _, err := client.New(local.url, "").Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
		t.Fatal(err)
	}
	if hub.lookups.Load() != lookups || hub.files.Load() != downloads {
		t.Fatal("a complete revision still reached the upstream")
	}
}

func TestPendingFilesWhenPullThroughIsOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	// Record a revision's listing the way pull-through does, then serve it
	// from a registry that has pull-through turned off.
	on := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	if resp := do(t, http.MethodGet, on.url+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	off := httptestServer(t, on.reg, server.Config{AnonymousRead: true, Upstream: hub.url})
	downloads := hub.files.Load()

	// The fetched file is served; the others say plainly what is going on.
	if resp := do(t, http.MethodGet, off+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("fetched file: status %d", resp.StatusCode)
	}
	resp := do(t, http.MethodGet, off+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
	msg := body(t, resp)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(msg, "modelctl mirror acme/tiny") || !strings.Contains(msg, "pull-through is off") {
		t.Fatalf("status %d: %s", resp.StatusCode, msg)
	}
	if hub.files.Load() != downloads {
		t.Fatal("a registry with pull-through off contacted the upstream")
	}

	// A revision cannot be built on files that were never fetched.
	status, out := commitTo(t, &instance{reg: on.reg, url: off, write: on.write}, "acme/tiny", "main", on.write,
		header("on top", ""), inlineFile("extra.txt", "x"))
	if status != http.StatusPreconditionFailed || !strings.Contains(out, "modelctl mirror acme/tiny") {
		t.Fatalf("upload onto a partly fetched branch: status %d: %s", status, out)
	}
	if c, err := on.reg.Resolve(ctx, "acme/tiny", "main"); err != nil || c.SHA != hub.pushed.SHA {
		t.Fatalf("the refused upload moved the branch: %v %v", c.SHA, err)
	}

	// Deleting the repository removes the listing along with the content.
	if _, err := on.reg.DeleteRepo(ctx, "acme/tiny"); err != nil {
		t.Fatal(err)
	}
	if _, err := on.reg.GetRepo(ctx, "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Fatalf("repository still exists: %v", err)
	}
	if _, ok, _ := on.reg.StatBlob(ctx, sha(modelFiles["config.json"])); ok {
		t.Fatal("the repository's fetched content was left behind")
	}
	// The refused upload's file was stored before the commit was refused. It
	// is unreferenced, and garbage collection removes it.
	if res, err := on.reg.GC(ctx, 0); err != nil || res.Blobs != 1 {
		t.Fatalf("gc = %+v, %v; want the one orphan removed", res, err)
	}
	if st, _ := on.reg.Stats(ctx); st.Blobs != 0 {
		t.Fatalf("%d blobs left", st.Blobs)
	}
}

// Small files on a hub are kept in git and identified by a git blob id, not
// a SHA-256. Pull-through has to list, describe and verify those too.
func TestPullThroughOfFilesIdentifiedByGitBlobID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := start(t, server.Config{AnonymousRead: true})
	// An upload from a Hugging Face client stores small files this way.
	if status, out := commitTo(t, hub, "acme/git", "main", hub.write, header("first", ""),
		inlineFile("config.json", `{"small":true}`), inlineFile("README.md", "# readme\n")); status != http.StatusOK {
		t.Fatalf("seed the hub: %d %s", status, out)
	}
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	head := do(t, http.MethodHead, local.url+"/acme/git/resolve/main/config.json", "", nil, nil)
	upstream := do(t, http.MethodHead, hub.url+"/acme/git/resolve/main/config.json", "", nil, nil)
	etag := head.Header.Get("ETag")
	if head.StatusCode != http.StatusOK || len(etag) != 42 || etag != upstream.Header.Get("ETag") {
		t.Fatalf("HEAD before fetching: status %d, ETag %s, upstream's %s", head.StatusCode, etag, upstream.Header.Get("ETag"))
	}
	c, _ := local.reg.Resolve(ctx, "acme/git", "main")
	if f, _ := local.reg.File(ctx, c, "config.json"); !f.Pending || f.LFS || f.SHA256 != "" {
		t.Fatalf("before fetching: %+v", f)
	}

	resp := do(t, http.MethodGet, local.url+"/acme/git/resolve/main/config.json", "", nil, nil)
	if resp.StatusCode != http.StatusOK || body(t, resp) != `{"small":true}` || resp.Header.Get("ETag") != etag {
		t.Fatalf("download: status %d ETag %s", resp.StatusCode, resp.Header.Get("ETag"))
	}
	if f, _ := local.reg.File(ctx, c, "config.json"); f.Pending || f.SHA256 != sha(`{"small":true}`) {
		t.Fatalf("after fetching: %+v", f)
	}
}
