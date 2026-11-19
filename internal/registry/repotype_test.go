// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A repository's type is a typed name in Go and a plain string on the wire
// and in the database. A document leaves it out for a model, so that every
// model document reads exactly as it did before datasets existed.
func TestRepoTypeWireValues(t *testing.T) {
	t.Parallel()
	if TypeModel != "model" || TypeDataset != "dataset" {
		t.Fatalf("the wire values changed: %q, %q", TypeModel, TypeDataset)
	}
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"model repo", Repo{Name: "acme/x", Type: TypeModel, CreatedAt: at},
			`{"name":"acme/x","created_at":"2026-11-19T12:00:00Z"}`},
		{"dataset repo", Repo{Name: "acme/x", Type: TypeDataset, CreatedAt: at},
			`{"name":"acme/x","type":"dataset","created_at":"2026-11-19T12:00:00Z"}`},
		{"model revision", Commit{Repo: "acme/x", RepoType: TypeModel, SHA: "abc", Source: SourcePush, CreatedAt: at},
			`{"repo":"acme/x","sha":"abc","source":"push","created_at":"2026-11-19T12:00:00Z"}`},
		{"dataset revision", Commit{Repo: "acme/x", RepoType: TypeDataset, SHA: "abc", Source: SourceMirror, CreatedAt: at},
			`{"repo":"acme/x","repo_type":"dataset","sha":"abc","source":"mirror","created_at":"2026-11-19T12:00:00Z"}`},
	}
	for _, tc := range tests {
		got, err := json.Marshal(tc.v)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s encodes as %s (%v), want %s", tc.name, got, err, tc.want)
		}
	}

	for in, want := range map[string]RepoType{"": TypeModel, "model": TypeModel, "dataset": TypeDataset} {
		if got, err := ParseRepoType(in); err != nil || got != want {
			t.Errorf("ParseRepoType(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"space", "datasets", "Dataset", "kernel", " model"} {
		if _, err := ParseRepoType(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseRepoType(%q) = %v, want it refused", bad, err)
		}
	}
}

// A key is the path the Hub gives a repository in its URLs, and reading it
// back gives the type and the name.
func TestRepoKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		typ  RepoType
		name string
	}{
		{"acme/x", TypeModel, "acme/x"},
		{"datasets/acme/x", TypeDataset, "acme/x"},
		// A model recorded in the "datasets" namespace before it was reserved.
		{"datasets/legacy", TypeModel, "datasets/legacy"},
		// Not a key of either kind; left as a model name that fails validation.
		{"datasets/a/b/c", TypeModel, "datasets/a/b/c"},
	}
	for _, tc := range tests {
		typ, name := SplitRepoKey(tc.key)
		if typ != tc.typ || name != tc.name {
			t.Errorf("SplitRepoKey(%q) = %q, %q; want %q, %q", tc.key, typ, name, tc.typ, tc.name)
		}
	}
	if RepoKey(TypeModel, "acme/x") != "acme/x" || RepoKey(TypeDataset, "acme/x") != "datasets/acme/x" || RepoKey("", "acme/x") != "acme/x" {
		t.Error("RepoKey does not give the Hub's paths")
	}
	for _, ok := range []string{"acme/x", "datasets/acme/x"} {
		if err := ValidateRepoKey(ok); err != nil {
			t.Errorf("ValidateRepoKey(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"datasets/legacy", "datasets/a/b/c", "datasets/datasets/x", "datasets/api/x", "x"} {
		if err := ValidateRepoKey(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateRepoKey(%q) = %v, want it refused", bad, err)
		}
	}
}

// "datasets" is reserved as a namespace the way "metrics" is: no new
// repository of either type is created in it.
func TestDatasetsIsAReservedNamespace(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"datasets/model", "Datasets/model", "DATASETS/x"} {
		if err := ValidateRepoName(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateRepoName(%q) = %v, want it refused", name, err)
		}
	}
	for _, name := range []string{"acme/datasets", "datasets-team/model", "my.datasets/model"} {
		if err := ValidateRepoName(name); err != nil {
			t.Errorf("ValidateRepoName(%q) = %v, want it accepted", name, err)
		}
	}
	r := open(t)
	_, err := r.Commit(context.Background(), CommitInput{Repo: "datasets/model", Branch: "main", Files: []File{file(t, r, "a", "a")}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a commit to the model datasets/model: %v", err)
	}
}

// A model and a dataset of the same name are two repositories: each has its
// own revisions, refs and files, and deleting one leaves the other, and the
// content they share, alone.
func TestModelAndDatasetOfTheSameName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	const model, dataset = "acme/same", "datasets/acme/same"

	shared := file(t, r, "README.md", "shared card")
	m, err := r.Commit(ctx, CommitInput{Repo: model, Branch: "main", Tags: []string{"v1"},
		Files: []File{shared, file(t, r, "model.bin", "weights")}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.Commit(ctx, CommitInput{Repo: dataset, Branch: "main",
		Files: []File{shared, file(t, r, "train.csv", "a,b\n1,2\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if m.RepoType != TypeModel || d.RepoType != TypeDataset || m.Repo != "acme/same" || d.Repo != "acme/same" {
		t.Fatalf("recorded %+v and %+v", m, d)
	}
	if m.Key() != model || d.Key() != dataset {
		t.Fatalf("keys %q and %q", m.Key(), d.Key())
	}

	paths := func(key string) string {
		t.Helper()
		c, err := r.Resolve(ctx, key, "main")
		if err != nil {
			t.Fatalf("resolve %s: %v", key, err)
		}
		files, err := r.Files(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range files {
			out = append(out, f.Path)
		}
		return strings.Join(out, ",")
	}
	if got := paths(model); got != "README.md,model.bin" {
		t.Errorf("the model lists %s", got)
	}
	if got := paths(dataset); got != "README.md,train.csv" {
		t.Errorf("the dataset lists %s", got)
	}
	// A file of one is not a file of the other.
	dc, _ := r.Resolve(ctx, dataset, "main")
	if _, err := r.File(ctx, dc, "model.bin"); !errors.Is(err, ErrEntryNotFound) {
		t.Errorf("the dataset has the model's file: %v", err)
	}
	// Refs are per repository: the model's tag is not the dataset's.
	if _, err := r.Resolve(ctx, dataset, "v1"); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("the dataset resolves the model's tag: %v", err)
	}
	if err := r.CreateBranch(ctx, dataset, "dev", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, model, "dev"); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("the model resolves the dataset's branch: %v", err)
	}
	// Creating one does not report the other as existing.
	if created, err := r.CreateRepo(ctx, "datasets/acme/only-model"); err != nil || !created {
		t.Fatalf("CreateRepo of a new dataset: %v, %v", created, err)
	}
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/only-model", Files: []File{file(t, r, "m", "m")}}); err != nil {
		t.Fatal(err)
	}
	if created, err := r.CreateRepo(ctx, "datasets/acme/only-model"); err != nil || created {
		t.Fatalf("CreateRepo of an existing dataset: %v, %v", created, err)
	}

	got, err := r.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, s := range got {
		listed = append(listed, s.Key())
	}
	if strings.Join(listed, " ") != "acme/only-model datasets/acme/only-model acme/same datasets/acme/same" {
		t.Errorf("listed %v", listed)
	}

	// Deleting the dataset frees what only it held, and not the card the
	// model also uses.
	res, err := r.DeleteRepo(ctx, dataset)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != 1 {
		t.Errorf("deleting the dataset freed %d blobs, want only train.csv", res.Blobs)
	}
	if got := paths(model); got != "README.md,model.bin" {
		t.Errorf("after deleting the dataset the model lists %s", got)
	}
	if _, err := r.GetRepo(ctx, dataset); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("the dataset is still there: %v", err)
	}
	if _, ok, _ := r.StatBlob(ctx, shared.SHA256); !ok {
		t.Error("the shared card was removed")
	}
	// And garbage collection does not take what the model still references.
	if _, err := r.GC(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := paths(model); got != "README.md,model.bin" {
		t.Errorf("after gc the model lists %s", got)
	}
}

// Pull-through's rule (never add to a repository created here) is a rule
// per repository: a pushed model does not stop the dataset of the same
// name from being recorded from upstream, and the reverse.
func TestOnlyFromUpstreamIsPerType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/x", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	lazy := LazyInput{Repo: "datasets/acme/x", SHA: upstreamSHA, Branch: "main", Upstream: "https://hub.example",
		OnlyFromUpstream: true, Files: []File{{Path: "train.csv", Size: 3, GitOID: strings.Repeat("c", 40)}}}
	c, err := r.CommitLazy(ctx, lazy)
	if err != nil || c.RepoType != TypeDataset {
		t.Fatalf("recording the dataset from upstream beside a pushed model: %+v, %v", c, err)
	}
	if _, err := r.Commit(ctx, CommitInput{Repo: "datasets/acme/pushed", Files: []File{file(t, r, "b", "b")}}); err != nil {
		t.Fatal(err)
	}
	lazy.Repo = "datasets/acme/pushed"
	if _, err := r.CommitLazy(ctx, lazy); !errors.Is(err, ErrLocalRepo) {
		t.Fatalf("recording into a pushed dataset from upstream: %v", err)
	}
	lazy.Repo = "acme/pushed"
	if _, err := r.CommitLazy(ctx, lazy); err != nil {
		t.Fatalf("recording the model beside a pushed dataset: %v", err)
	}
}

// What verify reports names a dataset by its key, so the operator can tell
// it from the model of the same name.
func TestVerifyNamesDatasetsByKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	f := file(t, r, "data.csv", "x")
	for _, key := range []string{"acme/x", "datasets/acme/x"} {
		if _, err := r.Commit(ctx, CommitInput{Repo: key, Files: []File{f}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.blobs.Delete(ctx, f.SHA256); err != nil {
		t.Fatal(err)
	}
	var found []BlobCheck
	if _, err := r.Verify(ctx, func(c BlobCheck) { found = append(found, c) }); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || strings.Join(found[0].Repos, " ") != "acme/x datasets/acme/x" {
		t.Fatalf("verify found %+v", found)
	}
}

// buildVersion2 writes a database in the layout v0.3.1 writes (schema
// version 2) into dir, holding a pushed model, a mirrored model with a tag
// and a revision with pending files, a model in the "datasets" namespace
// from before it was reserved, and a token.
func buildVersion2(t *testing.T, dir string) {
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
		when    = "2026-01-01T00:00:00.000000000Z"
		pending = "1111111111111111111111111111111111111111"
	)
	steps := []struct {
		stmt string
		args []any
	}{
		{schema, nil},
		{pendingSchema, nil},
		{"PRAGMA user_version = 2", nil},
		{"INSERT INTO repos (id, name, upstream, created_at) VALUES (1, 'acme/pushed', '', ?)", []any{when}},
		{"INSERT INTO repos (id, name, upstream, created_at) VALUES (2, 'hub/mirrored', 'https://hub.example', ?)", []any{when}},
		{"INSERT INTO repos (id, name, upstream, created_at) VALUES (3, 'datasets/legacy', '', ?)", []any{when}},
		{"INSERT INTO blobs (sha256, size, git_oid, created_at) VALUES (?, 4, ?, ?)", []any{digest, gitOID, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, created_at) VALUES (1, 1, ?, 't1', 'push', ?)", []any{upstreamSHA, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, upstream_info, created_at) VALUES (2, 2, ?, 't2', 'mirror', '{\"id\":\"hub/mirrored\"}', ?)", []any{upstreamSHA, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, created_at) VALUES (3, 2, ?, '', 'mirror', ?)", []any{pending, when}},
		{"INSERT INTO commits (id, repo_id, sha, tree, source, created_at) VALUES (4, 3, ?, 't4', 'push', ?)", []any{upstreamSHA, when}},
		{"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (1, 'a.txt', 4, ?, ?, 1)", []any{digest, gitOID}},
		{"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (2, 'config.json', 4, ?, ?, 0)", []any{digest, gitOID}},
		{"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (4, 'old.bin', 4, ?, ?, 1)", []any{digest, gitOID}},
		{"INSERT INTO pending_files (commit_id, path, size, sha256, git_oid, lfs) VALUES (3, 'weights.bin', 9, '', ?, 0)", []any{gitOID}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (1, 'main', 'branch', ?, ?)", []any{upstreamSHA, when}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (2, 'main', 'branch', ?, ?)", []any{pending, when}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (2, 'v1', 'tag', ?, ?)", []any{upstreamSHA, when}},
		{"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (3, 'main', 'branch', ?, ?)", []any{upstreamSHA, when}},
		{"INSERT INTO tokens (name, hash, role, created_at) VALUES ('ci', ?, 'write', ?)", []any{hashToken("md_version2secret0000"), when}},
	}
	for _, s := range steps {
		if _, err := old.ExecContext(ctx, s.stmt, s.args...); err != nil {
			t.Fatalf("building the version 2 database: %s: %v", s.stmt, err)
		}
	}
}

// checkVersion2Contents checks that everything buildVersion2 wrote reads
// back through the registry, every repository as a model.
func checkVersion2Contents(t *testing.T, r *Registry) {
	t.Helper()
	ctx := context.Background()
	repos, err := r.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, s := range repos {
		if s.ID <= 3 { // the rows buildVersion2 wrote
			if s.Type != TypeModel {
				t.Errorf("%s became a %s", s.Name, s.Type)
			}
			models = append(models, s.Name)
		}
	}
	if strings.Join(models, " ") != "acme/pushed datasets/legacy hub/mirrored" {
		t.Fatalf("repositories after the upgrade: %+v", repos)
	}
	mirrored, err := r.GetRepo(ctx, "hub/mirrored")
	if err != nil || mirrored.Upstream != "https://hub.example" || mirrored.Type != TypeModel {
		t.Fatalf("the mirrored repository: %+v, %v", mirrored, err)
	}
	refs, err := r.Refs(ctx, mirrored)
	if err != nil || len(refs) != 2 || refs[0].Name != "main" || refs[1].Kind != KindTag {
		t.Fatalf("its refs: %+v, %v", refs, err)
	}
	tagged, err := r.Resolve(ctx, "hub/mirrored", "v1")
	if err != nil || tagged.UpstreamInfo != `{"id":"hub/mirrored"}` || tagged.Source != SourceMirror {
		t.Fatalf("its tagged revision: %+v, %v", tagged, err)
	}
	head, err := r.Resolve(ctx, "hub/mirrored", "main")
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.Files(ctx, head)
	if err != nil || len(files) != 1 || files[0].Path != "weights.bin" || !files[0].Pending || files[0].Size != 9 {
		t.Fatalf("the revision with a pending file: %+v, %v", files, err)
	}
	for _, key := range []string{"acme/pushed", "datasets/legacy"} {
		c, err := r.Resolve(ctx, key, "main")
		if err != nil || c.RepoType != TypeModel {
			t.Fatalf("%s after the upgrade: %+v, %v", key, c, err)
		}
		files, err := r.Files(ctx, c)
		if err != nil || len(files) != 1 || files[0].Pending {
			t.Fatalf("%s's files: %+v, %v", key, files, err)
		}
	}
	tok, ok, err := r.Authenticate(ctx, "md_version2secret0000")
	if err != nil || !ok || tok.Name != "ci" || tok.Role != RoleWrite {
		t.Fatalf("the token after the upgrade: %+v, %v, %v", tok, ok, err)
	}
}

// A data directory written by v0.3.1 (schema version 2) opens, keeps
// everything it holds, and can then hold a dataset beside the model of the
// same name.
func TestUpgradeFromSchemaVersion2(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	buildVersion2(t, dir)

	r, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("open a version 2 data directory: %v", err)
	}
	defer func() { _ = r.Close() }()
	var v int
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("schema version = %d (%v), want %d", v, err, schemaVersion)
	}
	checkVersion2Contents(t, r)

	// Foreign keys are on again: a revision of no repository is refused.
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO commits (repo_id, sha, tree, source, created_at) VALUES (99, ?, 't', 'push', 'x')", upstreamSHA); err == nil {
		t.Fatal("a revision pointing at no repository was recorded: foreign keys are off")
	}
	// The table's rule holds: a type is a model or a dataset.
	if _, err := r.db.ExecContext(ctx, "INSERT INTO repos (type, name, created_at) VALUES ('space', 'a/b', 'x')"); err == nil {
		t.Fatal("a repository of type space was recorded")
	}

	// A dataset of the same name as an upgraded model, with other content.
	if _, err := r.Commit(ctx, CommitInput{Repo: "datasets/acme/pushed", Branch: "main",
		Files: []File{file(t, r, "train.csv", "a\n1\n")}}); err != nil {
		t.Fatalf("a dataset beside the upgraded model: %v", err)
	}
	d, err := r.Resolve(ctx, "datasets/acme/pushed", "main")
	if err != nil || d.RepoType != TypeDataset {
		t.Fatalf("the dataset: %+v, %v", d, err)
	}
	m, err := r.Resolve(ctx, "acme/pushed", "main")
	if err != nil || m.SHA != upstreamSHA {
		t.Fatalf("the model after the dataset was added: %+v, %v", m, err)
	}

	// Opening it again is a no-op.
	_ = r.Close()
	again, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("reopen after upgrade: %v", err)
	}
	defer func() { _ = again.Close() }()
	checkVersion2Contents(t, again)
}

// An upgrade that is interrupted leaves the database as it was, at its old
// version, and the next start upgrades it.
func TestInterruptedUpgradeLeavesTheOldVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	buildVersion2(t, dir)

	// The rebuild of the repositories table has run, inside the
	// transaction, when the upgrade is stopped.
	stop := errors.New("the process died")
	if r, err := openWithHook(dir, nil, func(v int) error {
		if v == 3 {
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
	var v, typeColumns, repos int
	err = raw.QueryRowContext(ctx, `SELECT (SELECT user_version FROM pragma_user_version),
		(SELECT COUNT(*) FROM pragma_table_info('repos') WHERE name = 'type'),
		(SELECT COUNT(*) FROM repos)`).Scan(&v, &typeColumns, &repos)
	_ = raw.Close()
	if err != nil || v != 2 || typeColumns != 0 || repos != 3 {
		t.Fatalf("after the interrupted upgrade: version %d, type column %d, %d repositories (%v); want the version 2 database untouched", v, typeColumns, repos, err)
	}

	r, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("open after the interrupted upgrade: %v", err)
	}
	defer func() { _ = r.Close() }()
	checkVersion2Contents(t, r)
}
