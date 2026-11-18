// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const upstreamSHA = "0123456789abcdef0123456789abcdef01234567"

func TestPendingFilesBecomeStoredOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	weights, config := put(t, r, "weights"), put(t, r, "config")
	in := LazyInput{Repo: "acme/model", Branch: "main", SHA: upstreamSHA, Upstream: "https://hub.example", Files: []File{
		{Path: "model.bin", Size: weights.Size, SHA256: weights.SHA256, GitOID: strings.Repeat("a", 40), LFS: true},
		{Path: "config.json", Size: config.Size, GitOID: config.GitOID}, // not LFS: no SHA-256 known yet
	}}
	c, err := r.CommitLazy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	// The revision resolves and lists every file, all of them pending.
	got, err := r.Resolve(ctx, "acme/model", "main")
	if err != nil || got.SHA != upstreamSHA {
		t.Fatalf("resolve = %v, %v", got.SHA, err)
	}
	files, _ := r.Files(ctx, c)
	if len(files) != 2 || !files[0].Pending || !files[1].Pending || files[0].Path != "config.json" {
		t.Fatalf("listing = %+v", files)
	}
	// What a client needs before downloading is already known.
	if f, _ := r.File(ctx, c, "model.bin"); f.ETag() != weights.SHA256 || f.Size != weights.Size {
		t.Errorf("pending LFS file: %+v", f)
	}
	if f, _ := r.File(ctx, c, "config.json"); f.ETag() != config.GitOID || f.SHA256 != "" {
		t.Errorf("pending git file: %+v", f)
	}
	if n, _ := r.PendingCount(ctx, c); n != 2 {
		t.Fatalf("pending = %d", n)
	}
	// Recording the same revision again changes nothing.
	if _, err := r.CommitLazy(ctx, in); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.PendingCount(ctx, c); n != 2 {
		t.Fatalf("pending after recording twice = %d", n)
	}

	t.Run("content that does not match the listing is refused", func(t *testing.T) {
		other := put(t, r, "something else")
		if err := r.Fulfil(ctx, c, "model.bin", other.SHA256); !errors.Is(err, ErrConflict) {
			t.Fatalf("wrong content: %v, want ErrConflict", err)
		}
		if err := r.Fulfil(ctx, c, "model.bin", strings.Repeat("0", 64)); !errors.Is(err, ErrBlobMissing) {
			t.Fatalf("content never stored: %v, want ErrBlobMissing", err)
		}
		if n, _ := r.PendingCount(ctx, c); n != 2 {
			t.Fatalf("a refused fulfilment changed the listing: %d pending", n)
		}
	})

	t.Run("a revision with pending files cannot be built on", func(t *testing.T) {
		_, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Merge: true, Files: []File{file(t, r, "extra.txt", "x")}})
		if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "modelctl mirror acme/model") {
			t.Fatalf("got %v", err)
		}
	})

	if err := r.Fulfil(ctx, c, "model.bin", weights.SHA256); err != nil {
		t.Fatal(err)
	}
	if f, _ := r.File(ctx, c, "model.bin"); f.Pending || f.SHA256 != weights.SHA256 || !f.LFS {
		t.Fatalf("after fulfilling: %+v", f)
	}
	if err := r.Fulfil(ctx, c, "model.bin", weights.SHA256); err != nil {
		t.Fatalf("fulfilling twice: %v", err)
	}
	if err := r.Fulfil(ctx, c, "config.json", config.SHA256); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.PendingCount(ctx, c); n != 0 {
		t.Fatalf("pending = %d", n)
	}

	// Complete now: it behaves like any other revision.
	next, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Merge: true, Files: []File{file(t, r, "extra.txt", "x")}})
	if err != nil {
		t.Fatalf("building on a completed revision: %v", err)
	}
	if next.Parent != upstreamSHA {
		t.Errorf("parent = %s", next.Parent)
	}
	// Deleting the repository leaves no listing behind.
	if _, err := r.DeleteRepo(ctx, "acme/model"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_files").Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d pending rows after delete (%v)", left, err)
	}
}

func TestCommitLazyRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	ok := []File{{Path: "a.bin", Size: 1, GitOID: strings.Repeat("a", 40)}}
	tests := []struct {
		name string
		in   LazyInput
	}{
		{"no files", LazyInput{Repo: "acme/m", SHA: upstreamSHA}},
		{"revision id is not 40 hex characters", LazyInput{Repo: "acme/m", SHA: "main", Files: ok}},
		{"repository without a namespace", LazyInput{Repo: "m", SHA: upstreamSHA, Files: ok}},
		{"path that escapes", LazyInput{Repo: "acme/m", SHA: upstreamSHA, Files: []File{{Path: "../x", Size: 1}}}},
		{"branch named like a revision id", LazyInput{Repo: "acme/m", SHA: upstreamSHA, Branch: upstreamSHA, Files: ok}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.CommitLazy(ctx, tc.in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
		})
	}
	if _, err := r.GetRepo(ctx, "acme/m"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("a refused recording created the repository: %v", err)
	}
}

