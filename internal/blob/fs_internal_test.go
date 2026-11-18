// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEntry is a directory entry whose Info answers as the test says.
type fakeEntry struct {
	name string
	dir  bool
	info fs.FileInfo
	err  error
}

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return e.dir }
func (e fakeEntry) Type() fs.FileMode          { return 0 }
func (e fakeEntry) Info() (fs.FileInfo, error) { return e.info, e.err }

type fakeInfo struct {
	name string
	size int64
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return 0o600 }
func (i fakeInfo) ModTime() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }

// The backup listing decides which backups are pruned, so a backup that is
// there and cannot be examined must fail the listing, not shorten it.
func TestArchiveListingReportsWhatItCannotExamine(t *testing.T) {
	t.Parallel()
	ok := func(name string, size int64) fakeEntry {
		return fakeEntry{name: name, info: fakeInfo{name: name, size: size}}
	}
	denied := errors.New("permission denied")

	tests := []struct {
		name      string
		entries   []fs.DirEntry
		wantNames []string
		wantErr   error
	}{
		{"backups are listed oldest first, and nothing else is",
			[]fs.DirEntry{ok("registry-2.db", 2), ok("registry-1.db", 1), ok("notes.txt", 9), ok(".incoming-123", 9),
				fakeEntry{name: "registry-0.db", dir: true}},
			[]string{"registry-1.db", "registry-2.db"}, nil},
		{"a backup removed since the directory was read is passed over",
			[]fs.DirEntry{ok("registry-1.db", 1), fakeEntry{name: "registry-2.db", err: fs.ErrNotExist}},
			[]string{"registry-1.db"}, nil},
		{"a backup that cannot be examined fails the listing",
			[]fs.DirEntry{ok("registry-1.db", 1), fakeEntry{name: "registry-2.db", err: denied}},
			nil, denied},
		{"a file that is not a backup is never examined",
			[]fs.DirEntry{ok("registry-1.db", 1), fakeEntry{name: "notes.txt", err: denied}},
			[]string{"registry-1.db"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := archiveInfos(tc.entries)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if got != nil || !strings.Contains(err.Error(), "registry-2.db") {
					t.Fatalf("got %v, %v; want no list and an error naming the backup", got, err)
				}
				return
			}
			var names []string
			for _, a := range got {
				names = append(names, a.Name)
			}
			if strings.Join(names, ",") != strings.Join(tc.wantNames, ",") {
				t.Fatalf("listed %v, want %v", names, tc.wantNames)
			}
		})
	}
}

// A filesystem failure says what was being done and to which blob or
// backup, as the S3 backend's errors do, and still wraps the cause.
func TestFilesystemErrorsNameTheOperation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const digest = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	root := t.TempDir()
	s, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	// A directory where a file is expected fails every operation on it, on
	// every platform, without being "not found".
	blobPath, _ := s.path(digest)
	if err := os.MkdirAll(filepath.Join(blobPath, "in-the-way"), 0o750); err != nil {
		t.Fatal(err)
	}
	const backup = "registry-20261118T120000Z.db"
	if err := os.MkdirAll(filepath.Join(s.archivePath(backup), "in-the-way"), 0o750); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(snapshot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		do   func() error
		want string
	}{
		{"delete a blob", func() error { return s.Delete(ctx, digest) }, "fs: delete " + digest},
		{"store a backup", func() error { return s.PutArchive(ctx, backup, snapshot) }, "fs: store backup " + backup},
		{"store a backup from a file that is not there", func() error {
			return s.PutArchive(ctx, "registry-other.db", filepath.Join(root, "absent.db"))
		}, "fs: store backup registry-other.db"},
		{"delete a backup", func() error { return s.DeleteArchive(ctx, backup) }, "fs: delete backup " + backup},
	}
	for _, tc := range tests {
		err := tc.do()
		if err == nil || !strings.HasPrefix(err.Error(), tc.want+": ") {
			t.Errorf("%s: error = %v, want one starting %q", tc.name, err, tc.want)
			continue
		}
		var pathErr *fs.PathError
		var linkErr *os.LinkError
		if !errors.As(err, &pathErr) && !errors.As(err, &linkErr) {
			t.Errorf("%s: %v does not wrap the operating system's error", tc.name, err)
		}
	}
}

// FS.path and the S3 object key are the same layout, built in one place.
func TestFilesystemPathFollowsTheBlobKey(t *testing.T) {
	t.Parallel()
	const digest = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	root := t.TempDir()
	s, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.path(digest)
	if want := filepath.Join(root, "blobs", "sha256", "2c", digest); err != nil || got != want {
		t.Fatalf("path = %q, %v; want %q", got, err, want)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 64), "../" + digest[3:], strings.ToUpper(digest)} {
		if _, err := s.path(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("path(%q) = %v, want it refused as not found", bad, err)
		}
	}
}
