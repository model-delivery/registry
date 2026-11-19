// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
)

// Backups are ordered by their names. The names of one second have to stay
// in the order the backups were taken however many there are; the numbered
// suffix used to put the tenth ("_10") between the first and the second.
func TestBackupNamesOfOneSecondSortInTheOrderTaken(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

	// Each name is chosen from the ones before it, as Backup chooses.
	const count = 1100 // past "_9", "_a99" and "_b999"
	var stored []Backup
	var names []string
	for range count {
		name := freeBackupName(at, stored)
		stored = append(stored, Backup{Name: name})
		names = append(names, name)
	}
	for i, want := range map[int]string{
		0: "registry-20261118T120000Z.db", 1: "registry-20261118T120000Z_2.db", 8: "registry-20261118T120000Z_9.db",
		9: "registry-20261118T120000Z_a10.db", 98: "registry-20261118T120000Z_a99.db",
		99: "registry-20261118T120000Z_b100.db", 999: "registry-20261118T120000Z_c1000.db",
	} {
		if names[i] != want {
			t.Errorf("backup %d of the second is named %s, want %s", i+1, names[i], want)
		}
	}
	for i := 1; i < count; i++ {
		// As text, which is how anything but the registry orders them.
		if names[i-1] >= names[i] {
			t.Fatalf("%s was taken before %s and sorts after it", names[i-1], names[i])
		}
		if n, ok := backupSeq(names[i], at); !ok || n != i+1 {
			t.Fatalf("backupSeq(%s) = %d, %v; want %d", names[i], n, ok, i+1)
		}
	}
	// And as the registry lists them.
	listed := slices.Clone(stored)
	slices.Reverse(listed)
	blob.SortArchives(listed)
	for i, b := range listed {
		if b.Name != names[i] {
			t.Fatalf("listed %s at position %d, want %s", b.Name, i, names[i])
		}
	}
	// A backup of the next second sorts after all of them, and one of the
	// second before sorts before.
	if next := BackupName(at.Add(time.Second)); next <= names[count-1] {
		t.Errorf("%s sorts before %s", next, names[count-1])
	}
	if before := laterBackupName(at.Add(-time.Second), 1000); before >= names[0] {
		t.Errorf("%s sorts after %s", before, names[0])
	}
}

// The name for a new backup, given what is stored. The names versions
// before v0.4.0 wrote ("_10" and up, with no letter) are counted like the
// current ones.
func TestFreeBackupName(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	const stamp = "registry-20261118T120000Z"
	cases := []struct {
		name   string
		stored []string
		want   string
	}{
		{"nothing stored", nil, stamp + ".db"},
		{"only other seconds", []string{"registry-20261118T115959Z.db", "registry-20261118T115959Z_2.db", "registry-20261118T120001Z.db"}, stamp + ".db"},
		{"the plain name", []string{stamp + ".db"}, stamp + "_2.db"},
		{"the plain name was pruned and a later one of the second is left: it is not reused", []string{stamp + "_3.db"}, stamp + "_4.db"},
		{"a gap is not filled", []string{stamp + ".db", stamp + "_5.db"}, stamp + "_6.db"},
		{"the ninth", []string{stamp + "_9.db"}, stamp + "_a10.db"},
		{"an old name of two digits", []string{stamp + ".db", stamp + "_2.db", stamp + "_10.db"}, stamp + "_a11.db"},
		{"an old name of three digits", []string{stamp + "_117.db"}, stamp + "_b118.db"},
		{"old and new together", []string{stamp + "_10.db", stamp + "_a11.db", stamp + "_9.db"}, stamp + "_a12.db"},
		{"the last of a width", []string{stamp + "_a99.db"}, stamp + "_b100.db"},
		{"names that are not backups of this second are not counted", []string{
			stamp + ".db", stamp + "_x.db", stamp + "_.db", stamp + "_a.db", stamp + "_-3.db", stamp + "_0.db",
			stamp + "_7.db.before", stamp + "x_8.db", "registry-unrelated.db", stamp + "_99999999999999999999999999.db",
		}, stamp + "_2.db"},
		{"a name in the way is passed over", []string{stamp + "_10.db", stamp + "_a11.db", stamp + "_a12.db"}, stamp + "_a13.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stored []Backup
			for _, n := range tc.stored {
				stored = append(stored, Backup{Name: n})
			}
			got := freeBackupName(at, stored)
			if got != tc.want {
				t.Fatalf("free name = %s, want %s", got, tc.want)
			}
			if slices.Contains(tc.stored, got) {
				t.Fatalf("%s is already stored", got)
			}
		})
	}
}

