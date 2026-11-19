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
	Repo         string // the repository's key (RepoKey)
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
	if err := ValidateRepoKey(in.Repo); err != nil {
		return Commit{}, err
	}
	if !IsSHA(in.SHA) {
		return Commit{}, errRevisionID()
	}
	if err := validateTarget(in.Branch, nil); err != nil {
		return Commit{}, err
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
	at, now := r.clock()

	repoID, err := upsertRepo(ctx, tx, repoUpsert{key: in.Repo, upstream: in.Upstream, onlyFromUpstream: in.OnlyFromUpstream, now: now})
	if err != nil {
		return Commit{}, err
	}
	var c Commit
	found, err := scanCommit(tx.QueryRowContext(ctx, commitCols+" WHERE c.repo_id = ? AND c.sha = ?", repoID, in.SHA), &c)
	if err != nil {
		return Commit{}, err
	}
	if !found {
		c = Commit{RepoID: repoID, SHA: in.SHA, Message: in.Message, Source: SourceMirror,
			UpstreamInfo: in.UpstreamInfo, CreatedAt: at}
		c.RepoType, c.Repo = SplitRepoKey(in.Repo)
		// tree stays empty until the last pending file arrives: it is the
		// digest of the complete file list, which is not known before then.
		if c.ID, err = insertCommit(ctx, tx, c, "", now); err != nil {
			return Commit{}, err
		}
		for _, f := range in.Files {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO pending_files (commit_id, path, size, sha256, git_oid, lfs) VALUES (?, ?, ?, ?, ?, ?)",
				c.ID, f.Path, f.Size, f.SHA256, f.GitOID, f.LFS); err != nil {
				return Commit{}, err
			}
		}
	}
	if err := moveBranch(ctx, tx, repoID, in.Branch, in.SHA, now); err != nil {
		return Commit{}, err
	}
	if err := tx.Commit(); err != nil {
		return Commit{}, err
	}
	return c, nil
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
	fulfilled, err := fulfil(ctx, tx, c.ID, path, sha256)
	if err != nil {
		return err
	}
	if fulfilled {
		if _, err := sealIfComplete(ctx, tx, c.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.awaited.arrived(sha256)
	return nil
}

// fulfil moves one pending file of a revision to its stored files, and
// reports whether it did: a file that is not pending is left alone. This is
// the only place a files row is written for a revision that already exists,
// and it writes one only for content the blobs table says is stored.
func fulfil(ctx context.Context, tx dbtx, commitID int64, path, sha256 string) (bool, error) {
	var want File
	err := tx.QueryRowContext(ctx, "SELECT size, sha256, git_oid, lfs FROM pending_files WHERE commit_id = ? AND path = ?", commitID, path).
		Scan(&want.Size, &want.SHA256, &want.GitOID, &want.LFS)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var size int64
	err = tx.QueryRowContext(ctx, "SELECT size FROM blobs WHERE sha256 = ?", sha256).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: %s (sha256 %s)", ErrBlobMissing, path, sha256)
	}
	if err != nil {
		return false, err
	}
	// The digests were checked when the content was stored. These two are
	// what can still be compared here, and a mismatch means a caller bug.
	if size != want.Size || (want.SHA256 != "" && want.SHA256 != sha256) {
		return false, fmt.Errorf("%w: stored content for %s does not match the revision's listing", ErrConflict, path)
	}
	if err := insertFile(ctx, tx, commitID, File{Path: path, Size: size, SHA256: sha256, GitOID: want.GitOID, LFS: want.LFS}); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_files WHERE commit_id = ? AND path = ?", commitID, path); err != nil {
		return false, err
	}
	return true, nil
}

// sealIfComplete gives a revision its tree digest once none of its files
// is pending, which is what makes it indistinguishable from a mirrored
// one. It returns how many files are still pending.
func sealIfComplete(ctx context.Context, tx dbtx, commitID int64) (int, error) {
	left, err := countPending(ctx, tx, commitID)
	if err != nil || left > 0 {
		return left, err
	}
	files, err := queryFiles(ctx, tx, commitID)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE commits SET tree = ? WHERE id = ?", treeDigest(files), commitID)
	return 0, err
}

