// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// A pending file is one a revision is known to contain but whose content
// has not been fetched yet. Pull-through caching records a revision from
// the upstream's file listing (every path, size and digest) and fetches
// each file's bytes only when a client downloads it.
//
// Pending files live in their own table, so the files table keeps its
// guarantee: every row there points at stored, verified content. A pending
// file becomes an ordinary one in a single transaction, once its content
// is stored and has been checked against the digest recorded here.

const pendingSchema = `
CREATE TABLE pending_files (
  commit_id INTEGER NOT NULL REFERENCES commits(id) ON DELETE CASCADE,
  path      TEXT NOT NULL,
  size      INTEGER NOT NULL,
  sha256    TEXT NOT NULL DEFAULT '',
  git_oid   TEXT NOT NULL,
  lfs       INTEGER NOT NULL,
  PRIMARY KEY (commit_id, path)
) WITHOUT ROWID;
`

// LazyInput describes an upstream revision to record before its content is
// fetched.
type LazyInput struct {
	Repo         string
	Branch       string // moved to the revision; empty records the revision only
	SHA          string // the upstream revision id
	Message      string
	Upstream     string
	UpstreamInfo string
	// OnlyFromUpstream refuses, with ErrLocalRepo, to record the revision in
	// a repository that exists and did not itself come from an upstream.
	// Pull-through sets it: a reader's request must never add upstream
	// content to a repository that was pushed here.
	OnlyFromUpstream bool
	// Files as the upstream lists them. SHA256 is set for LFS files, whose
	// content digest the upstream publishes, and empty for the rest, which
	// are identified by GitOID until they are fetched.
	Files []File
}

// CommitLazy records a revision whose files are all pending. If the
// revision is already recorded, complete or not, only the branch is moved.
func (r *Registry) CommitLazy(ctx context.Context, in LazyInput) (Commit, error) {
	if err := ValidateRepoName(in.Repo); err != nil {
		return Commit{}, err
	}
	if !IsSHA(in.SHA) {
		return Commit{}, fmt.Errorf("%w: revision id must be 40 hex characters", ErrInvalid)
	}
	if in.Branch != "" {
		if err := ValidateRefName(in.Branch); err != nil {
			return Commit{}, err
		}
	}
	if len(in.Files) == 0 {
		return Commit{}, fmt.Errorf("%w: a revision needs at least one file", ErrInvalid)
	}
	for _, f := range in.Files {
		if err := ValidatePath(f.Path); err != nil {
			return Commit{}, err
		}
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Commit{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := r.stamp()

	var repoID int64
	var heldUpstream string
	err = tx.QueryRowContext(ctx, "SELECT id, upstream FROM repos WHERE name = ?", in.Repo).Scan(&repoID, &heldUpstream)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, insErr := tx.ExecContext(ctx, "INSERT INTO repos (name, upstream, created_at) VALUES (?, ?, ?)", in.Repo, in.Upstream, now)
		if insErr != nil {
			return Commit{}, insErr
		}
		repoID, _ = res.LastInsertId()
	case err != nil:
		return Commit{}, err
	case in.OnlyFromUpstream && heldUpstream == "":
		// Checked inside the transaction, so a repository created by a push
		// between the caller's lookup and this one is still left alone.
		return Commit{}, fmt.Errorf("%w: %s", ErrLocalRepo, in.Repo)
	case in.Upstream != "":
		if _, err := tx.ExecContext(ctx, "UPDATE repos SET upstream = ? WHERE id = ?", in.Upstream, repoID); err != nil {
			return Commit{}, err
		}
	}

	var c Commit
	found, err := scanCommit(tx.QueryRowContext(ctx, commitCols+" WHERE c.repo_id = ? AND c.sha = ?", repoID, in.SHA), &c)
	if err != nil {
		return Commit{}, err
	}
	if !found {
		// tree stays empty until the last pending file arrives: it is the
		// digest of the complete file list, which is not known before then.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO commits (repo_id, sha, parent, tree, message, source, upstream_info, created_at)
			 VALUES (?, ?, '', '', ?, ?, ?, ?)`,
			repoID, in.SHA, in.Message, SourceMirror, in.UpstreamInfo, now)
		if err != nil {
			return Commit{}, err
		}
		id, _ := res.LastInsertId()
		c = Commit{ID: id, RepoID: repoID, Repo: in.Repo, SHA: in.SHA, Message: in.Message, Source: SourceMirror,
			UpstreamInfo: in.UpstreamInfo, CreatedAt: parseTime(now)}
		for _, f := range in.Files {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO pending_files (commit_id, path, size, sha256, git_oid, lfs) VALUES (?, ?, ?, ?, ?, ?)",
				id, f.Path, f.Size, f.SHA256, f.GitOID, f.LFS); err != nil {
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
			repoID, in.Branch, KindBranch, in.SHA, now); err != nil {
			return Commit{}, err
		}
	}
	return c, tx.Commit()
}

// Fulfil turns a pending file into a stored one. The content must already
// be in the blob store as sha256; the caller is expected to have verified
// it against the pending file's digest while storing it. When the last
// pending file of a revision is fulfilled the revision is complete.
// Fulfilling a file that is no longer pending is not an error.
func (r *Registry) Fulfil(ctx context.Context, c Commit, path, sha256 string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var want File
	err = tx.QueryRowContext(ctx, "SELECT size, sha256, git_oid, lfs FROM pending_files WHERE commit_id = ? AND path = ?", c.ID, path).
		Scan(&want.Size, &want.SHA256, &want.GitOID, &want.LFS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var size int64
	err = tx.QueryRowContext(ctx, "SELECT size FROM blobs WHERE sha256 = ?", sha256).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s (sha256 %s)", ErrBlobMissing, path, sha256)
	}
	if err != nil {
		return err
	}
	// The digests were checked when the content was stored. These two are
	// what can still be compared here, and a mismatch means a caller bug.
	if size != want.Size || (want.SHA256 != "" && want.SHA256 != sha256) {
		return fmt.Errorf("%w: stored content for %s does not match the revision's listing", ErrConflict, path)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (?, ?, ?, ?, ?, ?)",
		c.ID, path, size, sha256, want.GitOID, want.LFS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_files WHERE commit_id = ? AND path = ?", c.ID, path); err != nil {
		return err
	}
	var left int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_files WHERE commit_id = ?", c.ID).Scan(&left); err != nil {
		return err
	}
	if left == 0 {
		files, err := queryFiles(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE commits SET tree = ? WHERE id = ?", treeDigest(files), c.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingCount is how many of a revision's files have not been fetched.
func (r *Registry) PendingCount(ctx context.Context, c Commit) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_files WHERE commit_id = ?", c.ID).Scan(&n)
	return n, err
}

func queryPending(ctx context.Context, q querier, commitID int64) ([]File, error) {
	rows, err := q.QueryContext(ctx, "SELECT path, size, sha256, git_oid, lfs FROM pending_files WHERE commit_id = ? ORDER BY path", commitID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []File
	for rows.Next() {
		f := File{Pending: true}
		if err := rows.Scan(&f.Path, &f.Size, &f.SHA256, &f.GitOID, &f.LFS); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// allFiles is a revision's complete file list: stored files and pending
// ones together, in path order.
func allFiles(ctx context.Context, q querier, commitID int64) ([]File, error) {
	stored, err := queryFiles(ctx, q, commitID)
	if err != nil {
		return nil, err
	}
	pending, err := queryPending(ctx, q, commitID)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return stored, nil
	}
	out := append(stored, pending...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
