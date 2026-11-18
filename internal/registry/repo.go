// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"crypto/sha1" //nolint:gosec // revision ids are 40-hex to match git/Hugging Face; not a security boundary.
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Where a revision came from.
const (
	SourcePush   = "push"
	SourceMirror = "mirror"
)

// Ref kinds. Branches move; tags never do.
const (
	KindBranch = "branch"
	KindTag    = "tag"
)

var (
	repoPart = `[A-Za-z0-9][A-Za-z0-9._-]{0,95}`
	repoRe   = regexp.MustCompile(`^` + repoPart + `/` + repoPart + `$`)
	shaRe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// Top-level URL segments the server uses for itself.
	reservedNamespaces = map[string]bool{"api": true, "ui": true, "static": true, "healthz": true}
)

// IsSHA reports whether s is a full revision id.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// ValidateRepoName checks a "namespace/name" identifier.
func ValidateRepoName(name string) error {
	if !repoRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%w: repository must be namespace/name using letters, digits, '.', '_' or '-'", ErrInvalid)
	}
	if ns, _, _ := strings.Cut(name, "/"); reservedNamespaces[strings.ToLower(ns)] {
		return fmt.Errorf("%w: namespace %q is reserved", ErrInvalid, ns)
	}
	return nil
}

// ValidateRefName checks a branch or tag name.
func ValidateRefName(name string) error {
	switch {
	case name == "" || len(name) > 200:
		return fmt.Errorf("%w: ref name must be 1-200 characters", ErrInvalid)
	case IsSHA(name):
		return fmt.Errorf("%w: ref name cannot look like a revision id", ErrInvalid)
	case strings.ContainsAny(name, " \t\r\n\\~^:?*[") || strings.Contains(name, "..") ||
		strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//"):
		return fmt.Errorf("%w: ref name %q is not allowed", ErrInvalid, name)
	}
	return nil
}

// ValidatePath checks a file path inside a repository.
func ValidatePath(p string) error {
	if p == "" || len(p) > 1024 || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("%w: bad file path %q", ErrInvalid, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: bad file path %q", ErrInvalid, p)
		}
	}
	return nil
}

