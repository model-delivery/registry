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
	"sort"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/datadir"
)

// Backups protect the metadata database: the only record of which stored
// files make up which revision. Model files are not copied; they already
// live in the blob store under their digests and never change.
//
// A backup is a snapshot of the database taken while the registry runs,
// checked before it is kept, and stored next to the blobs (see
// blob.Archive). With object storage that puts it off the server's disk.

// Backup describes one stored backup.
type Backup = blob.ArchiveInfo

// backupTimeLayout is the time in a backup's name, to the second, in UTC.
// It sorts as text in time order, which is what "oldest first" relies on.
const backupTimeLayout = "20060102T150405Z"

// BackupName is the file name of a backup or snapshot taken at t.
func BackupName(t time.Time) string {
	return "registry-" + t.UTC().Format(backupTimeLayout) + ".db"
}

// laterBackupName is the name of the nth backup taken in the same second
// as t, for n of 2 and up. The suffix sorts after the plain name, so the
// later backup still lists later.
func laterBackupName(t time.Time, n int) string {
	return fmt.Sprintf("registry-%s_%d.db", t.UTC().Format(backupTimeLayout), n)
}

// SnapshotInfo is what a verified snapshot contains.
type SnapshotInfo struct {
	SchemaVersion int   `json:"schema_version"`
	Repos         int   `json:"repos"`
	Revisions     int   `json:"revisions"`
	Size          int64 `json:"size"`
}

// snapshotFile is the name of a snapshot inside its scratch directory.
const snapshotFile = "snapshot.db"

// snapshot writes a consistent copy of the metadata database to path,
// which must not exist. It is safe while the registry is serving: SQLite
// produces the copy from a single read transaction.
func (r *Registry) snapshot(ctx context.Context, path string) (SnapshotInfo, error) {
	if _, err := r.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return SnapshotInfo{}, fmt.Errorf("snapshot the database: %w", err)
	}
	ownerOnly(path) // a snapshot holds the same token hashes as the database
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

// scratch makes a new empty directory in the data directory's staging
// space, which is known to be writable. The caller removes it.
func scratch(dir datadir.Dir, pattern string) (string, error) {
	if err := os.MkdirAll(dir.Staging(), 0o750); err != nil {
		return "", err
	}
	return os.MkdirTemp(dir.Staging(), pattern)
}

// scratchSnapshot takes a snapshot into a scratch directory of its own and
// returns that directory, which the caller removes, with the snapshot's
// path inside it.
func (r *Registry) scratchSnapshot(ctx context.Context) (dir, path string, info SnapshotInfo, err error) {
	dir, err = scratch(r.dir, "snapshot-*")
	if err != nil {
		return "", "", SnapshotInfo{}, err
	}
	path = filepath.Join(dir, snapshotFile)
	if info, err = r.snapshot(ctx, path); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", SnapshotInfo{}, err
	}
	return dir, path, info, nil
}

// openSnapshot is a snapshot being read. Closing it removes it.
type openSnapshot struct {
	*os.File
	dir string
}

func (s *openSnapshot) Close() error {
	err := s.File.Close()
	if rmErr := os.RemoveAll(s.dir); err == nil {
		err = rmErr
	}
	return err
}

// OpenSnapshot takes a fresh, verified snapshot of the metadata database
// and returns it for reading, with what it contains. The snapshot is a
// temporary file; Close removes it, so the caller must call Close.
func (r *Registry) OpenSnapshot(ctx context.Context) (io.ReadSeekCloser, SnapshotInfo, error) {
	dir, path, info, err := r.scratchSnapshot(ctx)
	if err != nil {
		return nil, SnapshotInfo{}, err
	}
	f, err := os.Open(path) //nolint:gosec // a file this function just created.
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, SnapshotInfo{}, err
	}
	return &openSnapshot{File: f, dir: dir}, info, nil
}

// BackupResult is a backup that was taken and stored.
type BackupResult struct {
	Backup   Backup
	Contents SnapshotInfo
	// PruneErr is why older backups beyond the number to keep could not be
	// removed, if they could not. The backup itself is stored either way:
	// this is something to report, not a failed backup.
	PruneErr error
}

