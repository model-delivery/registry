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
	"github.com/model-delivery/registry/internal/datadir"
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

// SQLite keeps a database's write-ahead log and that log's index in two
// files beside it, named after it with these suffixes.
const (
	walSuffix = "-wal"
	shmSuffix = "-shm"
)

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
	// backupMu lets one backup be taken at a time (see Backup).
	backupMu sync.Mutex
	now      func() time.Time
	// dir holds the database and scratch space.
	dir datadir.Dir
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
	dir := datadir.Dir(dataDir)
	dsn := "file:" + filepath.ToSlash(dir.Database()) +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	r := &Registry{db: db, blobs: store, now: time.Now, dir: dir}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The database holds every token's hash. SQLite creates it readable by
	// everyone the data directory lets in; the write-ahead log and its
	// index take their mode from the database, so they follow.
	ownerOnly(dir.Database(), dir.Database()+walSuffix, dir.Database()+shmSuffix)
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

// migrate brings the database to schemaVersion. Each step adds to what
// the one before it left; nothing existing is ever rewritten.
func (r *Registry) migrate() error {
	ctx := context.Background()
	var v int
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if v == schemaVersion {
		return nil
	}
	if v > schemaVersion {
		return fmt.Errorf("data directory was written by a newer version (schema %d)", v)
	}
	// What each version added, in order. Index i takes a database from
	// version i to version i+1.
	steps := [schemaVersion]string{
		schema,
		// Version 2 adds the table for files a revision lists but has not
		// fetched yet.
		pendingSchema,
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for from := v; from < schemaVersion; from++ {
		if _, err := tx.ExecContext(ctx, steps[from]); err != nil {
			return fmt.Errorf("upgrade schema to version %d: %w", from+1, err)
		}
	}
	// A pragma takes no bound parameter; the value is this package's constant.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// clock reads the time once, as the value and as the text stored for it,
// so what a call returns and what it wrote are the same instant.
func (r *Registry) clock() (time.Time, string) {
	t := r.now().UTC().Round(0)
	return t, t.Format(timeLayout)
}

func (r *Registry) stamp() string {
	_, s := r.clock()
	return s
}

// parseTime reads a timestamp this package stored. One that does not parse
// means the database holds something this package did not write; that is
// reported, not turned into the year 1.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("the database holds a timestamp that cannot be read, %q: %w", s, err)
	}
	return t, nil
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

// Role is what a token allows. The zero value allows nothing.
type Role string

// Roles, weakest first. Each allows everything the one before it does.
const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	RoleAdmin Role = "admin"
)

// rank orders the roles; 0 is no role at all.
func (r Role) rank() int {
	switch r {
	case RoleRead:
		return 1
	case RoleWrite:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// Valid reports whether r is one of the roles.
func (r Role) Valid() bool { return r.rank() > 0 }

// Allows reports whether a token with role r may do what needs at least
// minimum. No role, and a name that is not a role, allow nothing.
func (r Role) Allows(minimum Role) bool {
	return r.Valid() && minimum.Valid() && r.rank() >= minimum.rank()
}

// Limits on what a token may be called and how short its secret may be.
const (
	maxTokenName    = 64
	minTokenSecret  = 16
	tokenSecretSize = 24 // random bytes in a minted secret, before hex
)

// Token is an access token's record. The secret itself is never stored.
type Token struct {
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken mints a token and returns its secret, which is shown once.
func (r *Registry) CreateToken(ctx context.Context, name string, role Role) (string, error) {
	buf := make([]byte, tokenSecretSize)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := "md_" + hex.EncodeToString(buf)
	return secret, r.SetToken(ctx, name, role, secret, false)
}

// SetToken records secret under name. With replace, an existing token of
// that name is overwritten; this is how a configured admin token is applied.
func (r *Registry) SetToken(ctx context.Context, name string, role Role, secret string, replace bool) error {
	if name == "" || len(name) > maxTokenName {
		return fmt.Errorf("%w: token name must be 1-%d characters", ErrInvalid, maxTokenName)
	}
	if !role.Valid() {
		return fmt.Errorf("%w: role must be read, write or admin", ErrInvalid)
	}
	if len(secret) < minTokenSecret {
		return fmt.Errorf("%w: token must be at least %d characters", ErrInvalid, minTokenSecret)
	}
	hash := hashToken(secret)
	if replace {
		_, err := r.db.ExecContext(ctx,
			`INSERT INTO tokens (name, hash, role, created_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (name) DO UPDATE SET hash = excluded.hash, role = excluded.role`,
			name, hash, role, r.stamp())
		return err
	}
	// A name or a secret that is already taken inserts nothing. Any other
	// failure is the database's and is returned as it is: a busy or broken
	// database is not "already exists".
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO tokens (name, hash, role, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		name, hash, role, r.stamp())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	var named bool
	if err := r.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM tokens WHERE name = ?)", name).Scan(&named); err != nil {
		return err
	}
	if named {
		return fmt.Errorf("%w: a token named %q already exists", ErrConflict, name)
	}
	return fmt.Errorf("%w: that secret is already another token's; choose a different one for %q", ErrConflict, name)
}

// Authenticate returns the token record for a presented secret. ok is
// false when no token has that secret, which is not an error; an error
// means the database could not answer, and says nothing about the secret.
func (r *Registry) Authenticate(ctx context.Context, secret string) (tok Token, ok bool, err error) {
	if secret == "" {
		return Token{}, false, nil
	}
	var t Token
	var created string
	err = r.db.QueryRowContext(ctx, "SELECT name, role, created_at FROM tokens WHERE hash = ?", hashToken(secret)).
		Scan(&t.Name, &t.Role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, err
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return Token{}, false, err
	}
	return t, true, nil
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
	if t.CreatedAt, err = parseTime(created); err != nil {
		return Token{}, err
	}
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
		if t.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
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