// Repo is a model repository.
type Repo struct {
	ID        int64     `json:"-"`
	Name      string    `json:"name"`
	Upstream  string    `json:"upstream,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Commit is one immutable revision of a repository.
type Commit struct {
	ID           int64     `json:"-"`
	RepoID       int64     `json:"-"`
	Repo         string    `json:"repo"`
	SHA          string    `json:"sha"`
	Parent       string    `json:"parent,omitempty"`
	Message      string    `json:"message,omitempty"`
	Source       string    `json:"source"`
	UpstreamInfo string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// File is one file in a revision.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// GitOID is the git object id Hugging Face lists for the file: the blob
	// id of the content, or of the LFS pointer when LFS is true.
	GitOID string `json:"git_oid"`
	LFS    bool   `json:"lfs"`
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
	Kind      string    `json:"kind"`
	SHA       string    `json:"sha"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CommitInput describes a revision to record.
type CommitInput struct {
	Repo string
	// Branch to move to the new revision. Empty records the revision only.
	Branch string
	// Tags to create at the new revision. An existing tag is never moved.
	Tags []string
	// SHA fixes the revision id (used to preserve upstream ids when
	// mirroring). Empty lets the registry derive one.
	SHA          string
	Message      string
	Source       string
	Upstream     string
	UpstreamInfo string
	// Files in the revision. SHA256 is required and must already be stored.
	// Size is always taken from the stored blob. GitOID defaults to it too.
	Files []File
	// Merge overlays Files on the branch's current revision instead of
	// replacing its file list.
	Merge bool
}

// Commit records a revision atomically: either the revision, its files and
// its refs all become visible, or none do.
func (r *Registry) Commit(ctx context.Context, in CommitInput) (Commit, error) {
	if err := ValidateRepoName(in.Repo); err != nil {
		return Commit{}, err
	}
	if in.Branch != "" {
		if err := ValidateRefName(in.Branch); err != nil {
			return Commit{}, err
		}
	}
	for _, t := range in.Tags {
		if err := ValidateRefName(t); err != nil {
			return Commit{}, err
		}
	}
	if in.SHA != "" && !IsSHA(in.SHA) {
		return Commit{}, fmt.Errorf("%w: revision id must be 40 hex characters", ErrInvalid)
	}
	if in.Source == "" {
		in.Source = SourcePush
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Commit{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := r.stamp()

	var repoID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM repos WHERE name = ?", in.Repo).Scan(&repoID)
	if errors.Is(err, sql.ErrNoRows) {
		res, insErr := tx.ExecContext(ctx, "INSERT INTO repos (name, upstream, created_at) VALUES (?, ?, ?)", in.Repo, in.Upstream, now)
		if insErr != nil {
			return Commit{}, insErr
		}
		repoID, _ = res.LastInsertId()
	} else if err != nil {
		return Commit{}, err
	} else if in.Upstream != "" {
		if _, err := tx.ExecContext(ctx, "UPDATE repos SET upstream = ? WHERE id = ?", in.Upstream, repoID); err != nil {
			return Commit{}, err
		}
	}

	var parent string
	var parentID int64
	var parentTree string
	if in.Branch != "" {
		err = tx.QueryRowContext(ctx,
			`SELECT c.sha, c.id, c.tree FROM refs r JOIN commits c ON c.repo_id = r.repo_id AND c.sha = r.sha
			 WHERE r.repo_id = ? AND r.name = ?`, repoID, in.Branch).Scan(&parent, &parentID, &parentTree)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Commit{}, err
		}
	}

	files := map[string]File{}
	if in.Merge && parentID != 0 {
		prev, err := queryFiles(ctx, tx, parentID)
		if err != nil {
			return Commit{}, err
		}
		for _, f := range prev {
			files[f.Path] = f
		}
	}
	seen := map[string]bool{}
	for _, f := range in.Files {
		if err := ValidatePath(f.Path); err != nil {
			return Commit{}, err
		}
		if seen[f.Path] {
			return Commit{}, fmt.Errorf("%w: file %q listed twice", ErrInvalid, f.Path)
		}
		seen[f.Path] = true
		var size int64
		var oid string
		err := tx.QueryRowContext(ctx, "SELECT size, git_oid FROM blobs WHERE sha256 = ?", f.SHA256).Scan(&size, &oid)
		if errors.Is(err, sql.ErrNoRows) {
			return Commit{}, fmt.Errorf("%w: %s (sha256 %s)", ErrBlobMissing, f.Path, f.SHA256)
		}
		if err != nil {
			return Commit{}, err
		}
		f.Size = size
		if f.GitOID == "" {
			f.GitOID = oid
		}
		files[f.Path] = f
	}
	if len(files) == 0 {
		return Commit{}, fmt.Errorf("%w: a revision needs at least one file", ErrInvalid)
	}
	// A path cannot be both a file and a directory.
	for p := range files {
		for i, ch := range p {
			if ch != '/' {
				continue
			}
			if _, clash := files[p[:i]]; clash {
				return Commit{}, fmt.Errorf("%w: %q is both a file and a directory", ErrInvalid, p[:i])
			}
		}
	}
	sorted := make([]File, 0, len(files))
	for _, f := range files {
		sorted = append(sorted, f)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	tree := treeDigest(sorted)

	c := Commit{RepoID: repoID, Repo: in.Repo, Parent: parent, Message: in.Message, Source: in.Source,
		UpstreamInfo: in.UpstreamInfo, CreatedAt: parseTime(now)}
	sha := in.SHA
	switch {
	case sha == "" && parentID != 0 && tree == parentTree:
		// Nothing changed: pushing the same content twice is not a new revision.
		sha = parent
	case sha == "":
		sum := sha1.Sum([]byte("revision\n" + in.Repo + "\n" + tree + "\n" + parent + "\n" + in.Message + "\n" + now)) //nolint:gosec // see import note.
		sha = hex.EncodeToString(sum[:])
	}
	if sha == parent {
		c.Parent = ""
	}

	var existing Commit
	found, err := scanCommit(tx.QueryRowContext(ctx, commitCols+" WHERE c.repo_id = ? AND c.sha = ?", repoID, sha), &existing)
	if err != nil {
		return Commit{}, err
	}
	if found {
		var existingTree string
		if err := tx.QueryRowContext(ctx, "SELECT tree FROM commits WHERE id = ?", existing.ID).Scan(&existingTree); err != nil {
			return Commit{}, err
		}
		if existingTree != tree {
			return Commit{}, fmt.Errorf("%w: revision %s already exists with different content", ErrConflict, sha)
		}
		c = existing
	} else {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO commits (repo_id, sha, parent, tree, message, source, upstream_info, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			repoID, sha, c.Parent, tree, in.Message, in.Source, in.UpstreamInfo, now)
		if err != nil {
			return Commit{}, err
		}
		c.ID, _ = res.LastInsertId()
		c.SHA = sha
		for _, f := range sorted {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (?, ?, ?, ?, ?, ?)",
				c.ID, f.Path, f.Size, f.SHA256, f.GitOID, f.LFS); err != nil {
				return Commit{}, err
			}
		}
	}

	if in.Branch != "" {
		var kind string
		err := tx.QueryRowContext(ctx, "SELECT kind FROM refs WHERE repo_id = ? AND name = ?", repoID, in.Branch).Scan(&kind)
		if err == nil && kind == KindTag {
			return Commit{}, fmt.Errorf("%w: %q is a tag and cannot be moved", ErrConflict, in.Branch)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (repo_id, name) DO UPDATE SET sha = excluded.sha, updated_at = excluded.updated_at`,
			repoID, in.Branch, KindBranch, c.SHA, now); err != nil {
			return Commit{}, err
		}
	}
	for _, t := range in.Tags {
		var cur string
		err := tx.QueryRowContext(ctx, "SELECT sha FROM refs WHERE repo_id = ? AND name = ?", repoID, t).Scan(&cur)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (?, ?, ?, ?, ?)",
				repoID, t, KindTag, c.SHA, now); err != nil {
				return Commit{}, err
			}
		case err != nil:
			return Commit{}, err
		case cur != c.SHA:
			return Commit{}, fmt.Errorf("%w: %q already names revision %s", ErrConflict, t, cur)
		}
	}
	if err := tx.Commit(); err != nil {
		return Commit{}, err
	}
	return c, nil
}

// treeDigest identifies a file list independent of when it was recorded.
func treeDigest(sorted []File) string {
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%t\n", f.Path, f.SHA256, f.GitOID, f.LFS)
	}
	return hex.EncodeToString(h.Sum(nil))
}

const commitCols = `SELECT c.id, c.repo_id, r.name, c.sha, c.parent, c.message, c.source, c.upstream_info, c.created_at
	FROM commits c JOIN repos r ON r.id = c.repo_id`

type rowScanner interface{ Scan(...any) error }

func scanCommit(row rowScanner, c *Commit) (bool, error) {
	var created string
	err := row.Scan(&c.ID, &c.RepoID, &c.Repo, &c.SHA, &c.Parent, &c.Message, &c.Source, &c.UpstreamInfo, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	c.CreatedAt = parseTime(created)
	return true, nil
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func queryFiles(ctx context.Context, q querier, commitID int64) ([]File, error) {
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

// GetRepo looks a repository up by name.
func (r *Registry) GetRepo(ctx context.Context, name string) (Repo, error) {
	var repo Repo
	var created string
	err := r.db.QueryRowContext(ctx, "SELECT id, name, upstream, created_at FROM repos WHERE name = ?", name).
		Scan(&repo.ID, &repo.Name, &repo.Upstream, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, name)
	}
	if err != nil {
		return Repo{}, err
	}
	repo.CreatedAt = parseTime(created)
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
func (r *Registry) Files(ctx context.Context, c Commit) ([]File, error) {
	return queryFiles(ctx, r.db, c.ID)
}

// File returns one file of a revision.
func (r *Registry) File(ctx context.Context, c Commit, path string) (File, error) {
	f := File{Path: path}
	err := r.db.QueryRowContext(ctx, "SELECT size, sha256, git_oid, lfs FROM files WHERE commit_id = ? AND path = ?", c.ID, path).
		Scan(&f.Size, &f.SHA256, &f.GitOID, &f.LFS)
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
		ref.UpdatedAt = parseTime(updated)
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
	rows, err := r.db.QueryContext(ctx, `SELECT c.id, c.repo_id, r.name, c.sha, c.parent, c.message, c.source, c.upstream_info, c.created_at,
		(SELECT COUNT(*) FROM files f WHERE f.commit_id = c.id),
		(SELECT COALESCE(SUM(size), 0) FROM files f WHERE f.commit_id = c.id)
		FROM commits c JOIN repos r ON r.id = c.repo_id WHERE c.repo_id = ? ORDER BY c.id DESC`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CommitSummary{}
	for rows.Next() {
		var s CommitSummary
		var created string
		if err := rows.Scan(&s.ID, &s.RepoID, &s.Repo, &s.SHA, &s.Parent, &s.Message, &s.Source, &s.UpstreamInfo, &created, &s.Files, &s.Bytes); err != nil {
			return nil, err
		}
		s.CreatedAt = parseTime(created)
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

// ListRepos returns every repository by name.
func (r *Registry) ListRepos(ctx context.Context) ([]RepoSummary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT r.id, r.name, r.upstream, r.created_at,
		(SELECT COUNT(*) FROM commits c WHERE c.repo_id = r.id),
		(SELECT COALESCE(SUM(b.size), 0) FROM blobs b WHERE b.sha256 IN
			(SELECT f.sha256 FROM files f JOIN commits c ON c.id = f.commit_id WHERE c.repo_id = r.id)),
		COALESCE((SELECT MAX(c.created_at) FROM commits c WHERE c.repo_id = r.id), r.created_at)
		FROM repos r ORDER BY r.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RepoSummary{}
	for rows.Next() {
		var s RepoSummary
		var created, updated string
		if err := rows.Scan(&s.ID, &s.Name, &s.Upstream, &created, &s.Revisions, &s.StoredBytes, &updated); err != nil {
			return nil, err
		}
		s.CreatedAt, s.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteRepo removes a repository, all its revisions, and every blob that
// no other repository still references.
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
	// repository uses are still referenced and are skipped.
	return r.dropUnreferenced(ctx, digests, r.stamp())
}