// Backup takes a snapshot, stores it next to the blobs, and then removes
// the oldest stored backups beyond keep (0 keeps them all).
//
// An error means no backup was stored. A backup that was stored but could
// not be followed by the removal of old ones is a result, with PruneErr
// set.
func (r *Registry) Backup(ctx context.Context, keep int) (BackupResult, error) {
	// One at a time: a backup's name is chosen from the names already
	// stored, and two choosing at once would choose the same one.
	r.backupMu.Lock()
	defer r.backupMu.Unlock()

	dir, path, info, err := r.scratchSnapshot(ctx)
	if err != nil {
		return BackupResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	stored, err := r.blobs.ListArchives(ctx)
	if err != nil {
		return BackupResult{}, fmt.Errorf("list the backups already stored: %w", err)
	}
	now := r.now().UTC()
	name := freeBackupName(now, stored)
	if err := r.blobs.PutArchive(ctx, name, path); err != nil {
		return BackupResult{}, fmt.Errorf("store the backup: %w", err)
	}
	res := BackupResult{Backup: Backup{Name: name, Size: info.Size, ModTime: now}, Contents: info}
	if keep > 0 {
		res.PruneErr = r.pruneBackups(ctx, append(stored, res.Backup), name, keep)
	}
	return res, nil
}

// freeBackupName is the name for a backup taken at t that no stored backup
// has. Names carry the time to the second; a second backup in the same
// second gets a numbered suffix instead of replacing the first.
func freeBackupName(t time.Time, stored []Backup) string {
	taken := make(map[string]bool, len(stored))
	for _, b := range stored {
		taken[b.Name] = true
	}
	name := BackupName(t)
	for n := 2; taken[name]; n++ {
		name = laterBackupName(t, n)
	}
	return name
}

// pruneBackups removes the oldest of all beyond keep, never the one just
// written. It tries every removal and returns what failed.
func (r *Registry) pruneBackups(ctx context.Context, all []Backup, justWritten string, keep int) error {
	// Names sort by time, so the oldest come first.
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	var failed []error
	for i := 0; i < len(all)-keep; i++ {
		if all[i].Name == justWritten {
			continue
		}
		if err := r.blobs.DeleteArchive(ctx, all[i].Name); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// Backups lists stored backups, oldest first.
func (r *Registry) Backups(ctx context.Context) ([]Backup, error) {
	return r.blobs.ListArchives(ctx)
}

// Restore installs a snapshot as the metadata database of dataDir, for a
// registry that is not running. The snapshot is verified first. If a
// database is already there it is refused unless replace is set, and then
// the old one is kept beside the new one, with its write-ahead log, under
// the returned name.
//
// On an error nothing has changed: a database that was put aside is put
// back.
func Restore(ctx context.Context, dataDir, snapshot string, replace bool) (SnapshotInfo, string, error) {
	return restore(ctx, datadir.Dir(dataDir), snapshot, replace, time.Now)
}

func restore(ctx context.Context, dir datadir.Dir, snapshot string, replace bool, now func() time.Time) (SnapshotInfo, string, error) {
	info, err := VerifySnapshot(ctx, snapshot)
	if err != nil {
		return SnapshotInfo{}, "", fmt.Errorf("%s cannot be restored: %w", snapshot, err)
	}
	if err := os.MkdirAll(string(dir), 0o750); err != nil {
		return SnapshotInfo{}, "", err
	}
	target := dir.Database()
	previous := ""
	if _, statErr := os.Stat(target); statErr == nil {
		if !replace {
			return SnapshotInfo{}, "", fmt.Errorf("%w: %s already holds a database; pass --replace to put it aside and restore over it", ErrConflict, dir)
		}
		previous = freePath(target + ".before-restore-" + now().UTC().Format(backupTimeLayout))
		if err := moveDatabase(target, previous); err != nil {
			return SnapshotInfo{}, "", fmt.Errorf("put the existing database aside: %w", err)
		}
	} else {
		// A write-ahead log with no database beside it belongs to nothing,
		// and would be replayed onto the restored one.
		for _, suffix := range []string{walSuffix, shmSuffix} {
			if err := os.Remove(target + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return SnapshotInfo{}, "", err
			}
		}
	}
	if err := copyFile(snapshot, target); err != nil {
		err = fmt.Errorf("install the snapshot: %w", err)
		if previous != "" {
			if backErr := moveDatabase(previous, target); backErr != nil {
				return SnapshotInfo{}, "", fmt.Errorf("%w; the database that was there could not be put back and is at %s: %w", err, previous, backErr)
			}
		}
		return SnapshotInfo{}, "", err
	}
	ownerOnly(target)
	return info, previous, nil
}

// freePath is path, or path with the first numbered suffix that makes it a
// name nothing has, so a database put aside never lands on another one.
func freePath(path string) string {
	free := path
	for n := 2; ; n++ {
		if _, err := os.Stat(free); errors.Is(err, os.ErrNotExist) {
			return free
		}
		free = fmt.Sprintf("%s_%d", path, n)
	}
}

// moveDatabase renames a database and, with it, its write-ahead log and
// that log's index, which SQLite finds by the database's name. After an
// unclean stop the log can hold committed transactions that are not in the
// database file yet, so a database moved without its log has lost them.
// Either all the files that exist move or none do.
func moveDatabase(from, to string) error {
	var moved []string
	for _, suffix := range []string{"", walSuffix, shmSuffix} {
		err := os.Rename(from+suffix, to+suffix)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue // cleanly closed: there is no log
		}
		if err != nil {
			for _, s := range moved {
				_ = os.Rename(to+s, from+s)
			}
			return err
		}
		moved = append(moved, suffix)
	}
	return nil
}

// Restored is what RestoreBackup did.
type Restored struct {
	// Backup is the name of the stored backup that was installed.
	Backup   string
	Contents SnapshotInfo
	// Previous is where the database that was there has been put; empty if
	// there was none.
	Previous string
}

// RestoreBackup fetches a stored backup and installs it as the metadata
// database of dataDir, as Restore does. An empty name means the newest
// backup; with none stored the error wraps blob.ErrArchiveNotFound.
func RestoreBackup(ctx context.Context, dataDir string, from blob.Archive, name string, replace bool) (Restored, error) {
	dir := datadir.Dir(dataDir)
	tmp, err := scratch(dir, "restore-*")
	if err != nil {
		return Restored{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	snapshot := filepath.Join(tmp, snapshotFile)
	fetched, err := fetchBackup(ctx, from, name, snapshot)
	if err != nil {
		return Restored{}, err
	}
	info, previous, err := restore(ctx, dir, snapshot, replace, time.Now)
	if err != nil {
		return Restored{}, fmt.Errorf("backup %s: %w", fetched, err)
	}
	return Restored{Backup: fetched, Contents: info, Previous: previous}, nil
}

// fetchBackup downloads a stored backup to path, which must not exist. An
// empty name means the newest. It returns the name that was fetched.
func fetchBackup(ctx context.Context, from blob.Archive, name, path string) (string, error) {
	if name == "" {
		all, err := from.ListArchives(ctx)
		if err != nil {
			return "", err
		}
		if len(all) == 0 {
			return "", fmt.Errorf("%w: the storage holds no backups", blob.ErrArchiveNotFound)
		}
		name = all[len(all)-1].Name
	}
	rc, err := from.OpenArchive(ctx, name)
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
