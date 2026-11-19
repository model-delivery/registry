// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request a test registry received.
type recorded struct {
	method, path, auth, contentType, body string
	length                                int64
}

// answering is a registry that records each request and answers every one
// with the same status and body.
func answering(t *testing.T, status int, body string) (*Client, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, recorded{
			method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: string(sent), length: r.ContentLength,
		})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "md_token"), func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), got...)
	}
}

// Each method asks for one route and gives back the document as a type,
// with the document itself beside it.
func TestTypedMethods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	when := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	t.Run("ListRepos", func(t *testing.T) {
		t.Parallel()
		const doc = `{"repos":[{"name":"acme/tiny","upstream":"https://huggingface.co","created_at":"2026-11-18T10:00:00Z","revisions":2,"stored_bytes":9,"updated_at":"2026-11-18T10:00:00Z"},{"name":"acme/pushed","created_at":"2026-11-18T10:00:00Z","revisions":0,"stored_bytes":0,"updated_at":"2026-11-18T10:00:00Z"}],"more":true}`
		cl, got := answering(t, http.StatusOK, doc)
		out, err := cl.ListRepos(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Repos) != 2 || out.Repos[0] != (RepoSummary{Name: "acme/tiny", Upstream: "https://huggingface.co", CreatedAt: when, Revisions: 2, StoredBytes: 9, UpdatedAt: when}) || out.Repos[1].Upstream != "" {
			t.Fatalf("decoded %+v", out.Repos)
		}
		if string(out.Raw) != doc {
			t.Fatalf("Raw = %s, want the document as sent", out.Raw)
		}
		if r := got(); len(r) != 1 || r[0].method != http.MethodGet || r[0].path != "/api/v1/repos" || r[0].auth != "Bearer md_token" || r[0].body != "" {
			t.Fatalf("requests: %+v", r)
		}
	})

	t.Run("Repo", func(t *testing.T) {
		t.Parallel()
		const doc = `{"name":"acme/tiny","upstream":"https://hub.example","created_at":"2026-11-18T10:00:00Z","refs":[{"name":"v1","kind":"tag","sha":"aa","updated_at":"2026-11-18T10:00:00Z"}],"revisions":[{"repo":"acme/tiny","sha":"aa","parent":"bb","source":"mirror","created_at":"2026-11-18T10:00:00Z","files":3,"bytes":42}]}`
		cl, got := answering(t, http.StatusOK, doc)
		out, err := cl.Repo(ctx, "acme/tiny")
		if err != nil {
			t.Fatal(err)
		}
		if out.Name != "acme/tiny" || out.Upstream != "https://hub.example" || len(out.Refs) != 1 || out.Refs[0] != (Ref{Name: "v1", Kind: "tag", SHA: "aa"}) ||
			len(out.Revisions) != 1 || out.Revisions[0] != (RevisionSummary{SHA: "aa", Source: "mirror", Files: 3, Bytes: 42, CreatedAt: when}) {
			t.Fatalf("decoded %+v", out)
		}
		if string(out.Raw) != doc || got()[0].path != "/api/v1/repos/acme/tiny" {
			t.Fatalf("Raw %s, path %s", out.Raw, got()[0].path)
		}
	})

	t.Run("DeleteRepo and GC", func(t *testing.T) {
		t.Parallel()
		const doc = `{"blobs":3,"bytes":28183891,"warning":"some files could not be removed"}`
		cl, got := answering(t, http.StatusOK, doc)
		deleted, err := cl.DeleteRepo(ctx, "acme/tiny")
		if err != nil || deleted.Blobs != 3 || deleted.Bytes != 28183891 || deleted.Warning != "some files could not be removed" || string(deleted.Raw) != doc {
			t.Fatalf("DeleteRepo = %+v, %v", deleted, err)
		}
		collected, err := cl.GC(ctx)
		if err != nil || collected.Blobs != 3 || collected.Warning == "" || string(collected.Raw) != doc {
			t.Fatalf("GC = %+v, %v", collected, err)
		}
		r := got()
		if len(r) != 2 || r[0].method != http.MethodDelete || r[0].path != "/api/v1/repos/acme/tiny" || r[1].method != http.MethodPost || r[1].path != "/api/v1/gc" {
			t.Fatalf("requests: %+v", r)
		}
	})

	t.Run("CreateToken", func(t *testing.T) {
		t.Parallel()
		const doc = `{"name":"ci","role":"write","token":"md_secret"}`
		cl, got := answering(t, http.StatusCreated, doc)
		out, err := cl.CreateToken(ctx, "ci", "write")
		if err != nil || out.Name != "ci" || out.Role != "write" || out.Token != "md_secret" || string(out.Raw) != doc {
			t.Fatalf("CreateToken = %+v, %v", out, err)
		}
		r := got()[0]
		if r.method != http.MethodPost || r.path != "/api/v1/tokens" || r.contentType != "application/json" || r.body != `{"name":"ci","role":"write"}` || r.length != int64(len(r.body)) {
			t.Fatalf("request: %+v", r)
		}
	})

	t.Run("ListTokens", func(t *testing.T) {
		t.Parallel()
		const doc = `{"tokens":[{"name":"admin","role":"admin","created_at":"2026-11-18T10:00:00Z"}]}`
		cl, got := answering(t, http.StatusOK, doc)
		out, err := cl.ListTokens(ctx)
		if err != nil || len(out.Tokens) != 1 || out.Tokens[0] != (TokenInfo{Name: "admin", Role: "admin", CreatedAt: when}) || string(out.Raw) != doc {
			t.Fatalf("ListTokens = %+v, %v", out, err)
		}
		if r := got()[0]; r.method != http.MethodGet || r.path != "/api/v1/tokens" {
			t.Fatalf("request: %+v", r)
		}
	})

	t.Run("RevokeToken: answered with no document", func(t *testing.T) {
		t.Parallel()
		cl, got := answering(t, http.StatusNoContent, "")
		if err := cl.RevokeToken(ctx, "ci"); err != nil {
			t.Fatalf("a 204 is success, got %v", err)
		}
		// A name is one path segment, whatever is in it.
		if err := cl.RevokeToken(ctx, "ci/cd bot?"); err != nil {
			t.Fatal(err)
		}
		r := got()
		if r[0].method != http.MethodDelete || r[0].path != "/api/v1/tokens/ci" || r[1].path != "/api/v1/tokens/ci%2Fcd%20bot%3F" {
			t.Fatalf("requests: %+v", r)
		}
	})

	t.Run("BackupNow and ListBackups", func(t *testing.T) {
		t.Parallel()
		const doc = `{"backup":{"name":"registry-20261118T100000Z.db","size":40960,"created_at":"2026-11-18T10:00:00Z"},"contents":{"schema_version":2,"repos":1,"revisions":2,"size":40960},"warning":"old backups could not be removed","backups":[{"name":"registry-20261118T100000Z.db","size":40960,"created_at":"2026-11-18T10:00:00Z"}]}`
		cl, got := answering(t, http.StatusCreated, doc)
		wantBackup := Backup{Name: "registry-20261118T100000Z.db", Size: 40960, CreatedAt: when}
		taken, err := cl.BackupNow(ctx)
		if err != nil || taken.Backup != wantBackup || taken.Contents != (SnapshotContents{SchemaVersion: 2, Repos: 1, Revisions: 2, Size: 40960}) ||
			taken.Warning != "old backups could not be removed" || string(taken.Raw) != doc {
			t.Fatalf("BackupNow = %+v, %v", taken, err)
		}
		list, err := cl.ListBackups(ctx)
		if err != nil || len(list.Backups) != 1 || list.Backups[0] != wantBackup || string(list.Raw) != doc {
			t.Fatalf("ListBackups = %+v, %v", list, err)
		}
		r := got()
		if r[0].method != http.MethodPost || r[0].path != "/api/v1/backups" || r[1].method != http.MethodGet || r[1].path != "/api/v1/backups" {
			t.Fatalf("requests: %+v", r)
		}
	})
}

