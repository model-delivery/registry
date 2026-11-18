// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"crypto/sha1" //nolint:gosec // revision ids are 40-hex to match git/Hugging Face; not a security boundary.
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Recording a revision is one transaction made of the steps in this file.
// Commit (a revision whose content is stored), CommitLazy (one whose
// content is still to be fetched) and CompleteRevision (the rest of a
// partly fetched one) are each a different selection of the same steps.

// dbtx is what a step needs of the transaction it runs in.
type dbtx interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
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
	Source       Source
	Upstream     string
	UpstreamInfo string
	// Files in the revision. SHA256 is required and must already be stored.
	// Size is always taken from the stored blob. GitOID defaults to it too.
	Files []File
	// Merge overlays Files on the branch's current revision instead of
	// replacing its file list.
	Merge bool
	// Delete removes paths from the branch's current file list before Files
	// are applied. It only has an effect with Merge. An entry ending in "/"
	// removes everything under that directory.
	Delete []string
	// ExpectParent, when set, is the revision the caller believes the branch
	// points at. If the branch has moved, the commit is refused, so two
	// writers cannot silently overwrite each other.
	ExpectParent string
}

// validate checks the names in the input, before anything is read.
func (in *CommitInput) validate() error {
	if err := ValidateRepoName(in.Repo); err != nil {
		return err
	}
	if err := validateTarget(in.Branch, in.Tags); err != nil {
		return err
	}
	if in.SHA != "" && !IsSHA(in.SHA) {
		return errRevisionID()
	}
	return nil
}

func errRevisionID() error {
	return fmt.Errorf("%w: revision id must be 40 hex characters", ErrInvalid)
}

// validateTarget checks the refs a revision is to be recorded under. An
// empty branch means none.
func validateTarget(branch string, tags []string) error {
	if branch != "" {
		if err := ValidateRefName(branch); err != nil {
			return err
		}
	}
	for _, t := range tags {
		if err := ValidateRefName(t); err != nil {
			return err
		}
	}
	return nil
}

// Commit records a revision atomically: either the revision, its files and
// its refs all become visible, or none do.
func (r *Registry) Commit(ctx context.Context, in CommitInput) (Commit, error) {
	if err := in.validate(); err != nil {
		return Commit{}, err
	}
	if in.Source == "" {
		in.Source = SourcePush
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Commit{}, err
	}
	defer func() { _ = tx.Rollback() }()
	at, now := r.clock()

	repoID, err := upsertRepo(ctx, tx, repoUpsert{name: in.Repo, upstream: in.Upstream, now: now})
	if err != nil {
		return Commit{}, err
	}
	parent, err := branchHead(ctx, tx, repoID, in.Branch)
	if err != nil {
		return Commit{}, err
	}
	if in.ExpectParent != "" && in.ExpectParent != parent.sha {
		at := parent.sha
		if at == "" {
			at = "no revision yet"
		}
		return Commit{}, fmt.Errorf("%w: %s moved; expected %s, it is at %s", ErrConflict, in.Branch, in.ExpectParent, at)
	}
	files, err := buildFileList(ctx, tx, in, parent)
	if err != nil {
		return Commit{}, err
	}
	tree := treeDigest(files)

	c := Commit{RepoID: repoID, Repo: in.Repo, Parent: parent.sha, Message: in.Message, Source: in.Source,
		UpstreamInfo: in.UpstreamInfo, CreatedAt: at}
	c.SHA = in.SHA
	switch {
	case c.SHA == "" && parent.id != 0 && tree == parent.tree:
		// Nothing changed: pushing the same content twice is not a new revision.
		c.SHA = parent.sha
	case c.SHA == "":
		sum := sha1.Sum([]byte("revision\n" + in.Repo + "\n" + tree + "\n" + parent.sha + "\n" + in.Message + "\n" + now)) //nolint:gosec // see import note.
		c.SHA = hex.EncodeToString(sum[:])
	}
	if c.SHA == parent.sha {
		c.Parent = ""
	}
	if c, err = recordRevision(ctx, tx, c, tree, files, now); err != nil {
		return Commit{}, err
	}
	if err := moveBranch(ctx, tx, repoID, in.Branch, c.SHA, now); err != nil {
		return Commit{}, err
	}
	if err := createTags(ctx, tx, repoID, in.Tags, c.SHA, now); err != nil {
		return Commit{}, err
	}
	if err := tx.Commit(); err != nil {
		return Commit{}, err
	}
	r.recorded(in.Files)
	return c, nil
}

