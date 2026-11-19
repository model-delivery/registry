// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/kit"
)

// Source is where a revision came from.
type Source string

// The sources of a revision.
const (
	SourcePush   Source = "push"
	SourceMirror Source = "mirror"
)

// RefKind is what sort of name a ref is.
type RefKind string

// Ref kinds. Branches move; tags never do.
const (
	KindBranch RefKind = "branch"
	KindTag    RefKind = "tag"
)

// RepoType is what kind of repository a repository is. It is part of a
// repository's identity: a model and a dataset of the same name are two
// repositories.
type RepoType string

// Repository types. The zero value means a model, as it does on the Hub.
const (
	TypeModel   RepoType = "model"
	TypeDataset RepoType = "dataset"
)

// datasetKeyPrefix is how the key of a dataset begins: the segment the Hub
// puts before a dataset's id in its URLs.
const datasetKeyPrefix = kit.DatasetsSegment + "/"

// ParseRepoType reads a repository type as a request or a flag names it.
// The empty string is a model.
func ParseRepoType(s string) (RepoType, error) {
	switch RepoType(s) {
	case "", TypeModel:
		return TypeModel, nil
	case TypeDataset:
		return TypeDataset, nil
	}
	return "", fmt.Errorf("%w: repository type must be model or dataset, not %q", ErrInvalid, s)
}

// IsZero reports whether t is a model, so that a document leaves the type
// out for models (encoding/json's omitzero) and is unchanged for them.
func (t RepoType) IsZero() bool { return t == "" || t == TypeModel }

// OrModel is t, or TypeModel when t is empty.
func (t RepoType) OrModel() RepoType {
	if t == "" {
		return TypeModel
	}
	return t
}

// RepoKey is how this package addresses a repository: the name of a model,
// and "datasets/" and the name of a dataset. That is the path the Hub gives
// each in its URLs, and it keeps a model and a dataset of the same name
// apart. Every method that takes a repository name takes its key.
//
// name must be a valid repository name (ValidateRepoName); a name of more
// than two segments would read as another key.
func RepoKey(t RepoType, name string) string {
	if t == TypeDataset {
		return kit.DatasetKey(name)
	}
	return name
}

// SplitRepoKey is the inverse of RepoKey. A key of two segments is a model,
// even one whose namespace is "datasets" (a model recorded before that
// namespace was reserved); "datasets/" followed by two segments is a
// dataset.
func SplitRepoKey(key string) (RepoType, string) {
	if name, dataset := kit.SplitKey(key); dataset {
		return TypeDataset, name
	}
	return TypeModel, key
}

// Limits on the names a revision records.
const (
	maxRefName = 200
	maxPathLen = 1024
)

var (
	repoPart = `[A-Za-z0-9][A-Za-z0-9._-]{0,95}`
	repoRe   = regexp.MustCompile(`^` + repoPart + `/` + repoPart + `$`)
	shaRe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// Top-level URL segments the server uses for itself. "datasets" begins
	// every dataset's address, as it does on the Hub.
	reservedNamespaces = map[string]bool{"api": true, "ui": true, "static": true, "healthz": true, "metrics": true, "datasets": true}
)

// IsSHA reports whether s is a full revision id.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// ValidateRepoName checks a "namespace/name" identifier, the name of a
// repository of either type.
func ValidateRepoName(name string) error {
	if !repoRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%w: repository must be namespace/name using letters, digits, '.', '_' or '-'", ErrInvalid)
	}
	if ns, _, _ := strings.Cut(name, "/"); reservedNamespaces[strings.ToLower(ns)] {
		return fmt.Errorf("%w: namespace %q is reserved", ErrInvalid, ns)
	}
	return nil
}

// ValidateRepoKey checks a repository key (RepoKey): the name it holds must
// be valid. A model recorded in the "datasets" namespace before it was
// reserved fails, so it can be read and deleted but not added to.
func ValidateRepoKey(key string) error {
	_, name := SplitRepoKey(key)
	return ValidateRepoName(name)
}

// ValidateRefName checks a branch or tag name.
func ValidateRefName(name string) error {
	switch {
	case name == "" || len(name) > maxRefName:
		return fmt.Errorf("%w: ref name must be 1-%d characters", ErrInvalid, maxRefName)
	case IsSHA(name):
		return fmt.Errorf("%w: ref name cannot look like a revision id", ErrInvalid)
	case strings.ContainsAny(name, " \t\r\n\\~^:?*[") || strings.Contains(name, "..") ||
		strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//"):
		return fmt.Errorf("%w: ref name %q is not allowed", ErrInvalid, name)
	}
	return nil
}

