// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// A file moves from the pending table to the stored one when its content
// arrives. A reader looking at that moment must find it in one or the
// other: looked up one table after the other, it was in neither, and a
// download waiting for the file was answered "no such file".
func TestAFileBeingFulfilledIsNeverMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	const count = 60
	in := LazyInput{Repo: "acme/model", Branch: "main", SHA: upstreamSHA, Upstream: "https://hub.example"}
	for i := range count {
		stored := put(t, r, fmt.Sprintf("content of file %d", i))
		in.Files = append(in.Files, File{Path: fmt.Sprintf("file-%02d.bin", i), Size: stored.Size, SHA256: stored.SHA256, GitOID: stored.GitOID, LFS: true})
	}
	c, err := r.CommitLazy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, want := range in.Files {
					if _, err := r.File(ctx, c, want.Path); err != nil {
						t.Errorf("File(%s) while files are being fulfilled: %v", want.Path, err)
						return
					}
				}
				files, err := r.Files(ctx, c)
				if err != nil || len(files) != count {
					t.Errorf("Files while files are being fulfilled: %d of %d, %v", len(files), count, err)
					return
				}
			}
		}()
	}
	for _, f := range in.Files {
		if err := r.Fulfil(ctx, c, f.Path, f.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	readers.Wait()

	// And at the end every file is a stored one.
	files, err := r.Files(ctx, c)
	if err != nil || len(files) != count {
		t.Fatalf("%d files, %v", len(files), err)
	}
	for _, f := range files {
		if f.Pending {
			t.Fatalf("%s is still pending", f.Path)
		}
		if got, err := r.File(ctx, c, f.Path); err != nil || got != f {
			t.Fatalf("File(%s) = %+v, %v; the listing says %+v", f.Path, got, err, f)
		}
	}
}
