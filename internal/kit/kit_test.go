// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package kit_test

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/kit"
)

func TestHumanBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1<<20 - 1, "1024.0 KiB"}, // rounds up to the next unit's threshold without crossing it
		{1 << 20, "1.0 MiB"},
		{5 << 20, "5.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{20 << 30, "20.0 GiB"},
		{1 << 40, "1.0 TiB"},
		{1 << 50, "1.0 PiB"},
		{1 << 60, "1.0 EiB"},
		{math.MaxInt64, "8.0 EiB"},
	}
	for _, tc := range tests {
		if got := kit.HumanBytes(tc.n); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestShort(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"0123456789ab", "0123456789ab"},
		{"f171d7baecaf37b5da5a3616d8833b9969753535", "f171d7baecaf"},
	}
	for _, tc := range tests {
		if got := kit.Short(tc.in); got != tc.want {
			t.Errorf("Short(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapePath(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"acme/tiny", "acme/tiny"},
		{"config.json", "config.json"},
		{"tokenizer/vocab.txt", "tokenizer/vocab.txt"},
		{"dir with space/file name.bin", "dir%20with%20space/file%20name.bin"},
		{"a/b?c#d", "a/b%3Fc%23d"},
		{"100%/x", "100%25/x"},
		{"unicode/模型.bin", "unicode/%E6%A8%A1%E5%9E%8B.bin"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := kit.EscapePath(tc.in); got != tc.want {
			t.Errorf("EscapePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The ids below are what `git hash-object` prints for the same content.
func TestGitBlobID(t *testing.T) {
	t.Parallel()
	tests := []struct{ content, want string }{
		{"", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{"hello\n", "ce013625030ba8dba906f756967f9e9ca394464a"},
	}
	for _, tc := range tests {
		got, err := kit.GitBlobID(strings.NewReader(tc.content), int64(len(tc.content)))
		if err != nil || got != tc.want {
			t.Errorf("GitBlobID(%q) = %q, %v; want %q", tc.content, got, err, tc.want)
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	t.Parallel()
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := kit.SHA256Hex(""); got != empty {
		t.Errorf("of an empty string: %s", got)
	}
	if got := kit.SHA256Hex([]byte(nil)); got != empty {
		t.Errorf("of no bytes: %s", got)
	}
	if kit.SHA256Hex("abc") != kit.SHA256Hex([]byte("abc")) {
		t.Error("a string and the same bytes hash differently")
	}
}

// failing is a reader that gives some content and then an error, as a
// dropped connection does.
type failing struct{ sent bool }

var errDropped = errors.New("connection dropped")

func (f *failing) Read(p []byte) (int, error) {
	if f.sent {
		return 0, errDropped
	}
	f.sent = true
	return copy(p, "partial"), nil
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestInstallFile(t *testing.T) {
	t.Parallel()
	open := func(t *testing.T, dir string) *os.File {
		t.Helper()
		tmp, err := os.CreateTemp(dir, ".incoming-*")
		if err != nil {
			t.Fatal(err)
		}
		return tmp
	}

	t.Run("replaces what was there, and leaves no temporary file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dst := filepath.Join(dir, "registry.db")
		if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		n, err := kit.InstallFile(open(t, dir), dst, strings.NewReader("the new content"))
		if err != nil || n != int64(len("the new content")) {
			t.Fatalf("wrote %d bytes, %v", n, err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "the new content" {
			t.Fatalf("dst holds %q", got)
		}
		if names := entries(t, dir); len(names) != 1 {
			t.Fatalf("left behind: %v", names)
		}
	})

	t.Run("a copy that fails leaves the old content and no temporary file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dst := filepath.Join(dir, "registry.db")
		if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := kit.InstallFile(open(t, dir), dst, &failing{}); !errors.Is(err, errDropped) {
			t.Fatalf("got %v, want the copy's error", err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "old" {
			t.Fatalf("dst now holds %q", got)
		}
		if names := entries(t, dir); len(names) != 1 {
			t.Fatalf("left behind: %v", names)
		}
	})

	t.Run("a destination that cannot be replaced is an error, with nothing left", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dst := filepath.Join(dir, "in-the-way")
		// A directory with something in it cannot be renamed over.
		if err := os.MkdirAll(filepath.Join(dst, "child"), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := kit.InstallFile(open(t, dir), dst, strings.NewReader("x")); err == nil {
			t.Fatal("installed a file over a directory")
		}
		if names := entries(t, dir); len(names) != 1 {
			t.Fatalf("left behind: %v", names)
		}
	})
}

func TestWriteNewFile(t *testing.T) {
	t.Parallel()

	t.Run("writes a file that was not there", func(t *testing.T) {
		t.Parallel()
		dst := filepath.Join(t.TempDir(), "snapshot.db")
		n, err := kit.WriteNewFile(dst, strings.NewReader("content"))
		if err != nil || n != 7 {
			t.Fatalf("wrote %d bytes, %v", n, err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "content" {
			t.Fatalf("dst holds %q", got)
		}
	})

	t.Run("never overwrites, and does not read the source to find out", func(t *testing.T) {
		t.Parallel()
		dst := filepath.Join(t.TempDir(), "snapshot.db")
		if err := os.WriteFile(dst, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		src := strings.NewReader("replacement")
		if _, err := kit.WriteNewFile(dst, src); !errors.Is(err, os.ErrExist) {
			t.Fatalf("got %v, want it refused because the file exists", err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "precious" {
			t.Fatalf("dst now holds %q", got)
		}
		if rest, _ := io.ReadAll(src); string(rest) != "replacement" {
			t.Fatalf("the source was read: %q is left", rest)
		}
	})

	t.Run("a copy that fails leaves no file", func(t *testing.T) {
		t.Parallel()
		dst := filepath.Join(t.TempDir(), "snapshot.db")
		if _, err := kit.WriteNewFile(dst, &failing{}); !errors.Is(err, errDropped) {
			t.Fatalf("got %v, want the copy's error", err)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a partial file was left: %v", err)
		}
	})
}
