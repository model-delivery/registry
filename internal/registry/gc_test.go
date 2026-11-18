// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
)

// A push stores its files, or is told they are already stored, and then
// records its revision. If the only repository holding one of those files
// is deleted in between, the file used to be removed and the push failed
// with "blob has not been uploaded" for content it had just been told was
// there.
func TestDeleteRepoSparesBlobsAWriterIsAboutToReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const content = "used by one repository, and wanted by a push to another"

	writers := []struct {
		name  string
		start func(t *testing.T, r *Registry, digest string)
	}{
		{"a push told the file is already stored", func(t *testing.T, r *Registry, digest string) {
			if _, ok, err := r.TouchBlob(ctx, digest); err != nil || !ok {
				t.Fatalf("TouchBlob = %v, %v", ok, err)
			}
		}},
		{"a push that sent the file again", func(t *testing.T, r *Registry, digest string) {
			if info := put(t, r, content); info.SHA256 != digest {
				t.Fatalf("stored as %s", info.SHA256)
			}
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			r := open(t)
			wanted := file(t, r, "wanted.bin", content)
			const otherContent = "used by the one repository only"
			other := file(t, r, "other.bin", otherContent)
			if _, err := r.Commit(ctx, CommitInput{Repo: "acme/old", Branch: "main", Files: []File{wanted, other}}); err != nil {
				t.Fatal(err)
			}

			w.start(t, r, wanted.SHA256)
			res, err := r.DeleteRepo(ctx, "acme/old")
			if err != nil {
				t.Fatal(err)
			}
			if res.Blobs != 1 || res.Bytes != int64(len(otherContent)) {
				t.Fatalf("delete freed %+v, want only the file nobody is waiting for", res)
			}
			if _, ok, _ := r.StatBlob(ctx, other.SHA256); ok {
				t.Fatal("the file nobody is waiting for was kept")
			}
			// The push now records its revision, and the content is there.
			if _, err := r.Commit(ctx, CommitInput{Repo: "acme/new", Branch: "main", Files: []File{{Path: "wanted.bin", SHA256: wanted.SHA256}}}); err != nil {
				t.Fatalf("the push failed after the other repository was deleted: %v", err)
			}
			rc, _, err := r.OpenBlob(ctx, wanted.SHA256)
			if err != nil {
				t.Fatalf("the content the push referenced is gone: %v", err)
			}
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(got) != content {
				t.Fatalf("content = %q", got)
			}
			// Recorded, the wait is over: deleting its repository frees it at
			// once, as deleting any repository frees what it alone holds.
			if res, err := r.DeleteRepo(ctx, "acme/new"); err != nil || res.Blobs != 1 {
				t.Fatalf("deleting the push's repository freed %+v, %v; want its one file", res, err)
			}
			if _, _, err := r.OpenBlob(ctx, wanted.SHA256); !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("the file is still stored: %v", err)
			}
		})
	}

	t.Run("a writer that never finishes holds a file no longer than garbage collection would", func(t *testing.T) {
		t.Parallel()
		r := open(t)
		clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		r.now = func() time.Time { return clock }
		for _, repo := range []string{"acme/first", "acme/second"} {
			if _, err := r.Commit(ctx, CommitInput{Repo: repo, Branch: "main", Files: []File{file(t, r, "wanted.bin", content)}}); err != nil {
				t.Fatal(err)
			}
		}
		digest := put(t, r, content).SHA256 // a push starts, and is abandoned
		clock = clock.Add(BlobGrace - time.Minute)
		if res, err := r.DeleteRepo(ctx, "acme/first"); err != nil || res.Blobs != 0 {
			t.Fatalf("delete while another repository uses the file: %+v, %v", res, err)
		}
		clock = clock.Add(2 * time.Minute)
		res, err := r.DeleteRepo(ctx, "acme/second")
		if err != nil || res.Blobs != 1 {
			t.Fatalf("delete after the grace period freed %+v, %v; want the file", res, err)
		}
		if _, ok, _ := r.StatBlob(ctx, digest); ok {
			t.Fatal("the file is still recorded")
		}
	})

	t.Run("a file spared for an abandoned push is collected later", func(t *testing.T) {
		t.Parallel()
		r := open(t)
		clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		r.now = func() time.Time { return clock }
		wanted := file(t, r, "wanted.bin", content)
		if _, err := r.Commit(ctx, CommitInput{Repo: "acme/old", Branch: "main", Files: []File{wanted}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.TouchBlob(ctx, wanted.SHA256); err != nil {
			t.Fatal(err)
		}
		if res, err := r.DeleteRepo(ctx, "acme/old"); err != nil || res.Blobs != 0 {
			t.Fatalf("delete freed %+v, %v; want the awaited file spared", res, err)
		}
		if res, err := r.GC(ctx, BlobGrace); err != nil || res.Blobs != 0 {
			t.Fatalf("gc within the grace period removed %+v, %v", res, err)
		}
		clock = clock.Add(BlobGrace + time.Minute)
		if res, err := r.GC(ctx, BlobGrace); err != nil || res.Blobs != 1 {
			t.Fatalf("gc after the grace period removed %+v, %v; want the spared file", res, err)
		}
	})
}

func sha256Of(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// stuckStore is a store that cannot remove the blobs it is told to.
type stuckStore struct {
	blob.Store
	mu    sync.Mutex
	stuck map[string]bool
}

var errStuck = errors.New("the disk is read-only")

func (s *stuckStore) Delete(ctx context.Context, digest string) error {
	s.mu.Lock()
	stuck := s.stuck[digest]
	s.mu.Unlock()
	if stuck {
		return errStuck
	}
	return s.Store.Delete(ctx, digest)
}

// When some blobs are freed and one cannot be, the result still counts
// what was done and the error names what was not. Nothing is left
// half-visible: the records are gone either way, so a file that could not
// be removed is wasted space, never a reference to content that is not
// there.
func TestRemovingBlobsReportsWhatItDidAndWhatFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callers := []struct {
		name string
		// drop makes the blobs unreferenced and removes them.
		drop func(r *Registry) (GCResult, error)
	}{
		{"DeleteRepo", func(r *Registry) (GCResult, error) { return r.DeleteRepo(ctx, "acme/model") }},
		{"GC", func(r *Registry) (GCResult, error) {
			if _, err := r.db.ExecContext(ctx, "DELETE FROM repos"); err != nil {
				return GCResult{}, err
			}
			return r.GC(ctx, 0)
		}},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fs, err := blob.NewFS(dir)
			if err != nil {
				t.Fatal(err)
			}
			store := &stuckStore{Store: fs, stuck: map[string]bool{}}
			r, err := Open(dir, store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })

			contents := []string{"first", "the second", "and a third"}
			files := []File{file(t, r, "a.bin", contents[0]), file(t, r, "b.bin", contents[1]), file(t, r, "c.bin", contents[2])}
			if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: files}); err != nil {
				t.Fatal(err)
			}
			stuck := files[1]
			store.mu.Lock()
			store.stuck[stuck.SHA256] = true
			store.mu.Unlock()

			res, err := c.drop(r)
			if !errors.Is(err, errStuck) || !strings.Contains(err.Error(), stuck.SHA256) {
				t.Fatalf("error = %v, want the failure, naming the blob that could not be removed", err)
			}
			var total int64
			for _, c := range contents {
				total += int64(len(c))
			}
			if res.Blobs != len(files) || res.Bytes != total {
				t.Fatalf("result = %+v, want all %d records forgotten (%d bytes): the failure does not stop the rest", res, len(files), total)
			}
			if _, err := r.GetRepo(ctx, "acme/model"); !errors.Is(err, ErrRepoNotFound) {
				t.Fatalf("the repository is still there: %v", err)
			}
			for _, f := range files {
				// No record is left, so nothing can come to reference content
				// whose removal was attempted.
				if _, ok, _ := r.StatBlob(ctx, f.SHA256); ok {
					t.Errorf("%s is still recorded", f.Path)
				}
				_, _, err := fs.Open(ctx, f.SHA256)
				if f.SHA256 == stuck.SHA256 {
					if err != nil {
						t.Errorf("%s: the blob that could not be removed is gone: %v", f.Path, err)
					}
				} else if !errors.Is(err, blob.ErrNotFound) {
					t.Errorf("%s: the blob after the one that failed was not removed: %v", f.Path, err)
				}
			}
			// The leftover bytes are not a blob as far as the registry goes:
			// sending the content again stores and records it afresh.
			store.mu.Lock()
			delete(store.stuck, stuck.SHA256)
			store.mu.Unlock()
			if info := put(t, r, "the second"); info.SHA256 != stuck.SHA256 {
				t.Fatalf("stored again as %s", info.SHA256)
			}
			if _, err := r.Commit(ctx, CommitInput{Repo: "acme/again", Branch: "main", Files: []File{{Path: "b.bin", SHA256: stuck.SHA256}}}); err != nil {
				t.Fatalf("recording the content again: %v", err)
			}
		})
	}
}

