// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/model-delivery/registry/internal/blob"
)

// Backups protect the metadata database: the only record of which stored
// files make up which revision. Model files are not copied; they already
// live in the blob store under their digests and never change.
//
// A backup is a snapshot of the database taken while the registry runs,
// checked before it is kept, and stored next to the blobs (see
// blob.Archive). With object storage that puts it off the server's disk.

// ErrNoBackupStore reports that the storage backend cannot keep backups.
var ErrNoBackupStore = errors.New("this storage backend cannot keep backups")

// Backup describes one stored backup.
type Backup = blob.ArchiveInfo

const backupTimeLayout = "20060102T150405Z"

// SnapshotInfo is what a verified snapshot contains.
type SnapshotInfo struct {
	SchemaVersion int   `json:"schema_version"`
	Repos         int   `json:"repos"`
	Revisions     int   `json:"revisions"`
	Size          int64 `json:"size"`
}

// Snapshot writes a consistent copy of the metadata database to path,
// which must not exist. It is safe while the registry is serving: SQLite
// produces the copy from a single read transaction.
func (r *Registry) Snapshot(ctx context.Context, path string) (SnapshotInfo, error) {
	if _, err := r.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return SnapshotInfo{}, fmt.Errorf("snapshot the database: %w", err)
	}
	info, err := VerifySnapshot(ctx, path)
	if err != nil {
		_ = os.Remove(path)
		return SnapshotInfo{}, fmt.Errorf("the snapshot failed its check and was discarded: %w", err)
	}
	return info, nil
}

// VerifySnapshot opens a snapshot file, runs SQLite's integrity check on
// it, and reports what it holds. It refuses a file written by a newer
// version of the registry than this one.
func VerifySnapshot(ctx context.Context, path string) (SnapshotInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return SnapshotInfo{}, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer func() { _ = db.Close() }()

	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return SnapshotInfo{}, fmt.Errorf("not a readable database: %w", err)
	}
	if check != "ok" {
		return SnapshotInfo{}, fmt.Errorf("integrity check failed: %s", check)
	}
	info := SnapshotInfo{Size: fi.Size()}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&info.SchemaVersion); err != nil {
		return SnapshotInfo{}, err
	}
	if info.SchemaVersion < 1 || info.SchemaVersion > schemaVersion {
		return SnapshotInfo{}, fmt.Errorf("it has schema version %d; this version of the registry reads versions 1 to %d", info.SchemaVersion, schemaVersion)
	}
	if err := db.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM repos), (SELECT COUNT(*) FROM commits)").
		Scan(&info.Repos, &info.Revisions); err != nil {
		return SnapshotInfo{}, fmt.Errorf("not a registry database: %w", err)
	}
	return info, nil
}

// Scratch makes a new empty directory for a snapshot in progress. It is
// under the data directory, which is known to be writable, and the caller
// removes it.
func (r *Registry) Scratch() (string, error) {
	scratch := filepath.Join(r.dataDir, "tmp")
	if err := os.MkdirAll(scratch, 0o750); err != nil {
		return "", err
	}
	return os.MkdirTemp(scratch, "snapshot-*")
}

func (r *Registry) archive() (blob.Archive, error) {
	a, ok := r.blobs.(blob.Archive)
	if !ok {
		return nil, ErrNoBackupStore
	}
	return a, nil
}

// Backup takes a snapshot, stores it next to the blobs, and then removes
// the oldest stored backups beyond keep (0 keeps them all).
func (r *Registry) Backup(ctx context.Context, keep int) (Backup, SnapshotInfo, error) {
	store, err := r.archive()
	if err != nil {
		return Backup{}, SnapshotInfo{}, err
	}
	dir, err := r.Scratch()
	if err != nil {
		return Backup{}, SnapshotInfo{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "snapshot.db")
	info, err := r.Snapshot(ctx, path)
	if err != nil {
		return Backup{}, SnapshotInfo{}, err
	}
	now := r.now().UTC()
	name := "registry-" + now.Format(backupTimeLayout) + ".db"
	if err := store.PutArchive(ctx, name, path); err != nil {
		return Backup{}, SnapshotInfo{}, fmt.Errorf("store the backup: %w", err)
	}
	b := Backup{Name: name, Size: info.Size, ModTime: now}

	if keep > 0 {
		all, err := store.ListArchives(ctx)
		if err != nil {
			return b, info, fmt.Errorf("the backup was stored, but old ones could not be listed for removal: %w", err)
		}
		// Names sort by time, so the oldest come first. The one just written
		// is never among those removed.
		for i := 0; i < len(all)-keep; i++ {
			if all[i].Name == name {
				continue
			}
			if err := store.DeleteArchive(ctx, all[i].Name); err != nil {
				return b, info, fmt.Errorf("the backup was stored, but removing old backup %s failed: %w", all[i].Name, err)
			}
		}
	}
	return b, info, nil
}

// Backups lists stored backups, oldest first.
func (r *Registry) Backups(ctx context.Context) ([]Backup, error) {
	store, err := r.archive()
	if err != nil {
		return nil, err
	}
	return store.ListArchives(ctx)
}

// Restore installs a snapshot as the metadata database of dataDir, for a
// registry that is not running. The snapshot is verified first. If a
// database is already there it is refused unless replace is set, and then
// the old one is kept beside the new one under the returned name.
func Restore(ctx context.Context, dataDir, snapshot string, replace bool) (info SnapshotInfo, previous string, err error) {
	info, err = VerifySnapshot(ctx, snapshot)
	if err != nil {
		return SnapshotInfo{}, "", fmt.Errorf("%s cannot be restored: %w", snapshot, err)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return SnapshotInfo{}, "", err
	}
	target := filepath.Join(dataDir, "registry.db")
	if _, statErr := os.Stat(target); statErr == nil {
		if !replace {
			return SnapshotInfo{}, "", fmt.Errorf("%w: %s already holds a database; pass --replace to put it aside and restore over it", ErrConflict, dataDir)
		}
		previous = target + ".before-restore-" + time.Now().UTC().Format(backupTimeLayout)
		if err := os.Rename(target, previous); err != nil {
			return SnapshotInfo{}, "", fmt.Errorf("put the existing database aside: %w", err)
		}
	}
	// A leftover write-ahead log belongs to the database that was just
	// moved away, and would be replayed onto the restored one.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(target + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return SnapshotInfo{}, previous, err
		}
	}
	if err := copyFile(snapshot, target); err != nil {
		return SnapshotInfo{}, previous, fmt.Errorf("install the snapshot: %w", err)
	}
	return info, previous, nil
}

// FetchBackup downloads a stored backup to path. An empty name means the
// newest. It returns the name that was fetched.
func FetchBackup(ctx context.Context, store blob.Store, name, path string) (string, error) {
	a, ok := store.(blob.Archive)
	if !ok {
		return "", ErrNoBackupStore
	}
	if name == "" {
		all, err := a.ListArchives(ctx)
		if err != nil {
			return "", err
		}
		if len(all) == 0 {
			return "", fmt.Errorf("%w: the storage holds no backups", blob.ErrArchiveNotFound)
		}
		name = all[len(all)-1].Name
	}
	rc, err := a.OpenArchive(ctx, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a path the caller chose.
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		_ = os.Remove(path)
		return "", err
	}
	return name, out.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // a snapshot path the operator named.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".restoring"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // inside the data directory.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
