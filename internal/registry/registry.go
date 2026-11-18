// Copyright 2026 Spectrum Labs LLC
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
	// ErrLocalRepo refuses to add an upstream revision, on a reader's
	// request, to a repository that was created in this registry.
	ErrLocalRepo = errors.New("repository was created in this registry, not from an upstream")
)

// schemaVersion is the database layout this version reads and writes.
const schemaVersion = 2

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
	// locks orders publishing a blob against removing it, one digest at a
	// time, so a blob can never be deleted between being written and being
	// recorded. Work on different digests never waits on each other.
	locks keyLock
	now   func() time.Time
	// dataDir holds the database and scratch space.
	dataDir string
}

// keyLock is a mutex per key.
type keyLock struct {
	mu sync.Mutex
	m  map[string]*keyEntry
}

type keyEntry struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until key is free and returns the function that releases it.
func (k *keyLock) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// Open opens the registry whose metadata lives in dataDir, creating it on
// first use. File content goes to store; nil means local files in dataDir.
func Open(dataDir string, store blob.Store) (*Registry, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if store == nil {
		fs, err := blob.NewFS(dataDir)
		if err != nil {
			return nil, err
		}
		store = fs
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dataDir, "registry.db")) +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	r := &Registry{db: db, blobs: store, now: time.Now, dataDir: dataDir}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The database holds every token's hash. SQLite creates it readable by
	// everyone the data directory lets in; the write-ahead log and its
	// index take their mode from the database, so they follow.
	path := filepath.Join(dataDir, "registry.db")
	ownerOnly(path, path+"-wal", path+"-shm")
	return r, nil
}

// ownerOnly makes each file that exists readable and writable by its owner
// alone. It is best effort: on Windows, where access is not expressed in
// mode bits, it changes nothing, and a failure is not worth refusing to
// start over.
func ownerOnly(paths ...string) {
	for _, p := range paths {
		_ = os.Chmod(p, 0o600)
	}
}

// Close releases the database.
func (r *Registry) Close() error { return r.db.Close() }

