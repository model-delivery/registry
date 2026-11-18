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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/datadir"
)

func TestBackupAndRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	r, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	f := file(t, r, "model.bin", "weights")
	before, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"}, Files: []File{f}})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := r.CreateToken(ctx, "ci", RoleWrite)
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.Backup(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, info := res.Backup, res.Contents
	if !strings.HasPrefix(b.Name, "registry-") || !strings.HasSuffix(b.Name, "Z.db") || info.Repos != 1 || info.Revisions != 1 || info.SchemaVersion != schemaVersion {
		t.Fatalf("backup = %+v, contents = %+v", b, info)
	}
	if res.PruneErr != nil {
		t.Fatalf("pruning with keep=0: %v", res.PruneErr)
	}

	// Work done after the backup is, by definition, not in it.
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/later", Branch: "main", Files: []File{file(t, r, "x.bin", "later")}}); err != nil {
		t.Fatal(err)
	}

	// Disaster: the database is lost. The blobs survive (same store).
	_ = r.Close()
	for _, name := range []string{"registry.db", "registry.db-wal", "registry.db-shm"} {
		_ = os.Remove(filepath.Join(dir, name))
	}

	store, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "fetched.db")
	name, err := fetchBackup(ctx, store, "", snapshot)
	if err != nil || name != b.Name {
		t.Fatalf("fetch newest = %q, %v; want %s", name, err, b.Name)
	}
	got, previous, err := Restore(ctx, dir, snapshot, false)
	if err != nil || previous != "" || got.Repos != 1 {
		t.Fatalf("restore = %+v, %q, %v", got, previous, err)
	}

	restored, err := Open(dir, store)
	if err != nil {
		t.Fatalf("open after restore: %v", err)
	}
	defer func() { _ = restored.Close() }()
	c, err := restored.Resolve(ctx, "acme/model", "v1")
	if err != nil || c.SHA != before.SHA {
		t.Fatalf("revision after restore = %v, %v; want %s", c.SHA, err, before.SHA)
	}
	rc, _, err := restored.OpenBlob(ctx, f.SHA256)
	if err != nil {
		t.Fatalf("content after restore: %v", err)
	}
	content, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(content) != "weights" {
		t.Fatalf("content after restore = %q", content)
	}
	if tok, ok, _ := restored.Authenticate(ctx, secret); !ok || tok.Role != RoleWrite {
		t.Fatal("tokens did not survive the restore")
	}
	if _, err := restored.Resolve(ctx, "acme/later", "main"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("a repository created after the backup is present: %v", err)
	}
}

func TestBackupKeepsOnlyTheNewest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	// One backup per second of a fake clock.
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { clock = clock.Add(time.Second); return clock }

	var names []string
	for range 5 {
		res, err := r.Backup(ctx, 3)
		if err != nil || res.PruneErr != nil {
			t.Fatal(err, res.PruneErr)
		}
		names = append(names, res.Backup.Name)
	}
	list, err := r.Backups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("%d backups kept, want 3", len(list))
	}
	for i, b := range list {
		if b.Name != names[i+2] {
			t.Errorf("kept %s at position %d, want %s (the three newest)", b.Name, i, names[i+2])
		}
	}
	// keep = 0 never removes.
	for range 2 {
		if _, err := r.Backup(ctx, 0); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := r.Backups(ctx); len(list) != 5 {
		t.Fatalf("%d backups after two more with keep=0, want 5", len(list))
	}
}