// With keep N, exactly N backups are left after every backup, they are the
// N taken last, and no name is used twice. Before, a backup taken in a
// second whose plain name had been pruned took that name again, listed as
// the oldest, and left N+1 stored.
func TestBackupKeepsExactlyTheNumberAsked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		keep int
		// perSecond is how many backups are taken in each second, in turn.
		perSecond []int
	}{
		{"one second, keep 1", 1, []int{12}},
		{"one second, keep 2", 2, []int{12}},
		{"one second, keep 3, more than ten", 3, []int{14}},
		{"one second, keep more than are taken", 20, []int{12}},
		{"several seconds, keep 3", 3, []int{1, 4, 1, 12, 2}},
		{"several seconds, keep 11", 11, []int{12, 12, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := open(t)
			at := start
			r.now = func() time.Time { return at }
			var taken []string
			for second, count := range tc.perSecond {
				at = start.Add(time.Duration(second) * time.Second)
				for range count {
					res, err := r.Backup(ctx, tc.keep)
					if err != nil || res.PruneErr != nil {
						t.Fatal(err, res.PruneErr)
					}
					if slices.Contains(taken, res.Backup.Name) {
						t.Fatalf("backup %d took the name %s, which an earlier backup had", len(taken)+1, res.Backup.Name)
					}
					taken = append(taken, res.Backup.Name)

					list, err := r.Backups(ctx)
					if err != nil {
						t.Fatal(err)
					}
					want := taken[max(0, len(taken)-tc.keep):]
					var got []string
					for _, b := range list {
						got = append(got, b.Name)
					}
					if !slices.Equal(got, want) {
						t.Fatalf("after backup %d (%s) with keep %d:\n stored %v\n want   %v", len(taken), res.Backup.Name, tc.keep, got, want)
					}
				}
			}
		})
	}
}

