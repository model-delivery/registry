// SPDX-License-Identifier: Apache-2.0

// Package registry is the model registry itself: repositories, immutable
// revisions, movable refs, access tokens and the blobs they point at.
// Metadata lives in SQLite; bytes live in a blob.Store.
package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite".

	"github.com/model-delivery/registry/internal/blob"
)

// Errors callers can match with errors.Is.
var (
	ErrRepoNotFound     = errors.New("repository not found")
	ErrRevisionNotFound = errors.New("revision not found")
	ErrEntryNotFound    = errors.New("file not found")
	ErrInvalid          = errors.New("invalid request")
	ErrConflict         = errors.New("conflict")
	ErrBlobMissing      = errors.New("blob has not been uploaded")
	ErrTokenNotFound    = errors.New("token not found")
)

const timeLayout = "2006-01-02T15:04:05.000000000Z"

const schema = `
CREATE TABLE repos (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  upstream   TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE blobs (
  sha256     TEXT PRIMARY KEY,
  size       INTEGER NOT NULL,
  git_oid    TEXT NOT NULL,
  created_at TEXT NOT NULL
) WITHOUT ROWID;
CREATE INDEX blobs_git_oid ON blobs(git_oid);
CREATE TABLE commits (
  id            INTEGER PRIMARY KEY,
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  sha           TEXT NOT NULL,
  parent        TEXT NOT NULL DEFAULT '',
  tree          TEXT NOT NULL,
  message       TEXT NOT NULL DEFAULT '',
  source        TEXT NOT NULL,
  upstream_info TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  UNIQUE (repo_id, sha)
);
CREATE TABLE files (
  commit_id INTEGER NOT NULL REFERENCES commits(id) ON DELETE CASCADE,
  path      TEXT NOT NULL,
  size      INTEGER NOT NULL,
  sha256    TEXT NOT NULL REFERENCES blobs(sha256),
  git_oid   TEXT NOT NULL,
  lfs       INTEGER NOT NULL,
  PRIMARY KEY (commit_id, path)
) WITHOUT ROWID;
CREATE INDEX files_sha256 ON files(sha256);
CREATE TABLE refs (
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  kind       TEXT NOT NULL,
  sha        TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, name)
) WITHOUT ROWID;
CREATE TABLE tokens (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  hash       TEXT NOT NULL UNIQUE,
  role       TEXT NOT NULL,
  created_at TEXT NOT NULL
);
`

// Registry is safe for concurrent use by one process. Running two processes
// against the same data directory is not supported.
type Registry struct {
	db    *sql.DB
	blobs blob.Store
	// blobMu orders blob publication against blob removal: publishers hold it
	// shared, garbage collection holds it exclusively, so a blob can never be
	// deleted between being written and being recorded.
	blobMu sync.RWMutex
	now    func() time.Time
}

// Open opens the registry stored in dataDir, creating it on first use.
func Open(dataDir string) (*Registry, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	store, err := blob.NewFS(dataDir)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dataDir, "registry.db")) +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	r := &Registry{db: db, blobs: store, now: time.Now}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

// Close releases the database.
func (r *Registry) Close() error { return r.db.Close() }

func (r *Registry) migrate() error {
	var v int
	if err := r.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch v {
	case 0:
		tx, err := r.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		if _, err := tx.Exec("PRAGMA user_version = 1"); err != nil {
			return err
		}
		return tx.Commit()
	case 1:
		return nil
	default:
		return fmt.Errorf("data directory was written by a newer version (schema %d)", v)
	}
}

func (r *Registry) stamp() string { return r.now().UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
	return t
}

// ---- blobs ----

