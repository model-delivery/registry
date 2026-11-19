// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package blob is the content-addressed artifact store. Every object is
// named by the SHA-256 of its bytes, is verified while it is written, and
// only becomes visible under its final name once it is complete.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/model-delivery/registry/internal/datadir"
	"github.com/model-delivery/registry/internal/kit"
)

// ErrMismatch reports that received bytes did not match what was expected.
var ErrMismatch = errors.New("blob verification failed")

// ErrNotFound reports that no blob with the given digest is stored.
var ErrNotFound = errors.New("blob not found")

// Expect is what a caller already knows about the bytes it is about to send.
// Empty fields (and a negative Size) are not checked.
type Expect struct {
	SHA256 string
	GitOID string
	Size   int64
	// Tail, when set, is told how the staging goes, so that readers can
	// follow the bytes while they arrive (see Tail). It is not a check, and
	// what is staged is verified and published the same with or without it.
	Tail *Tail
}

// Info describes stored bytes. GitOID is the git blob id of the content,
// which is what Hugging Face reports as the ETag of non-LFS files.
type Info struct {
	SHA256 string
	GitOID string
	Size   int64
}

// Staged is a fully received and verified blob that is not yet visible.
type Staged interface {
	Info() Info
	// Publish makes the blob visible under its digest. It is idempotent.
	Publish(ctx context.Context) error
	// Discard drops the staged bytes. It is safe to call after Publish.
	Discard()
}

// Store is the storage backend contract, implemented by FS (local disk)
// and S3 (any S3-compatible object store).
type Store interface {
	Stage(ctx context.Context, r io.Reader, want Expect) (Staged, error)
	// Open returns a stored blob and its size. The blob seeks, so HTTP
	// range requests can be served from it.
	Open(ctx context.Context, sha256 string) (io.ReadSeekCloser, int64, error)
	Delete(ctx context.Context, sha256 string) error
	// Every backend also keeps metadata backups next to its blobs.
	Archive
}

// Presigner is implemented by stores that can hand out a time-limited URL
// from which a client downloads a blob directly, bypassing the registry.
type Presigner interface {
	PresignGet(ctx context.Context, sha256 string, ttl time.Duration) (string, error)
}

// The kinds of store, as Kind names them.
const (
	KindFilesystem = "filesystem"
	KindS3         = "s3"
	KindOther      = "other"
)

// Kind names the backend a store is: KindFilesystem, KindS3, or KindOther
// for anything else, such as a store wrapped by a test. A new backend joins
// here.
func Kind(s Store) string {
	switch s.(type) {
	case *FS:
		return KindFilesystem
	case *S3:
		return KindS3
	}
	return KindOther
}

// blobKey is the path of a blob relative to a store's root:
// sha256/<first two hex>/<digest>.
func blobKey(digest string) (string, error) {
	if !ValidDigest(digest) {
		return "", fmt.Errorf("%w: malformed digest %q", ErrNotFound, digest)
	}
	return "sha256/" + digest[:2] + "/" + digest, nil
}

// FS stores blobs as files under root/blobs/sha256/<2 hex>/<digest>.
type FS struct {
	root datadir.Dir
}

// NewFS opens (creating if needed) a filesystem store in the data
// directory root. It removes nothing: what a previous run left in the
// staging directory is cleared by datadir.Dir.ClearStaging, which only the
// server calls.
func NewFS(root string) (*FS, error) {
	s := &FS{root: datadir.Dir(root)}
	for _, d := range []string{s.root.Blobs(), s.root.Staging()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("fs: create %s: %w", d, err)
		}
	}
	return s, nil
}

// ValidDigest reports whether s is a lowercase hex SHA-256.
func ValidDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *FS) path(digest string) (string, error) {
	key, err := blobKey(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root.Blobs(), filepath.FromSlash(key)), nil
}

// Stage streams r to a temporary file while hashing it, then checks the
// result against want. Nothing is buffered in memory beyond the copy buffer.
func (s *FS) Stage(ctx context.Context, r io.Reader, want Expect) (Staged, error) {
	return stage(ctx, s.root.Staging(), r, want, s.publish)
}

// publish moves a staged file to its digest path. A rename is atomic, so
// the blob is either fully present under its name or absent.
func (s *FS) publish(_ context.Context, tmp string, info Info) error {
	dst, err := s.path(info.SHA256)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil // same digest means same bytes: keep the copy already published
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("fs: publish %s: %w", info.SHA256, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil // lost a race against an identical upload
		}
		return fmt.Errorf("fs: publish %s: %w", info.SHA256, err)
	}
	return nil
}