// A bucket written by a version before v0.4.0 holds names in two forms,
// "registry-<time>.db" and "registry-<time>_<n>.db" with n of any length.
// They list in the order they were taken, the newest is the one restored,
// each restores by name, and pruning removes the oldest of them.
func TestBackupsNamedByAnEarlierVersion(t *testing.T) {
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
	const stamp = "registry-20261118T120000Z"

	// The old names, in the order an earlier version took them: one from
	// the day before, then twelve in one second. Each holds one more
	// repository than the one before, which tells them apart.
	old := []string{"registry-20261117T120000Z.db", stamp + ".db"}
	for n := 2; n <= 12; n++ {
		old = append(old, fmt.Sprintf("%s_%d.db", stamp, n))
	}
	// commitOne adds a repository, so that the next snapshot holds one more.
	repos := 0
	commitOne := func() {
		t.Helper()
		repo := fmt.Sprintf("acme/model-%d", repos)
		if _, err := r.Commit(ctx, CommitInput{Repo: repo, Branch: "main", Files: []File{file(t, r, "a", repo)}}); err != nil {
			t.Fatal(err)
		}
		repos++
	}
	for _, name := range old {
		commitOne()
		tmp, path, _, err := r.scratchSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.blobs.PutArchive(ctx, name, path); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(tmp); err != nil {
			t.Fatal(err)
		}
	}
	names := func() []string {
		t.Helper()
		list, err := r.Backups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range list {
			out = append(out, b.Name)
		}
		return out
	}
	// restored says which backup a restore into a new directory installs,
	// and how many repositories it holds.
	restored := func(name string) (string, int) {
		t.Helper()
		got, err := RestoreBackup(ctx, t.TempDir(), r.blobs, name, false)
		if err != nil {
			t.Fatalf("restore %q: %v", name, err)
		}
		return got.Backup, got.Contents.Repos
	}

	if got := names(); !slices.Equal(got, old) {
		t.Fatalf("old names list as\n %v\nwant the order they were taken in\n %v", got, old)
	}
	if name, n := restored(""); name != stamp+"_12.db" || n != len(old) {
		t.Fatalf("the newest is %s with %d repositories, want %s with %d", name, n, stamp+"_12.db", len(old))
	}
	if name, n := restored(stamp + "_10.db"); name != stamp+"_10.db" || n != 11 {
		t.Fatalf("restoring %s_10.db by name gave %s with %d repositories", stamp, name, n)
	}

	// This version takes a backup in that same second: it follows them all.
	commitOne()
	res, err := r.Backup(ctx, 0)
	if err != nil || res.PruneErr != nil {
		t.Fatal(err, res.PruneErr)
	}
	if res.Backup.Name != stamp+"_a13.db" {
		t.Fatalf("the new backup is named %s, want %s_a13.db", res.Backup.Name, stamp)
	}
	all := append(slices.Clone(old), res.Backup.Name)
	if got := names(); !slices.Equal(got, all) {
		t.Fatalf("with the new backup:\n %v\nwant\n %v", got, all)
	}
	if name, n := restored(""); name != res.Backup.Name || n != len(all) {
		t.Fatalf("the newest is %s with %d repositories, want the one just taken", name, n)
	}

	// Pruning to five leaves the five taken last, old names and new.
	commitOne()
	res, err = r.Backup(ctx, 5)
	if err != nil || res.PruneErr != nil {
		t.Fatal(err, res.PruneErr)
	}
	all = append(all, res.Backup.Name)
	want := []string{stamp + "_10.db", stamp + "_11.db", stamp + "_12.db", stamp + "_a13.db", stamp + "_a14.db"}
	if got := names(); !slices.Equal(got, want) || !slices.Equal(want, all[len(all)-5:]) {
		t.Fatalf("after keep=5:\n %v\nwant\n %v", got, want)
	}
	for _, gone := range all[:len(all)-5] {
		if _, err := r.blobs.OpenArchive(ctx, gone); !errors.Is(err, blob.ErrArchiveNotFound) {
			t.Errorf("%s was not removed: %v", gone, err)
		}
	}
	if name, n := restored(stamp + "_11.db"); name != stamp+"_11.db" || n != 12 {
		t.Fatalf("restoring an old name after pruning gave %s with %d repositories", name, n)
	}
}

// A name is not given out twice by one process even when the backup that
// had it is gone, whoever removed it.
func TestBackupNameIsNotReusedAfterItsBackupWasRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }
	var taken []string
	for range 3 {
		res, err := r.Backup(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		taken = append(taken, res.Backup.Name)
		// Someone empties the backups by hand.
		if err := r.blobs.DeleteArchive(ctx, res.Backup.Name); err != nil {
			t.Fatal(err)
		}
	}
	const stamp = "registry-20261118T120000Z"
	if want := []string{stamp + ".db", stamp + "_2.db", stamp + "_3.db"}; !slices.Equal(taken, want) {
		t.Fatalf("names = %v, want %v", taken, want)
	}
	// In a later second the plain name is the right one again.
	at = at.Add(time.Second)
	res, err := r.Backup(ctx, 0)
	if err != nil || !strings.HasSuffix(res.Backup.Name, "T120001Z.db") {
		t.Fatalf("the next second's backup = %+v, %v", res.Backup, err)
	}
	if _, err := fetchBackup(ctx, r.blobs, "", filepath.Join(t.TempDir(), "fetched.db")); err != nil {
		t.Fatal(err)
	}
}