// PutBlob stores the bytes from rd after verifying them against want.
// Storing bytes that are already present is a cheap no-op.
func (r *Registry) PutBlob(ctx context.Context, rd io.Reader, want blob.Expect) (blob.Info, error) {
	st, err := r.blobs.Stage(ctx, rd, want)
	if err != nil {
		return blob.Info{}, err
	}
	defer st.Discard()
	info := st.Info()

	r.blobMu.RLock()
	defer r.blobMu.RUnlock()
	if err := st.Publish(); err != nil {
		return blob.Info{}, err
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO blobs (sha256, size, git_oid, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (sha256) DO NOTHING`,
		info.SHA256, info.Size, info.GitOID, r.stamp())
	if err != nil {
		return blob.Info{}, fmt.Errorf("record blob: %w", err)
	}
	return info, nil
}

// StatBlob reports whether a blob is stored, and its details if so.
func (r *Registry) StatBlob(ctx context.Context, digest string) (blob.Info, bool, error) {
	info := blob.Info{SHA256: digest}
	err := r.db.QueryRowContext(ctx, "SELECT size, git_oid FROM blobs WHERE sha256 = ?", digest).
		Scan(&info.Size, &info.GitOID)
	if errors.Is(err, sql.ErrNoRows) {
		return blob.Info{}, false, nil
	}
	return info, err == nil, err
}

// FindBlobByGitOID looks a blob up by the git blob id of its content.
func (r *Registry) FindBlobByGitOID(ctx context.Context, oid string, size int64) (blob.Info, bool, error) {
	info := blob.Info{GitOID: oid, Size: size}
	err := r.db.QueryRowContext(ctx, "SELECT sha256 FROM blobs WHERE git_oid = ? AND size = ? LIMIT 1", oid, size).
		Scan(&info.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return blob.Info{}, false, nil
	}
	return info, err == nil, err
}

// OpenBlob opens stored bytes for reading.
func (r *Registry) OpenBlob(digest string) (blob.File, int64, error) {
	return r.blobs.Open(digest)
}

// GCResult reports what garbage collection removed.
type GCResult struct {
	Blobs int   `json:"blobs"`
	Bytes int64 `json:"bytes"`
}

// GC removes blobs that no revision references and that are older than
// minAge. The age guard protects uploads whose commit has not arrived yet.
func (r *Registry) GC(ctx context.Context, minAge time.Duration) (GCResult, error) {
	cutoff := r.now().Add(-minAge).UTC().Format(timeLayout)
	return r.removeBlobs(ctx, func(tx *sql.Tx) (*sql.Rows, error) {
		return tx.QueryContext(ctx,
			`SELECT sha256, size FROM blobs b
			 WHERE b.created_at <= ? AND NOT EXISTS (SELECT 1 FROM files f WHERE f.sha256 = b.sha256)`, cutoff)
	}, nil)
}

// removeBlobs deletes the blob rows selected by pick (run inside one write
// transaction, after before if given) and then the bytes behind them.
func (r *Registry) removeBlobs(ctx context.Context, pick func(*sql.Tx) (*sql.Rows, error), before func(*sql.Tx) error) (GCResult, error) {
	r.blobMu.Lock()
	defer r.blobMu.Unlock()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return GCResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := pick(tx)
	if err != nil {
		return GCResult{}, err
	}
	var res GCResult
	var digests []string
	for rows.Next() {
		var d string
		var size int64
		if err := rows.Scan(&d, &size); err != nil {
			_ = rows.Close()
			return GCResult{}, err
		}
		digests = append(digests, d)
		res.Blobs++
		res.Bytes += size
	}
	if err := rows.Close(); err != nil {
		return GCResult{}, err
	}
	if err := rows.Err(); err != nil {
		return GCResult{}, err
	}
	if before != nil {
		if err := before(tx); err != nil {
			return GCResult{}, err
		}
	}
	for _, d := range digests {
		if _, err := tx.ExecContext(ctx, "DELETE FROM blobs WHERE sha256 = ?", d); err != nil {
			return GCResult{}, fmt.Errorf("forget blob %s: %w", d, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return GCResult{}, err
	}
	// Rows are gone, so nothing can reference these any more. A file that
	// fails to delete is only wasted space, never a dangling reference.
	var firstErr error
	for _, d := range digests {
		if err := r.blobs.Delete(d); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove blob %s: %w", d, err)
		}
	}
	return res, firstErr
}

// ---- tokens ----

// Roles, weakest first.
const (
	RoleRead  = "read"
	RoleWrite = "write"
	RoleAdmin = "admin"
)

// ValidRole reports whether s names a role.
func ValidRole(s string) bool { return s == RoleRead || s == RoleWrite || s == RoleAdmin }

// Token is an access token's record. The secret itself is never stored.
type Token struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken mints a token and returns its secret, which is shown once.
func (r *Registry) CreateToken(ctx context.Context, name, role string) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := "md_" + hex.EncodeToString(buf)
	return secret, r.SetToken(ctx, name, role, secret, false)
}

// SetToken records secret under name. With replace, an existing token of
// that name is overwritten; this is how a configured admin token is applied.
func (r *Registry) SetToken(ctx context.Context, name, role, secret string, replace bool) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("%w: token name must be 1-64 characters", ErrInvalid)
	}
	if !ValidRole(role) {
		return fmt.Errorf("%w: role must be read, write or admin", ErrInvalid)
	}
	if len(secret) < 16 {
		return fmt.Errorf("%w: token must be at least 16 characters", ErrInvalid)
	}
	q := `INSERT INTO tokens (name, hash, role, created_at) VALUES (?, ?, ?, ?)`
	if replace {
		q += ` ON CONFLICT (name) DO UPDATE SET hash = excluded.hash, role = excluded.role`
	}
	if _, err := r.db.ExecContext(ctx, q, name, hashToken(secret), role, r.stamp()); err != nil {
		if !replace {
			return fmt.Errorf("%w: a token named %q already exists", ErrConflict, name)
		}
		return err
	}
	return nil
}

// Authenticate returns the token record for a presented secret.
func (r *Registry) Authenticate(ctx context.Context, secret string) (Token, bool) {
	if secret == "" {
		return Token{}, false
	}
	var t Token
	var created string
	err := r.db.QueryRowContext(ctx, "SELECT name, role, created_at FROM tokens WHERE hash = ?", hashToken(secret)).
		Scan(&t.Name, &t.Role, &created)
	if err != nil {
		return Token{}, false
	}
	t.CreatedAt = parseTime(created)
	return t, true
}

// ListTokens returns every token record, oldest first.
func (r *Registry) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT name, role, created_at FROM tokens ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Token{}
	for rows.Next() {
		var t Token
		var created string
		if err := rows.Scan(&t.Name, &t.Role, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(created)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken deletes a token by name.
func (r *Registry) RevokeToken(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM tokens WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrTokenNotFound, name)
	}
	return nil
}

// ---- stats ----

// Stats summarises what the registry holds. LogicalBytes counts every file
// in every revision; StoredBytes counts each distinct blob once.
type Stats struct {
	Repos        int   `json:"repos"`
	Revisions    int   `json:"revisions"`
	Blobs        int   `json:"blobs"`
	StoredBytes  int64 `json:"stored_bytes"`
	LogicalBytes int64 `json:"logical_bytes"`
}

// Stats returns registry-wide totals.
func (r *Registry) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := r.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM repos),
		(SELECT COUNT(*) FROM commits),
		(SELECT COUNT(*) FROM blobs),
		(SELECT COALESCE(SUM(size), 0) FROM blobs),
		(SELECT COALESCE(SUM(size), 0) FROM files)`).
		Scan(&s.Repos, &s.Revisions, &s.Blobs, &s.StoredBytes, &s.LogicalBytes)
	return s, err
}
