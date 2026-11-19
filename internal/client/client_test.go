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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/model-delivery/registry/internal/kit"
)

// The server package tests the client against a real registry. These tests
// use a registry that misbehaves, which a real one cannot be made to do.

const revisionID = "2222222222222222222222222222222222222222"

// lyingRegistry lists one file of acme/tiny as listed describes it, and
// answers a download of it by calling send.
func lyingRegistry(t *testing.T, listed File, send func(w http.ResponseWriter, r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/acme/tiny/revisions/"):
			_ = json.NewEncoder(w).Encode(Revision{Repo: "acme/tiny", SHA: revisionID, Files: []File{listed}})
		case strings.HasPrefix(r.URL.Path, "/acme/tiny/resolve/"+revisionID+"/"):
			send(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "")
}

// endless writes until the client stops reading, and counts what it wrote.
func endless(sent *atomic.Int64) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		for sent.Load() < 256<<20 { // a registry that never stopped would fill the disk
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}
}

func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func TestPullStopsAtTheListedSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const content = "ten bytes!"

	t.Run("a registry that sends more than it listed is cut off", func(t *testing.T) {
		t.Parallel()
		var sent atomic.Int64
		cl := lyingRegistry(t, File{Path: "model.bin", Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true}, endless(&sent))
		dir := t.TempDir()
		_, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil)
		if err == nil || !strings.Contains(err.Error(), "more data than the size it lists") || !strings.Contains(err.Error(), "model.bin") {
			t.Fatalf("got %v, want an error saying the registry sent too much", err)
		}
		// Nothing is left behind, and next to nothing was ever written.
		if n := dirBytes(t, dir); n != 0 {
			t.Fatalf("%d bytes left in the target directory", n)
		}
		if got := sent.Load(); got >= 256<<20 {
			t.Fatalf("the client read the registry's whole %d-byte answer for a %d-byte file", got, len(content))
		}
	})

	t.Run("also when resuming a partial download", func(t *testing.T) {
		t.Parallel()
		var sent atomic.Int64
		cl := lyingRegistry(t, File{Path: "model.bin", Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true},
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusPartialContent)
				endless(&sent)(w, r)
			})
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "model.bin.part"), []byte(content[:4]), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err == nil || !strings.Contains(err.Error(), "more data than the size it lists") {
			t.Fatalf("got %v", err)
		}
		if n := dirBytes(t, dir); n != 0 {
			t.Fatalf("%d bytes left in the target directory", n)
		}
	})

	t.Run("a file identified only by git blob id is bounded too", func(t *testing.T) {
		t.Parallel()
		var sent atomic.Int64
		cl := lyingRegistry(t, File{Path: "config.json", Size: int64(len(content)), GitOID: strings.Repeat("a", 40), Pending: true}, endless(&sent))
		dir := t.TempDir()
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err == nil || !strings.Contains(err.Error(), "more data than the size it lists") {
			t.Fatalf("got %v", err)
		}
		if n := dirBytes(t, dir); n != 0 {
			t.Fatalf("%d bytes left in the target directory", n)
		}
	})

	t.Run("exactly the listed size is accepted", func(t *testing.T) {
		t.Parallel()
		cl := lyingRegistry(t, File{Path: "model.bin", Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true},
			func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(content)) })
		dir := t.TempDir()
		if _, err := cl.Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "model.bin")); err != nil || string(got) != content {
			t.Fatalf("pulled %q, %v", got, err)
		}
	})

	t.Run("fewer bytes than listed still fails the integrity check", func(t *testing.T) {
		t.Parallel()
		cl := lyingRegistry(t, File{Path: "model.bin", Size: int64(len(content)), SHA256: kit.SHA256Hex(content), LFS: true},
			func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(content[:5])) })
		if _, err := cl.Pull(ctx, "acme/tiny", "main", t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "integrity check failed") {
			t.Fatalf("got %v", err)
		}
	})
}