// staged is a received and verified upload sitting in a staging directory.
// Every backend stages the same way and differs only in how it publishes.
type staged struct {
	tmp  string
	info Info
	// publish makes the staged file visible under its digest. It may move
	// the file away or leave it where it is; Publish removes what is left.
	publish func(ctx context.Context, tmp string, info Info) error
	done    bool
}

// stage receives and verifies r into dir (see stageToFile) and returns it
// ready to be made visible with publish.
func stage(ctx context.Context, dir string, r io.Reader, want Expect,
	publish func(ctx context.Context, tmp string, info Info) error) (Staged, error) {
	tmp, info, err := stageToFile(ctx, dir, r, want)
	if err != nil {
		return nil, err
	}
	return &staged{tmp: tmp, info: info, publish: publish}, nil
}

func (st *staged) Info() Info { return st.info }

func (st *staged) Publish(ctx context.Context) error {
	if st.done {
		return nil
	}
	if err := st.publish(ctx, st.tmp, st.info); err != nil {
		return err
	}
	st.Discard()
	return nil
}

func (st *staged) Discard() {
	if st.done {
		return
	}
	st.done = true
	_ = os.Remove(st.tmp)
}

// stageToFile receives r into a new file under dir, hashing as it goes,
// and checks the result against want. On any failure the file is removed.
// Every backend stages this way, so nothing unverified ever leaves the
// staging directory.
func stageToFile(ctx context.Context, dir string, r io.Reader, want Expect) (path string, info Info, err error) {
	tmp, err := os.CreateTemp(dir, "stage-*")
	if err != nil {
		return "", Info{}, fmt.Errorf("create staging file: %w", err)
	}
	path = tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(path)
			path, info = "", Info{}
		}
	}()

	h256 := sha256.New()
	writers := []io.Writer{tmp, h256}
	if want.Tail != nil {
		// After the file in the list, so a follower is only ever told of
		// bytes that are already in it.
		want.Tail.begin(path)
		writers = append(writers, tailWriter{want.Tail})
	}
	var hGit hash.Hash
	src := r
	if want.Size >= 0 {
		// The git blob id covers a header naming the length, so it can only be
		// computed in the same pass when the length is known up front.
		hGit = kit.GitBlobHasher(want.Size)
		writers = append(writers, hGit)
		src = io.LimitReader(r, want.Size+1)
	}
	n, err := io.Copy(io.MultiWriter(writers...), &ctxReader{ctx: ctx, r: src})
	if err != nil {
		return path, Info{}, fmt.Errorf("receive blob: %w", err)
	}
	if want.Size >= 0 && n != want.Size {
		return path, Info{}, fmt.Errorf("%w: expected %d bytes, received %s", ErrMismatch, want.Size, received(n, want.Size))
	}
	if err := tmp.Sync(); err != nil {
		return path, Info{}, fmt.Errorf("flush blob: %w", err)
	}
	if hGit == nil {
		hGit = kit.GitBlobHasher(n)
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return path, Info{}, fmt.Errorf("rewind blob: %w", err)
		}
		if _, err := io.Copy(hGit, &ctxReader{ctx: ctx, r: tmp}); err != nil {
			return path, Info{}, fmt.Errorf("hash blob: %w", err)
		}
	}
	info = Info{
		SHA256: hex.EncodeToString(h256.Sum(nil)),
		GitOID: hex.EncodeToString(hGit.Sum(nil)),
		Size:   n,
	}
	if want.SHA256 != "" && want.SHA256 != info.SHA256 {
		return path, Info{}, fmt.Errorf("%w: expected sha256 %s, received %s", ErrMismatch, want.SHA256, info.SHA256)
	}
	if want.GitOID != "" && want.GitOID != info.GitOID {
		return path, Info{}, fmt.Errorf("%w: expected git blob id %s, received %s", ErrMismatch, want.GitOID, info.GitOID)
	}
	if want.Tail != nil {
		// Every check has passed: from here the bytes are what was expected.
		want.Tail.verify()
	}
	return path, info, nil
}

func received(n, want int64) string {
	if n > want {
		return "more"
	}
	return fmt.Sprintf("%d", n)
}

// Open returns the blob and its size.
func (s *FS) Open(_ context.Context, digest string) (io.ReadSeekCloser, int64, error) {
	p, err := s.path(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p) //nolint:gosec // path is derived from a validated digest.
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("fs: open %s: %w", digest, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("fs: stat %s: %w", digest, err)
	}
	return f, fi.Size(), nil
}

// Delete removes a blob. Deleting a blob that is not there is not an error.
func (s *FS) Delete(_ context.Context, digest string) error {
	p, err := s.path(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fs: delete %s: %w", digest, err)
	}
	return nil
}

// ctxReader stops a long copy when its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