// CompleteInput names a recorded revision and the stored content of its
// files.
type CompleteInput struct {
	Repo string // the repository's key (RepoKey)
	SHA  string
	// Branch to move to the revision. Empty moves none.
	Branch string
	// Upstream, when set, is recorded as where the repository comes from.
	Upstream string
	// Files of the revision whose content is now stored: Path, and the
	// SHA256 it is stored under. Files that are not pending are ignored.
	Files []File
}

// CompleteRevision finishes a revision that is already recorded, perhaps
// with files still pending: it marks each given file stored and moves the
// branch, in one transaction. Either the revision is complete and the
// branch points at it afterwards, or nothing has changed.
//
// It fails with ErrRevisionNotFound when the repository or the revision is
// not recorded, which is the caller's cue to record it whole with Commit,
// and with ErrConflict when the files given leave some of the revision's
// files pending.
func (r *Registry) CompleteRevision(ctx context.Context, in CompleteInput) (Commit, error) {
	if err := ValidateRepoKey(in.Repo); err != nil {
		return Commit{}, err
	}
	if !IsSHA(in.SHA) {
		return Commit{}, errRevisionID()
	}
	if err := validateTarget(in.Branch, nil); err != nil {
		return Commit{}, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Commit{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var c Commit
	typ, name := SplitRepoKey(in.Repo)
	found, err := scanCommit(tx.QueryRowContext(ctx, commitCols+" WHERE r.type = ? AND r.name = ? AND c.sha = ?", typ, name, in.SHA), &c)
	if err != nil {
		return Commit{}, err
	}
	if !found {
		return Commit{}, fmt.Errorf("%w: %s@%s", ErrRevisionNotFound, in.Repo, in.SHA)
	}
	fulfilled := 0
	for _, f := range in.Files {
		done, err := fulfil(ctx, tx, c.ID, f.Path, f.SHA256)
		if err != nil {
			return Commit{}, fmt.Errorf("%s: %w", f.Path, err)
		}
		if done {
			fulfilled++
		}
	}
	// A revision with nothing pending was sealed when it was recorded or
	// when its last file arrived; only one this call completed needs it.
	left, err := countPending(ctx, tx, c.ID)
	if err != nil {
		return Commit{}, err
	}
	if left > 0 {
		return Commit{}, fmt.Errorf("%w: revision %s lists %d files that were not among those fetched, so it cannot be completed", ErrConflict, in.SHA, left)
	}
	if fulfilled > 0 {
		if _, err := sealIfComplete(ctx, tx, c.ID); err != nil {
			return Commit{}, err
		}
	}
	_, now := r.clock()
	if in.Upstream != "" {
		if _, err := tx.ExecContext(ctx, "UPDATE repos SET upstream = ? WHERE id = ?", in.Upstream, c.RepoID); err != nil {
			return Commit{}, err
		}
	}
	if err := moveBranch(ctx, tx, c.RepoID, in.Branch, c.SHA, now); err != nil {
		return Commit{}, err
	}
	if err := tx.Commit(); err != nil {
		return Commit{}, err
	}
	r.recorded(in.Files)
	return c, nil
}

// countPending is how many files of a revision have not been fetched.
func countPending(ctx context.Context, q dbtx, commitID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_files WHERE commit_id = ?", commitID).Scan(&n)
	return n, err
}

// PendingCount is how many of a revision's files have not been fetched.
func (r *Registry) PendingCount(ctx context.Context, c Commit) (int, error) {
	return countPending(ctx, r.db, c.ID)
}

func queryPending(ctx context.Context, q dbtx, commitID int64) ([]File, error) {
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
func allFiles(ctx context.Context, q dbtx, commitID int64) ([]File, error) {
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
