// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// elsewhere is a second server: somewhere the upstream should not be able
// to send the registry. It records every request that reaches it.
type elsewhere struct {
	*httptest.Server
	mu   sync.Mutex
	seen []*http.Request
}

func newElsewhere(t *testing.T, content string) *elsewhere {
	t.Helper()
	e := &elsewhere{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.seen = append(e.seen, r.Clone(context.Background()))
		e.mu.Unlock()
		_, _ = io.WriteString(w, content)
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *elsewhere) requests() []*http.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*http.Request(nil), e.seen...)
}

func (e *elsewhere) port(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(e.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

const (
	token    = "upstream-token-that-must-stay-home"
	treePath = "/api/models/acme/tiny/tree/" + hubSHA
	filePath = "/acme/tiny/resolve/" + hubSHA + "/config.json"
)

func TestListingPaginationStaysOnTheUpstreamHost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a next page on the same host is followed, with the token", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, map[string]string{"a.txt": "a", "b.txt": "b"})
		var tokens []string
		var mu sync.Mutex
		h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != treePath {
				return false
			}
			mu.Lock()
			tokens = append(tokens, r.Header.Get("Authorization"))
			mu.Unlock()
			listing := h.listing()
			if r.URL.Query().Get("cursor") == "" {
				// A relative link, as a hub may send.
				w.Header().Set("Link", `<`+treePath+`?cursor=2>; rel="next"`)
				writeJSON(w, listing[:1])
			} else {
				writeJSON(w, listing[1:])
			}
			return true
		}
		entries, err := (&Upstream{Endpoint: h.URL, Token: token}).tree(ctx, "acme/tiny", hubSHA)
		if err != nil || len(entries) != 2 {
			t.Fatalf("entries = %+v, %v", entries, err)
		}
		if len(tokens) != 2 || tokens[0] != "Bearer "+token || tokens[1] != "Bearer "+token {
			t.Fatalf("tokens sent = %q", tokens)
		}
	})

	t.Run("a next page on another host is refused, and never contacted", func(t *testing.T) {
		t.Parallel()
		other := newElsewhere(t, `[]`)
		h := newHub(t, map[string]string{"a.txt": "a"})
		h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != treePath {
				return false
			}
			w.Header().Set("Link", `<`+other.URL+`/page2>; rel="next"`)
			writeJSON(w, h.listing())
			return true
		}
		_, err := (&Upstream{Endpoint: h.URL, Token: token}).tree(ctx, "acme/tiny", hubSHA)
		if !errors.Is(err, ErrUnsafeUpstream) {
			t.Fatalf("got %v, want ErrUnsafeUpstream", err)
		}
		if got := other.requests(); len(got) != 0 {
			t.Fatalf("the other host was contacted %d times, with Authorization %q", len(got), got[0].Header.Get("Authorization"))
		}
	})
}