// repoUpsert is what upsertRepo needs to know.
type repoUpsert struct {
	name string
	// upstream, when set, is recorded as where the repository comes from.
	upstream string
	// onlyFromUpstream refuses, with ErrLocalRepo, a repository that exists
	// and did not come from an upstream.
	onlyFromUpstream bool
	now              string
}

// upsertRepo finds the repository, creating it if it is new, and returns
// its id.
func upsertRepo(ctx context.Context, tx dbtx, in repoUpsert) (int64, error) {
	var id int64
	var held string
	err := tx.QueryRowContext(ctx, "SELECT id, upstream FROM repos WHERE name = ?", in.name).Scan(&id, &held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, "INSERT INTO repos (name, upstream, created_at) VALUES (?, ?, ?)", in.name, in.upstream, in.now)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	case err != nil:
		return 0, err
	case in.onlyFromUpstream && held == "":
		// Checked inside the transaction, so a repository created by a push
		// between the caller's lookup and this one is still left alone.
		return 0, fmt.Errorf("%w: %s", ErrLocalRepo, in.name)
	case in.upstream != "":
		if _, err := tx.ExecContext(ctx, "UPDATE repos SET upstream = ? WHERE id = ?", in.upstream, id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// head is the revision a branch points at. The zero value means the branch
// does not exist yet.
type head struct {
	sha  string
	id   int64
	tree string
}

// branchHead reads what branch points at. An empty branch name, and a
// branch that does not exist, give the zero head.
func branchHead(ctx context.Context, tx dbtx, repoID int64, branch string) (head, error) {
	var h head
	if branch == "" {
		return h, nil
	}
	err := tx.QueryRowContext(ctx,
		`SELECT c.sha, c.id, c.tree FROM refs r JOIN commits c ON c.repo_id = r.repo_id AND c.sha = r.sha
		 WHERE r.repo_id = ? AND r.name = ?`, repoID, branch).Scan(&h.sha, &h.id, &h.tree)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return head{}, err
	}
	return h, nil
}

// buildFileList works out the new revision's complete file list, in path
// order: the parent's files when merging, less the deletions, with the
// input's files laid over them. Every file is checked to be stored.
func buildFileList(ctx context.Context, tx dbtx, in CommitInput, parent head) ([]File, error) {
	files := map[string]File{}
	if in.Merge && parent.id != 0 {
		pending, err := countPending(ctx, tx, parent.id)
		if err != nil {
			return nil, err
		}
		if pending > 0 {
			return nil, fmt.Errorf("%w: %s has %d files that were never fetched from upstream, so a revision cannot be built on it; run `modelctl mirror %s` to fetch them first",
				ErrConflict, in.Branch, pending, in.Repo)
		}
		prev, err := queryFiles(ctx, tx, parent.id)
		if err != nil {
			return nil, err
		}
		for _, f := range prev {
			files[f.Path] = f
		}
		for _, d := range in.Delete {
			if strings.HasSuffix(d, "/") {
				for p := range files {
					if strings.HasPrefix(p, d) {
						delete(files, p)
					}
				}
				continue
			}
			delete(files, d)
		}
	}
	seen := map[string]bool{}
	for _, f := range in.Files {
		if err := ValidatePath(f.Path); err != nil {
			return nil, err
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("%w: file %q listed twice", ErrInvalid, f.Path)
		}
		seen[f.Path] = true
		var size int64
		var oid string
		err := tx.QueryRowContext(ctx, "SELECT size, git_oid FROM blobs WHERE sha256 = ?", f.SHA256).Scan(&size, &oid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s (sha256 %s)", ErrBlobMissing, f.Path, f.SHA256)
		}
		if err != nil {
			return nil, err
		}
		f.Size = size
		if f.GitOID == "" {
			f.GitOID = oid
		}
		files[f.Path] = f
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: a revision needs at least one file", ErrInvalid)
	}
	// A path cannot be both a file and a directory.
	for p := range files {
		for i, ch := range p {
			if ch != '/' {
				continue
			}
			if _, clash := files[p[:i]]; clash {
				return nil, fmt.Errorf("%w: %q is both a file and a directory", ErrInvalid, p[:i])
			}
		}
	}
	sorted := make([]File, 0, len(files))
	for _, f := range files {
		sorted = append(sorted, f)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return sorted, nil
}

// recordRevision inserts revision c with its files and returns it with its
// row id. If a revision with that id is already recorded it is returned
// instead, provided its content is the same; different content under one
// id is a conflict.
func recordRevision(ctx context.Context, tx dbtx, c Commit, tree string, files []File, now string) (Commit, error) {
	var existing Commit
	found, err := scanCommit(tx.QueryRowContext(ctx, commitCols+" WHERE c.repo_id = ? AND c.sha = ?", c.RepoID, c.SHA), &existing)
	if err != nil {
		return Commit{}, err
	}
	if found {
		var existingTree string
		if err := tx.QueryRowContext(ctx, "SELECT tree FROM commits WHERE id = ?", existing.ID).Scan(&existingTree); err != nil {
			return Commit{}, err
		}
		if existingTree != tree {
			return Commit{}, fmt.Errorf("%w: revision %s already exists with different content", ErrConflict, c.SHA)
		}
		return existing, nil
	}
	if c.ID, err = insertCommit(ctx, tx, c, tree, now); err != nil {
		return Commit{}, err
	}
	for _, f := range files {
		if err := insertFile(ctx, tx, c.ID, f); err != nil {
			return Commit{}, err
		}
	}
	return c, nil
}

func insertCommit(ctx context.Context, tx dbtx, c Commit, tree, now string) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO commits (repo_id, sha, parent, tree, message, source, upstream_info, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.RepoID, c.SHA, c.Parent, tree, c.Message, c.Source, c.UpstreamInfo, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// insertFile records a file of a revision. The caller has established that
// its content is stored; the foreign key on blobs refuses it otherwise.
func insertFile(ctx context.Context, tx dbtx, commitID int64, f File) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO files (commit_id, path, size, sha256, git_oid, lfs) VALUES (?, ?, ?, ?, ?, ?)",
		commitID, f.Path, f.Size, f.SHA256, f.GitOID, f.LFS)
	return err
}

// moveBranch points branch at sha, creating it if needed. A tag of that
// name is never moved. An empty branch name does nothing.
func moveBranch(ctx context.Context, tx dbtx, repoID int64, branch, sha, now string) error {
	if branch == "" {
		return nil
	}
	var kind RefKind
	err := tx.QueryRowContext(ctx, "SELECT kind FROM refs WHERE repo_id = ? AND name = ?", repoID, branch).Scan(&kind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A new branch.
	case err != nil:
		return err
	case kind == KindTag:
		return fmt.Errorf("%w: %q is a tag and cannot be moved", ErrConflict, branch)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, name) DO UPDATE SET sha = excluded.sha, updated_at = excluded.updated_at`,
		repoID, branch, KindBranch, sha, now)
	return err
}

// createTags creates each tag at sha. A tag that already names sha is left
// as it is; one that names another revision is a conflict.
func createTags(ctx context.Context, tx dbtx, repoID int64, tags []string, sha, now string) error {
	for _, t := range tags {
		var cur string
		err := tx.QueryRowContext(ctx, "SELECT sha FROM refs WHERE repo_id = ? AND name = ?", repoID, t).Scan(&cur)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO refs (repo_id, name, kind, sha, updated_at) VALUES (?, ?, ?, ?, ?)",
				repoID, t, KindTag, sha, now); err != nil {
				return err
			}
		case err != nil:
			return err
		case cur != sha:
			return fmt.Errorf("%w: %q already names revision %s", ErrConflict, t, cur)
		}
	}
	return nil
}
