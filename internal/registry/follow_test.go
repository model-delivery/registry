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
	"time"
)

// mirrored records a revision of key as a mirror does, at branch main.
func mirrored(t *testing.T, r *Registry, key string) Commit {
	t.Helper()
	c, err := r.Commit(context.Background(), CommitInput{Repo: key, Branch: "main", SHA: upstreamSHA, Source: SourceMirror,
		Upstream: "https://hub.example", Files: []File{file(t, r, "config.json", "{}")}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Only a repository that came from an upstream can be followed, and only a
// branch or every ref: the rules, and the entry each one leaves.
func TestFollowRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	mirrored(t, r, "hub/model")
	mirrored(t, r, "datasets/hub/table")
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/pushed", Branch: "main", Files: []File{file(t, r, "a.txt", "a")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, CommitInput{Repo: "hub/model", Tags: []string{"v1"}, SHA: upstreamSHA, Source: SourceMirror,
		Files: []File{file(t, r, "config.json", "{}")}}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		key  string
		spec FollowSpec
		want error
	}{
		{"a pushed repository", "acme/pushed", FollowSpec{Branch: "main"}, ErrLocalRepo},
		{"a repository not held", "hub/absent", FollowSpec{Branch: "main"}, ErrRepoNotFound},
		{"a tag", "hub/model", FollowSpec{Branch: "v1"}, ErrConflict},
		{"neither a branch nor all", "hub/model", FollowSpec{}, ErrInvalid},
		{"both a branch and all", "hub/model", FollowSpec{Branch: "main", AllRefs: true}, ErrInvalid},
		{"a branch name that is not allowed", "hub/model", FollowSpec{Branch: "a..b"}, ErrInvalid},
		{"the model's branch", "hub/model", FollowSpec{Branch: "main"}, nil},
		{"every ref of the dataset", "datasets/hub/table", FollowSpec{AllRefs: true}, nil},
		{"a branch not held yet", "hub/model", FollowSpec{Branch: "dev"}, nil},
	}
	for _, tc := range tests {
		f, err := r.Follow(ctx, tc.key, tc.spec)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", tc.name, err, tc.want)
		}
		if err == nil && (f.Key() != tc.key || f.Spec() != tc.spec || f.CreatedAt.IsZero() || !f.LastCheck.IsZero()) {
			t.Fatalf("%s: entry %+v", tc.name, f)
		}
	}
	list, err := r.Follows(ctx)
	if err != nil || len(list) != 2 || list[0].Branch != "dev" || list[1].Key() != "datasets/hub/table" || !list[1].AllRefs {
		t.Fatalf("follow list %+v, %v", list, err)
	}
	if _, err := r.GetFollow(ctx, "acme/pushed"); !errors.Is(err, ErrNotFollowed) {
		t.Fatalf("a repository never followed: %v", err)
	}

	if err := r.Unfollow(ctx, "hub/model"); err != nil {
		t.Fatal(err)
	}
	if err := r.Unfollow(ctx, "hub/model"); !errors.Is(err, ErrNotFollowed) {
		t.Fatalf("unfollow twice: %v", err)
	}
	// Unfollowing changes nothing the repository holds.
	if c, err := r.Resolve(ctx, "hub/model", "main"); err != nil || c.SHA != upstreamSHA {
		t.Fatalf("after unfollowing: %+v, %v", c, err)
	}
	// Deleting a followed repository takes it off the list.
	if _, err := r.DeleteRepo(ctx, "datasets/hub/table"); err != nil {
		t.Fatal(err)
	}
	if list, err := r.Follows(ctx); err != nil || len(list) != 0 {
		t.Fatalf("after deleting the followed dataset: %+v, %v", list, err)
	}
}