// Publishing a blob and removing it take turns on that digest's lock.
// These run a push (store the content, then record a revision naming it)
// against removal of the same digest, many times, and check the one thing
// that must never happen: a recorded revision whose content is not stored.
// Run with -race.
func TestRemovalRacingAPushOfTheSameDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const rounds = 40

	// checkRecorded fails if any recorded revision names content that
	// cannot be read back intact.
	checkRecorded := func(t *testing.T, r *Registry) {
		t.Helper()
		res, err := r.Verify(ctx, func(c BlobCheck) {
			if c.Status != BlobOK {
				t.Errorf("blob %s used by %v is %s: %s", c.SHA256, c.Repos, c.Status, c.Detail)
			}
		})
		if err != nil || res.Failed != 0 {
			t.Fatalf("verify = %+v, %v", res, err)
		}
		repos, err := r.ListRepos(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, repo := range repos {
			c, err := r.Resolve(ctx, repo.Name, "main")
			if err != nil {
				t.Fatal(err)
			}
			files, err := r.Files(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				rc, _, err := r.OpenBlob(ctx, f.SHA256)
				if err != nil {
					t.Fatalf("%s lists %s, whose content is not stored: %v", repo.Name, f.Path, err)
				}
				_ = rc.Close()
			}
		}
	}

	t.Run("garbage collection", func(t *testing.T) {
		t.Parallel()
		r := open(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		recorded, refused := 0, 0
		for i := range rounds {
			content := fmt.Sprintf("round %d", i)
			wg.Add(2)
			go func() {
				defer wg.Done()
				info, err := r.PutBlob(ctx, strings.NewReader(content), blob.Expect{Size: -1})
				if err != nil {
					t.Errorf("round %d: store: %v", i, err)
					return
				}
				_, err = r.Commit(ctx, CommitInput{Repo: fmt.Sprintf("acme/model-%d", i), Branch: "main", Files: []File{{Path: "f", SHA256: info.SHA256}}})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					recorded++
				case errors.Is(err, ErrBlobMissing):
					// Collected before it was referenced: with no age guard
					// that is allowed, and the push is told so.
					refused++
				default:
					t.Errorf("round %d: record: %v", i, err)
				}
			}()
			go func() {
				defer wg.Done()
				// No age guard: every unreferenced blob is a candidate.
				if _, err := r.GC(ctx, 0); err != nil {
					t.Errorf("round %d: gc: %v", i, err)
				}
			}()
		}
		wg.Wait()
		if recorded+refused != rounds {
			t.Fatalf("%d recorded + %d refused, want %d outcomes", recorded, refused, rounds)
		}
		checkRecorded(t, r)
		if repos, _ := r.ListRepos(ctx); len(repos) != recorded {
			t.Fatalf("%d repositories for %d recorded revisions", len(repos), recorded)
		}
	})

	t.Run("deleting the only repository that holds it", func(t *testing.T) {
		t.Parallel()
		r := open(t)
		for i := range rounds {
			content := fmt.Sprintf("round %d", i)
			if _, err := r.Commit(ctx, CommitInput{Repo: fmt.Sprintf("acme/old-%d", i), Branch: "main", Files: []File{file(t, r, "f", content)}}); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		for i := range rounds {
			content := fmt.Sprintf("round %d", i)
			wg.Add(2)
			go func() {
				defer wg.Done()
				// Half the pushes send the content again, half ask and are
				// told it is stored. Either way the revision must land: the
				// push did nothing wrong.
				var digest string
				if i%2 == 0 {
					info, err := r.PutBlob(ctx, strings.NewReader(content), blob.Expect{Size: -1})
					if err != nil {
						t.Errorf("round %d: store: %v", i, err)
						return
					}
					digest = info.SHA256
				} else {
					info, ok, err := r.TouchBlob(ctx, sha256Of(content))
					if err != nil {
						t.Errorf("round %d: ask: %v", i, err)
						return
					}
					if !ok {
						// Already removed with its repository: send it.
						if info, err = r.PutBlob(ctx, strings.NewReader(content), blob.Expect{Size: -1}); err != nil {
							t.Errorf("round %d: store: %v", i, err)
							return
						}
					}
					digest = info.SHA256
				}
				if _, err := r.Commit(ctx, CommitInput{Repo: fmt.Sprintf("acme/new-%d", i), Branch: "main", Files: []File{{Path: "f", SHA256: digest}}}); err != nil {
					t.Errorf("round %d: the push failed: %v", i, err)
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := r.DeleteRepo(ctx, fmt.Sprintf("acme/old-%d", i)); err != nil {
					t.Errorf("round %d: delete: %v", i, err)
				}
			}()
		}
		wg.Wait()
		checkRecorded(t, r)
		if repos, _ := r.ListRepos(ctx); len(repos) != rounds {
			t.Fatalf("%d repositories, want the %d the pushes recorded", len(repos), rounds)
		}
	})
}
