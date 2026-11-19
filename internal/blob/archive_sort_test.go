// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/model-delivery/registry/internal/blob"
)

// Backups are listed oldest first, by name. A number in a name counts as
// the number it is, so the names of every version list in the order the
// backups were taken.
func TestSortArchives(t *testing.T) {
	t.Parallel()
	const day = "registry-20261118T120000Z"
	cases := []struct {
		name string
		// want is the order; it is shuffled and sorted back.
		want []string
	}{
		{"by time", []string{"registry-20251231T235959Z.db", "registry-20260101T000000Z.db", "registry-20261118T115959Z.db", day + ".db", "registry-20261118T120001Z.db"}},
		{"the plain name before the numbered ones", []string{day + ".db", day + "_2.db", day + "_3.db"}},
		{"numbers as written before v0.4.0, past ten", []string{
			day + ".db", day + "_2.db", day + "_3.db", day + "_9.db", day + "_10.db", day + "_11.db", day + "_20.db", day + "_99.db", day + "_100.db", day + "_101.db", day + "_1000.db",
		}},
		{"numbers as written now", []string{day + ".db", day + "_2.db", day + "_9.db", day + "_a10.db", day + "_a11.db", day + "_a99.db", day + "_b100.db", day + "_b999.db", day + "_c1000.db"}},
		{"both, in a second that saw both versions", []string{day + "_9.db", day + "_10.db", day + "_11.db", day + "_a12.db", day + "_a13.db"}},
		{"a numbered name of one second before the plain name of the next", []string{day + "_10.db", day + "_a11.db", "registry-20261118T120001Z.db", "registry-20261118T120001Z_2.db"}},
		{"names that differ only in leading zeros still have an order", []string{day + "_02.db", day + "_2.db", day + "_3.db"}},
		{"names that are not the registry's", []string{"copy-2.db", "copy-10.db", day + ".db", "registry.db", "snap.db"}},
		{"a number too long for any integer type", []string{day + "_9.db", day + "_99999999999999999999999999999998.db", day + "_99999999999999999999999999999999.db"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for round := range 20 {
				list := make([]blob.ArchiveInfo, len(tc.want))
				for i, p := range rand.New(rand.NewPCG(uint64(round), 7)).Perm(len(tc.want)) { //nolint:gosec // a shuffle for a test
					list[i] = blob.ArchiveInfo{Name: tc.want[p]}
				}
				blob.SortArchives(list)
				var got []string
				for _, a := range list {
					got = append(got, a.Name)
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("sorted\n %v\nwant\n %v", got, tc.want)
				}
			}
		})
	}
}

// Both backends list by that order, whatever order their storage returns
// names in.
func TestArchivesListNumberedNamesInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const day = "registry-20261118T120000Z"
	want := []string{day + ".db", day + "_2.db", day + "_9.db", day + "_10.db", day + "_11.db", day + "_a12.db", day + "_b100.db"}
	eachBackend(t, func(t *testing.T, store blob.Store, _ string) {
		local := filepath.Join(t.TempDir(), "snapshot.db")
		if err := os.WriteFile(local, []byte("a snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, i := range []int{3, 0, 6, 4, 1, 5, 2} {
			if err := store.PutArchive(ctx, want[i], local); err != nil {
				t.Fatal(err)
			}
		}
		list, err := store.ListArchives(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, a := range list {
			got = append(got, a.Name)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("listed\n %v\nwant\n %v", got, want)
		}
	})
}
