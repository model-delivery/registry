// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
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

	b, info, err := r.Backup(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.Name, "registry-") || !strings.HasSuffix(b.Name, "Z.db") || info.Repos != 1 || info.Revisions != 1 || info.SchemaVersion != schemaVersion {
		t.Fatalf("backup = %+v, contents = %+v", b, info)
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
	name, err := FetchBackup(ctx, store, "", snapshot)
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
	if tok, ok := restored.Authenticate(ctx, secret); !ok || tok.Role != RoleWrite {
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
		b, _, err := r.Backup(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, b.Name)
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
		if _, _, err := r.Backup(ctx, 0); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := r.Backups(ctx); len(list) != 5 {
		t.Fatalf("%d backups after two more with keep=0, want 5", len(list))
	}
}

func TestSnapshotIsConsistentWhileWriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	// Keep committing while snapshots are taken.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			content := strings.Repeat("x", i%7+1) + time.Now().String()
			info, err := r.PutBlob(ctx, strings.NewReader(content), blob.Expect{Size: -1})
			if err != nil {
				return
			}
			_, _ = r.Commit(ctx, CommitInput{Repo: "acme/busy", Branch: "main", Files: []File{{Path: "f", SHA256: info.SHA256}}})
		}
	}()
	for i := range 5 {
		path := filepath.Join(t.TempDir(), "snap.db")
		info, err := r.Snapshot(ctx, path)
		if err != nil {
			t.Fatalf("snapshot %d while writing: %v", i, err)
		}
		// Every snapshot is a complete, valid database.
		if info.Repos < 1 || info.Revisions < 1 {
			t.Fatalf("snapshot %d = %+v", i, info)
		}
	}
	close(stop)
	<-done
}

func TestRestoreRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := open(t)
	if _, err := src.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, src, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "good.db")
	if _, err := src.Snapshot(ctx, good); err != nil {
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

		// A stale write-ahead log from the old database must not be replayed
		// onto the restored one.
		if err := os.WriteFile(filepath.Join(target, "registry.db-wal"), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, previous, err := Restore(ctx, target, good, true)
		if err != nil || previous == "" {
			t.Fatalf("restore with replace: %q, %v", previous, err)
		}
		if _, err := os.Stat(filepath.Join(target, "registry.db-wal")); !os.IsNotExist(err) {
			t.Error("the old write-ahead log was left in place")
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
}

// openRaw opens a database file directly, for tests that need to alter one.
func openRaw(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.ToSlash(path))
}