func (r *Registry) migrate() error {
	ctx := context.Background()
	var v int
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch v {
	case 0:
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schema+pendingSchema); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
			return err
		}
		return tx.Commit()
	case 1:
		// Version 2 adds the table for files a revision lists but has not
		// fetched yet. Nothing existing changes.
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, pendingSchema); err != nil {
			return fmt.Errorf("upgrade schema to version 2: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
			return err
		}
		return tx.Commit()
	case 2:
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

	defer r.locks.lock(info.SHA256)()
	if err := st.Publish(ctx); err != nil {
		return blob.Info{}, err
	}
	// Re-sending a blob refreshes its age, so garbage collection keeps its
	// hands off content a push is about to reference.
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO blobs (sha256, size, git_oid, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (sha256) DO UPDATE SET created_at = excluded.created_at`,
		info.SHA256, info.Size, info.GitOID, r.stamp())
	if err != nil {
		return blob.Info{}, fmt.Errorf("record blob: %w", err)
	}
	return info, nil
}

// TouchBlob marks a stored blob as just used. A pusher that finds a file
// already stored calls this instead of uploading, which protects the blob
// from garbage collection until the push records its revision.
func (r *Registry) TouchBlob(ctx context.Context, digest string) (blob.Info, bool, error) {
	if _, err := r.db.ExecContext(ctx, "UPDATE blobs SET created_at = ? WHERE sha256 = ?", r.stamp(), digest); err != nil {
		return blob.Info{}, false, err
	}
	return r.StatBlob(ctx, digest)
}

// PresignBlob returns a direct download URL for a blob when the storage
// backend can provide one. ok is false when it cannot.
func (r *Registry) PresignBlob(ctx context.Context, digest string, ttl time.Duration) (string, bool, error) {
	p, can := r.blobs.(blob.Presigner)
	if !can {
		return "", false, nil
	}
	u, err := p.PresignGet(ctx, digest, ttl)
	return u, err == nil, err
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

// FindMirroredBlob looks a blob up by the git blob id of its content, among
// the blobs that a mirrored revision already uses.
//
// A git blob id is a SHA-1, and it is how an upstream names the files it
// does not track with LFS. Content that merely has the right id is not
// trusted to stand in for an upstream file: it has to have come from an
// upstream itself. A blob that was only pushed, or is not referenced at
// all, is never returned.
func (r *Registry) FindMirroredBlob(ctx context.Context, oid string, size int64) (blob.Info, bool, error) {
	info := blob.Info{GitOID: oid, Size: size}
	err := r.db.QueryRowContext(ctx,
		`SELECT b.sha256 FROM blobs b WHERE b.git_oid = ? AND b.size = ?
		 AND EXISTS (SELECT 1 FROM files f JOIN commits c ON c.id = f.commit_id
		             WHERE f.sha256 = b.sha256 AND c.source = ?)
		 LIMIT 1`, oid, size, SourceMirror).
		Scan(&info.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return blob.Info{}, false, nil
	}
	return info, err == nil, err
}

// OpenBlob opens stored bytes for reading.
func (r *Registry) OpenBlob(ctx context.Context, digest string) (io.ReadSeekCloser, int64, error) {
	return r.blobs.Open(ctx, digest)
}

// GCResult reports what garbage collection removed.
type GCResult struct {
	Blobs int   `json:"blobs"`
	Bytes int64 `json:"bytes"`
}

// GC removes blobs that no revision references and that were last written
// or touched more than minAge ago. The age guard protects uploads whose
// revision has not been recorded yet.
func (r *Registry) GC(ctx context.Context, minAge time.Duration) (GCResult, error) {
	cutoff := r.now().Add(-minAge).UTC().Format(timeLayout)
	rows, err := r.db.QueryContext(ctx,
		`SELECT sha256 FROM blobs b
		 WHERE b.created_at <= ? AND NOT EXISTS (SELECT 1 FROM files f WHERE f.sha256 = b.sha256)`, cutoff)
	if err != nil {
		return GCResult{}, err
	}
	digests, err := scanStrings(rows)
	if err != nil {
		return GCResult{}, err
	}
	return r.dropUnreferenced(ctx, digests, cutoff)
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// dropUnreferenced removes each candidate blob that is still unreferenced
// and not newer than cutoff. The check and the removal of the record are
// one statement, made while holding the blob's lock, so a blob that gained
// a reference or was re-uploaded since it was picked is left alone.
func (r *Registry) dropUnreferenced(ctx context.Context, digests []string, cutoff string) (GCResult, error) {
	var res GCResult
	var firstErr error
	for _, d := range digests {
		err := func() error {
			defer r.locks.lock(d)()
			var size int64
			err := r.db.QueryRowContext(ctx,
				`DELETE FROM blobs WHERE sha256 = ? AND created_at <= ?
				 AND NOT EXISTS (SELECT 1 FROM files f WHERE f.sha256 = blobs.sha256)
				 RETURNING size`, d, cutoff).Scan(&size)
			if errors.Is(err, sql.ErrNoRows) {
				return nil // referenced or refreshed in the meantime
			}
			if err != nil {
				return fmt.Errorf("forget blob %s: %w", d, err)
			}
			res.Blobs++
			res.Bytes += size
			// The record is gone, so nothing can reference these bytes any
			// more. If removing them fails they are wasted space, never a
			// dangling reference.
			if err := r.blobs.Delete(ctx, d); err != nil {
				return fmt.Errorf("remove blob %s: %w", d, err)
			}
			return nil
		}()
		if err != nil && firstErr == nil {
			firstErr = err
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

// Token returns the record of the token called name, or ErrTokenNotFound.
func (r *Registry) Token(ctx context.Context, name string) (Token, error) {
	t := Token{Name: name}
	var created string
	err := r.db.QueryRowContext(ctx, "SELECT role, created_at FROM tokens WHERE name = ?", name).Scan(&t.Role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("%w: %s", ErrTokenNotFound, name)
	}
	if err != nil {
		return Token{}, err
	}
	t.CreatedAt = parseTime(created)
	return t, nil
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