// What is not a success, or not the document expected, is an error: never a
// zero value that a caller would print as if it were an answer.
func TestAnswersThatAreNotTheDocument(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	calls := map[string]func(*Client) error{
		"ListRepos":   func(c *Client) error { _, err := c.ListRepos(ctx); return err },
		"Repo":        func(c *Client) error { _, err := c.Repo(ctx, "acme/tiny"); return err },
		"DeleteRepo":  func(c *Client) error { _, err := c.DeleteRepo(ctx, "acme/tiny"); return err },
		"GC":          func(c *Client) error { _, err := c.GC(ctx); return err },
		"CreateToken": func(c *Client) error { _, err := c.CreateToken(ctx, "ci", "read"); return err },
		"ListTokens":  func(c *Client) error { _, err := c.ListTokens(ctx); return err },
		"BackupNow":   func(c *Client) error { _, err := c.BackupNow(ctx); return err },
		"ListBackups": func(c *Client) error { _, err := c.ListBackups(ctx); return err },
		"Revision":    func(c *Client) error { _, err := c.Revision(ctx, "acme/tiny", "main"); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct{ body, wantErr string }{
				{"", "unreadable answer from the registry"},
				{"<html>not the registry</html>", "unreadable answer from the registry"},
				{`{"repos": [`, "unreadable answer from the registry"},
				{`"a string"`, "unexpected answer from the registry"},
			} {
				cl, _ := answering(t, http.StatusOK, tc.body)
				if err := call(cl); err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), cl.Endpoint) {
					t.Errorf("answered %q: got %v, want %q naming the registry", tc.body, err, tc.wantErr)
				}
			}

			cl, _ := answering(t, http.StatusConflict, `{"error":"a token named ci already exists"}`)
			var apiErr *APIError
			if err := call(cl); !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Message != "a token named ci already exists" {
				t.Errorf("a refusal: got %v, want the registry's message as an APIError", err)
			}
		})
	}

	t.Run("CreateToken: a document without the secret", func(t *testing.T) {
		t.Parallel()
		cl, _ := answering(t, http.StatusCreated, `{"name":"ci","role":"read"}`)
		if out, err := cl.CreateToken(ctx, "ci", "read"); err == nil || !strings.Contains(err.Error(), "without the new token's secret") {
			t.Fatalf("got %+v, %v", out, err)
		}
	})

	t.Run("RevokeToken: a refusal", func(t *testing.T) {
		t.Parallel()
		cl, _ := answering(t, http.StatusNotFound, `{"error":"token not found"}`)
		var apiErr *APIError
		if err := cl.RevokeToken(ctx, "absent"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDownloadSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("saves what the registry sends, from the backups address", func(t *testing.T) {
		t.Parallel()
		cl, got := answering(t, http.StatusOK, "the snapshot's bytes")
		dst := filepath.Join(t.TempDir(), "snapshot.db")
		n, err := cl.DownloadSnapshot(ctx, dst)
		if err != nil || n != int64(len("the snapshot's bytes")) {
			t.Fatalf("wrote %d bytes, %v", n, err)
		}
		if saved, _ := os.ReadFile(dst); string(saved) != "the snapshot's bytes" {
			t.Fatalf("saved %q", saved)
		}
		if r := got(); len(r) != 1 || r[0].method != http.MethodGet || r[0].path != "/api/v1/backups/snapshot" {
			t.Fatalf("requests: %+v", r)
		}
	})

	t.Run("never overwrites a file", func(t *testing.T) {
		t.Parallel()
		cl, _ := answering(t, http.StatusOK, "the snapshot's bytes")
		dst := filepath.Join(t.TempDir(), "snapshot.db")
		if err := os.WriteFile(dst, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.DownloadSnapshot(ctx, dst); !errors.Is(err, os.ErrExist) {
			t.Fatalf("got %v, want it refused because the file exists", err)
		}
		if kept, _ := os.ReadFile(dst); string(kept) != "mine" {
			t.Fatalf("the file now holds %q", kept)
		}
	})

	t.Run("a refusal writes no file", func(t *testing.T) {
		t.Parallel()
		cl, _ := answering(t, http.StatusForbidden, `{"error":"this token does not have permission for this operation"}`)
		dst := filepath.Join(t.TempDir(), "snapshot.db")
		if _, err := cl.DownloadSnapshot(ctx, dst); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("got %v", err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("a file was written: %v", err)
		}
	})

	t.Run("a download cut short leaves no file", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000000")
			_, _ = io.WriteString(w, "the first part, and then the connection goes")
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}))
		t.Cleanup(srv.Close)
		dst := filepath.Join(t.TempDir(), "snapshot.db")
		if _, err := New(srv.URL, "").DownloadSnapshot(ctx, dst); err == nil {
			t.Fatal("a snapshot that stopped part-way was accepted")
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("a partial file was left: %v", err)
		}
	})
}

// A file is uploaded with its length declared, so the registry can refuse
// one that is too large before it has received it.
func TestPushDeclaresTheLengthOfEachFile(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	lengths := map[string]int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			n, _ := io.Copy(io.Discard, r.Body)
			mu.Lock()
			lengths[r.Header.Get("Content-Type")] = r.ContentLength
			mu.Unlock()
			if n != r.ContentLength {
				http.Error(w, "the body is not the declared length", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			_, _ = io.WriteString(w, `{"repo":"acme/tiny","sha":"aa"}`)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "weights.bin"), []byte(strings.Repeat("w", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := New(srv.URL, "").Push(context.Background(), dir, "acme/tiny", PushOptions{Branch: "main"})
	if err != nil || res.SHA != "aa" {
		t.Fatalf("push: %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := lengths["application/octet-stream"]; got != 4096 {
		t.Fatalf("the upload declared a length of %d, want 4096", got)
	}
}
