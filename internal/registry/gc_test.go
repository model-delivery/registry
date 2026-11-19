// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/kit"
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

// stuckStore is a store that cannot remove the blobs it is told to.
type stuckStore struct {
	blob.Store
	mu    sync.Mutex
	stuck map[string]bool
	// lost makes a removal of a stuck blob take effect and still be
	// reported as failed, as when the answer does not make it back.
	lost bool
	// unreachable makes Open fail too, as for a bucket that cannot be reached.
	unreachable bool
	// onDelete, when set, runs before every removal.
	onDelete func()
}

var errStuck = errors.New("the disk is read-only")

// stick sets which blobs cannot be removed.
func (s *stuckStore) stick(digests ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stuck = map[string]bool{}
	for _, d := range digests {
		s.stuck[d] = true
	}
}

func (s *stuckStore) Delete(ctx context.Context, digest string) error {
	s.mu.Lock()
	stuck, lost, onDelete := s.stuck[digest], s.lost, s.onDelete
	s.mu.Unlock()
	if onDelete != nil {
		onDelete()
	}
	if stuck {
		if lost {
			if err := s.Store.Delete(ctx, digest); err != nil {
				return err
			}
		}
		return errStuck
	}
	return s.Store.Delete(ctx, digest)
}

func (s *stuckStore) Open(ctx context.Context, digest string) (io.ReadSeekCloser, int64, error) {
	s.mu.Lock()
	unreachable := s.unreachable
	s.mu.Unlock()
	if unreachable {
		return nil, 0, errors.New("the bucket cannot be reached")
	}
	return s.Store.Open(ctx, digest)
}

// openStuck opens a registry over a store whose removals can be made to fail.
func openStuck(t *testing.T) (*Registry, *stuckStore, blob.Store) {
	t.Helper()
	dir := t.TempDir()
	fs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &stuckStore{Store: fs}
	r, err := Open(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, store, fs
}

// When some blobs are freed and others cannot be, the result counts what
// was removed, and only that, and the error names every blob that was not.
// A blob storage would not remove keeps its record, so it is still a blob
// as far as the registry goes: stored, unreferenced, and found again by the
// next garbage collection. It used to lose its record, and then nothing
// ever looked for its bytes again.
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
			r, store, fs := openStuck(t)

			contents := []string{"first", "the second", "and a third", "the fourth of them"}
			files := []File{file(t, r, "a.bin", contents[0]), file(t, r, "b.bin", contents[1]),
				file(t, r, "c.bin", contents[2]), file(t, r, "d.bin", contents[3])}
			if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: files}); err != nil {
				t.Fatal(err)
			}
			// Two of the four cannot be removed, and they are not next to
			// each other: a failure must not stop the rest, and the second
			// failure must not hide behind the first.
			stuck := map[string]int64{files[1].SHA256: int64(len(contents[1])), files[3].SHA256: int64(len(contents[3]))}
			freedBytes := int64(len(contents[0]) + len(contents[2]))
			stuckBytes := int64(len(contents[1]) + len(contents[3]))
			store.stick(files[1].SHA256, files[3].SHA256)

			// check looks at every file: the ones in stuck are recorded and
			// stored as they were, the others are neither.
			check := func(t *testing.T, stuck map[string]int64) {
				t.Helper()
				for _, f := range files {
					info, recorded, err := r.StatBlob(ctx, f.SHA256)
					if err != nil {
						t.Fatal(err)
					}
					rc, _, openErr := fs.Open(ctx, f.SHA256)
					if openErr == nil {
						_ = rc.Close()
					}
					if size, kept := stuck[f.SHA256]; kept {
						if !recorded || info.Size != size {
							t.Errorf("%s could not be removed and the registry no longer knows it (%+v, recorded %v): no later gc will find it", f.Path, info, recorded)
						}
						if openErr != nil {
							t.Errorf("%s: the blob that could not be removed is gone: %v", f.Path, openErr)
						}
						continue
					}
					if recorded {
						t.Errorf("%s was removed and is still recorded", f.Path)
					}
					if !errors.Is(openErr, blob.ErrNotFound) {
						t.Errorf("%s was not removed from storage: %v", f.Path, openErr)
					}
				}
			}

			res, err := c.drop(r)
			if !errors.Is(err, ErrBlobsNotFreed) || !errors.Is(err, errStuck) {
				t.Fatalf("error = %v, want ErrBlobsNotFreed with the store's failure", err)
			}
			var notFreed *NotFreedError
			if !errors.As(err, &notFreed) || len(notFreed.Failures) != 2 || notFreed.Stuck != 2 {
				t.Fatalf("error = %#v, want both failures", err)
			}
			for digest := range stuck {
				if !strings.Contains(err.Error(), digest) {
					t.Errorf("the error does not name %s, which could not be removed: %v", digest, err)
				}
			}
			if res.Blobs != 2 || res.Bytes != freedBytes {
				t.Fatalf("result = %+v, want the 2 blobs that were removed (%d bytes) and not the 2 that were not", res, freedBytes)
			}
			if _, err := r.GetRepo(ctx, "acme/model"); !errors.Is(err, ErrRepoNotFound) {
				t.Fatalf("the repository is still there: %v", err)
			}
			check(t, stuck)

			// A garbage collection while storage still refuses finds the two
			// again, fails the same way, and loses neither.
			res, err = r.GC(ctx, 0)
			if !errors.Is(err, ErrBlobsNotFreed) || res.Blobs != 0 || res.Bytes != 0 {
				t.Fatalf("gc while storage still refuses: %+v, %v; want nothing freed and the failure", res, err)
			}
			check(t, stuck)

			// Storage is put right: the next collection removes them.
			store.stick()
			res, err = r.GC(ctx, 0)
			if err != nil || res.Blobs != 2 || res.Bytes != stuckBytes {
				t.Fatalf("gc after storage was put right: %+v, %v; want the 2 that were left (%d bytes)", res, err, stuckBytes)
			}
			check(t, nil)
			if st, err := r.Stats(ctx); err != nil || st.Blobs != 0 {
				t.Fatalf("stats = %+v, %v; want no blob left", st, err)
			}
		})
	}
}

