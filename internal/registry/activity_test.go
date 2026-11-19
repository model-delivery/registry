// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/kit"
)

// Totals counts what the registry holds: stored content once however many
// revisions use it, and files listed but not fetched apart from it.
func TestTotals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)

	if got, err := r.Totals(ctx); err != nil || got != (Totals{}) {
		t.Fatalf("an empty registry: %+v, %v", got, err)
	}

	shared := file(t, r, "shared.bin", "in two repositories")
	only := file(t, r, "only.bin", "in one")
	for _, repo := range []string{"acme/one", "acme/two"} {
		if _, err := r.Commit(ctx, CommitInput{Repo: repo, Branch: "main", Files: []File{shared}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/one", Branch: "main", Files: []File{shared, only}}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "stored, and in no revision yet")
	// A revision listed from an upstream, with nothing of it fetched.
	listed := []File{
		{Path: "weights.bin", Size: 4000, SHA256: kit.SHA256Hex("weights"), LFS: true},
		{Path: "config.json", Size: 20, GitOID: "0123456789012345678901234567890123456789"},
	}
	if _, err := r.CommitLazy(ctx, LazyInput{Repo: "hub/model", Branch: "main", SHA: "1111111111111111111111111111111111111111",
		Files: listed, Upstream: "https://hub.example"}); err != nil {
		t.Fatal(err)
	}

	want := Totals{
		Repos: 3, Revisions: 4,
		Blobs: 3, BlobBytes: int64(len("in two repositories") + len("in one") + len("stored, and in no revision yet")),
		PendingFiles: 2, PendingBytes: 4020,
	}
	if got, err := r.Totals(ctx); err != nil || got != want {
		t.Fatalf("totals = %+v, %v\nwant     %+v", got, err, want)
	}
	// The same numbers Stats gives for what the two have in common.
	if st, err := r.Stats(ctx); err != nil || st.Repos != want.Repos || st.Revisions != want.Revisions || st.Blobs != want.Blobs || st.StoredBytes != want.BlobBytes {
		t.Fatalf("stats = %+v, %v", st, err)
	}
}

// The registry names its storage backend, for the label metrics carry.
func TestStorageKind(t *testing.T) {
	t.Parallel()
	if got := open(t).StorageKind(); got != blob.KindFilesystem {
		t.Fatalf("a registry on local disk: %q", got)
	}
	r, _, _ := openStuck(t) // a store that wraps another
	if got := r.StorageKind(); got != blob.KindOther {
		t.Fatalf("a registry on a wrapped store: %q", got)
	}
}

// The registry keeps count of its own backups, whoever asked for them: the
// schedule calls Backup directly, and must be counted with the rest.
func TestActivityFollowsBackups(t *testing.T) {
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
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { clock = clock.Add(time.Hour); return clock }

	if got := r.Activity(); got.BackupsStored != 0 || got.BackupsFailed != 0 || !got.LastBackup.IsZero() || got.LastBackupBytes != 0 {
		t.Fatalf("before any backup: %+v", got)
	}

	first, err := r.Backup(ctx, 1)
	if err != nil || first.PruneErr != nil {
		t.Fatal(err, first.PruneErr)
	}
	got := r.Activity()
	if got.BackupsStored != 1 || got.BackupsFailed != 0 || got.BackupPruneFailures != 0 ||
		!got.LastBackup.Equal(first.Backup.ModTime) || got.LastBackupBytes != first.Backup.Size || first.Backup.Size == 0 {
		t.Fatalf("after one backup: %+v, want it counted with its time %s and size %d", got, first.Backup.ModTime, first.Backup.Size)
	}

	// Stored, and the first could not be removed: a backup, and a prune failure.
	second, err := r.Backup(ctx, 1)
	if err != nil || !errors.Is(second.PruneErr, errCannotDelete) {
		t.Fatalf("second backup = %+v, %v", second, err)
	}
	if got = r.Activity(); got.BackupsStored != 2 || got.BackupPruneFailures != 1 || !got.LastBackup.Equal(second.Backup.ModTime) {
		t.Fatalf("after a backup that could not prune: %+v", got)
	}

	// Not stored: the time and size of the last one that was stay.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Backup(cancelled, 1); err == nil {
		t.Fatal("a backup with a cancelled context succeeded")
	}
	if got = r.Activity(); got.BackupsStored != 2 || got.BackupsFailed != 1 || !got.LastBackup.Equal(second.Backup.ModTime) || got.LastBackupBytes != second.Backup.Size {
		t.Fatalf("after a backup that failed: %+v", got)
	}
}

// And of its verify runs: when the last complete one finished and what it
// found. A run that stops early changes neither.
func TestActivityFollowsVerifyRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	r, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }
	path := func(content string) string {
		digest := kit.SHA256Hex(content)
		return filepath.Join(dir, "blobs", "sha256", digest[:2], digest)
	}
	for _, content := range []string{"sound", "also sound", "to be removed", "to be altered"} {
		put(t, r, content)
	}

	if got := r.Activity(); got.VerifyRuns != 0 || !got.LastVerify.IsZero() || len(got.LastVerifyFound) != 0 {
		t.Fatalf("before any run: %+v", got)
	}
	run := func(ctx context.Context) error {
		_, err := r.Verify(ctx, func(BlobCheck) {})
		return err
	}
	if err := run(ctx); err != nil {
		t.Fatal(err)
	}
	got := r.Activity()
	if got.VerifyRuns != 1 || !got.LastVerify.Equal(at) || got.LastVerifyFound[BlobOK] != 4 || len(got.LastVerifyFound) != 1 {
		t.Fatalf("after a clean run: %+v", got)
	}

	if err := os.Remove(path("to be removed")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("to be altered"), []byte("TO BE ALTERED"), 0o600); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Hour)
	if err := run(ctx); err != nil {
		t.Fatal(err)
	}
	got = r.Activity()
	if got.VerifyRuns != 2 || !got.LastVerify.Equal(at) ||
		got.LastVerifyFound[BlobOK] != 2 || got.LastVerifyFound[BlobMissing] != 1 || got.LastVerifyFound[BlobDamaged] != 1 || got.LastVerifyFound[BlobUnreadable] != 0 {
		t.Fatalf("after a run over damaged storage: %+v", got)
	}
	// What Activity returned is the caller's: changing it changes nothing here.
	got.LastVerifyFound[BlobOK] = 99
	if again := r.Activity(); again.LastVerifyFound[BlobOK] != 2 {
		t.Fatalf("the registry's own count was changed through a copy: %+v", again)
	}

	// A run that is stopped is counted as one that failed, and what the
	// last complete run found stands.
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	later := at.Add(time.Hour)
	at = later
	if err := run(stopped); err == nil {
		t.Fatal("a run with a cancelled context finished")
	}
	got = r.Activity()
	if got.VerifyRuns != 2 || got.VerifyRunsFailed != 1 || !got.LastVerify.Equal(later.Add(-time.Hour)) || got.LastVerifyFound[BlobMissing] != 1 {
		t.Fatalf("after a run that was stopped: %+v", got)
	}
}

// The server keeps /metrics for itself, as it keeps /api, /ui, /static and
// /healthz, so no repository may live in a namespace of that name.
func TestMetricsIsAReservedNamespace(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"metrics/model", "Metrics/model", "METRICS/x"} {
		if err := ValidateRepoName(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateRepoName(%q) = %v, want it refused", name, err)
		}
	}
	for _, name := range []string{"acme/metrics", "metrics-team/model", "my.metrics/model"} {
		if err := ValidateRepoName(name); err != nil {
			t.Errorf("ValidateRepoName(%q) = %v, want it accepted", name, err)
		}
	}
	r := open(t)
	_, err := r.Commit(context.Background(), CommitInput{Repo: "metrics/model", Branch: "main", Files: []File{file(t, r, "a", "a")}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a commit to metrics/model: %v", err)
	}
}
