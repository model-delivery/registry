// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/kit"
)

// awaitTimeout bounds every wait in these tests. It is reached only when
// something that should have been announced never was.
const awaitTimeout = 10 * time.Second

// staging is one Stage call fed by the test, a piece at a time.
type staging struct {
	tail   *blob.Tail
	feed   *io.PipeWriter
	result chan stageResult
}

type stageResult struct {
	staged blob.Staged
	err    error
}

// startStaging begins staging content the test will send through feed,
// expecting it to be want.
func startStaging(t *testing.T, store blob.Store, want string) *staging {
	t.Helper()
	pr, pw := io.Pipe()
	st := &staging{tail: blob.NewTail(), feed: pw, result: make(chan stageResult, 1)}
	go func() {
		staged, err := store.Stage(context.Background(), pr, blob.Expect{SHA256: kit.SHA256Hex(want), Size: int64(len(want)), Tail: st.tail})
		// What a real sender sees when staging stops reading: an error, not
		// a write that blocks for ever.
		_ = pr.CloseWithError(errors.New("staging has ended"))
		st.result <- stageResult{staged, err}
	}()
	t.Cleanup(func() { _ = pw.Close() })
	return st
}

func (st *staging) send(t *testing.T, piece string) {
	t.Helper()
	if _, err := st.feed.Write([]byte(piece)); err != nil {
		t.Fatalf("staging stopped reading: %v", err)
	}
}

// sendLast sends the final piece and ends the content.
func (st *staging) sendLast(t *testing.T, piece string) {
	t.Helper()
	st.send(t, piece)
	if err := st.feed.Close(); err != nil {
		t.Fatal(err)
	}
}

func await(t *testing.T, f *blob.Follower, off int64, onlyVerified bool) (int64, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), awaitTimeout)
	defer cancel()
	written, verified, err := f.Await(ctx, off, onlyVerified)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("nothing was announced to a follower waiting at %d", off)
	}
	return written, verified, err
}

// readTo reads from off until total bytes have been read, waiting for the
// staging as often as it has to.
func readTo(t *testing.T, f *blob.Follower, off, total int64) []byte {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 7) // small, so a read never happens to cover a whole piece
	for off < total {
		written, _, err := await(t, f, off, false)
		if err != nil {
			t.Fatalf("waiting at %d: %v", off, err)
		}
		for off < written {
			n, err := f.ReadAt(buf, off)
			if err != nil {
				t.Fatalf("reading at %d of %d received: %v", off, written, err)
			}
			out.Write(buf[:n])
			off += int64(n)
		}
	}
	return out.Bytes()
}

func stagingFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Followers read what has arrived, each at its own position, are told when
// it is verified, and can finish after the blob has been published: the
// file they hold open has by then been renamed (filesystem) or removed
// (S3), which must neither fail nor take the bytes away from them.
func TestTailFollowersReadWhileStaging(t *testing.T) {
	t.Parallel()
	const first, second = "the first piece of the file, ", "and the piece that completes it"
	const content = first + second
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			store, dir := b.open(t)
			st := startStaging(t, store, content)

			early := st.tail.Follow()
			if early == nil {
				t.Fatal("a tail that has not ended refused a follower")
			}
			defer early.Close()
			st.send(t, first)
			if got := readTo(t, early, 0, int64(len(first))); string(got) != first {
				t.Fatalf("the early follower read %q", got)
			}
			// Nothing past what has arrived, and nothing is verified yet.
			if n, err := early.ReadAt(make([]byte, 8), int64(len(first))); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("read past what was received: %d bytes, %v", n, err)
			}
			if _, verified, err := await(t, early, 0, false); verified || err != nil {
				t.Fatalf("half a file: verified %v, %v", verified, err)
			}

			// One that joins midway starts from the beginning all the same.
			late := st.tail.Follow()
			defer late.Close()
			var wg sync.WaitGroup
			var lateGot []byte
			wg.Add(1)
			go func() {
				defer wg.Done()
				lateGot = readTo(t, late, 0, int64(len(content)))
			}()
			st.sendLast(t, second)
			res := <-st.result
			if res.err != nil {
				t.Fatal(res.err)
			}
			wg.Wait()
			if string(lateGot) != content {
				t.Fatalf("the follower that joined midway read %q", lateGot)
			}
			if written, verified, err := await(t, early, int64(len(first)), true); !verified || err != nil || written != int64(len(content)) {
				t.Fatalf("after staging: %d received, verified %v, %v", written, verified, err)
			}

			// Published and ended with a follower still holding the file.
			if err := res.staged.Publish(context.Background()); err != nil {
				t.Fatalf("publishing while a follower has the staging file open: %v", err)
			}
			if st.tail.End() {
				t.Fatal("verified bytes were reported as having left unverified")
			}
			if got := readTo(t, early, int64(len(first)), int64(len(content))); string(got) != second {
				t.Fatalf("after publishing, the early follower read %q", got)
			}
			if st.tail.Follow() != nil {
				t.Fatal("a tail that has ended accepted a follower")
			}
			rc, size, err := store.Open(context.Background(), kit.SHA256Hex(content))
			if err != nil || size != int64(len(content)) {
				t.Fatalf("the published blob: size %d, %v", size, err)
			}
			_ = rc.Close()
			early.Close()
			late.Close()
			if n := stagingFiles(t, dir); n != 0 {
				t.Fatalf("%d files left in the staging directory", n)
			}
		})
	}
}

