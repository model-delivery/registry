// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
)

// dropAfter answers a download with the right Content-Length and then
// closes the connection after sending only part of the body, which is what
// a connection lost mid-transfer looks like to the client.
func dropAfter(t *testing.T, w http.ResponseWriter, status int, body string, sent int) {
	t.Helper()
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", status, http.StatusText(status), len(body))
	_, _ = buf.WriteString(body[:sent])
	_ = buf.Flush()
}

// A download whose connection drops is resumed with a Range request. What
// comes back depends on the server, and each answer has its own branch.
func TestDownloadResumesAfterADroppedConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := strings.Repeat("0123456789", 2000) // 20 kB: more than one read
	const cut = 7777

	tests := []struct {
		name string
		// resume answers the second and later requests for the file.
		resume func(t *testing.T, w http.ResponseWriter, r *http.Request)
		// wantRanges is the Range header of each request, in order.
		wantRanges []string
	}{
		{"the server honours the range and sends the rest",
			func(_ *testing.T, w http.ResponseWriter, r *http.Request) {
				from, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-"))
				if err != nil {
					http.Error(w, "bad range", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(content)-1, len(content)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, content[from:])
			},
			[]string{"", "bytes=7777-"}},
		{"the server ignores the range and sends the whole file again",
			func(_ *testing.T, w http.ResponseWriter, _ *http.Request) {
				// 200, not 206: everything from the start. The part already
				// received must be skipped, not stored twice.
				_, _ = io.WriteString(w, content)
			},
			[]string{"", "bytes=7777-"}},
		{"the connection drops a second time, further on",
			func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "bytes=7777-" {
					// Honoured, and then lost again 5000 bytes later.
					dropAfter(t, w, http.StatusPartialContent, content[cut:], 5000)
					return
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, content[cut+5000:])
			},
			[]string{"", "bytes=7777-", "bytes=12777-"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHub(t, map[string]string{"config.json": content})
			var mu sync.Mutex
			var ranges []string
			h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != filePath {
					return false
				}
				mu.Lock()
				ranges = append(ranges, r.Header.Get("Range"))
				first := len(ranges) == 1
				mu.Unlock()
				if first {
					dropAfter(t, w, http.StatusOK, content, cut)
				} else {
					tc.resume(t, w, r)
				}
				return true
			}
			body := (&Upstream{Endpoint: h.URL}).open(ctx, "acme/tiny", hubSHA, "config.json")
			defer func() { _ = body.Close() }()
			body.(*resumeReader).backoff = 1 // do not spend seconds waiting
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != content {
				t.Fatalf("read %d bytes, want the file's %d exactly, each byte once", len(got), len(content))
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(ranges, "|") != strings.Join(tc.wantRanges, "|") {
				t.Fatalf("Range headers sent = %q, want %q", ranges, tc.wantRanges)
			}
		})
	}

	// The same through a whole mirror: what is stored is verified against
	// the listed digest, so a resend that was not skipped would be refused.
	t.Run("a mirror stores the file intact when the server ignores the range", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, map[string]string{"config.json": content})
		h.lfs = map[string]bool{"config.json": true}
		var mu sync.Mutex
		requests := 0
		h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != filePath {
				return false
			}
			mu.Lock()
			requests++
			first := requests == 1
			mu.Unlock()
			if first {
				dropAfter(t, w, http.StatusOK, content, cut)
				return true
			}
			return false // the hub's own handler: the whole file, whatever was asked
		}
		reg := openRegistry(t)
		plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		res, err := plan.Execute(ctx, reg, 1, nil)
		if err != nil || res.DownloadedBytes != int64(len(content)) {
			t.Fatalf("mirror = %+v, %v", res, err)
		}
		rc, size, err := reg.OpenBlob(ctx, kit.SHA256Hex(content))
		if err != nil || size != int64(len(content)) {
			t.Fatalf("stored blob: %d bytes, %v", size, err)
		}
		_ = rc.Close()
	})
}

// brokenBody fails every read after handing over a few bytes.
type brokenBody struct {
	data   string
	closed *int
}

var errConnectionLost = errors.New("connection reset by peer")

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, errConnectionLost
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *brokenBody) Close() error { *b.closed++; return nil }