// ValidatePath checks a file path that is about to be recorded in a
// revision. Paths already recorded are not checked again when they are
// read, so a revision written before a rule was added stays readable.
func ValidatePath(p string) error {
	if p == "" || len(p) > maxPathLen || strings.ContainsRune(p, '\\') {
		return fmt.Errorf("%w: bad file path %q", ErrInvalid, p)
	}
	// Control characters: NUL, and the ones a terminal acts on (escape,
	// newline, carriage return, backspace) when a listing prints the path.
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return fmt.Errorf("%w: file path %q contains a control character", ErrInvalid, p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: bad file path %q", ErrInvalid, p)
		}
	}
	return nil
}

// Repo is a repository: a model or a dataset.
type Repo struct {
	ID   int64  `json:"-"`
	Name string `json:"name"`
	// Type is absent from the document for a model.
	Type      RepoType  `json:"type,omitzero"`
	Upstream  string    `json:"upstream,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Key is the repository's key (RepoKey).
func (r Repo) Key() string { return RepoKey(r.Type, r.Name) }

// Commit is one immutable revision of a repository.
type Commit struct {
	ID     int64  `json:"-"`
	RepoID int64  `json:"-"`
	Repo   string `json:"repo"`
	// RepoType is the type of Repo; absent from the document for a model.
	RepoType     RepoType  `json:"repo_type,omitzero"`
	SHA          string    `json:"sha"`
	Parent       string    `json:"parent,omitempty"`
	Message      string    `json:"message,omitempty"`
	Source       Source    `json:"source"`
	UpstreamInfo string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// Key is the key (RepoKey) of the repository the revision belongs to.
func (c Commit) Key() string { return RepoKey(c.RepoType, c.Repo) }

// File is one file in a revision.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// GitOID is the git object id Hugging Face lists for the file: the blob
	// id of the content, or of the LFS pointer when LFS is true.
	GitOID string `json:"git_oid"`
	LFS    bool   `json:"lfs"`
	// Pending means the revision lists this file but its content has not
	// been fetched yet (see pending.go). SHA256 is then known only for LFS
	// files.
	Pending bool `json:"pending,omitempty"`
}

// ETag is the identifier Hugging Face clients cache the file under.
func (f File) ETag() string {
	if f.LFS {
		return f.SHA256
	}
	return f.GitOID
}

// Ref is a named pointer to a revision.
type Ref struct {
	Name      string    `json:"name"`
	Kind      RefKind   `json:"kind"`
	SHA       string    `json:"sha"`
	UpdatedAt time.Time `json:"updated_at"`
}

// treeDigest identifies a file list independent of when it was recorded.
func treeDigest(sorted []File) string {
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%t\n", f.Path, f.SHA256, f.GitOID, f.LFS)
	}
	return hex.EncodeToString(h.Sum(nil))
}

const commitCols = `SELECT c.id, c.repo_id, r.name, r.type, c.sha, c.parent, c.message, c.source, c.upstream_info, c.created_at
	FROM commits c JOIN repos r ON r.id = c.repo_id`

type rowScanner interface{ Scan(...any) error }

func scanCommit(row rowScanner, c *Commit) (bool, error) {
	var created string
	err := row.Scan(&c.ID, &c.RepoID, &c.Repo, &c.RepoType, &c.SHA, &c.Parent, &c.Message, &c.Source, &c.UpstreamInfo, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return false, err
	}
	return true, nil
}

func queryFiles(ctx context.Context, q dbtx, commitID int64) ([]File, error) {
	rows, err := q.QueryContext(ctx, "SELECT path, size, sha256, git_oid, lfs FROM files WHERE commit_id = ? ORDER BY path", commitID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Size, &f.SHA256, &f.GitOID, &f.LFS); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetRepo looks a repository up by its key (RepoKey).
func (r *Registry) GetRepo(ctx context.Context, key string) (Repo, error) {
	typ, name := SplitRepoKey(key)
	var repo Repo
	var created string
	err := r.db.QueryRowContext(ctx, "SELECT id, name, type, upstream, created_at FROM repos WHERE type = ? AND name = ?", typ, name).
		Scan(&repo.ID, &repo.Name, &repo.Type, &repo.Upstream, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, key)
	}
	if err != nil {
		return Repo{}, err
	}
	if repo.CreatedAt, err = parseTime(created); err != nil {
		return Repo{}, err
	}
	return repo, nil
}

// Resolve turns a revision (branch, tag or full revision id) into a commit.
func (r *Registry) Resolve(ctx context.Context, repoName, rev string) (Commit, error) {
	repo, err := r.GetRepo(ctx, repoName)
	if err != nil {
		return Commit{}, err
	}
	sha := rev
	if !IsSHA(rev) {
		err := r.db.QueryRowContext(ctx, "SELECT sha FROM refs WHERE repo_id = ? AND name = ?", repo.ID, rev).Scan(&sha)
		if errors.Is(err, sql.ErrNoRows) {
			return Commit{}, fmt.Errorf("%w: %s@%s", ErrRevisionNotFound, repoName, rev)
		}
		if err != nil {
			return Commit{}, err
		}
	}
	var c Commit
	found, err := scanCommit(r.db.QueryRowContext(ctx, commitCols+" WHERE c.repo_id = ? AND c.sha = ?", repo.ID, sha), &c)
	if err != nil {
		return Commit{}, err
	}
	if !found {
		return Commit{}, fmt.Errorf("%w: %s@%s", ErrRevisionNotFound, repoName, rev)
	}
	return c, nil
}

// Files lists a revision's files in path order.
//
// Stored and pending files are in two tables, and a file moves from one to
// the other when its content arrives (fulfil). Both are read in one
// transaction, so a file that moves meanwhile is listed once: never
// missing, never twice.
func (r *Registry) Files(ctx context.Context, c Commit) ([]File, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return allFiles(ctx, tx, c.ID)
}

// File returns one file of a revision.
//
// It asks both tables in one statement, for the reason given at Files:
// asked one after the other, a file whose content arrived in between is in
// neither answer, and a download that should wait for it is told the file
// does not exist.
func (r *Registry) File(ctx context.Context, c Commit, path string) (File, error) {
	f := File{Path: path}
	err := r.db.QueryRowContext(ctx,
		`SELECT size, sha256, git_oid, lfs, 0 AS pending FROM files WHERE commit_id = ? AND path = ?
		 UNION ALL
		 SELECT size, sha256, git_oid, lfs, 1 FROM pending_files WHERE commit_id = ? AND path = ?
		 ORDER BY pending LIMIT 1`, c.ID, path, c.ID, path).
		Scan(&f.Size, &f.SHA256, &f.GitOID, &f.LFS, &f.Pending)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, fmt.Errorf("%w: %s", ErrEntryNotFound, path)
	}
	return f, err
}

// Refs lists a repository's branches and tags by name.
func (r *Registry) Refs(ctx context.Context, repo Repo) ([]Ref, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT name, kind, sha, updated_at FROM refs WHERE repo_id = ? ORDER BY name", repo.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Ref{}
	for rows.Next() {
		var ref Ref
		var updated string
		if err := rows.Scan(&ref.Name, &ref.Kind, &ref.SHA, &updated); err != nil {
			return nil, err
		}
		if ref.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// CommitSummary is a revision plus the size of its file list.
type CommitSummary struct {
	Commit
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Commits lists a repository's revisions, newest first.
func (r *Registry) Commits(ctx context.Context, repo Repo) ([]CommitSummary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.id, c.repo_id, r.name, r.type, c.sha, c.parent, c.message, c.source, c.upstream_info, c.created_at,
		(SELECT COUNT(*) FROM files f WHERE f.commit_id = c.id) + (SELECT COUNT(*) FROM pending_files p WHERE p.commit_id = c.id),
		(SELECT COALESCE(SUM(size), 0) FROM files f WHERE f.commit_id = c.id) + (SELECT COALESCE(SUM(size), 0) FROM pending_files p WHERE p.commit_id = c.id)
		FROM commits c JOIN repos r ON r.id = c.repo_id WHERE c.repo_id = ? ORDER BY c.id DESC`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CommitSummary{}
	for rows.Next() {
		var s CommitSummary
		var created string
		if err := rows.Scan(&s.ID, &s.RepoID, &s.Repo, &s.RepoType, &s.SHA, &s.Parent, &s.Message, &s.Source, &s.UpstreamInfo, &created, &s.Files, &s.Bytes); err != nil {
			return nil, err
		}
		if s.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RepoSummary is a repository with the totals shown in listings.
// StoredBytes counts each distinct blob the repository references once.
type RepoSummary struct {
	Repo
	Revisions   int       `json:"revisions"`
	StoredBytes int64     `json:"stored_bytes"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ListRepos returns every repository, models and datasets, by name, a model
// before the dataset of the same name.
//
// The totals are computed for all repositories in one pass over the
// revisions and one over the files, not once per repository: this listing
// is what the home page and an anonymous API call run.
func (r *Registry) ListRepos(ctx context.Context) ([]RepoSummary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT r.id, r.name, r.type, r.upstream, r.created_at,
		COALESCE(rev.n, 0), COALESCE(held.bytes, 0), COALESCE(rev.latest, r.created_at)
		FROM repos r
		LEFT JOIN (SELECT repo_id, COUNT(*) AS n, MAX(created_at) AS latest FROM commits GROUP BY repo_id) rev
			ON rev.repo_id = r.id
		LEFT JOIN (SELECT d.repo_id, SUM(b.size) AS bytes
			FROM (SELECT DISTINCT c.repo_id, f.sha256 FROM files f JOIN commits c ON c.id = f.commit_id) d
			JOIN blobs b ON b.sha256 = d.sha256 GROUP BY d.repo_id) held
			ON held.repo_id = r.id
		ORDER BY r.name, r.type = 'dataset'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RepoSummary{}
	for rows.Next() {
		var s RepoSummary
		var created, updated string
		if err := rows.Scan(&s.ID, &s.Name, &s.Type, &s.Upstream, &created, &s.Revisions, &s.StoredBytes, &updated); err != nil {
			return nil, err
		}
		if s.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if s.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteRepo removes a repository, all its revisions, and every blob that
// no other repository still references.
//
// One kind of blob is spared: one that a writer stored again, or was told
// is "already stored", within BlobGrace and has not yet recorded a revision
// for. That writer's revision is on its way, and until it lands the blob
// looks unreferenced the moment this repository is gone. A blob spared
// here is removed by a later garbage collection if nothing came to use it.
func (r *Registry) DeleteRepo(ctx context.Context, name string) (GCResult, error) {
	repo, err := r.GetRepo(ctx, name)
	if err != nil {
		return GCResult{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return GCResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT f.sha256 FROM files f JOIN commits c ON c.id = f.commit_id WHERE c.repo_id = ?`, repo.ID)
	if err != nil {
		return GCResult{}, err
	}
	digests, err := scanStrings(rows)
	if err != nil {
		return GCResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM repos WHERE id = ?", repo.ID); err != nil {
		return GCResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return GCResult{}, err
	}
	// The repository is gone. Free what it alone was holding; blobs another
	// repository uses are still referenced and are skipped, and so are
	// those a writer is about to reference.
	return r.dropUnreferenced(ctx, digests, r.stamp(), true)
}

// CreateRepo records an empty repository, named by its key, so it exists
// before its first revision. It reports whether the repository was created
// by this call.
func (r *Registry) CreateRepo(ctx context.Context, key string) (bool, error) {
	if err := ValidateRepoKey(key); err != nil {
		return false, err
	}
	typ, name := SplitRepoKey(key)
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO repos (type, name, upstream, created_at) VALUES (?, ?, '', ?) ON CONFLICT (type, name) DO NOTHING", typ, name, r.stamp())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CreateBranch points a new branch at an existing revision of the
// repository. from is a branch, tag or revision id.
func (r *Registry) CreateBranch(ctx context.Context, repoName, branch, from string) error {
	if err := ValidateRefName(branch); err != nil {
		return err
	}
	c, err := r.Resolve(ctx, repoName, from)
	if err != nil {
		return err
	}
	// A name that is taken, by a branch or a tag, inserts nothing. Any other
	// failure is returned as it is.
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (repo_id, name) DO NOTHING",
		c.RepoID, branch, KindBranch, c.SHA, r.stamp())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: %q already exists in %s", ErrConflict, branch, repoName)
	}
	return nil
}