// Content that does not match is never verified. Followers are released
// with an error, read nothing more, and End reports that bytes had left.
func TestTailOfAStagingThatFails(t *testing.T) {
	t.Parallel()
	const want, sent = "what the listing promised", "what actually arrived 123"
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			store, dir := b.open(t)
			st := startStaging(t, store, want)
			reader, bystander := st.tail.Follow(), st.tail.Follow()
			defer reader.Close()
			defer bystander.Close()

			st.send(t, sent[:10])
			if got := readTo(t, reader, 0, 10); string(got) != sent[:10] {
				t.Fatalf("read %q", got)
			}
			waiting := make(chan error, 1)
			go func() {
				_, _, err := bystander.Await(context.Background(), 0, true)
				waiting <- err
			}()
			st.sendLast(t, sent[10:])
			if res := <-st.result; !errors.Is(res.err, blob.ErrMismatch) {
				t.Fatalf("staging altered content: %v", res.err)
			}
			if !st.tail.End() {
				t.Fatal("End did not report the unverified bytes a follower had read")
			}
			if err := <-waiting; !errors.Is(err, blob.ErrTailEnded) {
				t.Fatalf("a follower waiting for verification got %v", err)
			}
			if n, err := reader.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, blob.ErrTailEnded) {
				t.Fatalf("read %d bytes after the staging failed (%v)", n, err)
			}
			if _, _, err := await(t, reader, 0, false); !errors.Is(err, blob.ErrTailEnded) {
				t.Fatalf("waiting after the staging failed: %v", err)
			}
			reader.Close()
			bystander.Close()
			if n := stagingFiles(t, dir); n != 0 {
				t.Fatalf("%d files left in the staging directory", n)
			}
		})
	}
}

// A follower that only waited took nothing, so nothing left unverified.
func TestTailReportsNothingLeftWhenNothingWasRead(t *testing.T) {
	t.Parallel()
	store, _ := backends[0].open(t)
	st := startStaging(t, store, "expected")
	f := st.tail.Follow()
	defer f.Close()
	st.sendLast(t, "tampered")
	if res := <-st.result; !errors.Is(res.err, blob.ErrMismatch) {
		t.Fatalf("staging altered content: %v", res.err)
	}
	if st.tail.End() {
		t.Fatal("End reported unverified bytes although no follower read any")
	}
}

// The content was already stored, so staging never began: followers are
// released at End, and a wait gives up when its context does.
func TestTailThatNeverStaged(t *testing.T) {
	t.Parallel()
	tail := blob.NewTail()
	f := tail.Follow()
	defer f.Close()

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := f.Await(short, 0, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting on a tail with nothing to say: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		_, _, err := f.Await(context.Background(), 0, false)
		released <- err
	}()
	if tail.End() {
		t.Fatal("End reported unverified bytes for a staging that never began")
	}
	if err := <-released; !errors.Is(err, blob.ErrTailEnded) {
		t.Fatalf("released with %v", err)
	}
	if n, err := f.ReadAt(make([]byte, 1), 0); n != 0 || !errors.Is(err, blob.ErrTailEnded) {
		t.Fatalf("read %d bytes of nothing (%v)", n, err)
	}
}
