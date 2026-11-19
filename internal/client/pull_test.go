// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/kit"
)

// A registry names the files of a revision, and Pull writes each where its
// name says. A name must never take a file out of the directory the user
// chose, whatever the registry sends.

// where says on which systems a path must be refused.
type where int

const (
	nowhere where = iota // a path inside the directory: accepted everywhere
	everywhere
	// onWindows marks a path that only Windows reads as leaving the
	// directory or as a device. Elsewhere it is an ordinary, if odd, file
	// name, and writing it inside the directory is harmless.
	onWindows
)

var joinCases = []struct {
	name    string
	path    string
	refused where
}{
	{"a file", "config.json", nowhere},
	{"a file in a directory", "tokenizer/vocab.txt", nowhere},
	{"a name with dots in it", "model.v1..final.bin", nowhere},
	{"a name that starts with dots", "..config", nowhere},

	{"an empty path", "", everywhere},
	{"the parent directory", "..", everywhere},
	{"a file in the parent directory", "../escape.txt", everywhere},
	{"far above", "../../../../etc/passwd", everywhere},
	{"down and then further up", "a/../../escape.txt", everywhere},
	{"up at the end", "a/b/../../..", everywhere},
	{"an absolute path", "/etc/passwd", everywhere},
	{"an absolute path with two slashes", "//server/share/x", everywhere},

	{"a drive", "C:", onWindows},
	{"a path on a drive", "C:/Windows/System32/drivers/etc/hosts", onWindows},
	{"a drive-relative path", "C:escape.txt", onWindows},
	{"a path on a drive, with backslashes", `C:\Windows\win.ini`, onWindows},
	{"the parent directory, with a backslash", `..\escape.txt`, onWindows},
	{"an absolute path, with a backslash", `\Windows\win.ini`, onWindows},
	{"a network share", `\\server\share\x`, onWindows},
	{"a device path", `\\.\PhysicalDrive0`, onWindows},
	{"the NUL device", "NUL", onWindows},
	{"a device inside a directory", "weights/COM1", onWindows},
	{"a device, lower case", "sub/lpt1", onWindows},
}

func mustRefuse(w where) bool {
	return w == everywhere || (w == onWindows && runtime.GOOS == "windows")
}

func TestSafeJoin(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "target")
	for _, tc := range joinCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := safeJoin(dir, tc.path)
			if mustRefuse(tc.refused) {
				if err == nil {
					t.Fatalf("safeJoin(%q) = %q, want it refused", tc.path, got)
				}
				if !strings.Contains(err.Error(), "outside the target directory") {
					t.Fatalf("the error does not say why: %v", err)
				}
				return
			}
			// Accepted, here: then the result is inside the directory.
			if err != nil {
				if tc.refused == nowhere {
					t.Fatalf("safeJoin(%q): %v", tc.path, err)
				}
				return // stricter than it has to be on this system, which is fine
			}
			rel, relErr := filepath.Rel(dir, got)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				t.Fatalf("safeJoin(%q) = %q, which is not inside %q", tc.path, got, dir)
			}
		})
	}
}

// The same, end to end: a registry that lists such a path gets nothing
// written anywhere, and is never asked for the file.
func TestPullRefusesAPathOutsideTheTarget(t *testing.T) {
	t.Parallel()
	const content = "planted"
	for _, tc := range joinCases {
		if !mustRefuse(tc.refused) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var downloads int
			var mu sync.Mutex
			cl := lyingRegistry(t, File{Path: tc.path, Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true},
				func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					downloads++
					mu.Unlock()
					_, _ = w.Write([]byte(content))
				})
			// The target is a directory inside a directory the test owns, so
			// that a file written beside or above it is seen.
			parent := t.TempDir()
			target := filepath.Join(parent, "target")
			_, err := cl.Pull(context.Background(), "acme/tiny", "main", target, nil)
			if err == nil || !strings.Contains(err.Error(), "refusing to write outside the target directory") {
				t.Fatalf("got %v, want the path refused", err)
			}
			if n := dirBytes(t, parent); n != 0 {
				t.Fatalf("%d bytes were written under %s", n, parent)
			}
			mu.Lock()
			defer mu.Unlock()
			if downloads != 0 {
				t.Fatalf("the file was requested %d times before its path was refused", downloads)
			}
		})
	}

	t.Run("and the files listed before it are not a way around", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/repos/acme/tiny/revisions/") {
				_ = json.NewEncoder(w).Encode(Revision{Repo: "acme/tiny", SHA: revisionID, Files: []File{
					{Path: "ok.txt", Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true},
					{Path: "../escape.txt", Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true},
				}})
				return
			}
			_, _ = w.Write([]byte(content))
		}))
		t.Cleanup(srv.Close)
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		if _, err := New(srv.URL, "").Pull(context.Background(), "acme/tiny", "main", target, nil); err == nil {
			t.Fatal("the pull succeeded")
		}
		if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
			t.Fatalf("a file was written outside the target: %v", err)
		}
	})
}