// A backup's name carries the time to the second. Two taken in the same
// second used to share a name, and the second silently replaced the first.
func TestBackupsInTheSameSecondDoNotOverwriteEachOther(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }

	var names []string
	for i := range 3 {
		// Each backup holds one more repository than the one before, so the
		// stored copies can be told apart.
		repo := fmt.Sprintf("acme/model-%d", i)
		if _, err := r.Commit(ctx, CommitInput{Repo: repo, Branch: "main", Files: []File{file(t, r, "a", repo)}}); err != nil {
			t.Fatal(err)
		}
		res, err := r.Backup(ctx, 0)
		if err != nil || res.PruneErr != nil {
			t.Fatal(err, res.PruneErr)
		}
		names = append(names, res.Backup.Name)
	}
	want := []string{"registry-20261118T120000Z.db", "registry-20261118T120000Z_2.db", "registry-20261118T120000Z_3.db"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("names = %v, want %v", names, want)
	}
	list, err := r.Backups(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("%d backups stored (%v), want all 3", len(list), err)
	}
	// Listed oldest first, and each still holds what it held when taken.
	for i, b := range list {
		if b.Name != want[i] {
			t.Errorf("position %d is %s, want %s: a later backup must list later", i, b.Name, want[i])
		}
		path := filepath.Join(t.TempDir(), "fetched.db")
		if _, err := fetchBackup(ctx, r.blobs, b.Name, path); err != nil {
			t.Fatal(err)
		}
		if info, err := VerifySnapshot(ctx, path); err != nil || info.Repos != i+1 {
			t.Errorf("%s holds %d repositories (%v), want %d: it was overwritten", b.Name, info.Repos, err, i+1)
		}
	}
	// "Newest" is the last one taken, and pruning keeps it.
	res, err := r.Backup(ctx, 1)
	if err != nil || res.PruneErr != nil || res.Backup.Name != "registry-20261118T120000Z_4.db" {
		t.Fatalf("fourth backup = %+v, %v", res, err)
	}
	if list, _ := r.Backups(ctx); len(list) != 1 || list[0].Name != res.Backup.Name {
		t.Fatalf("after keep=1: %+v, want only %s", list, res.Backup.Name)
	}
}

// brokenArchive is a store whose backups cannot be removed.
type brokenArchive struct {
	blob.Store
	deletes atomic.Int32
}

var errCannotDelete = errors.New("the bucket refuses deletes")

func (b *brokenArchive) DeleteArchive(context.Context, string) error {
	b.deletes.Add(1)
	return errCannotDelete
}

// A backup that was stored is a success even when old backups could not
// then be removed. That used to come back as a backup and an error
// together, which callers reported as a failed backup.
func TestBackupStoredButNotPrunedIsNotAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &brokenArchive{Store: fs}
	r, err := Open(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { clock = clock.Add(time.Second); return clock }

	// Nothing to remove yet: no warning.
	first, err := r.Backup(ctx, 1)
	if err != nil || first.PruneErr != nil {
		t.Fatalf("first backup = %+v, %v", first, err)
	}
	for n := 2; n <= 3; n++ {
		res, err := r.Backup(ctx, 1)
		if err != nil {
			t.Fatalf("backup %d failed although it was stored: %v", n, err)
		}
		if !errors.Is(res.PruneErr, errCannotDelete) {
			t.Fatalf("backup %d: PruneErr = %v, want the reason old backups were not removed", n, res.PruneErr)
		}
		if res.Backup.Name == "" || res.Contents.Repos != 1 {
			t.Fatalf("backup %d: result = %+v, want the stored backup described", n, res)
		}
		list, err := r.Backups(ctx)
		if err != nil || len(list) != n || list[n-1].Name != res.Backup.Name {
			t.Fatalf("after backup %d: %+v, %v; want it stored beside the ones that could not be removed", n, list, err)
		}
	}
	// Every removal was tried, not only the first: 1 after the second
	// backup, 2 after the third.
	if got := store.deletes.Load(); got != 3 {
		t.Fatalf("%d removals attempted, want 3", got)
	}
}

func TestBackupName(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("elsewhere", 5*3600))
	if got := BackupName(at); got != "registry-20260101T220405Z.db" {
		t.Fatalf("BackupName = %q, want the time in UTC to the second", got)
	}
	// The suffix of a later backup in the same second sorts after the first.
	if first, later := BackupName(at), laterBackupName(at, 2); first >= later || laterBackupName(at, 2) >= laterBackupName(at, 3) {
		t.Fatalf("%q, %q: later backups must sort later", first, later)
	}
	if got := freeBackupName(at, []Backup{{Name: BackupName(at)}, {Name: laterBackupName(at, 2)}, {Name: "registry-unrelated.db"}}); got != laterBackupName(at, 3) {
		t.Fatalf("free name = %q", got)
	}
}

