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
	"strconv"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/datadir"
	"github.com/model-delivery/registry/internal/kit"
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
// as t, for n of 2 and up: the plain name with "_n" before the extension.
//
// Names are what orders backups, as text, wherever they are listed: here,
// by `ls`, by an object store's console. "_10" would come before "_2" that
// way. So a number of two digits or more is preceded by a letter that says
// how long it is, "a" for two digits, "b" for three: "_9", "_a10", "_a99",
// "_b100". A letter sorts after every digit, and a longer number has a
// later letter, so the names of one second sort in the order they were
// taken however many there are. The suffix sorts after the plain name.
//
// Versions before v0.4.0 wrote the bare number at any length ("_10").
// Those names are still read (backupSeq), and blob.SortArchives puts them
// where their number says.
func laterBackupName(t time.Time, n int) string {
	number := strconv.Itoa(n)
	width := ""
	if len(number) > 1 {
		// An int has at most nineteen digits, so the letter is never past "r".
		width = "abcdefghijklmnopqrstuvwxyz"[len(number)-2 : len(number)-1]
	}
	return "registry-" + t.UTC().Format(backupTimeLayout) + "_" + width + number + ".db"
}

// backupSeq reports which backup of the second t a stored name is: 1 for
// the plain name, n for a later one in either the current form or the one
// before v0.4.0. ok is false for a name of another second, or one this
// package did not write.
func backupSeq(name string, t time.Time) (n int, ok bool) {
	rest, ok := strings.CutPrefix(name, "registry-"+t.UTC().Format(backupTimeLayout))
	if !ok {
		return 0, false
	}
	if rest == ".db" {
		return 1, true
	}
	rest, ok = strings.CutPrefix(rest, "_")
	number, isDB := strings.CutSuffix(rest, ".db")
	if !ok || !isDB {
		return 0, false
	}
	if number != "" && number[0] >= 'a' && number[0] <= 'z' {
		number = number[1:] // the letter that says how long the number is
	}
	if number == "" || strings.Trim(number, "0123456789") != "" {
		return 0, false
	}
	// A number too long to read is not one this package wrote.
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > maxBackupSeq {
		return 0, false
	}
	return n, true
}

// maxBackupSeq is the largest number read from a backup's name. It is far
// beyond any number of backups a second can hold, and leaves room to add to.
const maxBackupSeq = 1 << 40

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
	res, err := r.backup(ctx, keep)
	r.noteBackup(res, err)
	return res, err
}

func (r *Registry) backup(ctx context.Context, keep int) (BackupResult, error) {
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
	// The name follows every backup stored for this second, and the last
	// one this process took: that one counts even if it has since been
	// removed, so that its name is not given out a second time.
	name := freeBackupName(now, append(stored[:len(stored):len(stored)], Backup{Name: r.lastBackup}))
	if err := r.blobs.PutArchive(ctx, name, path); err != nil {
		return BackupResult{}, fmt.Errorf("store the backup: %w", err)
	}
	r.lastBackup = name
	res := BackupResult{Backup: Backup{Name: name, Size: info.Size, ModTime: now}, Contents: info}
	if keep > 0 {
		res.PruneErr = r.pruneBackups(ctx, stored, name, keep)
	}
	return res, nil
}

// freeBackupName is the name for a backup taken at t: one that no stored
// backup has, and that sorts after every backup stored for the same second.
// Names carry the time to the second. The first backup of a second has the
// plain name; each later one has the number after the highest in use, so a
// name freed by pruning is never given out again while a later backup of
// that second exists, and a later backup never lists before an earlier one.
func freeBackupName(t time.Time, stored []Backup) string {
	taken := make(map[string]bool, len(stored))
	highest := 0
	for _, b := range stored {
		taken[b.Name] = true
		if n, ok := backupSeq(b.Name, t); ok && n > highest {
			highest = n
		}
	}
	if highest == 0 {
		return BackupName(t)
	}
	name := laterBackupName(t, highest+1)
	for n := highest + 2; taken[name]; n++ {
		name = laterBackupName(t, n) // a name this package did not write is in the way
	}
	return name
}

// pruneBackups removes the oldest of the backups stored before the one
// just written, so that keep are left counting that one. The one just
// written is never removed, wherever its name sorts. It tries every
// removal and returns what failed.
func (r *Registry) pruneBackups(ctx context.Context, earlier []Backup, justWritten string, keep int) error {
	older := make([]Backup, 0, len(earlier))
	for _, b := range earlier {
		if b.Name != justWritten { // it was stored over a backup of the same name
			older = append(older, b)
		}
	}
	// Oldest first, in the order backups are listed in.
	blob.SortArchives(older)
	var failed []error
	for i := 0; i < len(older)-(keep-1); i++ {
		if err := r.blobs.DeleteArchive(ctx, older[i].Name); err != nil {
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
	if _, err := kit.WriteNewFile(path, rc); err != nil {
		return "", err
	}
	return name, nil
}

// restoringSuffix names the file a snapshot is copied into before it takes
// the database's place. The name is fixed, so what an interrupted restore
// left is overwritten by the next one.
const restoringSuffix = ".restoring"

// copyFile puts a copy of src at dst, which holds all of it or is untouched.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // a snapshot path the operator named.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst+restoringSuffix, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // inside the data directory.
	if err != nil {
		return err
	}
	_, err = kit.InstallFile(out, dst, in)
	return err
}
