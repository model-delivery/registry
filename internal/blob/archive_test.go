// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/model-delivery/registry/internal/blob"
)

// Both storage backends keep backups, and must behave the same.
func TestArchives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eachBackend(t, func(t *testing.T, store blob.Store, _ string) {
		archive, ok := store.(blob.Archive)
		if !ok {
			t.Fatal("this backend cannot keep backups")
		}
		local := func(content string) string {
			p := filepath.Join(t.TempDir(), "snapshot.db")
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}
		names := func() []string {
			list, err := archive.ListArchives(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, a := range list {
				out = append(out, a.Name)
			}
			return out
		}

		if got := names(); len(got) != 0 {
			t.Fatalf("a new store lists backups: %v", got)
		}
		// Stored out of order; listed oldest first, which is name order.
		for _, n := range []string{"registry-20261118T120000Z.db", "registry-20261117T120000Z.db", "registry-20261119T120000Z.db"} {
			if err := archive.PutArchive(ctx, n, local("contents of "+n)); err != nil {
				t.Fatal(err)
			}
		}
		got := names()
		want := []string{"registry-20261117T120000Z.db", "registry-20261118T120000Z.db", "registry-20261119T120000Z.db"}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("listing = %v, want %v", got, want)
		}
		list, _ := archive.ListArchives(ctx)
		if list[0].Size != int64(len("contents of registry-20261117T120000Z.db")) || list[0].ModTime.IsZero() {
			t.Errorf("listing entry = %+v", list[0])
		}

		rc, err := archive.OpenArchive(ctx, "registry-20261118T120000Z.db")
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(content) != "contents of registry-20261118T120000Z.db" {
			t.Fatalf("read back %q", content)
		}

		// Storing under an existing name replaces it.
		if err := archive.PutArchive(ctx, "registry-20261118T120000Z.db", local("replaced")); err != nil {
			t.Fatal(err)
		}
		rc, _ = archive.OpenArchive(ctx, "registry-20261118T120000Z.db")
		content, _ = io.ReadAll(rc)
		_ = rc.Close()
		if string(content) != "replaced" || len(names()) != 3 {
			t.Fatalf("after replacing: %q, %v", content, names())
		}

		if err := archive.DeleteArchive(ctx, "registry-20261117T120000Z.db"); err != nil {
			t.Fatal(err)
		}
		if err := archive.DeleteArchive(ctx, "registry-20261117T120000Z.db"); err != nil {
			t.Fatalf("deleting twice: %v", err)
		}
		if _, err := archive.OpenArchive(ctx, "registry-20261117T120000Z.db"); !errors.Is(err, blob.ErrArchiveNotFound) {
			t.Fatalf("opening a deleted backup: %v", err)
		}
		if got := names(); len(got) != 2 {
			t.Fatalf("after delete: %v", got)
		}

		// A name can only ever be a plain file name in the backups area.
		for _, bad := range []string{"", "../registry.db", "sha256/ab/x.db", "a/b.db", "notadb.txt", ".hidden.db", "x..db"} {
			if err := archive.PutArchive(ctx, bad, local("x")); !errors.Is(err, blob.ErrArchiveNotFound) {
				t.Errorf("PutArchive(%q) = %v, want it refused", bad, err)
			}
			if _, err := archive.OpenArchive(ctx, bad); !errors.Is(err, blob.ErrArchiveNotFound) {
				t.Errorf("OpenArchive(%q) = %v, want it refused", bad, err)
			}
			if err := archive.DeleteArchive(ctx, bad); !errors.Is(err, blob.ErrArchiveNotFound) {
				t.Errorf("DeleteArchive(%q) = %v, want it refused", bad, err)
			}
		}

		// Backups and blobs do not see each other.
		st, err := store.Stage(ctx, io.NopCloser(io.LimitReader(repeat([]byte("blob")), 64)), blob.Expect{Size: -1})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Publish(ctx); err != nil {
			t.Fatal(err)
		}
		if got := names(); len(got) != 2 {
			t.Fatalf("a blob showed up as a backup: %v", got)
		}
		_ = store.Delete(ctx, st.Info().SHA256)
	})
}
