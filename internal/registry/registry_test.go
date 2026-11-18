// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
)

func open(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func put(t *testing.T, r *Registry, content string) blob.Info {
	t.Helper()
	info, err := r.PutBlob(context.Background(), strings.NewReader(content), blob.Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func file(t *testing.T, r *Registry, path, content string) File {
	t.Helper()
	return File{Path: path, SHA256: put(t, r, content).SHA256, LFS: true}
}

func TestCommitAndResolve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)

	first, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"},
		Files: []File{file(t, r, "config.json", "{}"), file(t, r, "weights/model.bin", "AAAA")}})
	if err != nil {
		t.Fatal(err)
	}
	if !IsSHA(first.SHA) {
		t.Fatalf("revision id %q is not 40 hex characters", first.SHA)
	}

	second, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main",
		Files: []File{file(t, r, "config.json", `{"v":2}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA == first.SHA || second.Parent != first.SHA {
		t.Fatalf("second revision = %+v, want a new id with parent %s", second, first.SHA)
	}

	tests := []struct {
		rev      string
		wantSHA  string
		wantPath []string
	}{
		{"main", second.SHA, []string{"config.json"}},
		{"v1", first.SHA, []string{"config.json", "weights/model.bin"}},
		{first.SHA, first.SHA, []string{"config.json", "weights/model.bin"}},
	}
	for _, tc := range tests {
		c, err := r.Resolve(ctx, "acme/model", tc.rev)
		if err != nil {
			t.Fatalf("resolve %s: %v", tc.rev, err)
		}
		if c.SHA != tc.wantSHA {
			t.Errorf("resolve %s = %s, want %s", tc.rev, c.SHA, tc.wantSHA)
		}
		files, err := r.Files(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		if strings.Join(paths, ",") != strings.Join(tc.wantPath, ",") {
			t.Errorf("files at %s = %v, want %v", tc.rev, paths, tc.wantPath)
		}
	}

	// The old revision still serves its original bytes after main moved on.
	old, _ := r.Resolve(ctx, "acme/model", first.SHA)
	f, err := r.File(ctx, old, "config.json")
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := r.OpenBlob(ctx, f.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "{}" {
		t.Fatalf("old revision content = %q", got)
	}
}

func TestCommitRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	base, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"},
		Files: []File{file(t, r, "a.txt", "a")}})
	if err != nil {
		t.Fatal(err)
	}
	changed := []File{file(t, r, "a.txt", "changed")}

	tests := []struct {
		name string
		in   CommitInput
		want error
	}{
		{"tag cannot move", CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"}, Files: changed}, ErrConflict},
		{"tag cannot be used as a branch", CommitInput{Repo: "acme/model", Branch: "v1", Files: changed}, ErrConflict},
		{"file must be uploaded first", CommitInput{Repo: "acme/model", Branch: "main",
			Files: []File{{Path: "x", SHA256: strings.Repeat("0", 64)}}}, ErrBlobMissing},
		{"empty revision", CommitInput{Repo: "acme/model", Branch: "main"}, ErrInvalid},
		{"duplicate path", CommitInput{Repo: "acme/model", Branch: "main", Files: append(changed, changed...)}, ErrInvalid},
		{"path escapes", CommitInput{Repo: "acme/model", Branch: "main",
			Files: []File{{Path: "../x", SHA256: changed[0].SHA256}}}, ErrInvalid},
		{"file and directory share a name", CommitInput{Repo: "acme/model", Branch: "main",
			Files: []File{{Path: "a", SHA256: changed[0].SHA256}, {Path: "a/b", SHA256: changed[0].SHA256}}}, ErrInvalid},
		{"repository needs a namespace", CommitInput{Repo: "model", Branch: "main", Files: changed}, ErrInvalid},
		{"reserved namespace", CommitInput{Repo: "api/model", Branch: "main", Files: changed}, ErrInvalid},
		{"branch that looks like a revision id", CommitInput{Repo: "acme/model", Branch: strings.Repeat("a", 40), Files: changed}, ErrInvalid},
		{"fixed id reused for different content", CommitInput{Repo: "acme/model", SHA: base.SHA, Files: changed}, ErrConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Commit(ctx, tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}

	// None of the rejected commits may have changed anything.
	c, err := r.Resolve(ctx, "acme/model", "main")
	if err != nil || c.SHA != base.SHA {
		t.Fatalf("main = %s (%v), want unchanged %s", c.SHA, err, base.SHA)
	}
}

func TestIdenticalPushIsNotANewRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	in := CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a.txt", "a")}}
	first, err := r.Commit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Commit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA != first.SHA {
		t.Fatalf("re-pushing identical content created revision %s", again.SHA)
	}
}

func TestMergeOverlaysOnBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main",
		Files: []File{file(t, r, "a.txt", "a"), file(t, r, "b.txt", "b")}}); err != nil {
		t.Fatal(err)
	}
	c, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Merge: true,
		Files: []File{file(t, r, "b.txt", "B2"), file(t, r, "c.txt", "c")}})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := r.Files(ctx, c)
	if len(files) != 3 {
		t.Fatalf("merged revision has %d files, want 3", len(files))
	}
}

func TestDeleteRepoFreesOnlyUnsharedBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	shared := file(t, r, "shared.bin", "shared")
	only := file(t, r, "only.bin", "only in one")
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/one", Branch: "main", Files: []File{shared, only}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/two", Branch: "main", Files: []File{shared}}); err != nil {
		t.Fatal(err)
	}
	st, _ := r.Stats(ctx)
	if st.Blobs != 2 || st.LogicalBytes <= st.StoredBytes {
		t.Fatalf("stats before delete = %+v, want 2 blobs with deduplication", st)
	}

	res, err := r.DeleteRepo(ctx, "acme/one")
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != 1 || res.Bytes != int64(len("only in one")) {
		t.Fatalf("delete freed %+v, want exactly the unshared blob", res)
	}
	if _, _, err := r.OpenBlob(ctx, only.SHA256); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("unshared blob still on disk: %v", err)
	}
	if rc, _, err := r.OpenBlob(ctx, shared.SHA256); err != nil {
		t.Fatalf("shared blob was removed: %v", err)
	} else {
		_ = rc.Close()
	}
	if _, err := r.Resolve(ctx, "acme/one", "main"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("deleted repo still resolves: %v", err)
	}
	if _, err := r.Resolve(ctx, "acme/two", "main"); err != nil {
		t.Fatalf("other repo broken by delete: %v", err)
	}
}

func TestGCKeepsReferencedAndRecentBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	kept := file(t, r, "kept.bin", "kept")
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{kept}}); err != nil {
		t.Fatal(err)
	}
	orphan := put(t, r, "uploaded but never committed")

	// Too young to collect: its commit may still be on the way.
	if res, err := r.GC(ctx, time.Hour); err != nil || res.Blobs != 0 {
		t.Fatalf("gc with grace = %+v, %v; want nothing removed", res, err)
	}
	res, err := r.GC(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != 1 {
		t.Fatalf("gc removed %d blobs, want 1", res.Blobs)
	}
	if _, ok, _ := r.StatBlob(ctx, orphan.SHA256); ok {
		t.Fatal("orphan blob survived gc")
	}
	if _, ok, _ := r.StatBlob(ctx, kept.SHA256); !ok {
		t.Fatal("gc removed a referenced blob")
	}
}

func TestTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	secret, err := r.CreateToken(ctx, "ci", RoleWrite)
	if err != nil {
		t.Fatal(err)
	}
	if tok, ok := r.Authenticate(ctx, secret); !ok || tok.Name != "ci" || tok.Role != RoleWrite {
		t.Fatalf("authenticate = %+v, %v", tok, ok)
	}
	for _, bad := range []string{"", "md_wrong", secret + "x"} {
		if _, ok := r.Authenticate(ctx, bad); ok {
			t.Errorf("token %q was accepted", bad)
		}
	}
	if _, err := r.CreateToken(ctx, "ci", RoleRead); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := r.CreateToken(ctx, "x", "root"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown role: %v", err)
	}
	if err := r.RevokeToken(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Authenticate(ctx, secret); ok {
		t.Fatal("revoked token still works")
	}
	if err := r.RevokeToken(ctx, "ci"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("revoking twice: %v", err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	r, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.PutBlob(ctx, strings.NewReader("persist me"), blob.Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{{Path: "f", SHA256: info.SHA256}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	got, err := r2.Resolve(ctx, "acme/model", "main")
	if err != nil || got.SHA != c.SHA {
		t.Fatalf("after reopen main = %s (%v), want %s", got.SHA, err, c.SHA)
	}
	rc, _, err := r2.OpenBlob(ctx, info.SHA256)
	if err != nil {
		t.Fatalf("blob missing after reopen: %v", err)
	}
	_ = rc.Close()
}

func TestDeleteAndParentCheckInMergeCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	base, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{
		file(t, r, "keep.txt", "keep"), file(t, r, "drop.txt", "drop"),
		file(t, r, "dir/a.bin", "a"), file(t, r, "dir/sub/b.bin", "b"), file(t, r, "dirx/c.bin", "c"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	next, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Merge: true, ExpectParent: base.SHA,
		Delete: []string{"drop.txt", "dir/", "never-existed.txt"},
		Files:  []File{file(t, r, "new.txt", "new")}})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := r.Files(ctx, next)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	// "dir/" removes the directory and everything under it, and nothing
	// that merely starts with the same letters.
	if got := strings.Join(paths, ","); got != "dirx/c.bin,keep.txt,new.txt" {
		t.Fatalf("files after delete = %s", got)
	}

	// The branch moved, so a commit made against the old revision is refused.
	_, err = r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Merge: true, ExpectParent: base.SHA,
		Files: []File{file(t, r, "late.txt", "late")}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale parent: got %v, want ErrConflict", err)
	}
	if c, _ := r.Resolve(ctx, "acme/model", "main"); c.SHA != next.SHA {
		t.Fatal("a refused commit moved the branch")
	}
	// Deleting every file leaves nothing to record.
	_, err = r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Merge: true,
		Delete: []string{"dirx/", "keep.txt", "new.txt"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty result: got %v, want ErrInvalid", err)
	}
}

func TestCreateRepoAndBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)

	created, err := r.CreateRepo(ctx, "acme/empty")
	if err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	if created, err := r.CreateRepo(ctx, "acme/empty"); err != nil || created {
		t.Fatalf("second create = %v, %v; want it to report the repository already existed", created, err)
	}
	if _, err := r.CreateRepo(ctx, "no-namespace"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	// An empty repository exists but has no revision to resolve.
	if _, err := r.Resolve(ctx, "acme/empty", "main"); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("resolve on an empty repository: %v, want ErrRevisionNotFound", err)
	}
	if err := r.CreateBranch(ctx, "acme/empty", "dev", "main"); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("branch from nothing: %v", err)
	}

	c, err := r.Commit(ctx, CommitInput{Repo: "acme/empty", Branch: "main", Files: []File{file(t, r, "a.txt", "a")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBranch(ctx, "acme/empty", "dev", "main"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(ctx, "acme/empty", "dev"); err != nil || got.SHA != c.SHA {
		t.Fatalf("dev = %s (%v), want %s", got.SHA, err, c.SHA)
	}
	if err := r.CreateBranch(ctx, "acme/empty", "dev", "main"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate branch: %v", err)
	}
	if err := r.CreateBranch(ctx, "acme/empty", strings.Repeat("a", 40), "main"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("branch named like a revision id: %v", err)
	}
}