// Pull-through records with OnlyFromUpstream. It may create a repository,
// and add to one that came from an upstream, but never touch one that was
// created here.
func TestCommitLazyOnlyFromUpstream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	listing := []File{{Path: "a.bin", Size: 1, GitOID: strings.Repeat("a", 40)}}
	other := strings.Repeat("b", 40)

	// A repository that was pushed, and one that was only created.
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/pushed", Branch: "main", Files: []File{file(t, r, "x", "x")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRepo(ctx, "acme/empty"); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/pushed", "acme/empty"} {
		_, err := r.CommitLazy(ctx, LazyInput{Repo: repo, Branch: "dev", SHA: upstreamSHA, Files: listing,
			Upstream: "https://hub.example", OnlyFromUpstream: true})
		if !errors.Is(err, ErrLocalRepo) {
			t.Fatalf("%s: got %v, want ErrLocalRepo", repo, err)
		}
		got, err := r.GetRepo(ctx, repo)
		if err != nil || got.Upstream != "" {
			t.Fatalf("%s: upstream = %q (%v), want it left empty", repo, got.Upstream, err)
		}
		if _, err := r.Resolve(ctx, repo, "dev"); !errors.Is(err, ErrRevisionNotFound) {
			t.Fatalf("%s: a branch was created: %v", repo, err)
		}
		if _, err := r.Resolve(ctx, repo, upstreamSHA); !errors.Is(err, ErrRevisionNotFound) {
			t.Fatalf("%s: the revision was recorded: %v", repo, err)
		}
	}

	// A repository it creates, and then adds a second revision to.
	for _, sha := range []string{upstreamSHA, other} {
		if _, err := r.CommitLazy(ctx, LazyInput{Repo: "acme/fetched", SHA: sha, Files: listing,
			Upstream: "https://hub.example", OnlyFromUpstream: true}); err != nil {
			t.Fatalf("revision %s: %v", sha, err)
		}
	}
	// An explicit mirror, by someone with the write role, may still record
	// into a pushed repository.
	if _, err := r.CommitLazy(ctx, LazyInput{Repo: "acme/pushed", SHA: upstreamSHA, Files: listing, Upstream: "https://hub.example"}); err != nil {
		t.Fatalf("without OnlyFromUpstream: %v", err)
	}
}

// A data directory written before pending files existed must open, keep
// everything it had, and gain the new table.
func TestUpgradeFromSchemaVersion1(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	// Build a version 1 database by hand: the original schema and one repository.
	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "registry.db"))+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const (
		digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" // sha256("test")
		gitOID = "30d74d258442c7c65512eafab474568dd706c430"
		when   = "2026-01-01T00:00:00.000000000Z"
	)
	steps := []struct {
		stmt string
		args []any
	}{
		{schema, nil},
		{"PRAGMA user_version = 1", nil},
		{"INSERT INTO repos (id, name, created_at) VALUES (1, ?, ?)", []any{"acme/old", when}},
		{"INSERT INTO blobs (sha256, size, git_oid, created_at) VALUES (?, 4, ?, ?)", []any{digest, gitOID, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, created_at) VALUES (1, 1, ?, ?, ?, ?)", []any{upstreamSHA, "t", SourcePush, when}},
		{"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (1, ?, 4, ?, ?, 1)", []any{"a.txt", digest, gitOID}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (1, ?, ?, ?, ?)", []any{"main", KindBranch, upstreamSHA, when}},
	}
	for _, s := range steps {
		if _, err := old.ExecContext(ctx, s.stmt, s.args...); err != nil {
			t.Fatalf("building the version 1 database: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("open a version 1 data directory: %v", err)
	}
	defer func() { _ = r.Close() }()
	var v int
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != 2 {
		t.Fatalf("schema version = %d (%v), want 2", v, err)
	}
	c, err := r.Resolve(ctx, "acme/old", "main")
	if err != nil {
		t.Fatalf("existing revision lost in the upgrade: %v", err)
	}
	files, err := r.Files(ctx, c)
	if err != nil || len(files) != 1 || files[0].Path != "a.txt" || files[0].Pending {
		t.Fatalf("existing files after upgrade: %+v, %v", files, err)
	}
	// The new table works on the upgraded database.
	if _, err := r.CommitLazy(ctx, LazyInput{Repo: "acme/new", SHA: strings.Repeat("b", 40),
		Files: []File{{Path: "x", Size: 1, GitOID: strings.Repeat("c", 40)}}}); err != nil {
		t.Fatalf("recording a pending revision after the upgrade: %v", err)
	}

	// Opening it again is a no-op.
	_ = r.Close()
	again, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("reopen after upgrade: %v", err)
	}
	_ = again.Close()
}