func writeJSON(w http.ResponseWriter, entries []map[string]any) {
	var b bytes.Buffer
	b.WriteString("[")
	for i, e := range entries {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"file","path":"` + e["path"].(string) + `","size":1,"oid":"` + e["oid"].(string) + `"}`)
	}
	b.WriteString("]")
	_, _ = w.Write(b.Bytes())
}

// redirectingHub answers the request for config.json with a redirect.
func redirectingHub(t *testing.T, location func() string) *hub {
	t.Helper()
	h := newHub(t, map[string]string{"config.json": "from the hub"})
	h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != filePath {
			return false
		}
		http.Redirect(w, r, location(), http.StatusFound)
		return true
	}
	return h
}

func TestRedirectsToLocalAddressesAreRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name string
		// to builds the redirect target from the other server's port.
		to func(port string) string
	}{
		{"an address written as a loopback literal", func(port string) string { return "http://127.0.0.1:" + port + "/internal" }},
		// A host name: only the connection itself shows where it leads.
		{"a name that resolves to a loopback address", func(port string) string { return "http://localhost:" + port + "/internal" }},
		{"the IPv6 loopback", func(port string) string { return "http://[::1]:" + port + "/internal" }},
		{"the unspecified address", func(port string) string { return "http://0.0.0.0:" + port + "/internal" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			other := newElsewhere(t, "an internal service's page")
			h := redirectingHub(t, func() string { return tc.to(other.port(t)) })
			// The default client: this is what the server uses.
			body := (&Upstream{Endpoint: h.URL, Token: token}).open(ctx, "acme/tiny", hubSHA, "config.json")
			defer func() { _ = body.Close() }()
			got, err := io.ReadAll(body)
			if !errors.Is(err, ErrUnsafeUpstream) {
				t.Fatalf("read %q, error %v; want ErrUnsafeUpstream", got, err)
			}
			if strings.Contains(err.Error(), "internal service") {
				t.Fatalf("the error carries the other service's content: %v", err)
			}
			if n := len(other.requests()); n != 0 {
				t.Fatalf("the local address was contacted %d times", n)
			}
		})
	}

	t.Run("a redirect within the upstream host is followed, with the token", func(t *testing.T) {
		t.Parallel()
		var auth string
		h := newHub(t, map[string]string{"config.json": "from the hub"})
		h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
			switch r.URL.Path {
			case filePath:
				http.Redirect(w, r, "/moved/config.json", http.StatusTemporaryRedirect)
			case "/moved/config.json":
				auth = r.Header.Get("Authorization")
				_, _ = io.WriteString(w, "from the hub")
			default:
				return false
			}
			return true
		}
		body := (&Upstream{Endpoint: h.URL, Token: token}).open(ctx, "acme/tiny", hubSHA, "config.json")
		defer func() { _ = body.Close() }()
		got, err := io.ReadAll(body)
		if err != nil || string(got) != "from the hub" || auth != "Bearer "+token {
			t.Fatalf("read %q, %v, Authorization %q", got, err, auth)
		}
	})

	t.Run("redirects that never end are given up on", func(t *testing.T) {
		t.Parallel()
		h := redirectingHub(t, func() string { return filePath })
		body := (&Upstream{Endpoint: h.URL}).open(ctx, "acme/tiny", hubSHA, "config.json")
		defer func() { _ = body.Close() }()
		// Not one of the errors that stop a retry, so cap the retries' wait.
		body.(*resumeReader).backoff = 1
		if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "redirected more than") {
			t.Fatalf("got %v", err)
		}
	})
}

// A hub's file storage is usually another host. The download follows the
// redirect there, and the token does not go with it.
func TestTokenIsOnlySentToTheUpstreamHost(t *testing.T) {
	t.Parallel()
	storage := newElsewhere(t, "from the hub")
	h := redirectingHub(t, func() string { return "http://cdn.hub.example/blob?signature=abc" })
	var hubAuth string
	redirect := h.wrap
	h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == filePath {
			hubAuth = r.Header.Get("Authorization")
		}
		return redirect(w, r)
	}
	// A client that reaches the stand-in for the hub's storage under a
	// public-looking name, since a test cannot connect anywhere public.
	storageAddr := strings.TrimPrefix(storage.URL, "http://")
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == "cdn.hub.example:80" {
				addr = storageAddr
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}

	body := (&Upstream{Endpoint: h.URL, Token: token, Client: client}).open(context.Background(), "acme/tiny", hubSHA, "config.json")
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "from the hub" {
		t.Fatalf("read %q, %v", got, err)
	}
	if hubAuth != "Bearer "+token {
		t.Fatalf("the upstream itself got Authorization %q", hubAuth)
	}
	seen := storage.requests()
	if len(seen) != 1 {
		t.Fatalf("storage saw %d requests, want 1", len(seen))
	}
	if auth := seen[0].Header.Get("Authorization"); auth != "" {
		t.Fatalf("the token was sent to another host: Authorization %q", auth)
	}
	// What the download needs does go along.
	if seen[0].Header.Get("Accept-Encoding") != "identity" || seen[0].URL.RawQuery != "signature=abc" {
		t.Fatalf("redirected request lost its headers or query: %v %q", seen[0].Header, seen[0].URL.RawQuery)
	}
}

func TestUpstreamErrorBodiesAreLoggedNotReturned(t *testing.T) {
	t.Parallel()
	const page = "<html>internal status page: db-password=hunter2</html>"
	h := newHub(t, map[string]string{"config.json": "{}"})
	h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/broken"):
			http.Error(w, page, http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/revision/gone"):
			// The one-line reason a hub gives in this header is passed on.
			w.Header().Set("X-Error-Message", "Revision not found")
			http.Error(w, page, http.StatusNotFound)
		default:
			return false
		}
		return true
	}
	var logged bytes.Buffer
	up := &Upstream{Endpoint: h.URL, Log: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	_, err := up.info(context.Background(), "acme/tiny", "broken")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("got %v, want an error naming the status", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error carries the upstream's response body: %v", err)
	}
	if !strings.Contains(logged.String(), "hunter2") {
		t.Fatalf("the response body was not logged: %s", logged.String())
	}

	_, err = up.info(context.Background(), "acme/tiny", "gone")
	if !errors.Is(err, ErrUpstreamNotFound) || !strings.Contains(err.Error(), "Revision not found") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("got %v", err)
	}
}

func TestIsLocal(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"127.0.0.1":        true,
		"127.8.9.10":       true,
		"::1":              true,
		"10.1.2.3":         true,
		"172.16.0.1":       true,
		"192.168.1.1":      true,
		"169.254.169.254":  true, // cloud metadata services
		"fe80::1":          true,
		"fe80::1%eth0":     true,
		"fc00::1":          true,
		"0.0.0.0":          true,
		"::":               true,
		"::ffff:127.0.0.1": true, // an IPv4 address in IPv6 clothing
		"::ffff:10.0.0.1":  true,
		"8.8.8.8":          false,
		"172.32.0.1":       false,
		"2606:4700::1111":  false,
		"::ffff:8.8.8.8":   false,
	}
	for addr, want := range tests {
		if got := isLocal(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isLocal(%s) = %v, want %v", addr, got, want)
		}
	}
}

// Files a hub keeps in git are named by git blob id, a SHA-1. A stored
// blob with the right id is reused only if it came from an upstream.
func TestGitBlobIDIsOnlyReusedFromMirroredContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const content = `{"small":true}`
	var downloads int
	var mu sync.Mutex
	h := newHub(t, map[string]string{"config.json": content})
	h.wrap = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == filePath {
			mu.Lock()
			downloads++
			mu.Unlock()
		}
		return false
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return downloads
	}
	reg := openRegistry(t)

	// The same bytes are already stored, uploaded by someone with the write
	// role and used by a revision that was pushed. Not reused: downloaded.
	seeded, err := reg.PutBlob(ctx, strings.NewReader(content), blob.Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Commit(ctx, registry.CommitInput{Repo: "acme/pushed", Branch: "main",
		Files: []registry.File{{Path: "config.json", SHA256: seeded.SHA256}}}); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}
	first, err := plan.Execute(ctx, reg, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count() != 1 || first.DownloadedBytes != int64(len(content)) || first.ReusedBytes != 0 {
		t.Fatalf("first mirror: %d downloads, %+v; want the file downloaded, not taken from a pushed blob", count(), first)
	}

	// Mirrored once, the content has come from an upstream: reused.
	second, err := plan.Execute(ctx, reg, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count() != 1 || second.ReusedBytes != int64(len(content)) {
		t.Fatalf("second mirror: %d downloads, %+v; want the mirrored blob reused", count(), second)
	}
}

// A mirror that reuses a stored blob marks it as in use, the way a push
// does, so garbage collection running meanwhile leaves it alone.
func TestMirrorTouchesTheBlobsItReuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const kept, other = "already stored, a while ago", "the other file"
	h := newHub(t, map[string]string{"a.bin": kept, "b.bin": other})
	h.lfs = map[string]bool{"a.bin": true, "b.bin": true}
	// The second file arrives damaged, so the mirror fails and records
	// nothing: the reused blob stays unreferenced, as it is mid-mirror.
	h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/b.bin") {
			_, _ = io.WriteString(w, "damaged on the way")
			return true
		}
		return false
	}
	reg := openRegistry(t)
	seeded, err := reg.PutBlob(ctx, strings.NewReader(kept), blob.Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	// Let the blob grow older than the age garbage collection spares.
	const spared = time.Second
	time.Sleep(spared + 200*time.Millisecond)

	plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Execute(ctx, reg, 1, nil); !errors.Is(err, blob.ErrMismatch) {
		t.Fatalf("mirror: %v, want it to fail on the damaged file", err)
	}
	if _, err := reg.GC(ctx, spared); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := reg.StatBlob(ctx, seeded.SHA256); !ok {
		t.Fatal("garbage collection removed a blob the mirror had just reused")
	}
}
