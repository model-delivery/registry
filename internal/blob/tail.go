// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
)

// ErrTailEnded is what a follower is told when the staging it follows has
// ended without the bytes being verified: it failed, was cancelled, or
// never took place because the content was already stored.
var ErrTailEnded = errors.New("the staging being followed ended before its content was verified")

// Tail lets readers follow a blob while it is being staged: each reads the
// bytes already received, at its own position, and waits for more.
//
// It exists for one caller, pull-through, which sends a file to its first
// clients while the file is still arriving from the upstream. The bytes a
// follower reads before Verified are exactly that: not verified. The Tail
// says when they are, and reports at the end whether any left unverified
// (End), and what is done with that is the caller's rule, not the store's.
// Nothing here publishes anything: a blob still becomes visible only
// through Staged.Publish, after Stage has checked it.
//
// The one who starts the staging hands the Tail to Stage in Expect.Tail and
// must call End when the whole operation is over, whether or not staging
// ever began. A Tail follows one staging.
type Tail struct {
	mu sync.Mutex
	// wake is closed, and dropped, when anything below changes. It is only
	// made when a follower is about to wait, so a staging nobody follows
	// allocates nothing per write.
	wake chan struct{}
	// file is the staging file, open for reading. It is opened when staging
	// begins and closed once the tail has ended and no follower is left, so
	// a follower can finish reading verified bytes after the file has been
	// published under another name or removed.
	file      *os.File
	written   int64
	verified  bool
	ended     bool
	followers int
	// leaked is set when a follower read bytes that were not verified yet.
	leaked bool
}

// NewTail returns a Tail for one staging.
func NewTail() *Tail { return &Tail{} }

func (t *Tail) signal() {
	if t.wake != nil {
		close(t.wake)
		t.wake = nil
	}
}

// begin is called by staging once its file exists. If the file cannot be
// opened a second time the staging carries on unfollowed: followers see no
// bytes and are released at End.
func (t *Tail) begin(path string) {
	file, err := openShared(path)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended || t.file != nil {
		_ = file.Close()
		return
	}
	t.file = file
}

// wrote is called by staging after n more bytes are in the file.
func (t *Tail) wrote(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file == nil {
		return
	}
	t.written += int64(n)
	t.signal()
}

// verify is called by staging once everything received has matched what
// was expected of it.
func (t *Tail) verify() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file == nil {
		return
	}
	t.verified = true
	t.signal()
}

// End says the operation the Tail belongs to is over. Followers waiting are
// released: with the rest of the bytes if they were verified, and with
// ErrTailEnded if not, after which no unverified byte is handed out.
//
// It reports whether unverified bytes left: true when a follower read some
// and they were never verified.
func (t *Tail) End() (unverifiedBytesLeft bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ended = true
	t.signal()
	t.closeIfUnused()
	return t.leaked && !t.verified
}

// closeIfUnused closes the staging file once nothing can read it again.
func (t *Tail) closeIfUnused() {
	if t.ended && t.followers == 0 && t.file != nil {
		_ = t.file.Close()
		t.file = nil
	}
}

// Follower is one reader of a Tail. Close it when done.
type Follower struct {
	t      *Tail
	closed bool
}

// Follow returns a reader of the staging, or nil when the tail has ended:
// then the blob is either stored, to be read from the store, or absent.
func (t *Tail) Follow() *Follower {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return nil
	}
	t.followers++
	return &Follower{t: t}
}

// Await blocks until there is something new for a reader at off, and
// returns how many bytes have been received and whether they are verified.
// It returns when the content has been verified (all of it is then
// readable), or, unless onlyVerified, when more than off bytes have been
// received. If the staging ends unverified first it fails with
// ErrTailEnded, and if ctx ends first, with ctx's error.
func (f *Follower) Await(ctx context.Context, off int64, onlyVerified bool) (written int64, verified bool, err error) {
	t := f.t
	for {
		t.mu.Lock()
		written = t.written
		switch {
		case t.verified:
			t.mu.Unlock()
			return written, true, nil
		case t.ended:
			t.mu.Unlock()
			return written, false, ErrTailEnded
		case !onlyVerified && written > off:
			t.mu.Unlock()
			return written, false, nil
		}
		if t.wake == nil {
			t.wake = make(chan struct{})
		}
		wake := t.wake
		t.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
}

// ReadAt reads received bytes. It never reads past what has been received
// (io.EOF there), and once the staging has ended unverified it reads
// nothing and fails with ErrTailEnded.
func (f *Follower) ReadAt(p []byte, off int64) (int, error) {
	t := f.t
	t.mu.Lock()
	if t.file == nil || (t.ended && !t.verified) {
		t.mu.Unlock()
		return 0, ErrTailEnded
	}
	if left := t.written - off; left <= 0 {
		t.mu.Unlock()
		return 0, io.EOF
	} else if int64(len(p)) > left {
		p = p[:left]
	}
	if !t.verified {
		// Noted before the read, under the lock End takes: either End sees
		// it, or this read comes after End and was refused above.
		t.leaked = true
	}
	file := t.file
	t.mu.Unlock()
	// The file stays open while this follower is: see closeIfUnused.
	return file.ReadAt(p, off)
}

// Close releases the follower.
func (f *Follower) Close() {
	t := f.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	t.followers--
	t.closeIfUnused()
}

// tailWriter announces to a Tail what staging has written to its file. It
// goes after the file in the list of writers, so the bytes are there.
type tailWriter struct{ t *Tail }

func (w tailWriter) Write(p []byte) (int, error) {
	w.t.wrote(len(p))
	return len(p), nil
}
