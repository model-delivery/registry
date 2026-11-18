// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package datadir_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/model-delivery/registry/internal/datadir"
)

// The layout is the on-disk format: a data directory written by an earlier
// release has to be found where this one looks.
func TestLayout(t *testing.T) {
	t.Parallel()
	d := datadir.Dir("data")
	tests := []struct {
		name, got, want string
	}{
		{"database", d.Database(), filepath.Join("data", "registry.db")},
		{"blobs", d.Blobs(), filepath.Join("data", "blobs")},
		{"backups", d.Backups(), filepath.Join("data", "backups")},
		{"staging", d.Staging(), filepath.Join("data", "tmp")},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestClearStaging(t *testing.T) {
	t.Parallel()
	d := datadir.Dir(t.TempDir())

	// On a directory that has never been used it only creates the space.
	if err := d.ClearStaging(); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(d.Staging(), "stage-123")
	nested := filepath.Join(d.Staging(), "snapshot-456", "snapshot.db")
	if err := os.MkdirAll(filepath.Dir(nested), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{leftover, nested, d.Database()} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.ClearStaging(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(d.Staging())
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging after clearing: %v, %v; want it empty and present", entries, err)
	}
	if _, err := os.Stat(d.Database()); err != nil {
		t.Fatalf("clearing staging touched something outside it: %v", err)
	}
}