// Each check is recorded with the time the checker gives: a success clears
// the last error and the count of failures, a failure adds to them.
func TestRecordSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	mirrored(t, r, "hub/model")
	if _, err := r.Follow(ctx, "hub/model", FollowSpec{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	steps := []struct {
		rec  SyncRecord
		want func(Follow) bool
	}{
		{SyncRecord{At: t0}, func(f Follow) bool {
			return f.LastCheck.Equal(t0) && f.LastSuccess.Equal(t0) && f.LastChange.IsZero() && f.Failures == 0
		}},
		{SyncRecord{At: t0.Add(time.Hour), Error: "upstream returned 503"}, func(f Follow) bool {
			return f.LastCheck.Equal(t0.Add(time.Hour)) && f.LastSuccess.Equal(t0) && f.LastError == "upstream returned 503" &&
				f.LastErrorAt.Equal(t0.Add(time.Hour)) && f.Failures == 1
		}},
		{SyncRecord{At: t0.Add(2 * time.Hour), Error: "again"}, func(f Follow) bool { return f.Failures == 2 && f.LastError == "again" }},
		{SyncRecord{At: t0.Add(3 * time.Hour), Changed: true}, func(f Follow) bool {
			return f.LastSuccess.Equal(t0.Add(3*time.Hour)) && f.LastChange.Equal(t0.Add(3*time.Hour)) &&
				f.LastError == "" && f.LastErrorAt.IsZero() && f.Failures == 0
		}},
	}
	for i, s := range steps {
		if err := r.RecordSync(ctx, "hub/model", s.rec); err != nil {
			t.Fatal(err)
		}
		f, err := r.GetFollow(ctx, "hub/model")
		if err != nil || !s.want(f) {
			t.Fatalf("after step %d: %+v, %v", i, f, err)
		}
	}
	// A repository taken off the list while it was checked is not put back.
	if err := r.Unfollow(ctx, "hub/model"); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordSync(ctx, "hub/model", SyncRecord{At: t0}); err != nil {
		t.Fatalf("recording a check of a repository no longer followed: %v", err)
	}
	if _, err := r.GetFollow(ctx, "hub/model"); !errors.Is(err, ErrNotFollowed) {
		t.Fatalf("the entry came back: %v", err)
	}
}

// A revision completed by a mirror can be given tags, and an existing tag
// that names another revision is refused with nothing changed.
func TestCompleteRevisionCreatesTags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	c := mirrored(t, r, "hub/model")
	if _, err := r.CompleteRevision(ctx, CompleteInput{Repo: "hub/model", SHA: c.SHA, Tags: []string{"v1"}}); err != nil {
		t.Fatal(err)
	}
	ref, err := r.Ref(ctx, "hub/model", "v1")
	if err != nil || ref.Kind != KindTag || ref.SHA != c.SHA {
		t.Fatalf("the tag: %+v, %v", ref, err)
	}
	other, err := r.Commit(ctx, CommitInput{Repo: "hub/model", SHA: strings.Repeat("2", 40), Source: SourceMirror,
		Files: []File{file(t, r, "config.json", "{\"v\":2}")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompleteRevision(ctx, CompleteInput{Repo: "hub/model", SHA: other.SHA, Branch: "dev", Tags: []string{"v1"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("moving a tag: %v", err)
	}
	if ref, _ := r.Ref(ctx, "hub/model", "v1"); ref.SHA != c.SHA {
		t.Fatalf("the tag moved to %s", ref.SHA)
	}
	if _, err := r.Ref(ctx, "hub/model", "dev"); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("the branch of the refused call was created: %v", err)
	}
}

// buildVersion3 writes a database in the layout v0.4.1 writes (schema
// version 3) into dir: a pushed model, a mirrored model with a tag and a
// revision with a pending file, a mirrored dataset of the same name as the
// pushed model, and a token.
func buildVersion3(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "registry.db"))+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	const (
		digest  = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" // sha256("test")
		gitOID  = "30d74d258442c7c65512eafab474568dd706c430"
		when    = "2026-11-01T00:00:00.000000000Z"
		pending = "1111111111111111111111111111111111111111"
	)
	steps := []struct {
		stmt string
		args []any
	}{
		{schema, nil},
		{pendingSchema, nil},
		{typesSchema, nil},
		{"PRAGMA user_version = 3", nil},
		{"INSERT INTO repos (id, type, name, upstream, created_at) VALUES (1, 'model', 'acme/pushed', '', ?)", []any{when}},
		{"INSERT INTO repos (id, type, name, upstream, created_at) VALUES (2, 'model', 'hub/mirrored', 'https://hub.example', ?)", []any{when}},
		{"INSERT INTO repos (id, type, name, upstream, created_at) VALUES (3, 'dataset', 'acme/pushed', 'https://hub.example', ?)", []any{when}},
		{"INSERT INTO blobs (sha256, size, git_oid, created_at) VALUES (?, 4, ?, ?)", []any{digest, gitOID, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, created_at) VALUES (1, 1, ?, 't1', 'push', ?)", []any{upstreamSHA, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, upstream_info, created_at) VALUES (2, 2, ?, 't2', 'mirror', '{\"id\":\"hub/mirrored\"}', ?)", []any{upstreamSHA, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, created_at) VALUES (3, 2, ?, '', 'mirror', ?)", []any{pending, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, created_at) VALUES (4, 3, ?, 't4', 'mirror', ?)", []any{upstreamSHA, when}},
		{"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (1, 'a.txt', 4, ?, ?, 1)", []any{digest, gitOID}},
		{"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (2, 'config.json', 4, ?, ?, 0)", []any{digest, gitOID}},
		{"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (4, 'train.csv', 4, ?, ?, 0)", []any{digest, gitOID}},
		{"INSERT INTO pending_files (commit_id, path, size, sha256, git_oid, lfs) VALUES (3, 'weights.bin', 9, '', ?, 0)", []any{gitOID}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (1, 'main', 'branch', ?, ?)", []any{upstreamSHA, when}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (2, 'main', 'branch', ?, ?)", []any{pending, when}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (2, 'v1', 'tag', ?, ?)", []any{upstreamSHA, when}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (3, 'main', 'branch', ?, ?)", []any{upstreamSHA, when}},
		{"INSERT INTO tokens (name, hash, role, created_at) VALUES ('ci', ?, 'write', ?)", []any{hashToken("md_version3secret0000"), when}},
	}
	for _, s := range steps {
		if _, err := old.ExecContext(ctx, s.stmt, s.args...); err != nil {
			t.Fatalf("building the version 3 database: %s: %v", s.stmt, err)
		}
	}
}

// checkVersion3Contents checks that everything buildVersion3 wrote reads
// back through the registry, and that nothing is followed.
func checkVersion3Contents(t *testing.T, r *Registry) {
	t.Helper()
	ctx := context.Background()
	repos, err := r.ListRepos(ctx)
	if err != nil || len(repos) != 3 {
		t.Fatalf("repositories: %+v, %v", repos, err)
	}
	var keys []string
	for _, s := range repos {
		keys = append(keys, s.Key())
	}
	if strings.Join(keys, " ") != "acme/pushed datasets/acme/pushed hub/mirrored" {
		t.Fatalf("repositories after the upgrade: %v", keys)
	}
	refs, err := r.Refs(ctx, repos[2].Repo)
	if err != nil || len(refs) != 2 || refs[0].Name != "main" || refs[1].Kind != KindTag {
		t.Fatalf("the mirrored model's refs: %+v, %v", refs, err)
	}
	head, err := r.Resolve(ctx, "hub/mirrored", "main")
	if err != nil {
		t.Fatal(err)
	}
	if files, err := r.Files(ctx, head); err != nil || len(files) != 1 || !files[0].Pending {
		t.Fatalf("the revision with a pending file: %+v, %v", files, err)
	}
	d, err := r.Resolve(ctx, "datasets/acme/pushed", "main")
	if err != nil || d.RepoType != TypeDataset {
		t.Fatalf("the dataset: %+v, %v", d, err)
	}
	if files, err := r.Files(ctx, d); err != nil || len(files) != 1 || files[0].Path != "train.csv" {
		t.Fatalf("the dataset's files: %+v, %v", files, err)
	}
	if tok, ok, err := r.Authenticate(ctx, "md_version3secret0000"); err != nil || !ok || tok.Name != "ci" {
		t.Fatalf("the token: %+v, %v, %v", tok, ok, err)
	}
}

// A data directory written by v0.4.1 (schema version 3) opens, keeps
// everything it holds, and can then follow what it mirrored.
func TestUpgradeFromSchemaVersion3(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	buildVersion3(t, dir)

	r, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("open a version 3 data directory: %v", err)
	}
	defer func() { _ = r.Close() }()
	var v int
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("schema version = %d (%v), want %d", v, err, schemaVersion)
	}
	checkVersion3Contents(t, r)
	if list, err := r.Follows(ctx); err != nil || len(list) != 0 {
		t.Fatalf("follow list after the upgrade: %+v, %v", list, err)
	}

	// What came from an upstream can be followed, of either type; what was
	// pushed cannot.
	if _, err := r.Follow(ctx, "hub/mirrored", FollowSpec{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Follow(ctx, "datasets/acme/pushed", FollowSpec{AllRefs: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Follow(ctx, "acme/pushed", FollowSpec{Branch: "main"}); !errors.Is(err, ErrLocalRepo) {
		t.Fatalf("following the pushed model: %v", err)
	}
	// The table's rule holds: one branch or all refs.
	if _, err := r.db.ExecContext(ctx, "INSERT INTO follows (repo_id, branch, all_refs, created_at) VALUES (1, 'main', 1, 'x')"); err == nil {
		t.Fatal("a follow of a branch and of all refs at once was recorded")
	}

	// The follow list survives reopening.
	_ = r.Close()
	again, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("reopen after upgrade: %v", err)
	}
	defer func() { _ = again.Close() }()
	checkVersion3Contents(t, again)
	if list, err := again.Follows(ctx); err != nil || len(list) != 2 {
		t.Fatalf("follow list after reopening: %+v, %v", list, err)
	}
}

// An upgrade from version 3 that is interrupted leaves the database at
// version 3 without the follow list, and the next start upgrades it.
func TestInterruptedUpgradeFromSchemaVersion3(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	buildVersion3(t, dir)

	stop := errors.New("the process died")
	if r, err := openWithHook(dir, nil, func(v int) error {
		if v == 4 {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		if r != nil {
			_ = r.Close()
		}
		t.Fatalf("open with the upgrade interrupted: %v", err)
	}

	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "registry.db"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var v, tables, repos int
	err = raw.QueryRowContext(ctx, `SELECT (SELECT user_version FROM pragma_user_version),
		(SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'follows'),
		(SELECT COUNT(*) FROM repos)`).Scan(&v, &tables, &repos)
	_ = raw.Close()
	if err != nil || v != 3 || tables != 0 || repos != 3 {
		t.Fatalf("after the interrupted upgrade: version %d, follows tables %d, %d repositories (%v); want the version 3 database untouched", v, tables, repos, err)
	}

	r, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("open after the interrupted upgrade: %v", err)
	}
	defer func() { _ = r.Close() }()
	checkVersion3Contents(t, r)
	if _, err := r.Follow(ctx, "hub/mirrored", FollowSpec{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
}
