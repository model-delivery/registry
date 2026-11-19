// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// tempBytes is the total size of the files directly in dir.
func tempBytes(dir string) int64 {
	var total int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
	}
	return total
}

// The sign-in form is open to everyone. A large multipart body sent to it
// must not be written to the server's temporary directory.
func TestFormBodiesAreBounded(t *testing.T) {
	// Not parallel: it points the process's temporary directory at its own.
	tmp := t.TempDir()
	for _, key := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(key, tmp)
	}
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	push(t, in, "acme/tiny", modelFiles, client.PushOptions{})

	// post streams a multipart body with one 48 MiB file part, which is more
	// than a form parser keeps in memory, and reports the most that was on
	// disk in the temporary directory while the request was open.
	post := func(path, field string, header map[string]string) (status int, spooled int64) {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			part, err := mw.CreateFormFile(field, "x.bin")
			chunk := make([]byte, 1<<20)
			for i := 0; err == nil && i < 48; i++ {
				_, err = part.Write(chunk)
			}
			_ = mw.Close()
			_ = pw.CloseWithError(err)
		}()
		var most atomic.Int64
		stop := make(chan struct{})
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			for {
				if n := tempBytes(tmp); n > most.Load() {
					most.Store(n)
				}
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
				}
			}
		}()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, in.URL+path, pr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		for k, v := range header {
			req.Header.Set(k, v)
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		// The server stops reading and closes the connection, so the client
		// may see its answer or only a broken connection. Either is fine.
		if err == nil {
			status = resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		_ = pr.Close()
		close(stop)
		<-sampled
		return status, most.Load()
	}

	t.Run("sign-in, without a token", func(t *testing.T) {
		status, spooled := post("/ui/login", "token", nil)
		if spooled > 1<<20 {
			t.Fatalf("%d bytes of an anonymous request body were written to the temporary directory", spooled)
		}
		if status != 0 && status != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", status)
		}
	})
	t.Run("delete, as an admin", func(t *testing.T) {
		status, spooled := post("/ui/repo/acme/tiny/delete", "confirm", map[string]string{"Cookie": "md_token=" + in.Admin})
		if spooled > 1<<20 {
			t.Fatalf("%d bytes of a request body were written to the temporary directory", spooled)
		}
		if status != 0 && status != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", status)
		}
		if _, err := in.Reg.GetRepo(context.Background(), "acme/tiny"); err != nil {
			t.Fatalf("the repository was deleted by a form that could not be read: %v", err)
		}
	})
	// An ordinary form still works.
	resp := do(t, http.MethodPost, in.URL+"/ui/login", "", strings.NewReader("token="+in.Admin),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ordinary sign-in: status %d", resp.StatusCode)
	}
}

// chunked hides a reader's length, so the request is sent without a
// Content-Length.
type chunked struct{ io.Reader }

func TestUploadSizeLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := regtest.Start(t, server.Config{AnonymousRead: true, MaxBlobBytes: 1000})
	small, large := strings.Repeat("s", 1000), strings.Repeat("L", 1001)
	stored := func(c string) bool {
		_, ok, _ := in.Reg.StatBlob(ctx, kit.SHA256Hex(c))
		return ok
	}
	refused := func(t *testing.T, resp *http.Response) {
		t.Helper()
		msg := body(t, resp)
		if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(msg, "REGISTRY_MAX_BLOB_BYTES") {
			t.Fatalf("status %d, want 413 naming the setting: %s", resp.StatusCode, msg)
		}
		if stored(large) {
			t.Fatal("a file over the limit was stored")
		}
	}

	t.Run("registry API, declared length", func(t *testing.T) {
		refused(t, do(t, http.MethodPut, in.URL+"/api/v1/blobs/"+kit.SHA256Hex(large), in.Write, strings.NewReader(large), nil))
	})
	t.Run("registry API, no declared length", func(t *testing.T) {
		refused(t, do(t, http.MethodPut, in.URL+"/api/v1/blobs/"+kit.SHA256Hex(large), in.Write, chunked{strings.NewReader(large)}, nil))
	})
	t.Run("a file at the limit is accepted either way", func(t *testing.T) {
		resp := do(t, http.MethodPut, in.URL+"/api/v1/blobs/"+kit.SHA256Hex(small), in.Write, chunked{strings.NewReader(small)}, nil)
		if resp.StatusCode != http.StatusCreated || !stored(small) {
			t.Fatalf("status %d, stored %v", resp.StatusCode, stored(small))
		}
	})
	t.Run("large-file batch refuses per object", func(t *testing.T) {
		other := strings.Repeat("o", 500)
		resp := do(t, http.MethodPost, in.URL+"/acme/up.git/info/lfs/objects/batch", in.Write, jsonBody(t, map[string]any{
			"operation": "upload",
			"objects":   []map[string]any{{"oid": kit.SHA256Hex(large), "size": len(large)}, {"oid": kit.SHA256Hex(other), "size": len(other)}},
		}), nil)
		var out struct {
			Objects []struct {
				OID     string         `json:"oid"`
				Actions map[string]any `json:"actions"`
				Error   *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"objects"`
		}
		decode(t, resp, &out)
		if resp.StatusCode != http.StatusOK || len(out.Objects) != 2 {
			t.Fatalf("status %d, %d objects", resp.StatusCode, len(out.Objects))
		}
		big, ok := out.Objects[0], out.Objects[1]
		if big.Error == nil || big.Error.Code != http.StatusRequestEntityTooLarge || len(big.Actions) != 0 ||
			!strings.Contains(big.Error.Message, "REGISTRY_MAX_BLOB_BYTES") {
			t.Fatalf("object over the limit: %+v", big)
		}
		if ok.Error != nil || len(ok.Actions) == 0 {
			t.Fatalf("object under the limit: %+v", ok)
		}
	})
	t.Run("a file sent inside a commit", func(t *testing.T) {
		status, out := commitTo(t, in, "acme/up", "main", in.Write, header("m", ""), inlineFile("big.txt", large))
		if status != http.StatusRequestEntityTooLarge || !strings.Contains(out, "REGISTRY_MAX_BLOB_BYTES") || stored(large) {
			t.Fatalf("status %d: %s", status, out)
		}
	})
}

func TestCommitBodyHasATotalLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := regtest.Start(t, server.Config{AnonymousRead: true, MaxCommitBytes: 1 << 20})
	if status, out := commitTo(t, in, "acme/up", "main", in.Write, header("first", ""), inlineFile("a.txt", "a")); status != http.StatusOK {
		t.Fatalf("a small commit: %d %s", status, out)
	}
	before, _ := in.Reg.Resolve(ctx, "acme/up", "main")

	// Every line is far below the per-file limit; together they are too much.
	lines := []map[string]any{header("many", "")}
	for i := range 8 {
		lines = append(lines, inlineFile(fmt.Sprintf("part-%d.bin", i), strings.Repeat("x", 200<<10)))
	}
	status, out := commitTo(t, in, "acme/up", "main", in.Write, lines...)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(out, "in total") {
		t.Fatalf("status %d: %s", status, out)
	}
	if after, _ := in.Reg.Resolve(ctx, "acme/up", "main"); after.SHA != before.SHA {
		t.Fatal("a refused commit moved the branch")
	}
}

// A client that sends its headers and then stalls on a small body must not
// hold a connection for ever.
func TestSmallBodiesHaveADeadline(t *testing.T) {
	t.Parallel()
	in := regtest.Start(t, server.Config{AnonymousRead: true, BodyTimeout: 200 * time.Millisecond})
	tests := []struct {
		name, path, token, contentType, sent string
		want                                 int
		say                                  string
	}{
		{"hugging face JSON route", "/api/repos/create", in.Write, "application/json", `{"name":`, http.StatusBadRequest, "did not arrive"},
		{"registry API JSON route", "/api/v1/tokens", in.Admin, "application/json", `{"name":`, http.StatusBadRequest, "did not arrive"},
		{"sign-in form, no token", "/ui/login", "", "application/x-www-form-urlencoded", "token=md_", http.StatusUnauthorized, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, err := net.Dial("tcp", strings.TrimPrefix(in.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			auth := ""
			if tc.token != "" {
				auth = "Authorization: Bearer " + tc.token + "\r\n"
			}
			// Promise 200 bytes, send a few, and then say nothing more.
			fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: registry\r\n%sContent-Type: %s\r\nContent-Length: 200\r\n\r\n%s",
				tc.path, auth, tc.contentType, tc.sent)
			started := time.Now()
			_ = conn.SetReadDeadline(started.Add(20 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("no answer to a stalled request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			msg, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want || !strings.Contains(string(msg), tc.say) {
				t.Fatalf("status %d, want %d mentioning %q: %s", resp.StatusCode, tc.want, tc.say, msg)
			}
			if took := time.Since(started); took > 10*time.Second {
				t.Fatalf("answered after %s; the deadline is 200ms", took)
			}
		})
	}
}

// The same holds on routes that read no body at all, for a request that is
// refused before its handler runs, and for a path that does not exist: the
// server would otherwise wait for the announced body before answering.
func TestAnnouncedBodiesHaveADeadlineOnEveryRoute(t *testing.T) {
	t.Parallel()
	open := regtest.Start(t, server.Config{AnonymousRead: true, BodyTimeout: 200 * time.Millisecond})
	private := regtest.Start(t, server.Config{AnonymousRead: false, BodyTimeout: 200 * time.Millisecond})
	tests := []struct {
		name   string
		in     *regtest.Instance
		method string
		path   string
		header string
		want   int
	}{
		{"a page", open, http.MethodGet, "/", "", http.StatusOK},
		{"a route that takes no body", open, http.MethodPost, "/ui/logout", "", http.StatusSeeOther},
		{"a Hugging Face route that ignores its body", open, http.MethodPost, "/api/validate-yaml", "", http.StatusOK},
		{"a path that does not exist", open, http.MethodPost, "/no/such/path/at/all/here", "", http.StatusNotFound},
		{"an API route that is not implemented", open, http.MethodPost, "/api/something-else", "", http.StatusNotFound},
		{"a request refused for lack of a token", private, http.MethodDelete, "/api/v1/repos/acme/tiny", "", http.StatusUnauthorized},
		{"a request refused as cross-site", open, http.MethodPost, "/api/v1/gc", "Sec-Fetch-Site: cross-site\r\n", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, err := net.Dial("tcp", strings.TrimPrefix(tc.in.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			// Announce a body and never send it.
			fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: registry\r\n%sContent-Length: 200\r\n\r\n", tc.method, tc.path, tc.header)
			started := time.Now()
			_ = conn.SetReadDeadline(started.Add(20 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("no answer to a request that never sent its body: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if took := time.Since(started); took > 10*time.Second {
				t.Fatalf("answered after %s; the deadline is 200ms", took)
			}
		})
	}
}

// slowly sends content in two halves with a pause between them that is
// several body deadlines long, and hides its length.
type slowly struct {
	content string
	pause   time.Duration
	sent    int
}

func (s *slowly) Read(p []byte) (int, error) {
	half := len(s.content) / 2
	switch {
	case s.sent >= len(s.content):
		return 0, io.EOF
	case s.sent == half:
		time.Sleep(s.pause)
		n := copy(p, s.content[half:])
		s.sent += n
		return n, nil
	default:
		n := copy(p, s.content[s.sent:half])
		s.sent += n
		return n, nil
	}
}

// File transfers are not timed, however slow: an upload of a large file
// over a slow link takes as long as it takes.
func TestFileTransfersHaveNoBodyDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := regtest.Start(t, server.Config{AnonymousRead: true, BodyTimeout: 100 * time.Millisecond})
	const pause = 600 * time.Millisecond
	stored := func(c string) bool {
		_, ok, _ := in.Reg.StatBlob(ctx, kit.SHA256Hex(c))
		return ok
	}

	t.Run("an upload through the registry API", func(t *testing.T) {
		t.Parallel()
		content := strings.Repeat("slow push ", 1000)
		resp := do(t, http.MethodPut, in.URL+"/api/v1/blobs/"+kit.SHA256Hex(content), in.Write, &slowly{content: content, pause: pause}, nil)
		if resp.StatusCode != http.StatusCreated || !stored(content) {
			t.Fatalf("status %d (%s)", resp.StatusCode, body(t, resp))
		}
	})
	t.Run("a large-file upload from a Hugging Face client", func(t *testing.T) {
		t.Parallel()
		content := strings.Repeat("slow large file ", 1000)
		href := batch(t, in, "acme/slow", content)[kit.SHA256Hex(content)]
		resp := do(t, http.MethodPut, href, "", &slowly{content: content, pause: pause}, nil)
		if resp.StatusCode != http.StatusOK || !stored(content) {
			t.Fatalf("status %d (%s)", resp.StatusCode, body(t, resp))
		}
	})
	t.Run("a commit carrying files, to a branch with a slash in its name", func(t *testing.T) {
		t.Parallel()
		lines := commitBody(t, header("slow", ""), inlineFile("a.txt", strings.Repeat("a", 5000)), inlineFile("b.txt", strings.Repeat("b", 5000)))
		raw, _ := io.ReadAll(lines)
		for _, branch := range []string{"main", "refs%2Fpr%2F1"} {
			resp := do(t, http.MethodPost, in.URL+"/api/models/acme/slow-commit/commit/"+branch, in.Write,
				&slowly{content: string(raw), pause: pause}, map[string]string{"Content-Type": "application/x-ndjson"})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d (%s)", branch, resp.StatusCode, body(t, resp))
			}
		}
	})
	t.Run("a commit carrying files, to a dataset", func(t *testing.T) {
		t.Parallel()
		lines := commitBody(t, header("slow", ""), inlineFile("train.csv", strings.Repeat("c", 5000)))
		raw, _ := io.ReadAll(lines)
		resp := do(t, http.MethodPost, in.URL+"/api/datasets/acme/slow-commit/commit/main", in.Write,
			&slowly{content: string(raw), pause: pause}, map[string]string{"Content-Type": "application/x-ndjson"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d (%s)", resp.StatusCode, body(t, resp))
		}
	})
}

// The deadline is for the request body only. A response that takes longer
// than it, such as a mirror's progress stream, is not cut short.
func TestBodyDeadlineDoesNotCutALongResponse(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	hub.hold = make(chan struct{})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, BodyTimeout: 100 * time.Millisecond})
	go func() {
		time.Sleep(600 * time.Millisecond) // several deadlines long
		close(hub.hold)
	}()
	done, err := client.New(local.URL, local.Write).Mirror(context.Background(), "acme/tiny", "main", "", nil)
	if err != nil || done.SHA != hub.pushed.SHA {
		t.Fatalf("mirror = %+v, %v", done, err)
	}
}

// The listing's totals: each distinct file counted once per repository.
func TestRepositoryListingTotals(t *testing.T) {
	t.Parallel()
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	shared := strings.Repeat("shared", 100)
	push(t, in, "acme/one", map[string]string{"a.bin": shared, "b.bin": "only-one"}, client.PushOptions{})
	// A second revision that keeps one file and replaces the other.
	push(t, in, "acme/one", map[string]string{"a.bin": shared, "b.bin": "changed"}, client.PushOptions{})
	push(t, in, "acme/two", map[string]string{"copy.bin": shared}, client.PushOptions{})
	if resp := do(t, http.MethodPost, in.URL+"/api/repos/create", in.Write, jsonBody(t, map[string]any{"name": "empty", "organization": "acme"}), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d", resp.StatusCode)
	}

	resp := do(t, http.MethodGet, in.URL+"/api/v1/repos", "", nil, nil)
	var raw struct {
		Repos []map[string]json.RawMessage `json:"repos"`
	}
	decode(t, resp, &raw)
	type summary struct {
		revisions   int
		storedBytes int64
	}
	got := map[string]summary{}
	var order []string
	for _, r := range raw.Repos {
		var name string
		var s summary
		_ = json.Unmarshal(r["name"], &name)
		_ = json.Unmarshal(r["revisions"], &s.revisions)
		_ = json.Unmarshal(r["stored_bytes"], &s.storedBytes)
		got[name] = s
		order = append(order, name)
		// The shape documented in docs/api.md.
		for _, field := range []string{"name", "created_at", "revisions", "stored_bytes", "updated_at"} {
			if _, ok := r[field]; !ok {
				t.Errorf("%s: field %q is missing", name, field)
			}
		}
	}
	want := map[string]summary{
		"acme/empty": {0, 0},
		"acme/one":   {2, int64(len(shared) + len("only-one") + len("changed"))},
		"acme/two":   {1, int64(len(shared))},
	}
	if strings.Join(order, ",") != "acme/empty,acme/one,acme/two" {
		t.Fatalf("order = %v", order)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %+v, want %+v", name, got[name], w)
		}
	}
}
