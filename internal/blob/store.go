// SPDX-License-Identifier: Apache-2.0

// Package blob is the content-addressed artifact store. Every object is
// named by the SHA-256 of its bytes, is verified while it is written, and
// only becomes visible under its final name once it is complete.
package blob

import (
	"context"
	"crypto/sha1" //nolint:gosec // git blob ids are SHA-1 by definition; integrity rests on SHA-256.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"
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
}

// Info describes stored bytes. GitOID is the git blob id of the content,
// which is what Hugging Face reports as the ETag of non-LFS files.
type Info struct {
	SHA256 string
	GitOID string
	Size   int64
}

// File is an open blob. It seeks so HTTP range requests can be served.
type File interface {
	io.ReadSeekCloser
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
	Open(ctx context.Context, sha256 string) (File, int64, error)
	Delete(ctx context.Context, sha256 string) error
}

// Presigner is implemented by stores that can hand out a time-limited URL
// from which a client downloads a blob directly, bypassing the registry.
type Presigner interface {
	PresignGet(ctx context.Context, sha256 string, ttl time.Duration) (string, error)
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
	blobDir string
	tmpDir  string
}

// NewFS opens (creating if needed) a filesystem store rooted at root.
// Leftover partial uploads from a previous run are removed.
func NewFS(root string) (*FS, error) {
	s := &FS{
		blobDir: filepath.Join(root, "blobs", "sha256"),
		tmpDir:  filepath.Join(root, "tmp"),
	}
	if err := os.RemoveAll(s.tmpDir); err != nil {
		return nil, fmt.Errorf("clear staging dir: %w", err)
	}
	for _, d := range []string{s.blobDir, s.tmpDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
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
	if !ValidDigest(digest) {
		return "", fmt.Errorf("%w: malformed digest %q", ErrNotFound, digest)
	}
	return filepath.Join(s.blobDir, digest[:2], digest), nil
}

// Stage streams r to a temporary file while hashing it, then checks the
// result against want. Nothing is buffered in memory beyond the copy buffer.
func (s *FS) Stage(ctx context.Context, r io.Reader, want Expect) (Staged, error) {
	tmp, info, err := stageToFile(ctx, s.tmpDir, r, want)
	if err != nil {
		return nil, err
	}
	return &fsStaged{store: s, tmp: tmp, info: info}, nil
}

// stageToFile receives r into a new file under dir, hashing as it goes,
// and checks the result against want. On any failure the file is removed.
// Every backend stages this way, so nothing unverified ever leaves the
// staging directory.
func stageToFile(ctx context.Context, dir string, r io.Reader, want Expect) (string, Info, error) {
	info, path, err := receive(ctx, dir, r, want)
	if err != nil {
		if path != "" {
			_ = os.Remove(path)
		}
		return "", Info{}, err
	}
	return path, info, nil
}

func receive(ctx context.Context, dir string, r io.Reader, want Expect) (Info, string, error) {
	tmp, err := os.CreateTemp(dir, "stage-*")
	if err != nil {
		return Info{}, "", fmt.Errorf("create staging file: %w", err)
	}
	path := tmp.Name()
	defer func() { _ = tmp.Close() }()

	h256 := sha256.New()
	writers := []io.Writer{tmp, h256}
	var hGit hash.Hash
	src := r
	if want.Size >= 0 {
		// The git blob id covers a header naming the length, so it can only be
		// computed in the same pass when the length is known up front.
		hGit = gitHasher(want.Size)
		writers = append(writers, hGit)
		src = io.LimitReader(r, want.Size+1)
	}
	n, err := io.Copy(io.MultiWriter(writers...), &ctxReader{ctx: ctx, r: src})
	if err != nil {
		return Info{}, path, fmt.Errorf("receive blob: %w", err)
	}
	if want.Size >= 0 && n != want.Size {
		return Info{}, path, fmt.Errorf("%w: expected %d bytes, received %s", ErrMismatch, want.Size, received(n, want.Size))
	}
	if err := tmp.Sync(); err != nil {
		return Info{}, path, fmt.Errorf("flush blob: %w", err)
	}
	if hGit == nil {
		hGit = gitHasher(n)
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return Info{}, path, fmt.Errorf("rewind blob: %w", err)
		}
		if _, err := io.Copy(hGit, &ctxReader{ctx: ctx, r: tmp}); err != nil {
			return Info{}, path, fmt.Errorf("hash blob: %w", err)
		}
	}
	info := Info{
		SHA256: hex.EncodeToString(h256.Sum(nil)),
		GitOID: hex.EncodeToString(hGit.Sum(nil)),
		Size:   n,
	}
	if want.SHA256 != "" && want.SHA256 != info.SHA256 {
		return Info{}, path, fmt.Errorf("%w: expected sha256 %s, received %s", ErrMismatch, want.SHA256, info.SHA256)
	}
	if want.GitOID != "" && want.GitOID != info.GitOID {
		return Info{}, path, fmt.Errorf("%w: expected git blob id %s, received %s", ErrMismatch, want.GitOID, info.GitOID)
	}
	return info, path, nil
}

func received(n, want int64) string {
	if n > want {
		return "more"
	}
	return fmt.Sprintf("%d", n)
}

// Open returns the blob and its size.
func (s *FS) Open(_ context.Context, digest string) (File, int64, error) {
	p, err := s.path(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p) //nolint:gosec // path is derived from a validated digest.
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
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
		return err
	}
	return nil
}

type fsStaged struct {
	store *FS
	tmp   string
	info  Info
	done  bool
}

func (st *fsStaged) Info() Info { return st.info }

func (st *fsStaged) Publish(context.Context) error {
	if st.done {
		return nil
	}
	dst, err := st.store.path(st.info.SHA256)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		// Same digest means same bytes: keep the copy already published.
		st.Discard()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(st.tmp, dst); err != nil {
		if _, statErr := os.Stat(dst); statErr == nil {
			st.Discard() // lost a race against an identical upload
			return nil
		}
		return fmt.Errorf("publish blob: %w", err)
	}
	st.done = true
	return nil
}

func (st *fsStaged) Discard() {
	if st.done {
		return
	}
	st.done = true
	_ = os.Remove(st.tmp)
}

// gitHasher returns a SHA-1 primed with the git blob header for size bytes.
func gitHasher(size int64) hash.Hash {
	h := sha1.New() //nolint:gosec // see import note.
	fmt.Fprintf(h, "blob %d\x00", size)
	return h
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