func TestResumeReaderGivesUp(t *testing.T) {
	t.Parallel()

	t.Run("after the maximum number of resumes, with the last error", func(t *testing.T) {
		t.Parallel()
		var offsets []int64
		closed := 0
		r := &resumeReader{ctx: context.Background(), backoff: 1, open: func(offset int64) (io.ReadCloser, error) {
			offsets = append(offsets, offset)
			return &brokenBody{data: "abc", closed: &closed}, nil // three more bytes, then the connection is lost
		}}
		got, err := io.ReadAll(r)
		if !errors.Is(err, errConnectionLost) {
			t.Fatalf("error = %v, want the connection's own error once resuming is given up", err)
		}
		// The first connection and maxResumes more, each continuing where
		// the one before stopped.
		if len(offsets) != maxResumes+1 {
			t.Fatalf("connected %d times, want 1 + %d resumes", len(offsets), maxResumes)
		}
		for i, off := range offsets {
			if off != int64(3*i) {
				t.Fatalf("connection %d asked for offset %d, want %d: %v", i, off, 3*i, offsets)
			}
		}
		if want := strings.Repeat("abc", maxResumes+1); string(got) != want {
			t.Fatalf("read %q, want every byte that arrived, once: %q", got, want)
		}
		if closed != maxResumes+1 {
			t.Fatalf("%d bodies closed, want all %d", closed, maxResumes+1)
		}
	})

	t.Run("when connecting itself keeps failing", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		r := &resumeReader{ctx: context.Background(), backoff: 1, open: func(int64) (io.ReadCloser, error) {
			attempts++
			return nil, errConnectionLost
		}}
		if _, err := io.ReadAll(r); !errors.Is(err, errConnectionLost) || attempts != maxResumes+1 {
			t.Fatalf("error = %v after %d attempts, want the error after %d", err, attempts, maxResumes+1)
		}
	})

	t.Run("at once on an answer that trying again cannot change", func(t *testing.T) {
		t.Parallel()
		for _, final := range []error{ErrUpstreamNotFound, ErrUpstreamAuth, ErrUnsafeUpstream} {
			attempts := 0
			r := &resumeReader{ctx: context.Background(), backoff: 1, open: func(int64) (io.ReadCloser, error) {
				attempts++
				return nil, fmt.Errorf("%w: for this file", final)
			}}
			if _, err := io.ReadAll(r); !errors.Is(err, final) || attempts != 1 {
				t.Errorf("%v: error = %v after %d attempts, want it returned after 1", final, err, attempts)
			}
		}
	})

	t.Run("at once when the caller has gone", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		closed := 0
		r := &resumeReader{ctx: ctx, open: func(int64) (io.ReadCloser, error) { // the real, growing wait
			attempts++
			cancel() // the caller gives up while the body is being read
			return &brokenBody{data: "abc", closed: &closed}, nil
		}}
		got, err := io.ReadAll(r)
		if !errors.Is(err, errConnectionLost) || attempts != 1 || string(got) != "abc" {
			t.Fatalf("read %q, %v after %d attempts; want no resume once the context has ended", got, err, attempts)
		}
	})
}

// A revision with more files than fit one page of the hub's listing is
// listed by following every "next" link, in order, to the last.
func TestTreeFollowsEveryPageOfAListing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const pages, perPage = 5, 3

	// The whole listing: files kept in git, LFS files, and the directory
	// entries a recursive listing also contains.
	type entry = map[string]any
	var all []entry
	var want []Entry
	files := map[string]string{}
	for i := range pages * perPage {
		switch i % 3 {
		case 0:
			all = append(all, entry{"type": "directory", "path": fmt.Sprintf("dir-%02d", i), "oid": strings.Repeat("d", 40), "size": 0})
		case 1:
			path, content := fmt.Sprintf("dir-%02d/config-%02d.json", i-1, i), fmt.Sprintf(`{"n":%d}`, i)
			files[path] = content
			all = append(all, entry{"type": "file", "path": path, "oid": gitBlobID(content), "size": len(content)})
			want = append(want, Entry{Path: path, Size: int64(len(content)), OID: gitBlobID(content)})
		case 2:
			path, content := fmt.Sprintf("dir-%02d/weights-%02d.bin", i-2, i), strings.Repeat("w", 100+i)
			files[path] = content
			// For an LFS file the entry's own size and oid are the pointer's;
			// the content's are under "lfs".
			all = append(all, entry{"type": "file", "path": path, "oid": strings.Repeat("a", 40), "size": 130,
				"lfs": entry{"oid": kit.SHA256Hex(content), "size": len(content)}})
			want = append(want, Entry{Path: path, Size: int64(len(content)), OID: strings.Repeat("a", 40), LFSSHA256: kit.SHA256Hex(content)})
		}
	}

	h := newHub(t, files)
	var mu sync.Mutex
	var cursors []string
	h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != treePath {
			return false
		}
		cursor := r.URL.Query().Get("cursor")
		mu.Lock()
		cursors = append(cursors, cursor)
		mu.Unlock()
		page := 0
		if cursor != "" {
			page, _ = strconv.Atoi(cursor)
		}
		if page < pages-1 {
			// An absolute link that keeps the query, among other relations,
			// the way a hub writes it.
			next := fmt.Sprintf("%s%s?recursive=true&expand=false&cursor=%d", h.URL, treePath, page+1)
			w.Header().Set("Link", `<`+h.URL+treePath+`>; rel="first", <`+next+`>; rel="next"`)
		}
		_ = json.NewEncoder(w).Encode(all[page*perPage : (page+1)*perPage])
		return true
	}

	got, err := (&Upstream{Endpoint: h.URL}).tree(ctx, "acme/tiny", hubSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d files listed, want %d from %d pages", len(got), len(want), pages)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	mu.Lock()
	if strings.Join(cursors, ",") != ",1,2,3,4" {
		t.Errorf("pages asked for = %q, want each once, in order", cursors)
	}
	mu.Unlock()

	// And a mirror of it records every file of every page.
	reg := openRegistry(t)
	plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Execute(ctx, reg, 0, nil)
	if err != nil || res.Files != len(want) {
		t.Fatalf("mirror = %+v, %v; want %d files", res, err, len(want))
	}
	c, err := reg.Resolve(ctx, "acme/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := reg.Files(ctx, c)
	if err != nil || len(recorded) != len(want) {
		t.Fatalf("%d files recorded (%v), want %d", len(recorded), err, len(want))
	}
	byPath := map[string]registry.File{}
	for _, f := range recorded {
		byPath[f.Path] = f
	}
	for _, e := range want {
		f, ok := byPath[e.Path]
		if !ok || f.Size != e.Size || f.SHA256 != kit.SHA256Hex(files[e.Path]) || f.LFS != (e.LFSSHA256 != "") {
			t.Errorf("%s recorded as %+v (found %v)", e.Path, f, ok)
		}
	}
}
