// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A followed repository is one the registry keeps up to date with its
// upstream: on a schedule, or when asked, the server checks what the
// followed refs point at upstream and mirrors what has moved. This file is
// the durable half of that, the follow list and how each entry's checks
// have gone; the checking itself is the server's (internal/server/sync.go)
// and the mirroring internal/mirror's.
//
// Only a repository that came from an upstream can be followed. A sync adds
// upstream revisions to the repository, and a repository created here by a
// push or an upload never receives upstream content, the rule pull-through
// obeys too.

// followsSchema is version 4: the follow list. One row per followed
// repository, removed with it. A row follows either one branch or every
// branch and tag, never both and never neither. The times are empty until
// the first check; error and error_at hold the last failure until a check
// succeeds, and failures counts the checks that have failed in a row.
const followsSchema = `
CREATE TABLE follows (
  repo_id      INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  branch       TEXT NOT NULL DEFAULT '',
  all_refs     INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL,
  checked_at   TEXT NOT NULL DEFAULT '',
  succeeded_at TEXT NOT NULL DEFAULT '',
  changed_at   TEXT NOT NULL DEFAULT '',
  error        TEXT NOT NULL DEFAULT '',
  error_at     TEXT NOT NULL DEFAULT '',
  failures     INTEGER NOT NULL DEFAULT 0,
  CHECK ((all_refs = 1) = (branch = ''))
);
`

// ErrNotFollowed reports that a repository is not on the follow list.
var ErrNotFollowed = errors.New("repository is not followed")

// FollowSpec says which upstream refs of a repository are followed: one
// branch, or every branch and tag.
type FollowSpec struct {
	// Branch is the upstream branch followed. Empty with AllRefs.
	Branch string
	// AllRefs follows every branch and every tag the upstream has.
	AllRefs bool
}

// validate checks that the spec names one branch or all refs.
func (s FollowSpec) validate() error {
	switch {
	case s.AllRefs && s.Branch != "":
		return fmt.Errorf("%w: follow one branch or every branch and tag, not both", ErrInvalid)
	case s.AllRefs:
		return nil
	case s.Branch == "":
		return fmt.Errorf("%w: name the branch to follow, or follow every branch and tag", ErrInvalid)
	}
	return ValidateRefName(s.Branch)
}

// Follow is a repository on the follow list and how its checks have gone.
type Follow struct {
	Name string `json:"name"`
	// Type is absent from the document for a model.
	Type RepoType `json:"type,omitzero"`
	// Branch is the followed branch; empty when AllRefs is set.
	Branch    string    `json:"branch,omitempty"`
	AllRefs   bool      `json:"all_refs,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// LastCheck is when the upstream was last asked, whatever came of it;
	// LastSuccess when that last went well; LastChange when a check last
	// moved or added a ref. Each is absent until it has happened.
	LastCheck   time.Time `json:"last_check,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastChange  time.Time `json:"last_change,omitzero"`
	// LastError is why the last check failed, and LastErrorAt when; both
	// are cleared by a check that succeeds.
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	// Failures is how many checks in a row have failed.
	Failures int `json:"failures,omitempty"`
}

// Key is the followed repository's key (RepoKey).
func (f Follow) Key() string { return RepoKey(f.Type, f.Name) }

// Spec is what the entry follows.
func (f Follow) Spec() FollowSpec { return FollowSpec{Branch: f.Branch, AllRefs: f.AllRefs} }

