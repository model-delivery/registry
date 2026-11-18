// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
)

// hookStore is the filesystem store with a hook that runs when a blob is
// opened, which is the moment Verify starts reading it.
type hookStore struct {
	blob.Store
	mu     sync.Mutex
	onOpen func(digest string) error
}

func (h *hookStore) Open(ctx context.Context, digest string) (blob.File, int64, error) {
	h.mu.Lock()
	hook := h.onOpen
	h.mu.Unlock()
	if hook != nil {
		if err := hook(digest); err != nil {
			return nil, 0, err
		}
	}
	return h.Store.Open(ctx, digest)
}

func (h *hookStore) hook(fn func(digest string) error) {
	h.mu.Lock()
	h.onOpen = fn
	h.mu.Unlock()
}

func openHooked(t *testing.T) (*Registry, *hookStore, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &hookStore{Store: fs}
	r, err := Open(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, store, dir
}

func verifyAll(t *testing.T, r *Registry) (VerifyResult, map[string]BlobCheck) {
	t.Helper()
	checks := map[string]BlobCheck{}
	res, err := r.Verify(context.Background(), func(c BlobCheck) { checks[c.SHA256] = c })
	if err != nil {
		t.Fatal(err)
	}
	return res, checks
}

func TestVerify(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _, dir := openHooked(t)
	used := file(t, r, "weights.bin", strings.Repeat("weights", 1000))
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{used}}); err != nil {
		t.Fatal(err)
	}
	loose := put(t, r, "uploaded, not part of any revision")
	path := func(d string) string { return filepath.Join(dir, "blobs", "sha256", d[:2], d) }

	res, checks := verifyAll(t, r)
	if res != (VerifyResult{Blobs: 2, Bytes: 7000 + loose.Size}) || len(checks) != 2 {
		t.Fatalf("intact: %+v, %d checks", res, len(checks))
	}

	// Damage both: the one a revision uses says which repository; the
	// other has none to name.
	if err := os.WriteFile(path(used.SHA256), []byte(strings.Repeat("WEIGHTS", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path(loose.SHA256)); err != nil {
		t.Fatal(err)
	}
	res, checks = verifyAll(t, r)
	if res.Blobs != 2 || res.Failed != 2 {
		t.Fatalf("damaged: %+v", res)
	}
	if c := checks[used.SHA256]; c.Status != BlobDamaged || len(c.Repos) != 1 || c.Repos[0] != "acme/model" || c.Size != 7000 {
		t.Errorf("damaged blob: %+v", c)
	}
	if c := checks[loose.SHA256]; c.Status != BlobMissing || len(c.Repos) != 0 {
		t.Errorf("missing blob: %+v", c)
	}
}

// A check runs for as long as it takes to read everything. Nothing else may
// wait for it: not a download, not an upload of the very blob being read.
func TestVerifyHoldsNothingWhileItReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, store, _ := openHooked(t)
	const content = "being checked right now"
	info := put(t, r, content)
	other := put(t, r, "another blob")

	// These run while Verify is in the middle of checking a blob. If Verify
	// held the blob's lock, or a lock on the database, they would never
	// return and the test would time out.
	var during []error
	store.hook(func(digest string) error {
		store.hook(nil) // once; the calls below open blobs themselves
		f, _, err := r.OpenBlob(ctx, other.SHA256)
		if err == nil {
			_ = f.Close()
		}
		_, upErr := r.PutBlob(ctx, strings.NewReader(content), blob.Expect{SHA256: info.SHA256, Size: info.Size})
		_, _, touchErr := r.TouchBlob(ctx, digest)
		_, gcErr := r.GC(ctx, time.Hour)
		_, commitErr := r.Commit(ctx, CommitInput{Repo: "acme/during", Branch: "main", Files: []File{{Path: "f", SHA256: info.SHA256}}})
		during = []error{err, upErr, touchErr, gcErr, commitErr}
		return nil
	})
	res, err := r.Verify(ctx, func(BlobCheck) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(during) == 0 {
		t.Fatal("the hook never ran")
	}
	for i, err := range during {
		if err != nil {
			t.Errorf("operation %d during the check: %v", i, err)
		}
	}
	if res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
}

func TestVerifyPassesOverWhatIsRemovedMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, store, _ := openHooked(t)
	doomed := put(t, r, "removed by garbage collection during the check")
	kept := put(t, r, "still here")
	// Garbage collection removes the blob just as the check gets to it.
	store.hook(func(digest string) error {
		if digest == doomed.SHA256 {
			store.hook(nil)
			if res, err := r.GC(ctx, 0); err != nil || res.Blobs != 2 {
				t.Errorf("gc = %+v, %v", res, err)
			}
		}
		return nil
	})
	res, checks := verifyAll(t, r)
	// Both were unreferenced, so both went; neither is reported as missing.
	if res.Failed != 0 || len(checks) > 1 {
		t.Fatalf("result %+v, checks %+v", res, checks)
	}
	if c, ok := checks[kept.SHA256]; ok && c.Status != BlobOK {
		t.Fatalf("the other blob: %+v", c)
	}
}

func TestVerifyReportsWhatItCannotRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, store, _ := openHooked(t)
	a := put(t, r, "first")
	b := put(t, r, "second")
	store.hook(func(digest string) error {
		if digest == a.SHA256 {
			return errors.New("the bucket is not answering")
		}
		return nil
	})
	res, checks := verifyAll(t, r)
	if res.Blobs != 2 || res.Failed != 1 {
		t.Fatalf("result %+v", res)
	}
	// Unreadable is not the same claim as damaged, and the run carries on.
	if c := checks[a.SHA256]; c.Status != BlobUnreadable || !strings.Contains(c.Detail, "not answering") {
		t.Errorf("unreadable blob: %+v", c)
	}
	if c := checks[b.SHA256]; c.Status != BlobOK {
		t.Errorf("the other blob: %+v", c)
	}

	// Stopping the request stops the run, with an error and not a verdict.
	store.hook(nil)
	stopping, cancel := context.WithCancel(ctx)
	seen := 0
	_, err := r.Verify(stopping, func(BlobCheck) {
		seen++
		cancel()
	})
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("cancelled run: %v after %d blobs", err, seen)
	}
}