// A download that was interrupted leaves "<file>.part". The next pull
// starts from it. These are the ways that can go.
func TestPullResumesFromAPartFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const content = "0123456789abcdefghij" // 20 bytes
	listed := File{Path: "model.bin", Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true}

	// serving answers downloads the way a registry does, honouring Range,
	// and records the Range header of each request it got.
	type served struct {
		mu     sync.Mutex
		ranges []string
	}
	honest := func(s *served) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.ranges = append(s.ranges, r.Header.Get("Range"))
			s.mu.Unlock()
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(content))
		}
	}
	requests := func(s *served) []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]string(nil), s.ranges...)
	}
	// leave writes what an earlier attempt left behind.
	leave := func(t *testing.T, dir, part string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "model.bin"+partSuffix), []byte(part), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// pulled checks that the file is complete and the part file is gone.
	pulled := func(t *testing.T, dir string) {
		t.Helper()
		if got, err := os.ReadFile(filepath.Join(dir, "model.bin")); err != nil || string(got) != content {
			t.Fatalf("model.bin holds %q, %v", got, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "model.bin"+partSuffix)); !os.IsNotExist(err) {
			t.Fatalf("the part file is still there: %v", err)
		}
	}

	t.Run("a part that is the start of the file: only the rest is asked for", func(t *testing.T) {
		t.Parallel()
		s := &served{}
		cl, dir := lyingRegistry(t, listed, honest(s)), t.TempDir()
		leave(t, dir, content[:8])
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatal(err)
		}
		pulled(t, dir)
		if got := requests(s); len(got) != 1 || got[0] != "bytes=8-" {
			t.Fatalf("requests carried Range %q, want one for bytes=8-", got)
		}
	})

	t.Run("an empty part: the whole file is asked for, without a range", func(t *testing.T) {
		t.Parallel()
		s := &served{}
		cl, dir := lyingRegistry(t, listed, honest(s)), t.TempDir()
		leave(t, dir, "")
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatal(err)
		}
		pulled(t, dir)
		if got := requests(s); len(got) != 1 || got[0] != "" {
			t.Fatalf("requests carried Range %q, want one request with none", got)
		}
	})

	t.Run("a part larger than the file: thrown away, and the file fetched whole", func(t *testing.T) {
		t.Parallel()
		s := &served{}
		cl, dir := lyingRegistry(t, listed, honest(s)), t.TempDir()
		leave(t, dir, content+"and more than the registry lists")
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatal(err)
		}
		pulled(t, dir)
		if got := requests(s); len(got) != 1 || got[0] != "" {
			t.Fatalf("requests carried Range %q, want one request for the whole file", got)
		}
	})

	t.Run("a part of the full size and the right content: nothing is fetched", func(t *testing.T) {
		t.Parallel()
		s := &served{}
		cl, dir := lyingRegistry(t, listed, honest(s)), t.TempDir()
		leave(t, dir, content)
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatal(err)
		}
		pulled(t, dir)
		if got := requests(s); len(got) != 0 {
			t.Fatalf("the file was requested %d times although all of it was there", len(got))
		}
	})

	t.Run("a part of the full size and the wrong content: refused, removed, and the next pull starts again", func(t *testing.T) {
		t.Parallel()
		s := &served{}
		cl, dir := lyingRegistry(t, listed, honest(s)), t.TempDir()
		leave(t, dir, strings.Repeat("x", len(content)))
		_, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil)
		if err == nil || !strings.Contains(err.Error(), "integrity check failed") || !strings.Contains(err.Error(), "model.bin") {
			t.Fatalf("got %v, want the integrity check to fail", err)
		}
		// Nothing was fetched, nothing is kept, and nothing was installed.
		if got := requests(s); len(got) != 0 {
			t.Fatalf("requested %d times", len(got))
		}
		if n := dirBytes(t, dir); n != 0 {
			t.Fatalf("%d bytes left in the target directory", n)
		}
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatalf("the pull after that: %v", err)
		}
		pulled(t, dir)
	})

	t.Run("a part that is not the start of the file: the result fails its check and is removed", func(t *testing.T) {
		t.Parallel()
		s := &served{}
		cl, dir := lyingRegistry(t, listed, honest(s)), t.TempDir()
		leave(t, dir, "XXXXXXXX")
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err == nil || !strings.Contains(err.Error(), "integrity check failed") {
			t.Fatalf("got %v, want the integrity check to fail", err)
		}
		if n := dirBytes(t, dir); n != 0 {
			t.Fatalf("%d bytes left in the target directory", n)
		}
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatalf("the pull after that: %v", err)
		}
		pulled(t, dir)
	})

	t.Run("a registry that ignores the range: its answer is not appended to the part", func(t *testing.T) {
		t.Parallel()
		// It answers 200 with the whole file, whatever was asked for. Taking
		// that as the rest of the file would produce a file that starts
		// twice; the pull stops instead, and what was there is untouched.
		cl := lyingRegistry(t, listed, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(content)) })
		dir := t.TempDir()
		leave(t, dir, content[:8])
		_, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil)
		if err == nil || !strings.Contains(err.Error(), "did not honour the resume request") || !strings.Contains(err.Error(), "model.bin") {
			t.Fatalf("got %v, want the pull to stop", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "model.bin")); !os.IsNotExist(err) {
			t.Fatalf("a file was installed: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "model.bin"+partSuffix)); err != nil || string(got) != content[:8] {
			t.Fatalf("the part file now holds %q, %v; want it as it was", got, err)
		}
	})

	t.Run("a file already complete and correct is not fetched again", func(t *testing.T) {
		t.Parallel()
		s := &served{}
		cl, dir := lyingRegistry(t, listed, honest(s)), t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "model.bin"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var fetched []bool
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, func(_ string, _ int64, f bool) { fetched = append(fetched, f) }); err != nil {
			t.Fatal(err)
		}
		if got := requests(s); len(got) != 0 || len(fetched) != 1 || fetched[0] {
			t.Fatalf("requested %d times, progress said fetched=%v", len(got), fetched)
		}
	})
}