// stagingEntries lists what is in a registry's staging directory.
func stagingEntries(t *testing.T, r *Registry) []string {
	t.Helper()
	entries, err := os.ReadDir(r.dir.Staging())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestOpenSnapshotRemovesItsFilesOnClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	snapshot, info, err := r.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Repos != 1 || info.Revisions != 1 || info.SchemaVersion != schemaVersion || info.Size == 0 {
		t.Fatalf("contents = %+v", info)
	}
	if got := stagingEntries(t, r); len(got) != 1 {
		t.Fatalf("staging while the snapshot is open = %v, want its one scratch directory", got)
	}
	// What is read is the snapshot, whole, and it seeks (for range requests).
	saved := filepath.Join(t.TempDir(), "saved.db")
	out, err := os.Create(saved)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(out, snapshot)
	_ = out.Close()
	if err != nil || n != info.Size {
		t.Fatalf("read %d bytes (%v), want %d", n, err, info.Size)
	}
	if pos, err := snapshot.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("seek = %d, %v", pos, err)
	}
	if got, err := VerifySnapshot(ctx, saved); err != nil || got.Repos != 1 {
		t.Fatalf("what was read is not the snapshot: %+v, %v", got, err)
	}

	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stagingEntries(t, r); len(got) != 0 {
		t.Fatalf("staging after Close = %v, want nothing left behind", got)
	}
	// A backup cleans up after itself the same way.
	if _, err := r.Backup(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := stagingEntries(t, r); len(got) != 0 {
		t.Fatalf("staging after a backup = %v", got)
	}
}

// The background writer's failures used to be dropped, so this test passed
// when nothing was written while the snapshots were taken.
func TestSnapshotIsConsistentWhileWriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	// Keep committing while snapshots are taken. Every commit is a new
	// revision, and each one that succeeds is counted.
	var commits atomic.Int64
	stop := make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				writer <- nil
				return
			default:
			}
			info, err := r.PutBlob(ctx, strings.NewReader(fmt.Sprintf("content %d", i)), blob.Expect{Size: -1})
			if err != nil {
				writer <- fmt.Errorf("write %d: store the content: %w", i, err)
				return
			}
			if _, err := r.Commit(ctx, CommitInput{Repo: "acme/busy", Branch: "main", Files: []File{{Path: "f", SHA256: info.SHA256}}}); err != nil {
				writer <- fmt.Errorf("write %d: record the revision: %w", i, err)
				return
			}
			commits.Add(1)
		}
	}()
	// waitForCommits blocks until the writer has made more commits than
	// seen, and returns how many it has made.
	waitForCommits := func(seen int64) int64 {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for commits.Load() <= seen {
			select {
			case err := <-writer:
				t.Fatalf("the writer stopped after %d commits: %v", commits.Load(), err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("the writer made no commit beyond %d in 30s", seen)
			}
			time.Sleep(time.Millisecond)
		}
		return commits.Load()
	}

	take := func(i int) SnapshotInfo {
		t.Helper()
		info, err := r.snapshot(ctx, filepath.Join(t.TempDir(), "snap.db"))
		if err != nil {
			t.Fatalf("snapshot %d while writing: %v", i, err)
		}
		return info
	}
	first := take(0)
	// Every snapshot is a complete, valid database.
	if first.Repos < 1 || first.Revisions < 1 {
		t.Fatalf("snapshot 0 = %+v", first)
	}
	// Counting from the first snapshot's own total, so that "more commits
	// than seen" always means more revisions than it held.
	seen := max(commits.Load(), int64(first.Revisions))
	for i := 1; i < 5; i++ {
		// At least one more revision was committed before this snapshot
		// began than before the previous one, so it must hold more than the
		// first snapshot did.
		before := waitForCommits(seen)
		info := take(i)
		if info.Revisions <= first.Revisions {
			t.Fatalf("snapshot %d holds %d revisions, the first held %d: the writes made meanwhile are not in it", i, info.Revisions, first.Revisions)
		}
		// 1 for the revision committed before the writer started.
		if int64(info.Revisions) < before+1 {
			t.Fatalf("snapshot %d holds %d revisions; %d were committed before it was taken", i, info.Revisions, before+1)
		}
		seen = before
	}
	close(stop)
	if err := <-writer; err != nil {
		t.Fatalf("the writer failed: %v", err)
	}
	if commits.Load() == 0 {
		t.Fatal("the writer never committed")
	}
}

func TestRestoreRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := open(t)
	if _, err := src.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, src, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "good.db")
	if _, err := src.snapshot(ctx, good); err != nil {
		t.Fatal(err)
	}

	t.Run("refuses a file that is not a registry snapshot", func(t *testing.T) {
		bad := map[string]string{
			"garbage":         "this is not a database",
			"an empty file":   "",
			"a truncated one": "",
		}
		raw, _ := os.ReadFile(good)
		bad["a truncated one"] = string(raw[:len(raw)/2])
		for name, content := range bad {
			p := filepath.Join(t.TempDir(), "bad.db")
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			if _, _, err := Restore(ctx, target, p, false); err == nil {
				t.Errorf("%s was restored", name)
			}
			if _, err := os.Stat(filepath.Join(target, "registry.db")); err == nil {
				t.Errorf("%s: a database was installed", name)
			}
		}
		if _, _, err := Restore(ctx, t.TempDir(), filepath.Join(t.TempDir(), "absent.db"), false); err == nil {
			t.Error("a missing file was restored")
		}
	})

	t.Run("refuses a snapshot from a newer version", func(t *testing.T) {
		newer := filepath.Join(t.TempDir(), "newer.db")
		raw, _ := os.ReadFile(good)
		if err := os.WriteFile(newer, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := openRaw(newer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		_, _, err = Restore(ctx, t.TempDir(), newer, false)
		if err == nil || !strings.Contains(err.Error(), "schema version 99") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("never overwrites an existing database unless told to, and then keeps it", func(t *testing.T) {
		target := t.TempDir()
		live, err := Open(target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := live.Commit(ctx, CommitInput{Repo: "acme/existing", Branch: "main", Files: []File{file(t, live, "e", "e")}}); err != nil {
			t.Fatal(err)
		}
		_ = live.Close()

		if _, _, err := Restore(ctx, target, good, false); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "--replace") {
			t.Fatalf("restore over an existing database: %v", err)
		}
		// Untouched by the refusal.
		check, _ := Open(target, nil)
		if _, err := check.Resolve(ctx, "acme/existing", "main"); err != nil {
			t.Fatalf("the existing database was damaged by a refused restore: %v", err)
		}
		_ = check.Close()

		_, previous, err := Restore(ctx, target, good, true)
		if err != nil || previous == "" {
			t.Fatalf("restore with replace: %q, %v", previous, err)
		}
		after, err := Open(target, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = after.Close() }()
		if _, err := after.Resolve(ctx, "acme/model", "main"); err != nil {
			t.Fatalf("restored content missing: %v", err)
		}
		if _, err := after.Resolve(ctx, "acme/existing", "main"); !errors.Is(err, ErrRepoNotFound) {
			t.Fatalf("the replaced database's content is still there: %v", err)
		}
		// The database that was replaced is kept, intact.
		if info, err := VerifySnapshot(ctx, previous); err != nil || info.Repos != 1 {
			t.Fatalf("the put-aside database: %+v, %v", info, err)
		}
	})

	t.Run("a write-ahead log with no database beside it is not replayed onto the restored one", func(t *testing.T) {
		target := t.TempDir()
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := os.WriteFile(filepath.Join(target, "registry.db"+suffix), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, previous, err := Restore(ctx, target, good, false); err != nil || previous != "" {
			t.Fatalf("restore = %q, %v", previous, err)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(filepath.Join(target, "registry.db"+suffix)); !os.IsNotExist(err) {
				t.Errorf("the orphan %s file was left in place: %v", suffix, err)
			}
		}
		after, err := Open(target, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = after.Close() }()
		if _, err := after.Resolve(ctx, "acme/model", "main"); err != nil {
			t.Fatalf("restored content missing: %v", err)
		}
	})

	t.Run("the name of the database put aside comes from the clock, and never lands on another", func(t *testing.T) {
		target := t.TempDir()
		at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		now := func() time.Time { return at }
		var kept []string
		for range 3 {
			_, previous, err := restore(ctx, datadir.Dir(target), good, true, now)
			if err != nil {
				t.Fatal(err)
			}
			if previous != "" {
				kept = append(kept, filepath.Base(previous))
			}
		}
		want := []string{"registry.db.before-restore-20261118T120000Z", "registry.db.before-restore-20261118T120000Z_2"}
		if strings.Join(kept, " ") != strings.Join(want, " ") {
			t.Fatalf("put aside as %v, want %v", kept, want)
		}
		for _, name := range want {
			if info, err := VerifySnapshot(ctx, filepath.Join(target, name)); err != nil || info.Repos != 1 {
				t.Errorf("%s: %+v, %v; want a database that was kept", name, info, err)
			}
		}
	})

	t.Run("a restore that fails puts the existing database back", func(t *testing.T) {
		target := t.TempDir()
		live, err := Open(target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := live.Commit(ctx, CommitInput{Repo: "acme/existing", Branch: "main", Files: []File{file(t, live, "e", "e")}}); err != nil {
			t.Fatal(err)
		}
		_ = live.Close()
		// Something in the way of the file the snapshot is copied into.
		if err := os.MkdirAll(filepath.Join(target, "registry.db.restoring", "in-the-way"), 0o750); err != nil {
			t.Fatal(err)
		}
		info, previous, err := Restore(ctx, target, good, true)
		if err == nil || !strings.Contains(err.Error(), "install the snapshot") || previous != "" || info != (SnapshotInfo{}) {
			t.Fatalf("restore = %+v, %q, %v; want only an error", info, previous, err)
		}
		entries, _ := os.ReadDir(target)
		for _, e := range entries {
			if strings.Contains(e.Name(), "before-restore") {
				t.Errorf("%s was left behind by a restore that failed", e.Name())
			}
		}
		back, err := Open(target, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = back.Close() }()
		if _, err := back.Resolve(ctx, "acme/existing", "main"); err != nil {
			t.Fatalf("the database that was there is not back in place: %v", err)
		}
	})
}

// A restore usually follows a crash, and after a crash the write-ahead log
// can hold committed transactions that are not in the database file yet.
// Putting the old database aside has to take its log with it, or the copy
// that is "kept" has lost them. The log used to be deleted.
func TestRestoreReplaceKeepsTheOldDatabasesWriteAheadLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A snapshot to restore, from some other registry.
	src := open(t)
	if _, err := src.Commit(ctx, CommitInput{Repo: "acme/from-backup", Branch: "main", Files: []File{file(t, src, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "good.db")
	if _, err := src.snapshot(ctx, good); err != nil {
		t.Fatal(err)
	}

	// A registry that stops without closing its database: its files are
	// copied as they are while it is still open, which is what a crash
	// leaves on disk. Checkpointing is off, so what it committed is in the
	// write-ahead log and nowhere else.
	liveDir := t.TempDir()
	live, err := Open(liveDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	live.db.SetMaxOpenConns(1) // the pragma below is per connection
	if _, err := live.db.ExecContext(ctx, "PRAGMA wal_autocheckpoint = 0"); err != nil {
		t.Fatal(err)
	}
	const revisions = 5
	for i := range revisions {
		repo := fmt.Sprintf("acme/crashed-%d", i)
		if _, err := live.Commit(ctx, CommitInput{Repo: repo, Branch: "main", Files: []File{file(t, live, "a", repo)}}); err != nil {
			t.Fatal(err)
		}
	}
	crashed := t.TempDir()
	for _, name := range []string{"registry.db", "registry.db-wal", "registry.db-shm"} {
		raw, err := os.ReadFile(filepath.Join(liveDir, name))
		if err != nil {
			t.Fatalf("copy the crashed registry's %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(crashed, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The premise: without its log, the database file does not hold those
	// revisions.
	alone := filepath.Join(t.TempDir(), "registry.db")
	raw, _ := os.ReadFile(filepath.Join(crashed, "registry.db"))
	if err := os.WriteFile(alone, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := countRepos(ctx, alone); err == nil && n == revisions {
		t.Fatalf("the database file alone already holds all %d repositories; this test needs them to be only in the log", n)
	}
	if fi, err := os.Stat(filepath.Join(crashed, "registry.db-wal")); err != nil || fi.Size() == 0 {
		t.Fatalf("the crashed registry has no write-ahead log to lose: %v", err)
	}

	_, previous, err := Restore(ctx, crashed, good, true)
	if err != nil || previous == "" {
		t.Fatalf("restore with replace = %q, %v", previous, err)
	}
	// The log went with the database it belongs to, under the same name.
	if fi, err := os.Stat(previous + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the old database's write-ahead log was not kept beside it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(crashed, "registry.db-wal")); !os.IsNotExist(err) {
		t.Fatalf("the old log was left where the restored database would replay it: %v", err)
	}
	// Opening the kept copy replays the log: every committed row is there.
	if n, err := countRepos(ctx, previous); err != nil || n != revisions {
		t.Fatalf("the kept database holds %d repositories (%v), want the %d that were committed before the crash", n, err, revisions)
	}
	kept, err := sql.Open("sqlite", "file:"+filepath.ToSlash(previous))
	if err != nil {
		t.Fatal(err)
	}
	var check string
	if err := kept.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("the kept database: integrity check = %q, %v", check, err)
	}
	_ = kept.Close()

	// And the restored registry is the snapshot, with none of the old rows.
	after, err := Open(crashed, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = after.Close() }()
	if _, err := after.Resolve(ctx, "acme/from-backup", "main"); err != nil {
		t.Fatalf("restored content missing: %v", err)
	}
	if repos, err := after.ListRepos(ctx); err != nil || len(repos) != 1 {
		t.Fatalf("the restored registry lists %d repositories (%v), want only the snapshot's", len(repos), err)
	}
}

func TestRestoreBackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	r, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := r.blobs

	// With nothing stored there is nothing to restore, and that is told
	// apart from a failure.
	if _, err := RestoreBackup(ctx, t.TempDir(), store, "", false); !errors.Is(err, blob.ErrArchiveNotFound) {
		t.Fatalf("no backups: %v, want ErrArchiveNotFound", err)
	}

	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	var names []string
	for i := range 2 {
		repo := fmt.Sprintf("acme/model-%d", i)
		if _, err := r.Commit(ctx, CommitInput{Repo: repo, Branch: "main", Files: []File{file(t, r, "a", repo)}}); err != nil {
			t.Fatal(err)
		}
		res, err := r.Backup(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, res.Backup.Name)
	}
	_ = r.Close()

	// An upload in progress, as a running server would have: fetching a
	// backup into the same staging directory must leave it alone.
	inProgress := filepath.Join(r.dir.Staging(), "stage-in-progress")
	if err := os.WriteFile(inProgress, []byte("half an upload"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RestoreBackup(ctx, dir, store, "", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("over an existing database without replace: %v", err)
	}
	if _, err := RestoreBackup(ctx, dir, store, "registry-absent.db", true); !errors.Is(err, blob.ErrArchiveNotFound) {
		t.Fatalf("a backup that is not stored: %v", err)
	}

	tests := []struct {
		name      string
		ask       string
		wantName  string
		wantRepos int
	}{
		{"the newest when none is named", "", names[1], 2},
		{"the one that is named", names[0], names[0], 1},
	}
	for _, tc := range tests {
		got, err := RestoreBackup(ctx, dir, store, tc.ask, true)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Backup != tc.wantName || got.Contents.Repos != tc.wantRepos || got.Previous == "" {
			t.Errorf("%s: restored %+v, want %s with %d repositories and the old database kept", tc.name, got, tc.wantName, tc.wantRepos)
		}
		if n, err := countRepos(ctx, filepath.Join(dir, "registry.db")); err != nil || n != tc.wantRepos {
			t.Errorf("%s: the installed database holds %d repositories (%v)", tc.name, n, err)
		}
	}
	// Only the upload in progress is left in staging: no fetched copies.
	entries, _ := os.ReadDir(r.dir.Staging())
	if len(entries) != 1 || entries[0].Name() != "stage-in-progress" {
		var left []string
		for _, e := range entries {
			left = append(left, e.Name())
		}
		t.Fatalf("staging after restoring = %v, want only the upload that was in progress", left)
	}
}

// countRepos opens a database file as SQLite would after a restart,
// replaying its write-ahead log if it has one, and counts repositories.
func countRepos(ctx context.Context, path string) (int, error) {
	db, err := openRaw(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var n int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos").Scan(&n)
	return n, err
}

// openRaw opens a database file directly, for tests that need to alter one.
func openRaw(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.ToSlash(path))
}