// Follow puts the repository with key key on the follow list, or changes
// what it follows if it is already there; how its checks have gone is kept.
// The repository must exist and have come from an upstream: one created
// here is refused with ErrLocalRepo. A branch that is a tag here is refused
// with ErrConflict, since a tag never moves.
func (r *Registry) Follow(ctx context.Context, key string, spec FollowSpec) (Follow, error) {
	if err := ValidateRepoKey(key); err != nil {
		return Follow{}, err
	}
	if err := spec.validate(); err != nil {
		return Follow{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Follow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	typ, name := SplitRepoKey(key)
	var id int64
	var upstream string
	err = tx.QueryRowContext(ctx, "SELECT id, upstream FROM repos WHERE type = ? AND name = ?", typ, name).Scan(&id, &upstream)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Follow{}, fmt.Errorf("%w: %s", ErrRepoNotFound, key)
	case err != nil:
		return Follow{}, err
	case upstream == "":
		return Follow{}, fmt.Errorf("%w: %s; only a repository created by mirroring or pull-through can be followed", ErrLocalRepo, key)
	}
	if spec.Branch != "" {
		var kind RefKind
		err := tx.QueryRowContext(ctx, "SELECT kind FROM refs WHERE repo_id = ? AND name = ?", id, spec.Branch).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return Follow{}, err
		case kind == KindTag:
			return Follow{}, fmt.Errorf("%w: %q is a tag in %s, and a tag never moves; follow a branch, or every branch and tag", ErrConflict, spec.Branch, key)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO follows (repo_id, branch, all_refs, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (repo_id) DO UPDATE SET branch = excluded.branch, all_refs = excluded.all_refs`,
		id, spec.Branch, spec.AllRefs, r.stamp()); err != nil {
		return Follow{}, err
	}
	if err := tx.Commit(); err != nil {
		return Follow{}, err
	}
	return r.GetFollow(ctx, key)
}

// Unfollow takes the repository with key key off the follow list. Nothing
// it holds changes. It fails with ErrNotFollowed if it was not on it.
func (r *Registry) Unfollow(ctx context.Context, key string) error {
	typ, name := SplitRepoKey(key)
	res, err := r.db.ExecContext(ctx,
		"DELETE FROM follows WHERE repo_id = (SELECT id FROM repos WHERE type = ? AND name = ?)", typ, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFollowed, key)
	}
	return nil
}

const followCols = `SELECT r.name, r.type, f.branch, f.all_refs, f.created_at, f.checked_at, f.succeeded_at, f.changed_at,
	f.error, f.error_at, f.failures FROM follows f JOIN repos r ON r.id = f.repo_id`

func scanFollow(row rowScanner) (Follow, error) {
	var f Follow
	var created, checked, succeeded, changed, errorAt string
	if err := row.Scan(&f.Name, &f.Type, &f.Branch, &f.AllRefs, &created, &checked, &succeeded, &changed,
		&f.LastError, &errorAt, &f.Failures); err != nil {
		return Follow{}, err
	}
	var err error
	if f.CreatedAt, err = parseTime(created); err != nil {
		return Follow{}, err
	}
	for _, t := range []struct {
		dst *time.Time
		src string
	}{{&f.LastCheck, checked}, {&f.LastSuccess, succeeded}, {&f.LastChange, changed}, {&f.LastErrorAt, errorAt}} {
		if t.src == "" {
			continue
		}
		if *t.dst, err = parseTime(t.src); err != nil {
			return Follow{}, err
		}
	}
	return f, nil
}

// GetFollow returns the follow list's entry for the repository with key
// key, or ErrNotFollowed.
func (r *Registry) GetFollow(ctx context.Context, key string) (Follow, error) {
	typ, name := SplitRepoKey(key)
	f, err := scanFollow(r.db.QueryRowContext(ctx, followCols+" WHERE r.type = ? AND r.name = ?", typ, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Follow{}, fmt.Errorf("%w: %s", ErrNotFollowed, key)
	}
	return f, err
}

// Follows lists the follow list by name, a model before the dataset of the
// same name.
func (r *Registry) Follows(ctx context.Context) ([]Follow, error) {
	rows, err := r.db.QueryContext(ctx, followCols+" ORDER BY r.name, r.type = 'dataset'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Follow{}
	for rows.Next() {
		f, err := scanFollow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SyncRecord is how one check of a followed repository went.
type SyncRecord struct {
	// At is when the check was made, by the clock of whoever made it.
	At time.Time
	// Changed is set when the check moved or added a ref.
	Changed bool
	// Error is why the check failed; empty for one that succeeded.
	Error string
}

// RecordSync records a check of the followed repository with key key. A
// repository taken off the follow list while it was being checked is not
// put back, and that is not an error.
func (r *Registry) RecordSync(ctx context.Context, key string, rec SyncRecord) error {
	typ, name := SplitRepoKey(key)
	at := rec.At.UTC().Format(timeLayout)
	const which = " WHERE repo_id = (SELECT id FROM repos WHERE type = ? AND name = ?)"
	var err error
	switch {
	case rec.Error != "":
		_, err = r.db.ExecContext(ctx,
			"UPDATE follows SET checked_at = ?, error = ?, error_at = ?, failures = failures + 1"+which,
			at, rec.Error, at, typ, name)
	case rec.Changed:
		_, err = r.db.ExecContext(ctx,
			"UPDATE follows SET checked_at = ?, succeeded_at = ?, changed_at = ?, error = '', error_at = '', failures = 0"+which,
			at, at, at, typ, name)
	default:
		_, err = r.db.ExecContext(ctx,
			"UPDATE follows SET checked_at = ?, succeeded_at = ?, error = '', error_at = '', failures = 0"+which,
			at, at, typ, name)
	}
	return err
}
