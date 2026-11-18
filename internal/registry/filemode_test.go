// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The database, its write-ahead log, snapshots and restored databases all
// hold token hashes. None of them may be readable by other accounts on the
// host, whatever mode SQLite creates files with.
func TestDatabaseFilesAreOwnerOnly(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not express file access in mode bits")
	}
	ctx := context.Background()
	mode := func(path string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	// Whichever of the database's files exist must be owner-only, and the
	// database itself must exist.
	checkDatabase := func(when, dir string) {
		t.Helper()
		db := filepath.Join(dir, "registry.db")
		if got := mode(db); got != 0o600 {
			t.Errorf("%s: registry.db has mode %o, want 600", when, got)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(db + suffix); err == nil {
				if got := mode(db + suffix); got != 0o600 {
					t.Errorf("%s: registry.db%s has mode %o, want 600", when, suffix, got)
				}
			}
		}
	}

	dir := t.TempDir()
	r, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkDatabase("after the first start", dir)
	// Writing brings the write-ahead log into use.
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateToken(ctx, "ci", RoleWrite); err != nil {
		t.Fatal(err)
	}
	checkDatabase("after writing", dir)

	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := r.snapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if got := mode(snapshot); got != 0o600 {
		t.Errorf("a snapshot has mode %o, want 600", got)
	}
	_ = r.Close()

	// A database left world-readable by an earlier version is tightened
	// the next time it is opened.
	if err := os.Chmod(filepath.Join(dir, "registry.db"), 0o644); err != nil { //nolint:gosec // the loose mode is the test's starting point.
		t.Fatal(err)
	}
	again, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkDatabase("after reopening a world-readable database", dir)
	if _, err := again.CreateToken(ctx, "second", RoleRead); err != nil {
		t.Fatal(err)
	}
	checkDatabase("after writing to it again", dir)
	_ = again.Close()

	// A restore installs an owner-only database even from a snapshot file
	// that is not.
	if err := os.Chmod(snapshot, 0o644); err != nil { //nolint:gosec // as above.
		t.Fatal(err)
	}
	restored := t.TempDir()
	if _, _, err := Restore(ctx, restored, snapshot, false); err != nil {
		t.Fatal(err)
	}
	checkDatabase("after a restore", restored)
}