// What a failed removal leaves is a blob like any other. These are the ways
// that could go wrong.
func TestABlobThatCouldNotBeRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const content = "storage would not let go of this"

	// leftover makes one unreferenced blob and fails to collect it.
	leftover := func(t *testing.T, r *Registry, store *stuckStore) string {
		t.Helper()
		digest := put(t, r, content).SHA256
		store.stick(digest)
		if res, err := r.GC(ctx, 0); !errors.Is(err, errStuck) || res.Blobs != 0 {
			t.Fatalf("gc = %+v, %v; want the store's failure and nothing freed", res, err)
		}
		return digest
	}

	t.Run("it keeps its age, so the next collection does not wait an hour for it", func(t *testing.T) {
		t.Parallel()
		r, store, _ := openStuck(t)
		clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		r.now = func() time.Time { return clock }
		digest := put(t, r, content).SHA256
		store.stick(digest)
		clock = clock.Add(BlobGrace + time.Minute)
		if res, err := r.GC(ctx, BlobGrace); !errors.Is(err, errStuck) || res.Blobs != 0 {
			t.Fatalf("gc = %+v, %v", res, err)
		}
		store.stick()
		clock = clock.Add(time.Minute)
		if res, err := r.GC(ctx, BlobGrace); err != nil || res.Blobs != 1 || res.Bytes != int64(len(content)) {
			t.Fatalf("the next gc freed %+v, %v; want the blob that was left", res, err)
		}
	})

	t.Run("a revision can come to use it, and then it is kept", func(t *testing.T) {
		t.Parallel()
		r, store, _ := openStuck(t)
		digest := leftover(t, r, store)
		store.stick()
		// The registry says it is stored, and it is.
		if _, ok, err := r.TouchBlob(ctx, digest); err != nil || !ok {
			t.Fatalf("TouchBlob = %v, %v", ok, err)
		}
		if _, err := r.Commit(ctx, CommitInput{Repo: "acme/again", Branch: "main", Files: []File{{Path: "f.bin", SHA256: digest}}}); err != nil {
			t.Fatal(err)
		}
		if res, err := r.GC(ctx, 0); err != nil || res.Blobs != 0 {
			t.Fatalf("gc removed %+v, %v; the blob is referenced now", res, err)
		}
		rc, _, err := r.OpenBlob(ctx, digest)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != content {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a removal that failed after it took effect leaves no record of bytes that are gone", func(t *testing.T) {
		t.Parallel()
		r, store, fs := openStuck(t)
		store.lost = true
		digest := put(t, r, content).SHA256
		store.stick(digest)
		res, err := r.GC(ctx, 0)
		if err != nil || res.Blobs != 1 || res.Bytes != int64(len(content)) {
			t.Fatalf("gc = %+v, %v; the blob is gone, which is what was asked", res, err)
		}
		if _, ok, _ := r.StatBlob(ctx, digest); ok {
			t.Fatal("a record was kept for bytes that are not stored: a push would be told they are")
		}
		if _, _, err := fs.Open(ctx, digest); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("the blob is still stored: %v", err)
		}
	})

	t.Run("storage that cannot be reached at all: the record is kept", func(t *testing.T) {
		t.Parallel()
		r, store, _ := openStuck(t)
		store.mu.Lock()
		store.unreachable = true
		store.mu.Unlock()
		digest := leftover(t, r, store)
		if _, ok, _ := r.StatBlob(ctx, digest); !ok {
			t.Fatal("the record is gone, and the blob with it as far as any later gc can tell")
		}
		store.mu.Lock()
		store.unreachable = false
		store.mu.Unlock()
		store.stick()
		if res, err := r.GC(ctx, 0); err != nil || res.Blobs != 1 {
			t.Fatalf("gc once storage is back: %+v, %v", res, err)
		}
	})

	t.Run("the caller going away mid-removal does not lose the record", func(t *testing.T) {
		t.Parallel()
		r, store, _ := openStuck(t)
		digest := put(t, r, content).SHA256
		store.stick(digest)
		gone, cancel := context.WithCancel(ctx)
		defer cancel()
		store.mu.Lock()
		store.onDelete = cancel // the request ends while storage is asked
		store.mu.Unlock()
		if res, err := r.GC(gone, 0); !errors.Is(err, ErrBlobsNotFreed) || res.Blobs != 0 {
			t.Fatalf("gc = %+v, %v", res, err)
		}
		if _, ok, _ := r.StatBlob(ctx, digest); !ok {
			t.Fatal("the record was not put back because the request had ended")
		}
	})
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
					info, ok, err := r.TouchBlob(ctx, kit.SHA256Hex(content))
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
