// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Known git blob ids, from `git hash-object`.
const (
	gitOIDHello = "ce013625030ba8dba906f756967f9e9ca394464a" // "hello\n"
	gitOIDEmpty = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" // ""
)

func TestStageVerifies(t *testing.T) {
	t.Parallel()
	hello := []byte("hello\n")
	tests := []struct {
		name    string
		data    []byte
		want    Expect
		wantErr bool
	}{
		{"no expectations", hello, Expect{Size: -1}, false},
		{"all correct", hello, Expect{SHA256: digest(hello), GitOID: gitOIDHello, Size: 6}, false},
		{"git id checked without a known size", hello, Expect{GitOID: gitOIDHello, Size: -1}, false},
		{"empty file", nil, Expect{GitOID: gitOIDEmpty, Size: 0}, false},
		{"wrong sha256", hello, Expect{SHA256: digest([]byte("other")), Size: -1}, true},
		{"wrong git id", hello, Expect{GitOID: gitOIDEmpty, Size: -1}, true},
		{"shorter than declared", hello, Expect{Size: 7}, true},
		{"longer than declared", hello, Expect{Size: 5}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := NewFS(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Stage(context.Background(), bytes.NewReader(tc.data), tc.want)
			if tc.wantErr {
				if !errors.Is(err, ErrMismatch) {
					t.Fatalf("got error %v, want ErrMismatch", err)
				}
				// A rejected upload must leave nothing behind.
				if left, _ := os.ReadDir(store.tmpDir); len(left) != 0 {
					t.Fatalf("rejected upload left %d staging files", len(left))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info := st.Info()
			if info.SHA256 != digest(tc.data) || info.Size != int64(len(tc.data)) {
				t.Fatalf("info = %+v", info)
			}
			// Not visible until published.
			if _, _, err := store.Open(info.SHA256); !errors.Is(err, ErrNotFound) {
				t.Fatalf("staged blob visible before publish: %v", err)
			}
			if err := st.Publish(); err != nil {
				t.Fatal(err)
			}
			f, size, err := store.Open(info.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			got, _ := io.ReadAll(f)
			if !bytes.Equal(got, tc.data) || size != int64(len(tc.data)) {
				t.Fatalf("read back %q (size %d)", got, size)
			}
		})
	}
}

func TestBlobsAreStoredByDigest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("model weights")
	st, err := store.Stage(context.Background(), bytes.NewReader(data), Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(); err != nil {
		t.Fatal(err)
	}
	d := digest(data)
	if _, err := os.Stat(filepath.Join(root, "blobs", "sha256", d[:2], d)); err != nil {
		t.Fatalf("blob is not at its digest path: %v", err)
	}
}

func TestConcurrentIdenticalUploadsStoreOneCopy(t *testing.T) {
	t.Parallel()
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("same bytes ", 4096))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := store.Stage(context.Background(), bytes.NewReader(data), Expect{SHA256: digest(data), Size: int64(len(data))})
			if err == nil {
				err = st.Publish()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := os.ReadDir(store.tmpDir); len(left) != 0 {
		t.Fatalf("%d staging files left after identical uploads", len(left))
	}
	f, _, err := store.Open(digest(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got, _ := io.ReadAll(f); !bytes.Equal(got, data) {
		t.Fatal("stored copy differs from the upload")
	}
}

func TestMalformedDigestsNeverTouchTheFilesystem(t *testing.T) {
	t.Parallel()
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "abc", "../../etc/passwd", strings.Repeat("Z", 64), strings.Repeat("A", 64)} {
		if _, _, err := store.Open(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", bad, err)
		}
		if err := store.Delete(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q) = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(digest([]byte("never stored"))); err != nil {
		t.Fatalf("deleting an absent blob: %v", err)
	}
}
