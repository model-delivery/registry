// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Check reads a stored blob back, all of it, and compares it with its
// name. Content is verified on the way in (Stage); this is the same
// question asked again later, of what the disk or the bucket holds now.
//
// It returns nil when the blob is intact, an error wrapping ErrNotFound
// when it is not there, and one wrapping ErrMismatch when its size is not
// size or its content no longer hashes to digest. Any other error means
// the blob could not be read, which says nothing about its content. A
// negative size is not checked.
//
// It works through Store.Open alone, so it holds for every backend, and it
// streams: memory use does not depend on the blob's size.
func Check(ctx context.Context, s Store, digest string, size int64) error {
	f, stored, err := s.Open(ctx, digest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if size >= 0 && stored != size {
		// No need to read it: it cannot be the right content.
		return fmt.Errorf("%w: it is %d bytes in storage and should be %d", ErrMismatch, stored, size)
	}
	h := sha256.New()
	n, err := io.Copy(h, &ctxReader{ctx: ctx, r: f})
	if err != nil {
		return fmt.Errorf("read blob %s: %w", digest, err)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("%w: %d bytes could be read and it should be %d", ErrMismatch, n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("%w: its content now has sha256 %s", ErrMismatch, got)
	}
	return nil
}
